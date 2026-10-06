package subtitles

// Subtitle providers, called directly (never through Plex): the OpenSubtitles legacy REST API, Gestdown (Addic7ed's
// TV subtitles) and SITEC Subtitles (movies), none of which needs a key, and OpenSubtitles.com and Subdl, each only when
// its API key file exists. Ranking, the file hash, the release name, the ad stripping and the legacy API's download
// quota are ports of subfetch2.py v8. Every request goes through its provider's pacer: requests a gap apart (1 s;
// 2 s for Gestdown and SITEC; 8 min for legacy downloads), exponential backoff on 429/5xx/network errors, and parking
// for the rest of the run once the provider keeps failing, refuses a search or spends its download quota, so the
// titles that need it wait for the next run.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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
	TVDB    int    // TheTVDB id of the show (from TMDB), 0 when unknown
	TMDB    string // TMDB id from the folder name, "" when the folder has none
	Name    string // show name, for the text search of episodes
	Show    bool   // an episode; its Season is 0 for a special
	Season  int
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
	Rating          float64 // OpenSubtitles' SubRating (0-10), SITEC's vote score: rank() adds 100 a point
	FPS             float64 // the frame rate it was timed for, where the provider says (OpenSubtitles); 0 otherwise
	Encoding        string
	Season, Episode string
	ours            bool // labelled with this title: its film (IMDb id, or found by it) or its season and episode
	unusable        bool // not SRT, foreign parts only or marked bad: counted by the search logic, never tried
	link            string
	provider        Provider
}

type Provider interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Candidate, error)
	Download(ctx context.Context, c Candidate) (string, error) // decoded text
	pacer() *pacer                                             // its searches': a refused search parks it
}

var (
	// errParked: the provider is out for now (quota spent, failing, or refusing), so titles that need it wait.
	errParked = errors.New("provider parked")
	errQuota  = errors.New("download quota spent")
)

func (c Candidate) key() string { return c.provider.Name() + ":" + c.ID }

// metered providers ration their downloads (OpenSubtitles: 180 a day per IP), so they come last.
func metered(p Provider) bool { return p.Name() == "opensubtitles" || p.Name() == "opensubtitles-com" }

// parked is why p would refuse a search now (wrapping errParked), nil when it would not; the legacy API also refuses
// while its downloads wait out the quota.
func parked(p Provider) error {
	if l, ok := p.(*legacy); ok {
		if err := l.dl.parkedErr(); err != nil {
			return err
		}
	}
	return p.pacer().parkedErr()
}

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
	adBlockRe    = regexp.MustCompile(`(?i)opensubtitles|advertise your product|osdb\.link`) // osdb.link: OpenSubtitles' rotating ad
	blankLineRe  = regexp.MustCompile(`\n` + pySpace + `*\n`)
)

// rank orders candidates: a file-hash match first, then the release group, then source tags in common, then
// popularity and rating; hearing-impaired copies lose 20000.
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
	return score + min(c.Downloads, 99999)/100 + int(c.Rating*100)
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

// order ranks candidates best first (ties keep search order, as subfetch2.py's stable sort does) and drops unusable
// ones and repeats.
func order(cands []Candidate, release string) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i], release) > rank(cands[j], release) })
	seen := map[string]bool{}
	out := cands[:0]
	for _, c := range cands {
		if !c.unusable && !seen[c.key()] {
			seen[c.key()] = true
			out = append(out, c)
		}
	}
	return out
}

// ---- pacing ----

type pacer struct {
	mu       sync.Mutex
	next     time.Time
	gap      time.Duration
	quota429 bool // a 429 is the download quota, answered at once rather than retried (legacy downloads)
	parked   error
	until    time.Time // when parked ends; zero: at the end of the run
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
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
	if p.out() == nil {
		p.parked = err
	}
}

// pause parks the provider until a given time (its download quota is spent).
func (p *pacer) pause(until time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parked, p.until = err, until
}

// out is why the provider is parked, nil when it is not; p.mu must be held.
func (p *pacer) out() error {
	if p.parked != nil && !p.until.IsZero() && !p.now().Before(p.until) {
		p.parked, p.until = nil, time.Time{}
	}
	return p.parked
}

// parkedErr is out() for callers outside the pacer, wrapped as a pacer refusal.
func (p *pacer) parkedErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.out(); err != nil {
		return fmt.Errorf("%w: %w", errParked, err)
	}
	return nil
}

// do is doH for callers that need no response header.
func (p *pacer) do(ctx context.Context, hc *http.Client, req func() (*http.Request, error)) ([]byte, error) {
	body, _, err := p.doH(ctx, hc, req)
	return body, err
}

// doH sends req (rebuilt per attempt) at most once per gap across all workers. 429 (unless quota429), 5xx and network
// errors back off exponentially with jitter (Retry-After wins), six attempts in all (subfetch2.py tries ten); after
// that the provider is parked. A 2xx response's body and header are returned to the caller, any other status as a
// *statusError.
func (p *pacer) doH(ctx context.Context, hc *http.Client, req func() (*http.Request, error)) ([]byte, http.Header, error) {
	const attempts = 6
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		p.mu.Lock()
		if err := p.out(); err != nil {
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("%w: %w", errParked, err)
		}
		wait := p.next.Sub(p.now())
		p.next = p.now().Add(max(wait, 0) + p.gap)
		p.mu.Unlock()
		if wait > 0 {
			if err := p.sleep(ctx, wait); err != nil {
				return nil, nil, err
			}
		}
		r, err := req()
		if err != nil {
			return nil, nil, err
		}
		backoff := min(15*time.Minute, 10*time.Second<<attempt)
		backoff = backoff/2 + rand.N(backoff/2)
		resp, err := hc.Do(r.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
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
				return body, resp.Header, nil
			case resp.StatusCode == 429 && !p.quota429 || resp.StatusCode/100 == 5:
				last = fmt.Errorf("HTTP %d", resp.StatusCode)
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
					backoff = time.Duration(s) * time.Second
				}
			default:
				return nil, nil, &statusError{resp.StatusCode, body}
			}
		}
		if attempt < attempts-1 {
			if err := p.sleep(ctx, backoff); err != nil {
				return nil, nil, err
			}
		}
	}
	p.park(last)
	return nil, nil, fmt.Errorf("%w: %w", errParked, last)
}

type statusError struct {
	code int
	body []byte
}

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
	SubSumCD            flexString
	SubHearingImpaired  flexString
	SubDownloadsCnt     flexString
	SubRating           flexString
	SubEncoding         flexString
	MovieFPS            flexString
	SubDownloadLink     flexString
	SeriesSeason        flexString
	SeriesEpisode       flexString
	IDMovieImdb         flexString
}

// The legacy API lets one IP download about 200 subtitles per ~24 h, counting down what is left in a Download-Quota
// header; past it a 404 page says the IP "exceeded" and denies it for ~24 h, and an IP that keeps trying is firewalled.
// subfetch2.py v8's limits: one download every dlGap, at most dlPerDay in any 24 h, and nothing (not even a search)
// for quotaWait once the header is down to quotaFloor or a download is refused. The download times and the resume
// time are kept in the store, so a restart neither bursts nor tries a spent quota.
const (
	dlGap      = 8 * time.Minute
	dlPerDay   = 180
	quotaFloor = 10
	quotaWait  = 24 * time.Hour
)

// quotaState is the legacy API's download history as kept in the store.
type quotaState struct {
	Downloads   []time.Time `json:"downloads"`    // the last 24 h's downloads, oldest first
	ResumeAfter time.Time   `json:"resume_after"` // no download or search before this
}

type legacy struct {
	base      string // .../search/
	userAgent string
	hc        *http.Client
	pace      *pacer // searches
	dl        *pacer // downloads: dlGap apart, parked until ResumeAfter
	mu        sync.Mutex
	st        quotaState       // guarded by mu, which also runs downloads one at a time (subfetch2.py's DL_LOCK)
	keep      func(quotaState) // saves st after every download
}

func (o *legacy) Name() string  { return "opensubtitles" }
func (o *legacy) pacer() *pacer { return o.pace }

// restore resumes the downloads a previous run left: the gap after its last download, and its quota wait.
func (o *legacy) restore(st quotaState) {
	o.st = st
	if n := len(st.Downloads); n > 0 {
		o.dl.next = st.Downloads[n-1].Add(dlGap)
	}
	if o.dl.now().Before(st.ResumeAfter) {
		o.dl.pause(st.ResumeAfter, quotaErr("quota wait in force", st.ResumeAfter))
	}
}

func quotaErr(reason string, until time.Time) error {
	return fmt.Errorf("%w: %s; no downloads before %s", errQuota, reason, until.UTC().Format(time.RFC3339))
}

// spent stops downloads and searches until `until` and returns the refusal; o.mu must be held.
func (o *legacy) spent(reason string, until time.Time) error {
	if until.After(o.st.ResumeAfter) {
		o.st.ResumeAfter = until
	}
	err := quotaErr(reason, o.st.ResumeAfter)
	o.dl.pause(o.st.ResumeAfter, err)
	return fmt.Errorf("%w: %w", errParked, err)
}

func (o *legacy) get(ctx context.Context, p *pacer, link string) ([]byte, http.Header, error) {
	return p.doH(ctx, o.hc, func() (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, link, nil)
		if err == nil {
			r.Header.Set("User-Agent", o.userAgent)
			r.Header.Set("X-User-Agent", o.userAgent)
		}
		return r, err
	})
}

// search runs one query; the API wants its path parameters in alphabetical order.
func (o *legacy) search(ctx context.Context, q Query, source string, params ...string) ([]Candidate, error) {
	params = append(params, "sublanguageid-eng")
	sort.Strings(params)
	body, _, err := o.get(ctx, o.pace, o.base+strings.Join(params, "/"))
	var se *statusError
	if errors.As(err, &se) && se.code/100 == 3 {
		return nil, nil // a search the API rewrites (season 0, an unpadded IMDb id) is redirected to host "_": nothing
	}
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
		rating, _ := strconv.ParseFloat(string(r.SubRating), 64)
		fps, _ := strconv.ParseFloat(string(r.MovieFPS), 64)
		imdb, _ := strconv.Atoi(string(r.IDMovieImdb))
		out = append(out, Candidate{
			Source: source, ID: string(r.IDSubtitleFile), Release: string(r.MovieReleaseName), FileName: string(r.SubFileName),
			HashMatch: r.MatchedBy == "moviehash", HearingImpaired: r.SubHearingImpaired == "1", Downloads: downloads,
			Rating: rating, FPS: fps, Encoding: string(r.SubEncoding), Season: string(r.SeriesSeason), Episode: string(r.SeriesEpisode),
			link: string(r.SubDownloadLink), provider: o,
			unusable: r.SubFormat != "srt" || r.SubSumCD != "" && r.SubSumCD != "1" || r.SubForeignPartsOnly == "1" || r.SubBad == "1",
			ours:     imdb != 0 && imdb == q.IMDb, // an episode's own test replaces this in Search
		})
	}
	return out, nil
}

// Search is candidates() of subfetch2.py: file hash, then IMDb id (with season and episode for an episode), then for
// an episode with nothing beyond hash hits, the show's name. Outside a hash match an episode result must be ours.
// While downloads are parked there is nothing to search for.
func (o *legacy) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if err := o.dl.parkedErr(); err != nil {
		return nil, err
	}
	found, err := o.search(ctx, q, "opensubtitles-hash", fmt.Sprintf("moviebytesize-%d", q.Size), "moviehash-"+q.Hash)
	if err != nil {
		return nil, err
	}
	hashHits := len(found)
	if !q.Show {
		if q.IMDb != 0 {
			more, err := o.search(ctx, q, "opensubtitles-imdb", fmt.Sprintf("imdbid-%07d", q.IMDb))
			return append(found, more...), err
		}
		return found, nil
	}
	s, e := fmt.Sprintf("season-%d", q.Season), fmt.Sprintf("episode-%d", q.Episode)
	if q.IMDb != 0 {
		more, err := o.search(ctx, q, "opensubtitles-imdb", fmt.Sprintf("imdbid-%07d", q.IMDb), s, e)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	if len(found) == hashHits {
		more, err := o.search(ctx, q, "opensubtitles-query", "query-"+pyQuote(pyLower(q.Name)), s, e)
		if err != nil {
			return nil, err
		}
		found = append(found, more...)
	}
	kept := found[:0]
	for _, c := range found {
		c.ours = c.Season == strconv.Itoa(q.Season) && c.Episode == strconv.Itoa(q.Episode)
		if c.Source == "opensubtitles-hash" || c.ours {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// Download is fetch() of subfetch2.py v8: one download at a time, dlGap apart, within dlPerDay per 24 h; a refusal
// (407, 429, the 404 "exceeded" deny page, or a page instead of gzip) or a Download-Quota header at quotaFloor stops
// downloads for quotaWait (the download that reported the floor is still used).
func (o *legacy) Download(ctx context.Context, c Candidate) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	defer func() { o.keep(o.st) }()
	if err := o.dl.parkedErr(); err != nil {
		return "", err
	}
	day := o.dl.now().Add(-24 * time.Hour)
	for len(o.st.Downloads) > 0 && !o.st.Downloads[0].After(day) {
		o.st.Downloads = o.st.Downloads[1:]
	}
	if len(o.st.Downloads) >= dlPerDay {
		return "", o.spent(fmt.Sprintf("%d downloads in 24 h", dlPerDay), o.st.Downloads[0].Add(quotaWait))
	}
	body, h, err := o.get(ctx, o.dl, c.link)
	var se *statusError
	if errors.As(err, &se) && (se.code == 407 || se.code == 429 || se.code == 404 && bytes.Contains(bytes.ToLower(se.body), []byte("exceeded"))) {
		return "", o.spent(fmt.Sprintf("download answered HTTP %d", se.code), o.dl.now().Add(quotaWait))
	}
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		return "", o.spent("download answered with a page instead of a file", o.dl.now().Add(quotaWait))
	}
	o.st.Downloads = append(o.st.Downloads, o.dl.now())
	if left, err := strconv.Atoi(strings.TrimSpace(h.Get("Download-Quota"))); err == nil && left <= quotaFloor {
		o.spent(fmt.Sprintf("download-quota header down to %d", left), o.dl.now().Add(quotaWait))
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

func (o *dotCom) Name() string  { return "opensubtitles-com" }
func (o *dotCom) pacer() *pacer { return o.pace }

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
	case q.Show && q.IMDb != 0:
		v.Set("parent_imdb_id", strconv.Itoa(q.IMDb))
	case q.IMDb != 0:
		v.Set("imdb_id", strconv.Itoa(q.IMDb))
	case q.Show:
		v.Set("query", pyLower(q.Name))
	}
	if q.Show {
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	}
	var res struct {
		Data []struct {
			Attributes struct {
				Release           string  `json:"release"`
				DownloadCount     int     `json:"download_count"`
				Ratings           float64 `json:"ratings"`
				HearingImpaired   bool    `json:"hearing_impaired"`
				ForeignPartsOnly  bool    `json:"foreign_parts_only"`
				MachineTranslated bool    `json:"machine_translated"`
				AITranslated      bool    `json:"ai_translated"`
				FPS               float64 `json:"fps"`
				MoviehashMatch    bool    `json:"moviehash_match"`
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
			HashMatch: a.MoviehashMatch, HearingImpaired: a.HearingImpaired, Downloads: a.DownloadCount, Rating: a.Ratings, provider: o,
			FPS: a.FPS, Season: strconv.Itoa(a.Feature.Season), Episode: strconv.Itoa(a.Feature.Episode),
		}
		// searched by the film's IMDb id, or labelled with the episode
		c.ours = !q.Show && q.IMDb != 0 || q.Show && c.Season == strconv.Itoa(q.Season) && c.Episode == strconv.Itoa(q.Episode)
		if !q.Show || c.HashMatch || c.ours {
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

// ---- Subdl (needs an API key) ----

type subdl struct {
	api, dl string // https://api.subdl.com/api/v1/subtitles, https://dl.subdl.com
	keyFile string
	hc      *http.Client
	pace    *pacer
}

func (o *subdl) Name() string  { return "subdl" }
func (o *subdl) pacer() *pacer { return o.pace }

// Search asks by TMDB id (else IMDb id, else the show's name); an episode's results must name that episode.
func (o *subdl) Search(ctx context.Context, q Query) ([]Candidate, error) {
	key, err := os.ReadFile(o.keyFile)
	if err != nil {
		return nil, err
	}
	v := url.Values{"api_key": {strings.TrimSpace(string(key))}, "languages": {"EN"}, "subs_per_page": {"30"}, "hi": {"1"}, "type": {"movie"}}
	switch {
	case q.TMDB != "":
		v.Set("tmdb_id", q.TMDB)
	case q.IMDb != 0:
		v.Set("imdb_id", fmt.Sprintf("tt%07d", q.IMDb))
	case q.Show:
		v.Set("film_name", q.Name)
	default:
		return nil, nil
	}
	if q.Show {
		v.Set("type", "tv")
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	}
	raw, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, o.api+"?"+v.Encode(), nil) })
	if err != nil {
		return nil, err
	}
	var res struct {
		Subtitles []struct {
			Release string          `json:"release_name"`
			Name    string          `json:"name"`
			URL     string          `json:"url"`
			HI      bool            `json:"hi"`
			Season  json.RawMessage `json:"season"`
			Episode json.RawMessage `json:"episode"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("subdl search: %w", err)
	}
	var out []Candidate
	for _, r := range res.Subtitles {
		season, episode := string(bytes.Trim(r.Season, `"`)), string(bytes.Trim(r.Episode, `"`))
		if q.Show && (season != strconv.Itoa(q.Season) || episode != strconv.Itoa(q.Episode)) {
			continue
		}
		id, _, _ := strings.Cut(r.URL, "?") // the API appends the key to url: an id is kept in the history
		out = append(out, Candidate{Source: "subdl", ID: id, Release: r.Release, FileName: r.Name, HearingImpaired: r.HI,
			Season: season, Episode: episode, link: o.dl + r.URL, provider: o})
	}
	return out, nil
}

// Download fetches the zip and returns its one SRT; anything but a zip is the quota (or a block page): park.
func (o *subdl) Download(ctx context.Context, c Candidate) (string, error) {
	body, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, c.link, nil) })
	if err != nil {
		return "", err
	}
	txt, err := zipSRT(body)
	if errors.Is(err, zip.ErrFormat) {
		o.pace.park(errQuota)
		return "", fmt.Errorf("%w: %w", errParked, errQuota)
	}
	return txt, err
}

// zipSRT decodes the one SRT in a zip (a season pack or a multi-CD set is not one sidecar); zip.ErrFormat when body
// is not a zip.
func zipSRT(body []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", err
	}
	var srts []*zip.File
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Ext(f.Name), ".srt") {
			srts = append(srts, f)
		}
	}
	if len(srts) != 1 {
		return "", fmt.Errorf("zip holds %d SRT files", len(srts))
	}
	rc, err := srts[0].Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	if err != nil {
		return "", err
	}
	return decodeDownload(raw, ""), nil
}

// ---- Gestdown: Addic7ed's TV subtitles (api.gestdown.info, no key) ----

type gestdown struct {
	base  string // https://api.gestdown.info
	hc    *http.Client
	pace  *pacer
	mu    sync.Mutex
	shows map[string]string // "tvdb:N" or "name:..." -> Gestdown's show id, "" when it has none
}

func (o *gestdown) Name() string  { return "gestdown" }
func (o *gestdown) pacer() *pacer { return o.pace }

func (o *gestdown) get(ctx context.Context, link string) ([]byte, error) {
	return o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, link, nil) })
}

// getJSON decodes a 2xx answer into v; a 404 is no results.
func (o *gestdown) getJSON(ctx context.Context, path string, v any) error {
	body, err := o.get(ctx, o.base+path)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("gestdown %s: %w", path, err)
	}
	return nil
}

// show is Gestdown's id for the show: by TheTVDB id (it has no TMDB lookup), else by an exact name match.
func (o *gestdown) show(ctx context.Context, q Query) (string, error) {
	key, path := "name:"+pyLower(q.Name), "/shows/search/"+url.PathEscape(q.Name)
	if q.TVDB != 0 {
		key, path = fmt.Sprintf("tvdb:%d", q.TVDB), fmt.Sprintf("/shows/external/tvdb/%d", q.TVDB)
	}
	o.mu.Lock()
	id, ok := o.shows[key]
	o.mu.Unlock()
	if ok {
		return id, nil
	}
	var res struct {
		Shows []struct{ ID, Name string } `json:"shows"`
	}
	if err := o.getJSON(ctx, path, &res); err != nil {
		return "", err
	}
	for _, s := range res.Shows {
		if q.TVDB != 0 || strings.EqualFold(s.Name, q.Name) {
			id = s.ID
			break
		}
	}
	o.mu.Lock()
	o.shows[key] = id
	o.mu.Unlock()
	return id, nil
}

// Search lists the episode's complete English subtitles (episodes only: Addic7ed has no films).
func (o *gestdown) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if !q.Show {
		return nil, nil
	}
	show, err := o.show(ctx, q)
	if err != nil || show == "" {
		return nil, err
	}
	var res struct {
		Subtitles []struct {
			ID              string `json:"subtitleId"`
			Version         string `json:"version"`
			Completed       bool   `json:"completed"`
			HearingImpaired bool   `json:"hearingImpaired"`
			DownloadURI     string `json:"downloadUri"`
			Language        string `json:"language"`
			DownloadCount   int    `json:"downloadCount"`
		} `json:"matchingSubtitles"`
		Episode struct{ Season, Number int } `json:"episode"`
	}
	if err := o.getJSON(ctx, fmt.Sprintf("/subtitles/get/%s/%d/%d/English", show, q.Season, q.Episode), &res); err != nil {
		return nil, err
	}
	if res.Episode.Season != q.Season || res.Episode.Number != q.Episode {
		return nil, nil
	}
	var out []Candidate
	for _, s := range res.Subtitles {
		if s.Completed && s.Language == "English" { // an incomplete one is a translation still in progress
			out = append(out, Candidate{Source: "gestdown", ID: s.ID, Release: s.Version, HearingImpaired: s.HearingImpaired,
				Downloads: s.DownloadCount, Season: strconv.Itoa(q.Season), Episode: strconv.Itoa(q.Episode),
				link: o.base + s.DownloadURI, provider: o, ours: true})
		}
	}
	return out, nil
}

// Download fetches the SRT itself.
func (o *gestdown) Download(ctx context.Context, c Candidate) (string, error) {
	raw, err := o.get(ctx, c.link)
	if err != nil {
		return "", err
	}
	return decodeDownload(raw, ""), nil
}

// ---- SITEC Subtitles: films by IMDb id (subtitles.example.com, no key) ----

var (
	sitecRowRe    = regexp.MustCompile(`(?s)<tr data-id="\d+">.*?</tr>`)
	sitecLinkRe   = regexp.MustCompile(`<a href="/subtitles/([^"]+)"><span class="text-muted">subtitle</span>([^<]*)</a>`)
	sitecRatingRe = regexp.MustCompile(`class="label[^"]*">(-?\d+)<`)
	sitecZipRe    = regexp.MustCompile(`href="(/subtitle/[^"]+\.zip)"`)
)

type sitec struct {
	base string // https://subtitles.example.com
	hc   *http.Client
	pace *pacer
}

func (o *sitec) Name() string  { return "sitec" }
func (o *sitec) pacer() *pacer { return o.pace }

// Search reads the film's page: one table row per subtitle, of every language.
func (o *sitec) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if q.Show || q.IMDb == 0 {
		return nil, nil
	}
	body, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, fmt.Sprintf("%s/movie-imdb/tt%07d", o.base, q.IMDb), nil)
	})
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, row := range sitecRowRe.FindAllString(string(body), -1) {
		link := sitecLinkRe.FindStringSubmatch(row)
		if link == nil || !strings.Contains(row, `class="sub-lang">English<`) {
			continue
		}
		c := Candidate{Source: "sitec", ID: link[1], Release: html.UnescapeString(strings.TrimSpace(link[2])),
			HearingImpaired: strings.Contains(row, `class="hi-subtitle"`), link: o.base + "/subtitles/" + link[1], provider: o,
			ours: true} // the film's own page
		if m := sitecRatingRe.FindStringSubmatch(row); m != nil {
			c.Rating, _ = strconv.ParseFloat(m[1], 64)
		}
		out = append(out, c)
	}
	return out, nil
}

// Download reads the subtitle's page for its zip (whose name can differ from the page's: Amélie's page is
// le-fabuleux-destin-damelie-poulain-2001-..., its zip amelie-2001-...), then fetches it from that page as a browser
// would: without the Referer the site answers a challenge page, which parks the provider, as a 403 does.
func (o *sitec) Download(ctx context.Context, c Candidate) (string, error) {
	get := func(link, referer string) ([]byte, error) {
		body, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) {
			r, err := http.NewRequest(http.MethodGet, link, nil)
			if err == nil && referer != "" {
				r.Header.Set("Referer", referer)
			}
			return r, err
		})
		if se := (*statusError)(nil); errors.As(err, &se) && se.code == http.StatusForbidden {
			o.pace.park(errors.New("sitec answered 403"))
			return nil, o.pace.parkedErr()
		}
		return body, err
	}
	page, err := get(c.link, "")
	if err != nil {
		return "", err
	}
	zipLink := sitecZipRe.FindSubmatch(page)
	if zipLink == nil {
		return "", errors.New("sitec: no zip on the subtitle's page")
	}
	body, err := get(o.base+string(zipLink[1]), c.link)
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(body, []byte("PK")) {
		o.pace.park(errors.New("sitec answered a challenge page instead of the zip"))
		return "", o.pace.parkedErr()
	}
	return zipSRT(body)
}
