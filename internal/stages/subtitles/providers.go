package subtitles

// Subtitle providers, called directly (never through Plex): OpenSubtitles through Stremio's add-on (uncapped, the first
// OpenSubtitles route) and its legacy REST API, Gestdown (Addic7ed's TV subtitles) and SITEC Subtitles (movies), none
// of which needs a key; SubSource (the Subscene archive) and Subdl,
// each only when its API key file exists; and OpenSubtitles.com, only when its API key and a free user's username and
// password files exist. Ranking, the file hash, the release name, the ad stripping and the legacy API's download quota
// are ports of subfetch2.py v8. Every request goes through its provider's pacer: requests a gap apart (1 s; 2 s for
// Gestdown, SITEC and SubSource), exponential backoff on 429/5xx/network errors, and parking for the rest of the run
// once the provider keeps failing, refuses a search or spends its download quota, so the titles that need it wait for
// the next run. Legacy downloads take a slot every 8 min that no title waits for: one that finds it taken goes on
// without OpenSubtitles (errBusy), and the next to ask once it is free takes it. The legacy API's quota, Subdl's and
// SubSource's daily limits (a 429, answered at once) and OpenSubtitles.com's day of downloads park the provider until
// they reset, a time the store keeps, so later runs wait too.

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
	// A special's own IMDb id (TMDB's episode external_ids): OpenSubtitles may file it as another season's episode.
	EpisodeIMDb int
	// The film whose TV cut this miniseries is (identify): OpenSubtitles files such a cut under the film, a CD a part.
	FilmIMDb int
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
	// OpenSubtitles' id of the subtitle, the same through both its APIs (the legacy IDSubtitle, .com's
	// legacy_subtitle_id): a subtitle both find is one candidate.
	openSubtitlesID string
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
	// errBusy: a metered download slot is taken (OpenSubtitles': one download every dlGap). The title asking takes the
	// provider as out for this look rather than wait, which would hold up every title behind it.
	errBusy = fmt.Errorf("%w: its download slot is taken", errParked)
)

func (c Candidate) key() string { return c.provider.Name() + ":" + c.ID }

// metered providers ration their downloads (OpenSubtitles: 180 a day per IP), so they come last.
func metered(p Provider) bool { return p.Name() == "opensubtitles" || p.Name() == "opensubtitles-com" }

// openSubtitles tells whether p is a route to OpenSubtitles' catalog, which matches a file by its hash.
func openSubtitles(p Provider) bool { return strings.HasPrefix(p.Name(), "opensubtitles") }

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

// cleanSRT rebuilds an SRT from its timing lines in the shape alass's SubRip parser needs: the cues numbered in order,
// each its timing as it was, then its text lines but the blank ones and those that are only a separator ("-----"). An
// index glued to its timing ("5200:30:21,770 --> ...") or set apart by a blank line goes with the old numbering, and a
// cue left without text goes. Text without a timing line is returned as it is.
func cleanSRT(txt string) string {
	type cue struct {
		timing string
		text   []string
	}
	var cs []cue
	for _, line := range strings.Split(strings.ReplaceAll(txt, "\r\n", "\n"), "\n") {
		if m := cueRe.FindStringIndex(line); m != nil {
			if n := len(cs); n > 0 && len(cs[n-1].text) > 0 && strings.Trim(cs[n-1].text[len(cs[n-1].text)-1], "0123456789 \t") == "" {
				cs[n-1].text = cs[n-1].text[:len(cs[n-1].text)-1] // this cue's index
			}
			cs = append(cs, cue{timing: line[m[0]:m[1]]})
			continue
		}
		if t := strings.TrimSpace(line); len(cs) > 0 && strings.Trim(t, "-=_*~") != "" {
			cs[len(cs)-1].text = append(cs[len(cs)-1].text, line)
		}
	}
	if len(cs) == 0 {
		return txt
	}
	var b strings.Builder
	n := 0
	for _, c := range cs {
		if len(c.text) > 0 {
			n++
			fmt.Fprintf(&b, "%d\n%s\n%s\n\n", n, c.timing, strings.Join(c.text, "\n"))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// order ranks candidates best first (ties keep search order, as subfetch2.py's stable sort does) and drops unusable
// ones and repeats, an OpenSubtitles subtitle found through both its APIs included.
func order(cands []Candidate, release string) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i], release) > rank(cands[j], release) })
	seen := map[string]bool{}
	out := cands[:0]
	for _, c := range cands {
		k := c.key()
		if c.openSubtitlesID != "" {
			k = "opensubtitles:" + c.openSubtitlesID
		}
		if !c.unusable && !seen[k] {
			seen[k] = true
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
	slot     bool // a request before next is refused (errBusy), not waited for; the next slot's caller retries a failure
	failed   int  // slot: requests in a row that failed, across callers
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

// resumeAt parks the provider until a quota wait a previous run stored ends; a wait already over parks nothing.
func (p *pacer) resumeAt(t time.Time) {
	if p.now().Before(t) {
		p.pause(t, quotaErr("quota wait in force", t))
	}
}

// spend parks the provider, its quota spent, until `until`, and keeps the wait so later runs wait too; it returns the
// refusal (a quota: routine, logged without an alert).
func (p *pacer) spend(reason string, until time.Time, keep func(quotaState)) error {
	err := quotaErr(reason, until)
	p.pause(until, err)
	keep(quotaState{ResumeAfter: until})
	return fmt.Errorf("%w: %w", errParked, err)
}

// limited spends the quota after a 429 (with quota429 set: answered at once, never retried) until its Retry-After,
// else until fallback.
func (p *pacer) limited(h http.Header, fallback time.Time, keep func(quotaState)) error {
	until := fallback
	if d := retryAfter(h); d > 0 {
		until = p.now().Add(d)
	}
	return p.spend("answered HTTP 429", until, keep)
}

// nextDay is the 00:00 UTC after t: when a daily limit whose reset the docs do not state is assumed to reset.
func nextDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
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
// *statusError. A slot pacer sleeps neither for its gap nor to retry: each caller makes one attempt, and the six count
// across callers.
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
		if p.slot && wait > 0 {
			p.mu.Unlock()
			return nil, nil, errBusy
		}
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
				p.mu.Lock()
				p.failed = 0
				p.mu.Unlock()
				return body, resp.Header, nil
			case resp.StatusCode == 429 && !p.quota429 || resp.StatusCode/100 == 5:
				last = fmt.Errorf("HTTP %d", resp.StatusCode)
				if d := retryAfter(resp.Header); d > 0 {
					backoff = d
				}
			default:
				return nil, nil, &statusError{resp.StatusCode, body, resp.Header}
			}
		}
		if p.slot {
			p.mu.Lock()
			p.failed++
			failed := p.failed
			p.mu.Unlock()
			if failed < attempts {
				return nil, nil, fmt.Errorf("%w: %w", errBusy, last)
			}
			break
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
	code   int
	body   []byte
	header http.Header
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// retryAfter is a response's Retry-After in seconds, 0 when it has none (or gives a date).
func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

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
	IDSubtitle          flexString
	IDSubtitleFile      flexString
	SubFileName         flexString
	MovieReleaseName    flexString
	SubFormat           flexString
	SubForeignPartsOnly flexString
	SubBad              flexString
	SubSumCD            flexString
	SubActualCD         flexString
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

// quotaState is a provider's quota as kept in the store: the legacy API's download history and wait, Subdl's wait.
type quotaState struct {
	Downloads   []time.Time `json:"downloads"`    // the last 24 h's downloads, oldest first
	ResumeAfter time.Time   `json:"resume_after"` // no download or search before this
}

type legacy struct {
	base      string // .../search/
	userAgent string
	hc        *http.Client
	pace      *pacer // searches
	dl        *pacer // downloads: a slot each dlGap, parked until ResumeAfter
	mu        sync.Mutex
	st        quotaState       // guarded by mu, held by the one download under way (subfetch2.py's DL_LOCK)
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
	o.dl.resumeAt(st.ResumeAfter)
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
		cd, _ := strconv.Atoi(string(r.SubActualCD))
		part := q.FilmIMDb != 0 && imdb == q.FilmIMDb // a TV cut's episode: one CD of the film's set is one part
		if part && (r.SubSumCD == "" || r.SubSumCD == "1" || max(cd-q.Episode, q.Episode-cd) > 1) {
			continue // the film whole, or a part too far from this episode's number to be it
		}
		out = append(out, Candidate{
			Source: source, ID: string(r.IDSubtitleFile), Release: string(r.MovieReleaseName), FileName: string(r.SubFileName),
			HashMatch: r.MatchedBy == "moviehash", HearingImpaired: r.SubHearingImpaired == "1", Downloads: downloads,
			Rating: rating, FPS: fps, Encoding: string(r.SubEncoding), Season: string(r.SeriesSeason), Episode: string(r.SeriesEpisode),
			link: string(r.SubDownloadLink), provider: o, openSubtitlesID: string(r.IDSubtitle),
			unusable: r.SubFormat != "srt" || !part && r.SubSumCD != "" && r.SubSumCD != "1" || r.SubForeignPartsOnly == "1" || r.SubBad == "1",
			ours:     imdb != 0 && imdb == q.IMDb, // an episode's own test replaces this in Search
		})
	}
	return out, nil
}

// Search is candidates() of subfetch2.py: file hash, then IMDb id (with season and episode for an episode), then for
// an episode with nothing beyond hash hits, the show's name. Outside a hash match an episode result must be ours.
// A special is also searched by its own IMDb id, and a TV cut's episode by its film's (each part one CD of the film's
// set, within one of the episode's number). While downloads are parked there is nothing to search for.
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
	// A special by its own id and a TV cut's episode by its film's, each kept whatever OpenSubtitles labels it
	for _, alt := range []Query{{IMDb: q.EpisodeIMDb}, {IMDb: q.FilmIMDb, FilmIMDb: q.FilmIMDb, Show: true, Episode: q.Episode}} {
		if alt.IMDb == 0 {
			continue
		}
		more, err := o.search(ctx, alt, "opensubtitles-imdb", fmt.Sprintf("imdbid-%07d", alt.IMDb))
		if err != nil {
			return nil, err
		}
		for _, c := range more {
			if c.ours {
				kept = append(kept, c)
			}
		}
	}
	return kept, nil
}

// Download is fetch() of subfetch2.py v8: one download at a time, dlGap apart, within dlPerDay per 24 h, except that
// no caller waits for the gap: before it has passed since the last download it answers errBusy at once. A refusal
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

// ---- OpenSubtitles through Stremio's "OpenSubtitles v3" add-on (opensubtitles-v3.strem.io, no key) ----

// The whole OpenSubtitles catalog with no key and no daily cap (its downloads answer a Download-Quota of 999,999,999),
// so it is the first OpenSubtitles route: the legacy and .com routes are searched only for a title it did not answer
// (fetch). It is Stremio's own integration, not a published API: requests a second apart, under projectionist's name.
type stremio struct {
	base string // https://opensubtitles-v3.strem.io
	hc   *http.Client
	pace *pacer // searches and downloads
}

func (o *stremio) Name() string  { return "opensubtitles-stremio" }
func (o *stremio) pacer() *pacer { return o.pace }

func (o *stremio) get(ctx context.Context, link string) ([]byte, error) {
	return o.pace.do(ctx, o.hc, func() (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, link, nil)
		if err == nil {
			r.Header.Set("User-Agent", userAgent)
		}
		return r, err
	})
}

// Search asks once, by the IMDb id (the show's, with season and episode, for an episode) and the file's hash and size;
// the add-on marks the subtitles whose hash matches ("m": "h").
func (o *stremio) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if q.IMDb == 0 {
		return nil, nil
	}
	id := fmt.Sprintf("movie/tt%07d", q.IMDb)
	if q.Show {
		id = fmt.Sprintf("series/tt%07d:%d:%d", q.IMDb, q.Season, q.Episode)
	}
	link := o.base + "/subtitles/" + id
	if q.Hash != "" {
		link += fmt.Sprintf("/videoHash=%s&videoSize=%d", q.Hash, q.Size)
	}
	body, err := o.get(ctx, link+".json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Subtitles []struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			Lang     string `json:"lang"`
			Match    string `json:"m"`
			FileName string `json:"subtitleFileName"`
			Release  string `json:"movieReleaseName"`
			FPSMilli int    `json:"fpsMilli"`
			Season   int    `json:"season"`
			Episode  int    `json:"episode"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("opensubtitles-stremio search: %w", err)
	}
	var out []Candidate
	for _, s := range res.Subtitles {
		c := Candidate{Source: "opensubtitles-stremio", ID: s.ID, Release: s.Release, FileName: s.FileName, HashMatch: s.Match == "h",
			FPS: float64(s.FPSMilli) / 1000, Season: strconv.Itoa(s.Season), Episode: strconv.Itoa(s.Episode), link: s.URL, provider: o,
			openSubtitlesID: s.ID, ours: !q.Show || s.Season == q.Season && s.Episode == q.Episode}
		if s.Lang == "eng" && c.ours {
			out = append(out, c)
		}
	}
	return out, nil
}

// Download fetches the SRT, which the add-on has re-encoded as UTF-8.
func (o *stremio) Download(ctx context.Context, c Candidate) (string, error) {
	raw, err := o.get(ctx, c.link)
	if err != nil {
		return "", err
	}
	return decodeDownload(raw, ""), nil
}

// ---- OpenSubtitles.com REST (a consumer's API key and a free user's login) ----

// A login answers a token and the host every later request goes to (base_url). Logins are limited (1 a second, 30 an
// hour), so a run logs in once, on first need, logs in again only when a request answers 401 (the token expired),
// and once a request after that fresh login is refused too, gives up for the run; it logs out at its end, as the
// API's best practices ask. A free user downloads 20 subtitles a day: each download answers how many remain and when
// the count resets, and a download past it answers 406.
type dotCom struct {
	base      string // https://api.opensubtitles.com/api/v1: the login's; later requests go to the host it answers
	secrets   string // secrets_dir: opensubtitles_api_key, opensubtitles_username, opensubtitles_password
	userAgent string // the consumer's registered name and a version
	hc        *http.Client
	pace      *pacer           // quota429
	keep      func(quotaState) // saves the wait for the next day of downloads, so the next run waits too
	mu        sync.Mutex       // guards api and token, and runs one login at a time
	api       string           // base on the host the login answered
	token     string           // "" until the first login
}

func (o *dotCom) Name() string  { return "opensubtitles-com" }
func (o *dotCom) pacer() *pacer { return o.pace }

func (o *dotCom) secret(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(o.secrets, name))
	return strings.TrimSpace(string(b)), err
}

// send is one request to link with the key and, once logged in, the token; out (when set) decodes the answer. A 429
// (5 requests a second) parks the provider until its Retry-After, else for a minute.
func (o *dotCom) send(ctx context.Context, method, link, token string, payload, out any) error {
	key, err := o.secret("opensubtitles_api_key")
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
		r, err := http.NewRequest(method, link, bytes.NewReader(body))
		if err == nil {
			r.Header.Set("Api-Key", key)
			r.Header.Set("User-Agent", o.userAgent)
			r.Header.Set("Accept", "application/json")
			r.Header.Set("Content-Type", "application/json")
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}
		return r, err
	})
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusTooManyRequests {
		return o.pace.limited(se.header, o.pace.now().Add(time.Minute), o.keep)
	}
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// session is the current login: a new one when there is none yet or the request with token `stale` answered 401.
func (o *dotCom) session(ctx context.Context, stale string) (api, token string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" && o.token != stale {
		return o.api, o.token, nil
	}
	user, err := o.secret("opensubtitles_username")
	if err != nil {
		return "", "", err
	}
	pass, err := o.secret("opensubtitles_password")
	if err != nil {
		return "", "", err
	}
	var res struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if err := o.send(ctx, http.MethodPost, o.base+"/login", "", map[string]string{"username": user, "password": pass}, &res); err != nil {
		return "", "", fmt.Errorf("opensubtitles-com login: %w", err)
	}
	u, err := url.Parse(o.base)
	if err != nil {
		return "", "", err
	}
	if res.BaseURL != "" {
		u.Host = res.BaseURL
	}
	o.api, o.token = u.String(), res.Token
	return o.api, o.token, nil
}

// call sends a request on the session, logging in again, once, when its token has expired.
func (o *dotCom) call(ctx context.Context, method, path string, payload, out any) error {
	api, token, err := o.session(ctx, "")
	if err != nil {
		return err
	}
	err = o.send(ctx, method, api+path, token, payload, out)
	if se := (*statusError)(nil); errors.As(err, &se) && se.code == http.StatusUnauthorized {
		if api, token, err = o.session(ctx, token); err != nil {
			return err
		}
		err = o.send(ctx, method, api+path, token, payload, out)
	}
	return err
}

// logout ends the run's session, if it opened one; a failed logout costs nothing, as the token expires on its own.
func (o *dotCom) logout(ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" {
		o.send(ctx, http.MethodDelete, o.api+"/logout", o.token, nil, nil)
		o.token = ""
	}
}

// Search asks once with everything known (a special by its own IMDb id); the API flags file-hash matches itself.
func (o *dotCom) Search(ctx context.Context, q Query) ([]Candidate, error) {
	v := url.Values{"languages": {"en"}, "moviehash": {q.Hash}}
	own := q.Show && q.EpisodeIMDb != 0 // a special, by its own id: OpenSubtitles may number it as another season's
	switch {
	case own:
		v.Set("imdb_id", strconv.Itoa(q.EpisodeIMDb))
	case q.Show && q.IMDb != 0:
		v.Set("parent_imdb_id", strconv.Itoa(q.IMDb))
	case q.IMDb != 0:
		v.Set("imdb_id", strconv.Itoa(q.IMDb))
	case q.Show && q.TMDB != "":
		v.Set("parent_tmdb_id", q.TMDB)
	case q.TMDB != "":
		v.Set("tmdb_id", q.TMDB)
	case q.Show:
		v.Set("query", pyLower(q.Name))
	}
	if q.Show && !own {
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
				LegacyID          int     `json:"legacy_subtitle_id"`
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
		if a.LegacyID != 0 {
			c.openSubtitlesID = strconv.Itoa(a.LegacyID)
		}
		// searched by the film's id or the special's own, or labelled with the episode
		c.ours = own || !q.Show && (q.IMDb != 0 || q.TMDB != "") || q.Show && c.Season == strconv.Itoa(q.Season) && c.Episode == strconv.Itoa(q.Episode)
		if !q.Show || c.HashMatch || c.ours {
			out = append(out, c)
		}
	}
	return out, nil
}

// Download asks for a link (what the day's quota counts), then fetches the file. When the answer leaves no download,
// or refuses this one (406), the provider waits for the reset the API names, else for a day.
func (o *dotCom) Download(ctx context.Context, c Candidate) (string, error) {
	var link struct {
		Link      string `json:"link"`
		Remaining int    `json:"remaining"`
		Reset     string `json:"reset_time_utc"`
	}
	spent := func() error {
		until, err := time.Parse(time.RFC3339, link.Reset)
		if err != nil {
			until = o.pace.now().Add(24 * time.Hour)
		}
		return o.pace.spend("no downloads left today", until, o.keep)
	}
	id, _ := strconv.Atoi(c.ID)
	err := o.call(ctx, http.MethodPost, "/download", map[string]any{"file_id": id, "sub_format": "srt"}, &link)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotAcceptable {
		json.Unmarshal(se.body, &link)
		return "", spent()
	}
	if err != nil {
		return "", err
	}
	raw, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, link.Link, nil) })
	if link.Remaining <= 0 {
		spent() // after the fetch, which this download's link still allows
	}
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
	pace    *pacer           // searches and downloads; quota429
	keep    func(quotaState) // saves the wait for the daily limit, so the next run waits too
}

func (o *subdl) Name() string  { return "subdl" }
func (o *subdl) pacer() *pacer { return o.pace }

// get fetches link. A 429 is the plan's daily limit (2,000 searches, 300 downloads), not an outage: Subdl waits, this
// run and later ones, until Retry-After, else the next 00:00 UTC (assumed: the docs state a daily limit, not its reset).
func (o *subdl) get(ctx context.Context, link string) ([]byte, error) {
	body, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, link, nil) })
	if se := (*statusError)(nil); errors.As(err, &se) && se.code == http.StatusTooManyRequests {
		return nil, o.pace.limited(se.header, nextDay(o.pace.now()), o.keep)
	}
	return body, err
}

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
	raw, err := o.get(ctx, o.api+"?"+v.Encode())
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
	body, err := o.get(ctx, c.link)
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

// ---- SubSource: the Subscene archive (api.subsource.net, needs an API key) ----

// subsourcePages bounds the list requests per film or season: at most this many pages of 100 English subtitles.
const subsourcePages = 5

type subsource struct {
	base    string // https://api.subsource.net/api/v1
	keyFile string
	hc      *http.Client
	pace    *pacer           // 2 s apart: under the key's 60 requests a minute and 1,800 an hour; quota429
	keep    func(quotaState) // saves the wait for the daily limit, so the next run waits too
}

func (o *subsource) Name() string  { return "subsource" }
func (o *subsource) pacer() *pacer { return o.pace }

// get fetches path. A 429 is the key's daily limit (7,200 requests), not an outage: SubSource waits, this run and
// later ones, until Retry-After, else the next 00:00 UTC (the docs state the limit, not its reset).
func (o *subsource) get(ctx context.Context, path string) ([]byte, error) {
	key, err := os.ReadFile(o.keyFile)
	if err != nil {
		return nil, err
	}
	body, err := o.pace.do(ctx, o.hc, func() (*http.Request, error) {
		r, err := http.NewRequest(http.MethodGet, o.base+path, nil)
		if err == nil {
			r.Header.Set("X-API-Key", strings.TrimSpace(string(key)))
			r.Header.Set("User-Agent", userAgent)
		}
		return r, err
	})
	if se := (*statusError)(nil); errors.As(err, &se) && se.code == http.StatusTooManyRequests {
		return nil, o.pace.limited(se.header, nextDay(o.pace.now()), o.keep)
	}
	return body, err
}

// getJSON decodes a 2xx answer into v; a 404 is no results.
func (o *subsource) getJSON(ctx context.Context, path string, v any) error {
	body, err := o.get(ctx, path)
	if se := (*statusError)(nil); errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("subsource %s: %w", path, err)
	}
	return nil
}

// Search finds the film by its IMDb id, or the show's season by the show's IMDb id, else by its name keeping the
// show TMDB names (the API searches by no TMDB id), then lists its English subtitles. A season's list holds every
// episode's, so an episode keeps those whose release names name it.
func (o *subsource) Search(ctx context.Context, q Query) ([]Candidate, error) {
	v := url.Values{"searchType": {"imdb"}, "imdb": {fmt.Sprintf("tt%07d", q.IMDb)}, "type": {"movie"}}
	switch {
	case q.IMDb != 0:
	case q.Show && q.TMDB != "":
		v = url.Values{"searchType": {"text"}, "q": {q.Name}}
	default:
		return nil, nil
	}
	if q.Show {
		v.Set("type", "series")
		v.Set("season", strconv.Itoa(q.Season))
	}
	var found struct {
		Data []struct {
			MovieID int        `json:"movieId"`
			TMDB    flexString `json:"tmdbId"`
		} `json:"data"`
	}
	if err := o.getJSON(ctx, "/movies/search?"+v.Encode(), &found); err != nil {
		return nil, err
	}
	var out []Candidate
	for _, m := range found.Data {
		if q.IMDb != 0 || string(m.TMDB) == q.TMDB {
			more, err := o.list(ctx, q, m.MovieID)
			if err != nil {
				return nil, err
			}
			out = append(out, more...)
		}
	}
	return out, nil
}

// list is the English subtitles SubSource holds for one film or season, as this title's candidates.
func (o *subsource) list(ctx context.Context, q Query, movieID int) ([]Candidate, error) {
	var out []Candidate
	for page := 1; page <= subsourcePages; page++ {
		var res struct {
			Data []struct {
				ID              int        `json:"subtitleId"`
				ReleaseInfo     []string   `json:"releaseInfo"`
				Files           int        `json:"files"`
				HearingImpaired bool       `json:"hearingImpaired"`
				ForeignParts    bool       `json:"foreignParts"`
				Framerate       flexString `json:"framerate"`
				Downloads       int        `json:"downloads"`
			} `json:"data"`
			Pagination struct {
				Pages int `json:"pages"`
			} `json:"pagination"`
		}
		v := url.Values{"movieId": {strconv.Itoa(movieID)}, "language": {"english"}, "limit": {"100"}, "page": {strconv.Itoa(page)}}
		if err := o.getJSON(ctx, "/subtitles?"+v.Encode(), &res); err != nil {
			return nil, err
		}
		for _, s := range res.Data {
			release := strings.Join(s.ReleaseInfo, ", ")
			if s.Files > 1 || s.ForeignParts || q.Show && parseRelease(release).sxe != sxe(q.Season, q.Episode) {
				continue // not one sidecar (a season pack, a multi-CD set), foreign parts only, or another episode's
			}
			fps, _ := strconv.ParseFloat(string(s.Framerate), 64)
			c := Candidate{Source: "subsource", ID: strconv.Itoa(s.ID), Release: release, HearingImpaired: s.HearingImpaired,
				Downloads: s.Downloads, FPS: fps, provider: o, ours: true} // the film's, or naming this episode
			if q.Show {
				c.Season, c.Episode = strconv.Itoa(q.Season), strconv.Itoa(q.Episode)
			}
			out = append(out, c)
		}
		if page >= res.Pagination.Pages {
			break
		}
	}
	return out, nil
}

// Download fetches the subtitle's zip and returns its one SRT.
func (o *subsource) Download(ctx context.Context, c Candidate) (string, error) {
	body, err := o.get(ctx, "/subtitles/"+c.ID+"/download")
	if err != nil {
		return "", err
	}
	return zipSRT(body)
}
