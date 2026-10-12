package rename

// Identity: which film or show each video is, and the episode rows of each TV video.
//
//	1. a scheme folder ("Title (Year) {tmdb-N}") or a tag in the name ({tmdb-N}, {imdb-ttN}, {tvdb-N})
//	2. TMDB search, direct: accepted only when exactly one hit has the same normalized title and a year within ±1,
//	   or the search returns a single hit
//	3. otherwise the video stays where it is and goes to _needs-review.txt with the top 3 candidates
//
// The library's own folders never name a new arrival (a UK pack of "The Office" is not the US show already there);
// they only lend their spelling: a title already present as a scheme folder keeps that folder's name and year, so
// nothing already named churns when TMDB spells it differently. Nothing is cached here: env.TMDB keeps every answer
// for a day, so a miss is asked again tomorrow and a hit costs nothing until then.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

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

type identity struct {
	Title      Title
	Why        string // set when no title was accepted
	Candidates []string
}

// identifier resolves identities for one run. kind is "movie" or "show".
type identifier struct {
	tmdb *tmdb.Client
	byID map[string]Title // kind:id → the scheme folder's title, the name everything else adopts. Keyed by kind:
	// TMDB's film and TV ids overlap (film 1398 is "Stalker", show 1398 "The Sopranos").
}

func newIdentifier(c *tmdb.Client) *identifier {
	return &identifier{tmdb: c, byID: map[string]Title{}}
}

func kindOf(kind string) tmdb.Kind {
	if kind == "show" {
		return tmdb.TV
	}
	return tmdb.Movie
}

func yearOf(r tmdb.Result) int {
	if r.Date.IsZero() {
		return 0
	}
	return r.Date.Year()
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
		if _, seen := id.byID[kind+":"+m[3]+"-"+m[4]]; !seen {
			id.byID[kind+":"+m[3]+"-"+m[4]] = Title{ID: m[3] + "-" + m[4], Name: m[1], Year: atoi(m[2])}
		}
	}
}

// canonical gives a title the name of its scheme folder when the library has one.
func (id *identifier) canonical(kind string, t Title) Title {
	if s, ok := id.byID[kind+":"+t.ID]; ok {
		s.Original = t.Original
		return s
	}
	return t
}

// resolve names the film or show a top-level folder (or loose file) holds; Why is set when it cannot. videos are the
// folder's video paths: a name without a year borrows one from them ('Shogun/S01/Shogun.2024.S01E01...').
func (id *identifier) resolve(ctx context.Context, kind, top string, videos []string) (identity, error) {
	name := path.Base(top)
	if strings.Contains(name, ".") && videoExt[lowerExt(name)] { // a loose file
		name = stem(name)
	}
	re := schemeMovieDir
	if kind == "show" {
		re = schemeShowDir
	}
	if m := re.FindStringSubmatch(name); m != nil && (m[3] == "tmdb" || (kind == "show" && m[3] == "tvdb")) {
		return identity{Title: id.canonical(kind, Title{ID: m[3] + "-" + m[4], Name: m[1], Year: atoi(m[2])})}, nil
	}
	c, err := id.lookup(ctx, kind, name, videos)
	c.Title = id.canonical(kind, c.Title)
	return c, err
}

func (id *identifier) lookup(ctx context.Context, kind, name string, videos []string) (identity, error) {
	title, year := parseRelease(name)
	for _, v := range videos {
		if year != 0 {
			break
		}
		if t, y := parseRelease(stem(path.Base(v))); y != 0 && normTitle(t) == normTitle(title) {
			year = y
		}
	}
	if m := tagInName.FindStringSubmatch(name); m != nil {
		return id.byTag(ctx, kind, m[1], m[2], title, year)
	}
	if title == "" {
		return identity{Why: "no title in the name"}, nil
	}
	hits, err := id.tmdb.Search(ctx, kindOf(kind), title, year)
	if err == nil && len(hits) == 0 && year > 0 {
		hits, err = id.tmdb.Search(ctx, kindOf(kind), title, 0)
	}
	if err != nil {
		return identity{}, err
	}
	if r, ok := confident(hits, title, year); ok {
		return identity{Title: Title{ID: fmt.Sprintf("tmdb-%d", r.ID), Name: r.Title, Original: r.OriginalTitle, Year: yearOf(r)}}, nil
	}
	c := identity{Why: fmt.Sprintf("no confident TMDB match for %s", withYear(title, year))}
	for _, r := range hits[:min(3, len(hits))] {
		c.Candidates = append(c.Candidates, fmt.Sprintf("%s (%d) {tmdb-%d}", r.Title, yearOf(r), r.ID))
	}
	if len(c.Candidates) == 0 {
		c.Why += ": TMDB lists nothing"
	}
	return c, nil
}

// byTag maps a tag in the name to a TMDB title; an id TMDB cannot map stays as it is (the planner leaves it).
func (id *identifier) byTag(ctx context.Context, kind, source, value, title string, year int) (identity, error) {
	switch {
	case source == "tmdb":
		return identity{Title: Title{ID: "tmdb-" + value, Name: title, Year: year}}, nil
	case source == "tvdb" && kind == "show":
		if t, ok := id.byID["show:tvdb-"+value]; ok {
			return identity{Title: t}, nil
		}
	}
	external := map[string]string{"imdb": "imdb_id", "tvdb": "tvdb_id"}[source]
	movies, shows, err := id.tmdb.Find(ctx, value, external)
	if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
		return identity{}, err
	}
	hits := movies
	if kind == "show" {
		hits = shows
	}
	if len(hits) == 0 {
		return identity{Title: Title{ID: source + "-" + value, Name: title, Year: year},
			Why: fmt.Sprintf("TMDB knows no %s for {%s-%s}", kind, source, value)}, nil
	}
	r := hits[0]
	if title == "" { // the folder's own name wins (the owner's spelling); TMDB's otherwise
		title, year = r.Title, yearOf(r)
	}
	return identity{Title: Title{ID: fmt.Sprintf("tmdb-%d", r.ID), Name: title, Original: r.OriginalTitle, Year: year}}, nil
}

// confident is the acceptance rule: one exact title within a year, else the only hit.
func confident(hits []tmdb.Result, title string, year int) (tmdb.Result, bool) {
	want := normTitle(title)
	var exact []tmdb.Result
	for _, r := range hits {
		y := yearOf(r)
		if (normTitle(r.Title) == want || normTitle(r.OriginalTitle) == want) && year > 0 && y > 0 && abs(y-year) <= 1 {
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
	// a torrent site's tag in front of the release: a www. host, or any host in brackets ('www.site.org  -  ',
	// '[ www.site.com ] - ', '[SITE.MX]'). A bracket with no host in it is a title ('[REC]').
	siteTag = regexp.MustCompile(`(?i)^(?:www\.[\w-]+\.[a-z]{2,}|\[ *(?:www\.)?[\w-]+\.[a-z]{2,} *\])[ ._\-–]*`)
)

// parseRelease reads (title, year) from a release name: 'Alien.1979.Theatrical.Cut.UHD.BluRay.2160p-X' → Alien, 1979;
// 'A Beautiful Day In The Neighborhood (2019) [1080p]' → ..., 2019; 'Breaking.Bad.S01.1080p' → Breaking Bad, 0.
func parseRelease(name string) (string, int) {
	name = siteTag.ReplaceAllString(tagInName.ReplaceAllString(name, ""), "")
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

// showDetails is a TMDB show's record; found is false when TMDB has no such show.
func (id *identifier) showDetails(ctx context.Context, show int) (tmdb.Details, bool, error) {
	d, err := id.tmdb.Details(ctx, tmdb.TV, show)
	if errors.Is(err, tmdb.ErrNotFound) {
		return d, false, nil
	}
	return d, err == nil, err
}

// episodeList is every episode TMDB lists for a show in the given seasons (nil: all of them, specials included), in
// (season, episode) order; empty for a non-TMDB id. A season TMDB does not have lists nothing.
func (id *identifier) episodeList(ctx context.Context, show Title, seasons []int) ([]Episode, error) {
	n, err := numericID(show.ID)
	if err != nil {
		return nil, nil
	}
	if seasons == nil {
		d, found, err := id.showDetails(ctx, n)
		if err != nil || !found {
			return nil, err
		}
		for _, s := range d.Seasons {
			seasons = append(seasons, s.Number)
		}
	}
	var out []Episode
	for _, s := range seasons {
		eps, err := id.tmdb.Episodes(ctx, n, s)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return nil, err
		}
		for _, e := range eps {
			out = append(out, Episode{e.Season, e.Number, e.Name})
		}
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
