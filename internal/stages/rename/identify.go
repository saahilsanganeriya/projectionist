package rename

// Identity: which film or show each video is, and the episode rows of each TV video.
//
//	1. a scheme folder ("Title (Year) {tmdb-N}") or a tag in the name ({tmdb-N}, {imdb-ttN}, {tvdb-N})
//	2. the library's own scheme folders: the parsed name equals exactly one title already there (a new season pack)
//	3. TMDB search, direct: accepted only when exactly one hit has the same normalized title and a year within ±1,
//	   or the search returns a single hit
//	4. otherwise the video stays where it is and goes to _needs-review.txt with the top 3 candidates
//
// A title already present as a scheme folder keeps that folder's name and year, so nothing already named churns when
// TMDB spells it differently. Lookups are cached in the store: hits for good, misses for Options.Retry.

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/tmdb"
)

var (
	schemeMovieDir = regexp.MustCompile(`^(.+) \((\d{4})\) \{(tmdb|imdb)-([0-9a-z]+)\}$`)
	schemeShowDir  = regexp.MustCompile(`^(.+) \((\d{4})\) \{(tmdb|tvdb)-(\d+)\}$`)
	tagInName      = regexp.MustCompile(`\{(tmdb|imdb|tvdb)-([0-9a-z]+)\}`)
	schemeEpisode  = regexp.MustCompile(`^(.+ \(\d{4}\)) - S(\d{2,})E(\d{2,})(?:-E(\d{2,}))?(?: - (.*?))?(?: \[[^\]]*\])?$`)
)

// reviewItem is one line of _needs-review.txt.
type reviewItem struct{ Path, Why string }

type cachedIdentity struct {
	Title      Title     `json:"title"`
	Why        string    `json:"why,omitempty"` // set when no title was accepted
	Candidates []string  `json:"candidates,omitempty"`
	At         time.Time `json:"at"`
}

// identifier resolves identities for one run. kind is "movie" or "show".
type identifier struct {
	tmdb    *tmdb.Client
	cache   kvCache
	now     time.Time
	retry   time.Duration
	library map[string][]Title // kind + normalized name → titles present as scheme folders
	byID    map[string]Title   // scheme folder title per id: the name everything else adopts
}

// kvCache is the slice of the store the identifier needs (get/put JSON under a key).
type kvCache interface {
	get(ctx context.Context, key string, v any) bool
	put(ctx context.Context, key string, v any)
}

func newIdentifier(c *tmdb.Client, cache kvCache, now time.Time, retry time.Duration) *identifier {
	return &identifier{tmdb: c, cache: cache, now: now, retry: retry, library: map[string][]Title{}, byID: map[string]Title{}}
}

// learn records the scheme folders already in the library.
func (id *identifier) learn(kind string, tops []string) {
	re := schemeMovieDir
	if kind == "show" {
		re = schemeShowDir
	}
	for _, top := range tops {
		m := re.FindStringSubmatch(path.Base(top))
		if m == nil || (m[3] != "tmdb" && m[3] != "tvdb") {
			continue
		}
		t := Title{ID: m[3] + "-" + m[4], Name: m[1], Year: atoi(m[2])}
		if _, seen := id.byID[t.ID]; !seen {
			id.byID[t.ID] = t
			id.library[kind+":"+normTitle(t.Name)] = append(id.library[kind+":"+normTitle(t.Name)], t)
		}
	}
}

// canonical gives a title the name of its scheme folder when the library has one.
func (id *identifier) canonical(t Title) Title {
	if s, ok := id.byID[t.ID]; ok {
		s.Original = t.Original
		return s
	}
	return t
}

// resolve names the film or show a top-level folder (or loose file) holds; Why is set when it cannot.
func (id *identifier) resolve(ctx context.Context, kind, top string) (cachedIdentity, error) {
	name := path.Base(top)
	if strings.Contains(name, ".") && videoExt[lowerExt(name)] { // a loose file
		name = stem(name)
	}
	re := schemeMovieDir
	if kind == "show" {
		re = schemeShowDir
	}
	if m := re.FindStringSubmatch(name); m != nil && (m[3] == "tmdb" || (kind == "show" && m[3] == "tvdb")) {
		return cachedIdentity{Title: id.canonical(Title{ID: m[3] + "-" + m[4], Name: m[1], Year: atoi(m[2])})}, nil
	}
	var c cachedIdentity
	key := "identity:" + kind + ":" + top
	if !id.cache.get(ctx, key, &c) || (c.Why != "" && id.now.Sub(c.At) >= id.retry) {
		var err error
		if c, err = id.lookup(ctx, kind, name); err != nil {
			return cachedIdentity{}, err // TMDB unreachable: nothing is cached, the next run asks again
		}
		c.At = id.now
		id.cache.put(ctx, key, c)
	}
	c.Title = id.canonical(c.Title)
	return c, nil
}

func (id *identifier) lookup(ctx context.Context, kind, name string) (cachedIdentity, error) {
	title, year := parseRelease(name)
	if m := tagInName.FindStringSubmatch(name); m != nil {
		return id.byTag(ctx, kind, m[1], m[2], title, year)
	}
	if title == "" {
		return cachedIdentity{Why: "no title in the name"}, nil
	}
	var mine []Title // step 2: exactly one scheme folder with this name (and year)
	for _, t := range id.library[kind+":"+normTitle(title)] {
		if year == 0 || abs(t.Year-year) <= 1 {
			mine = append(mine, t)
		}
	}
	if len(mine) == 1 {
		return cachedIdentity{Title: mine[0]}, nil
	}
	search := id.tmdb.SearchMovie
	if kind == "show" {
		search = id.tmdb.SearchTV
	}
	hits, err := search(ctx, title, year)
	if err == nil && len(hits) == 0 && year > 0 {
		hits, err = search(ctx, title, 0)
	}
	if err != nil {
		return cachedIdentity{}, err
	}
	if r, ok := confident(hits, title, year); ok {
		return cachedIdentity{Title: Title{ID: fmt.Sprintf("tmdb-%d", r.ID), Name: r.Name, Original: r.Original, Year: r.Year}}, nil
	}
	c := cachedIdentity{Why: fmt.Sprintf("no confident TMDB match for %s", withYear(title, year))}
	for _, r := range hits[:min(3, len(hits))] {
		c.Candidates = append(c.Candidates, fmt.Sprintf("%s (%d) {tmdb-%d}", r.Name, r.Year, r.ID))
	}
	if len(c.Candidates) == 0 {
		c.Why += ": TMDB lists nothing"
	}
	return c, nil
}

// byTag maps a tag in the name to a TMDB title; an id TMDB cannot map stays as it is (the planner leaves it).
func (id *identifier) byTag(ctx context.Context, kind, source, value, title string, year int) (cachedIdentity, error) {
	switch {
	case source == "tmdb":
		return cachedIdentity{Title: Title{ID: "tmdb-" + value, Name: title, Year: year}}, nil
	case source == "tvdb" && kind == "show":
		if t, ok := id.byID["tvdb-"+value]; ok {
			return cachedIdentity{Title: t}, nil
		}
	}
	external := map[string]string{"imdb": "imdb_id", "tvdb": "tvdb_id"}[source]
	movies, shows, err := id.tmdb.Find(ctx, value, external)
	if err != nil {
		return cachedIdentity{}, err
	}
	hits := movies
	if kind == "show" {
		hits = shows
	}
	if len(hits) == 0 {
		return cachedIdentity{Title: Title{ID: source + "-" + value, Name: title, Year: year},
			Why: fmt.Sprintf("TMDB knows no %s for {%s-%s}", kind, source, value)}, nil
	}
	r := hits[0]
	if title == "" { // the folder's own name wins (the owner's spelling); TMDB's otherwise
		title, year = r.Name, r.Year
	}
	return cachedIdentity{Title: Title{ID: fmt.Sprintf("tmdb-%d", r.ID), Name: title, Original: r.Original, Year: year}}, nil
}

// confident is the acceptance rule: one exact title within a year, else the only hit.
func confident(hits []tmdb.Result, title string, year int) (tmdb.Result, bool) {
	want := normTitle(title)
	var exact []tmdb.Result
	for _, r := range hits {
		if (normTitle(r.Name) == want || normTitle(r.Original) == want) && year > 0 && r.Year > 0 && abs(r.Year-year) <= 1 {
			exact = append(exact, r)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], true
	case len(exact) == 0 && len(hits) == 1:
		return hits[0], true
	}
	return tmdb.Result{}, false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var (
	titleYearParens = regexp.MustCompile(`^(.+?)` + pyS + `*\((\d{4})\)`)
	releaseCut      = regexp.MustCompile(`(?i)(?:^|[ \[(])(?:S\d{1,2}(?:E\d+)?|season|series|complete|\d{3,4}p|web[ -]?(?:dl|rip)?|blu[ -]?ray|bdrip|brrip|hdtv|dvdrip|x26[45]|h[ .]?26[45]|hevc|remux|uhd|imax|extended|unrated|proper|repack)(?:$|[ \])\-])`)
	yearToken       = regexp.MustCompile(`\b((?:19|20)\d\d)\b`)
	bracketOpen     = regexp.MustCompile(`[\[(]`)
)

// parseRelease reads (title, year) from a release name: 'Alien.1979.Theatrical.Cut.UHD.BluRay.2160p-X' → Alien, 1979;
// 'A Beautiful Day In The Neighborhood (2019) [1080p]' → ..., 2019; 'Breaking.Bad.S01.1080p' → Breaking Bad, 0.
func parseRelease(name string) (string, int) {
	name = tagInName.ReplaceAllString(name, "")
	if m := titleYearParens.FindStringSubmatch(name); m != nil && strings.TrimSpace(m[1]) != "" {
		return strings.Trim(clean(m[1]), " -–"), atoi(m[2])
	}
	flat := strings.NewReplacer(".", " ", "_", " ").Replace(name)
	cut, year := len(flat), 0
	for _, m := range yearToken.FindAllStringSubmatchIndex(flat, -1) {
		if strings.TrimSpace(flat[:m[2]]) != "" { // a year needs a title before it ('1917 2019', not '2012')
			cut, year = m[0], atoi(flat[m[2]:m[3]])
			break
		}
	}
	if m := releaseCut.FindStringIndex(flat); m != nil && m[0] < cut && strings.TrimSpace(flat[:m[0]]) != "" {
		cut = m[0]
	}
	if m := bracketOpen.FindStringIndex(flat); m != nil && m[0] < cut && m[0] > 0 {
		cut = m[0]
	}
	return strings.Trim(clean(flat[:cut]), " -–"), year
}

// ---- episode rows

type cachedSeason struct {
	Episodes []tmdb.Episode `json:"episodes"`
	Found    bool           `json:"found"`
	At       time.Time      `json:"at"`
}

const seasonTTL = 7 * 24 * time.Hour

// season lists a TMDB season, cached for a week (a missing one for a day: new seasons appear).
func (id *identifier) season(ctx context.Context, show, n int) ([]Episode, error) {
	key := fmt.Sprintf("tmdb:season:%d:%d", show, n)
	var c cachedSeason
	if !id.cache.get(ctx, key, &c) || id.now.Sub(c.At) > seasonTTL || (!c.Found && id.now.Sub(c.At) > 24*time.Hour) {
		eps, found, err := id.tmdb.Season(ctx, show, n)
		if err != nil {
			return nil, err
		}
		c = cachedSeason{eps, found, id.now}
		id.cache.put(ctx, key, c)
	}
	out := make([]Episode, 0, len(c.Episodes))
	for _, e := range c.Episodes {
		out = append(out, Episode{e.Season, e.Episode, e.Name})
	}
	return out, nil
}

type cachedShow struct {
	Show  tmdb.Show `json:"show"`
	Found bool      `json:"found"`
	At    time.Time `json:"at"`
}

func (id *identifier) show(ctx context.Context, show int) (tmdb.Show, bool, error) {
	key := fmt.Sprintf("tmdb:tv:%d", show)
	var c cachedShow
	if !id.cache.get(ctx, key, &c) || id.now.Sub(c.At) > seasonTTL {
		s, found, err := id.tmdb.TV(ctx, show)
		if err != nil {
			return tmdb.Show{}, false, err
		}
		c = cachedShow{s, found, id.now}
		id.cache.put(ctx, key, c)
	}
	return c.Show, c.Found, nil
}

// episodeList is every episode TMDB lists for a show, in (season, episode) order; empty for a non-TMDB id.
func (id *identifier) episodeList(ctx context.Context, show Title, seasons []int) ([]Episode, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(show.ID, "tmdb-"))
	if err != nil || !strings.HasPrefix(show.ID, "tmdb-") {
		return nil, nil
	}
	if seasons == nil {
		s, found, err := id.show(ctx, n)
		if err != nil || !found {
			return nil, err
		}
		seasons = s.Seasons
	}
	var out []Episode
	for _, s := range seasons {
		eps, err := id.season(ctx, n, s)
		if err != nil {
			return nil, err
		}
		out = append(out, eps...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		return out[i].Episode < out[j].Episode
	})
	return out, nil
}

// ownEpisode reads the rows of a file already named by the scheme inside its show's folder: its own numbers and
// title, so a re-run never renames it because TMDB spells a title differently.
func ownEpisode(f string, show Title) ([]Episode, bool) {
	m := schemeEpisode.FindStringSubmatch(stem(path.Base(f)))
	if m == nil || m[1] != withYear(show.Name, show.Year) {
		return nil, false
	}
	first, last := atoi(m[3]), atoi(m[3])
	if m[4] != "" {
		last = atoi(m[4])
	}
	rows := []Episode{}
	for n := first; n <= last; n++ {
		rows = append(rows, Episode{atoi(m[2]), n, m[5]})
	}
	return rows, true
}
