package subtitles

// Subtitle providers, called directly (never through Plex): the OpenSubtitles legacy REST API (free, no key) and
// OpenSubtitles.com (only when its API key file exists). Ranking, the file hash, the release name and the ad
// stripping are ports of subfetch2.py. Every request goes through a pacer: one request per second per provider,
// exponential backoff on 429/5xx/network errors, and a circuit breaker that parks the provider for the rest of the
// run once its download quota is spent or it keeps failing, so the remaining titles wait for the next run.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Query is what a provider may search by.
type Query struct {
	Size    int64
	Hash    string // OpenSubtitles hash, 16 hex digits
	IMDb    int    // 0 when unknown
	Name    string // show name, for the text search of episodes
	Season  int    // 0 for a movie
	Episode int
}

// Candidate is one downloadable subtitle, normalised across providers to the fields rank() reads.
type Candidate struct {
	Source          string // which search found it: opensubtitles-hash | -imdb | -query | opensubtitles-com
	ID              string // provider file id
	Release         string
	FileName        string
	HashMatch       bool
	HearingImpaired bool
	Downloads       int
	Encoding        string
	Season, Episode string
	unusable        bool // not SRT, foreign parts only or marked bad: counted by the search logic, never tried
	link            string
	provider        Provider
}

type Provider interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Candidate, error)
	Download(ctx context.Context, c Candidate) (string, error) // decoded text
}

var (
	// errParked: the provider is out for the rest of this run (quota spent or persistently failing).
	errParked = errors.New("provider parked for this run")
	errQuota  = errors.New("download quota spent")
)

// ---- ports of subfetch2.py ----

// openSubtitlesHash is oshash(): the file size plus the sums of the 64-bit little-endian words of the first and the
// last 64 KiB, modulo 2^64.
func openSubtitlesHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := st.Size()
	h := uint64(size)
	buf := make([]byte, 65536)
	for _, pos := range []int64{0, max(0, size-65536)} {
		n, err := f.ReadAt(buf, pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", 0, err
		}
		for i := 0; i+8 <= n; i += 8 {
			h += binary.LittleEndian.Uint64(buf[i:])
		}
	}
	return fmt.Sprintf("%016x", h), size, nil
}

var (
	// The Python's lookahead (?= |$) consumes the space here; no match can start with a space, so findall is unchanged.
	groupRe      = regexp.MustCompile(`-([a-z0-9]+)(?:\[[^\]]*\])?(?:\.[a-z0-9]{2,4})?(?: |$)`)
	tokenRe      = regexp.MustCompile(`[a-z0-9]+`)
	sourceTokens = []string{"bluray", "remux", "web", "webrip", "hdtv", "dvdrip", "amzn", "nf", "dsnp", "hmax", "atvp"}
	originalRe   = regexp.MustCompile(`(?m)^Original (?:name|folder):` + pySpace + `*(.+?)` + pySpace + `*$`)
	adBlockRe    = regexp.MustCompile(`(?i)opensubtitles|advertise your product`)
	blankLineRe  = regexp.MustCompile(`\n` + pySpace + `*\n`)
)

// rank orders candidates: a file-hash match first, then the release group, then source tags in common, then
// popularity; hearing-impaired copies lose 20000.
func rank(c Candidate, release string) int {
	t := pyLower(c.Release + " " + c.FileName)
	score := 0
	if c.HashMatch {
		score += 10_000_000
	}
	if c.HearingImpaired {
		score -= 20_000
	}
	stem := pyLower(release)
	if g := groupRe.FindStringSubmatch(stem); g != nil && strings.Contains(t, g[1]) {
		score += 100_000
	}
	toks := map[string]bool{}
	for _, k := range tokenRe.FindAllString(stem, -1) {
		toks[k] = true
	}
	for _, k := range sourceTokens {
		if toks[k] && strings.Contains(t, k) {
			score += 5000
		}
	}
	return score + min(c.Downloads, 99999)/100
}

// releaseName is the video's pre-rename file and folder names from its .details note (the rename dropped the release
// group and source), else the file name.
func releaseName(video string) string {
	note, err := os.ReadFile(filepath.Join(filepath.Dir(video), ".details", filepath.Base(video)+".txt"))
	if err != nil {
		return filepath.Base(video)
	}
	var names []string
	for _, m := range originalRe.FindAllStringSubmatch(string(note), -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		return filepath.Base(video)
	}
	return strings.Join(names, " ")
}

// stripAds drops the cue blocks OpenSubtitles adds to every download.
func stripAds(txt string) string {
	var kept []string
	for _, b := range blankLineRe.Split(pyStrip(strings.ReplaceAll(txt, "\r\n", "\n")), -1) {
		if !adBlockRe.MatchString(b) {
			kept = append(kept, b)
		}
	}
	return strings.Join(kept, "\n\n") + "\n"
}

// order ranks candidates best first (a stable sort, so ties keep search order) and drops unusable ones and repeats.
func order(cands []Candidate, release string) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i], release) > rank(cands[j], release) })
	seen := map[string]bool{}
	out := cands[:0]
	for _, c := range cands {
		if key := c.provider.Name() + ":" + c.ID; !c.unusable && !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// ---- pacing ----

type pacer struct {
	mu     sync.Mutex
	next   time.Time
	gap    time.Duration
	parked error
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *pacer) park(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.parked == nil {
		p.parked = err
	}
}

// do sends req (rebuilt per attempt) at most once per gap across all workers. 429, 5xx and network errors back off
// exponentially with jitter (Retry-After wins), six attempts in all; after that the provider is parked. A 2xx
// response is returned to the caller, any other status as an error.
func (p *pacer) do(ctx context.Context, hc *http.Client, req func() (*http.Request, error)) ([]byte, error) {
	const attempts = 6
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		p.mu.Lock()
		if p.parked != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("%w: %v", errParked, p.parked)
		}
		wait := p.next.Sub(p.now())
		p.next = p.now().Add(max(wait, 0) + p.gap)
		p.mu.Unlock()
		if wait > 0 {
			if err := p.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		r, err := req()
		if err != nil {
			return nil, err
		}
		backoff := min(15*time.Minute, 10*time.Second<<attempt)
		backoff = backoff/2 + rand.N(backoff/2)
		resp, err := hc.Do(r.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // the URL may carry a key; never let it into an error or a log
			}
			last = err
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			switch {
			case rerr != nil:
				last = rerr
			case resp.StatusCode/100 == 2:
				return body, nil
			case resp.StatusCode == 429 || resp.StatusCode/100 == 5:
				last = fmt.Errorf("HTTP %d", resp.StatusCode)
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
					backoff = time.Duration(s) * time.Second
				}
			default:
				return nil, &statusError{resp.StatusCode}
			}
		}
		if attempt < attempts-1 {
			if err := p.sleep(ctx, backoff); err != nil {
				return nil, err
			}
		}
	}
	p.park(last)
	return nil, fmt.Errorf("%w: %v", errParked, last)
}

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// ---- OpenSubtitles legacy REST ----

// flexString accepts a JSON string or number (the API quotes every value today, but its numbers must not break a run).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		*f = flexString(bytes.Trim(b, `"`))
		return nil
	}
	*f = flexString(s)
	return nil
}

type legacyResult struct {
	MatchedBy           flexString
	IDSubtitleFile      flexString
	SubFileName         flexString
	MovieReleaseName    flexString
	SubFormat           flexString
	SubForeignPartsOnly flexString
	SubBad              flexString
	SubHearingImpaired  flexString
	SubDownloadsCnt     flexString
	SubEncoding         flexString
	SubDownloadLink     flexString
	SeriesSeason        flexString
	SeriesEpisode       flexString
}

type legacy struct {
	base      string // .../search/
	userAgent string
	hc        *http.Client
	pace      *pacer
}

func (o *legacy) Name() string { return "opensubtitles" }

func (o *legacy) get(ctx context.Context, link string) ([]byte, error) {
	return o.pace.do(ctx, o.hc, func() (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, link, nil)
		if err == nil {
			r.Header.Set("User-Agent", o.userAgent)
			r.Header.Set("X-User-Agent", o.userAgent)
		}
		return r, err
	})
}

// search runs one query; the API wants its path parameters in alphabetical order.
func (o *legacy) search(ctx context.Context, source string, params ...string) ([]Candidate, error) {
	params = append(params, "sublanguageid-eng")
	sort.Strings(params)
	body, err := o.get(ctx, o.base+strings.Join(params, "/"))
	if err != nil {
		return nil, err
	}
	var res []legacyResult
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '[' {
		return nil, nil // an object instead of a list: no results
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("opensubtitles search: %w", err)
	}
	var out []Candidate
	for _, r := range res {
		downloads, _ := strconv.Atoi(string(r.SubDownloadsCnt))
		out = append(out, Candidate{
			Source: source, ID: string(r.IDSubtitleFile), Release: string(r.MovieReleaseName), FileName: string(r.SubFileName),
			HashMatch: r.MatchedBy == "moviehash", HearingImpaired: r.SubHearingImpaired == "1", Downloads: downloads,
			Encoding: string(r.SubEncoding), Season: string(r.SeriesSeason), Episode: string(r.SeriesEpisode),
			link: string(r.SubDownloadLink), provider: o,
			unusable: r.SubFormat != "srt" || r.SubForeignPartsOnly == "1" || r.SubBad == "1",
		})
	}
	return out, nil
}

// Search is candidates() of subfetch2.py: file hash, then IMDb id (with season and episode for an episode), then for
// an episode with nothing beyond hash hits, the show's name. Outside a hash match an episode result must be ours.
func (o *legacy) Search(ctx context.Context, q Query) ([]Candidate, error) {
	found, err := o.search(ctx, "opensubtitles-hash", fmt.Sprintf("moviebytesize-%d", q.Size), "moviehash-"+q.Hash)
	if err != nil {
		return nil, err
	}
	hashHits := len(found)
	if q.Season == 0 {
		if q.IMDb != 0 {
			more, err := o.search(ctx, "opensubtitles-imdb", fmt.Sprintf("imdbid-%07d", q.IMDb))
			return append(found, more...), err
		}
		return found, nil
	}
	s, e := fmt.Sprintf("season-%d", q.Season), fmt.Sprintf("episode-%d", q.Episode)
	if q.IMDb != 0 {
		more, err := o.search(ctx, "opensubtitles-imdb", e, fmt.Sprintf("imdbid-%07d", q.IMDb), s)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	if len(found) == hashHits {
		more, err := o.search(ctx, "opensubtitles-query", e, "query-"+pyQuote(pyLower(q.Name)), s)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	kept := found[:0]
	for _, c := range found {
		if c.Source == "opensubtitles-hash" || (c.Season == strconv.Itoa(q.Season) && c.Episode == strconv.Itoa(q.Episode)) {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// Download fetches the gzip file. A page instead of gzip, or HTTP 407, is the download quota: park the provider.
func (o *legacy) Download(ctx context.Context, c Candidate) (string, error) {
	body, err := o.get(ctx, c.link)
	var se *statusError
	if errors.As(err, &se) && se.code == 407 || err == nil && !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		o.pace.park(errQuota)
		return "", fmt.Errorf("%w: %w", errParked, errQuota)
	}
	if err != nil {
		return "", err
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return "", err
	}
	return decodeDownload(raw, c.Encoding), nil
}

// pyQuote is urllib.parse.quote(s): everything but unreserved characters and "/" percent-encoded.
func pyQuote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"), "%2F", "/")
}

// ---- OpenSubtitles.com REST (needs an API key) ----

type dotCom struct {
	base      string // .../api/v1
	keyFile   string
	userAgent string
	hc        *http.Client
	pace      *pacer
}

func (o *dotCom) Name() string { return "opensubtitles-com" }

func (o *dotCom) call(ctx context.Context, method, path string, payload any, out any) error {
	key, err := os.ReadFile(o.keyFile)
	if err != nil {
		return err
	}
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}
	raw, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) {
		r, err := http.NewRequest(method, o.base+path, bytes.NewReader(body))
		if err == nil {
			r.Header.Set("Api-Key", strings.TrimSpace(string(key)))
			r.Header.Set("User-Agent", o.userAgent)
			r.Header.Set("Accept", "application/json")
			r.Header.Set("Content-Type", "application/json")
		}
		return r, err
	})
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Search asks once with everything known; the API flags file-hash matches itself.
func (o *dotCom) Search(ctx context.Context, q Query) ([]Candidate, error) {
	v := url.Values{"languages": {"en"}, "moviehash": {q.Hash}}
	switch {
	case q.Season != 0 && q.IMDb != 0:
		v.Set("parent_imdb_id", strconv.Itoa(q.IMDb))
	case q.IMDb != 0:
		v.Set("imdb_id", strconv.Itoa(q.IMDb))
	case q.Season != 0:
		v.Set("query", pyLower(q.Name))
	}
	if q.Season != 0 {
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	}
	var res struct {
		Data []struct {
			Attributes struct {
				Release           string `json:"release"`
				DownloadCount     int    `json:"download_count"`
				HearingImpaired   bool   `json:"hearing_impaired"`
				ForeignPartsOnly  bool   `json:"foreign_parts_only"`
				MachineTranslated bool   `json:"machine_translated"`
				AITranslated      bool   `json:"ai_translated"`
				MoviehashMatch    bool   `json:"moviehash_match"`
				Files             []struct {
					FileID   int    `json:"file_id"`
					FileName string `json:"file_name"`
				} `json:"files"`
				Feature struct {
					Season  int `json:"season_number"`
					Episode int `json:"episode_number"`
				} `json:"feature_details"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := o.call(ctx, http.MethodGet, "/subtitles?"+v.Encode(), nil, &res); err != nil { // Encode sorts, as the API asks
		return nil, err
	}
	var out []Candidate
	for _, d := range res.Data {
		a := d.Attributes
		if len(a.Files) != 1 || a.ForeignPartsOnly || a.MachineTranslated || a.AITranslated {
			continue // multi-CD sets cannot be one sidecar
		}
		c := Candidate{
			Source: "opensubtitles-com", ID: strconv.Itoa(a.Files[0].FileID), Release: a.Release, FileName: a.Files[0].FileName,
			HashMatch: a.MoviehashMatch, HearingImpaired: a.HearingImpaired, Downloads: a.DownloadCount, provider: o,
			Season: strconv.Itoa(a.Feature.Season), Episode: strconv.Itoa(a.Feature.Episode),
		}
		if q.Season == 0 || c.HashMatch || (c.Season == strconv.Itoa(q.Season) && c.Episode == strconv.Itoa(q.Episode)) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Download asks for a link (this is what the daily quota counts; 406 means it is spent), then fetches the file.
func (o *dotCom) Download(ctx context.Context, c Candidate) (string, error) {
	var link struct {
		Link string `json:"link"`
	}
	id, _ := strconv.Atoi(c.ID)
	err := o.call(ctx, http.MethodPost, "/download", map[string]any{"file_id": id, "sub_format": "srt"}, &link)
	var se *statusError
	if errors.As(err, &se) && se.code == 406 {
		o.pace.park(errQuota)
		return "", fmt.Errorf("%w: %w", errParked, errQuota)
	}
	if err != nil {
		return "", err
	}
	raw, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, link.Link, nil) })
	if err != nil {
		return "", err
	}
	return decodeDownload(raw, ""), nil
}

// ---- TMDB: the IMDb id OpenSubtitles searches by ----

type tmdb struct {
	base      string // .../3
	tokenFile string // v4 read-access token, sent as a bearer header (never in a URL)
	hc        *http.Client
	mu        sync.Mutex
	cache     map[string]int
}

// imdb returns the IMDb number of a TMDB movie or show ("movie" | "tv"), 0 when TMDB has none or no token exists.
func (t *tmdb) imdb(ctx context.Context, kind string, id string) (int, error) {
	key := kind + "/" + id
	t.mu.Lock()
	n, ok := t.cache[key]
	t.mu.Unlock()
	if ok {
		return n, nil
	}
	token, err := os.ReadFile(t.tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/"+key+"/external_ids", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("tmdb %s: %w", key, err)
	}
	defer resp.Body.Close()
	var ids struct {
		IMDb string `json:"imdb_id"`
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
	case resp.StatusCode/100 != 2:
		return 0, fmt.Errorf("tmdb %s: HTTP %d", key, resp.StatusCode)
	default:
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ids); err != nil {
			return 0, fmt.Errorf("tmdb %s: %w", key, err)
		}
	}
	n, _ = strconv.Atoi(strings.TrimPrefix(ids.IMDb, "tt"))
	t.mu.Lock()
	t.cache[key] = n
	t.mu.Unlock()
	return n, nil
}
