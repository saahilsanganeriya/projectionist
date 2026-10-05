// Package lists shelves Letterboxd lists in Plex and Jellyfin: the canon (Letterboxd's Top 500, the Criterion
// Collection, Sight and Sound, TSPDT, Best Picture, the Palme d'Or) and the owner's own public lists. Each list
// becomes a Plex collection and a Jellyfin BoxSet holding the films on the NAS, in list order, with a summary
// ("On the NAS: 312 of 500 · seen 201 · <list URL>") and a generated poster; chosen lists get a Plex Home row.
//
// Sources: Letterboxd list pages, the owner's export, the movie folder names ({tmdb-N}). Plex and Jellyfin are
// sinks: they are read only to address items (TMDB id → item id; that same answer carries the owner's watched
// flags) and to see the stage's own collections. Writes are reconciled every run, retried with backoff, bounded by
// a per-app budget, and never fail the run. A collection the stage did not make is never touched.
package lists

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"projectionist/internal/stage"
)

const name = "lists"

// Source is one canon list.
type Source struct {
	Name    string `toml:"name"`
	URL     string `toml:"url"`
	Limit   int    `toml:"limit"`   // keep the first N entries: the Top 250 is the Top 500's first 250
	Reverse bool   `toml:"reverse"` // the list runs newest-first; shelve it oldest-first (Criterion spine order)
	Poster  string `toml:"poster"`  // an image file to use instead of the generated poster
}

type Options struct {
	DryRun          bool          `toml:"dry_run"` // read and count everything, write nothing
	Only            []string      `toml:"only"`    // shelve only these lists (by name); empty = all
	User            string        `toml:"letterboxd_user"`
	LetterboxdURL   string        `toml:"letterboxd_url"`
	ShortURL        string        `toml:"shortlink_url"`
	UserAgent       string        `toml:"user_agent"`
	RequestInterval time.Duration `toml:"request_interval"`
	CanonRefresh    time.Duration `toml:"canon_refresh"` // canon list pages are read again after this
	OwnRefresh      time.Duration `toml:"own_refresh"`   // the owner's lists, films page and diary feed
	ExportDir       string        `toml:"export_dir"`
	OwnLists        bool          `toml:"own_lists"`
	SkipOwn         []string      `toml:"skip_own"` // the owner's lists to leave out, by name or slug
	MoviesDir       string        `toml:"movies_dir"`
	MissingFile     string        `toml:"missing_file"`
	PlexSection     int           `toml:"plex_section"`
	PlexRefresh     time.Duration `toml:"plex_refresh"` // the TMDB id → ratingKey map is read at most this often
	PlexBudget      time.Duration `toml:"plex_budget"`
	JellyfinUser    string        `toml:"jellyfin_user"`
	JellyfinBudget  time.Duration `toml:"jellyfin_budget"`
	Home            []string      `toml:"home"`       // lists promoted to the owner's Plex Home, in row order
	HomeAfter       string        `toml:"home_after"` // the Plex hub the first promoted row follows
	Canon           []Source      `toml:"canon"`
}

func defaults() Options {
	return Options{
		User:            "saahilsan",
		LetterboxdURL:   "https://letterboxd.com",
		ShortURL:        "https://boxd.it",
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 projectionist/1.0 (+personal media server)",
		RequestInterval: 2 * time.Second,
		CanonRefresh:    7 * 24 * time.Hour,
		OwnRefresh:      20 * time.Hour,
		ExportDir:       "/volume1/docker/projectionist/data/letterboxd",
		OwnLists:        true,
		MoviesDir:       "/volume1/media/Movies",
		MissingFile:     "/volume1/media/_lists-missing.txt",
		PlexSection:     4,
		PlexRefresh:     24 * time.Hour,
		PlexBudget:      30 * time.Minute, // the first build of ~70 shelves is ~2,000 calls; later runs send a handful
		JellyfinUser:    "saahil",
		JellyfinBudget:  15 * time.Minute,
		HomeAfter:       "movie.recentlyadded",
	}
}

// The list defaults are applied after decoding, never decoded into: TOML fills an existing slice element by
// element, so a configured canon entry would inherit the default's limit or reverse (and the shared defaults
// would change for the next run).
var defaultHome = []string{"Letterboxd Top 500", "Criterion Collection", "Sight & Sound 2022"}

// defaultCanon was checked on 2026-10-05. Letterboxd retired its standalone Top 250 when the Top 500 launched
// (2026-02-23; the old list answers 404), so the Top 250 is the Top 500's first 250 and costs no extra request.
var defaultCanon = []Source{
	{Name: "Letterboxd Top 500", URL: "https://letterboxd.com/official/list/letterboxds-top-500-films/"},
	{Name: "Letterboxd Top 250", URL: "https://letterboxd.com/official/list/letterboxds-top-500-films/", Limit: 250},
	{Name: "Criterion Collection", URL: "https://letterboxd.com/jbutts15/list/the-complete-criterion-collection/", Reverse: true},
	{Name: "Sight & Sound 2022", URL: "https://letterboxd.com/sight_and_sound/list/the-greatest-films-of-all-time-2022/"},
	{Name: "Sight & Sound 2022 Directors", URL: "https://letterboxd.com/sight_and_sound/list/directors-100-greatest-films-of-all-time/"},
	{Name: "TSPDT 1000", URL: "https://letterboxd.com/tspdtfanaccount/list/the-1000-greatest-films-21st-edition-2026/"},
	{Name: "Best Picture", URL: "https://letterboxd.com/oscars/list/oscar-winning-films-best-picture/"},
	{Name: "Palme d'Or", URL: "https://letterboxd.com/festival_cannes/list/70-years-of-the-palme-dor-70-ans-de-la-palme/"},
}

// list is one Letterboxd list in shelf order.
type list struct {
	Key      string // stable id: the canon URL (+ limit), or "own:<boxd.it code>"
	Name     string // shelf title
	URL      string
	Canon    bool
	Order    int // canon position, for the sort title
	Slug     string
	Poster   string
	Entries  []entry
	Complete bool // every page was read (or is cached); an incomplete list is counted but not shelved
	about    string
}

func options(env *stage.Env) (Options, error) {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return opt, err
	}
	if opt.Canon == nil {
		opt.Canon = defaultCanon
	}
	if opt.Home == nil {
		opt.Home = defaultHome
	}
	return opt, nil
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt, err := options(env)
	if err != nil {
		return err
	}
	if _, err := env.Store.DB().ExecContext(ctx, cacheSchema+sinkSchema); err != nil {
		return fmt.Errorf("lists tables: %w", err)
	}
	start := env.Now()
	pace := &pacer{interval: opt.RequestInterval, now: env.Now, sleep: sleep}
	lb := &letterboxd{base: strings.TrimRight(opt.LetterboxdURL, "/"), short: strings.TrimRight(opt.ShortURL, "/"),
		user: opt.User, f: newFetcher(opt.UserAgent, pace, env.Log), db: env.Store.DB(), now: env.Now, log: env.Log}

	listLIDs, watched, err := readExport(opt.ExportDir)
	if err != nil {
		env.Log.Warn("letterboxd export unreadable", "event", "export_error", "err", err)
	}
	lists, err := gather(ctx, env, opt, lb, rep, listLIDs)
	if err != nil {
		return err
	}
	known, err := lb.films(ctx)
	if err != nil {
		return err
	}
	var slugs []string
	for _, l := range lists {
		for _, e := range l.Entries {
			slugs = append(slugs, e.Slug)
		}
	}
	resolveStart := env.Now()
	resolved, err := lb.resolve(ctx, slugs, known)
	if err != nil {
		return err
	}
	rep.Count("films_resolved", resolved)
	env.Log.Info("films resolved", "event", "resolve_done", "films", resolved, "requests", lb.f.requests,
		"duration_s", int(env.Now().Sub(resolveStart).Seconds()))

	seenLB, err := lb.refreshSeen(ctx, opt.OwnRefresh)
	if err != nil {
		return err
	}
	seen := seenSet{lb: seenLB, lids: map[string]bool{}}
	for _, lid := range watched {
		seen.lids[lid] = true
	}
	owned, err := ownedMovies(opt.MoviesDir)
	if err != nil {
		return err
	}

	// the addressing maps (their watched flags join "seen"); the apps answer side by side, so a slow one never
	// holds up the other
	px := &plexSink{env: env, opt: opt, log: env.Log}
	jf := &jellyfinSink{env: env, opt: opt, log: env.Log}
	both(func() { px.load(ctx) }, func() { jf.load(ctx) })
	seen.plex, seen.jf = px.watched, jf.watched

	var shelves []shelf
	for _, l := range lists {
		s := plan(l, known, owned, seen)
		shelves = append(shelves, s)
		env.Log.Info("list counted", "event", "list_counts", "list", l.Name, "complete", l.Complete, "total", s.Total,
			"resolved", s.Resolved, "owned", s.Owned, "seen", s.Seen)
	}
	unresolved := 0
	for _, s := range shelves {
		unresolved += s.Total - s.Resolved
	}
	blockedRuns(ctx, env, rep, lb.f.blocked, unresolved)
	rep.Count("letterboxd_requests", lb.f.requests)
	rep.Count("lists", len(lists))
	summary := fmt.Sprintf("%d lists, %d films unresolved, %d Letterboxd requests in %s", len(lists), unresolved,
		lb.f.requests, env.Now().Sub(start).Round(time.Second))
	if opt.DryRun {
		rep.Summary = "dry run: " + summary
		return nil
	}

	if err := writeMissing(opt.MissingFile, shelves); err != nil {
		env.Log.Warn("missing-films file not written", "event", "missing_file_error", "path", opt.MissingFile, "err", err)
	}
	var shelve []shelf
	for _, s := range shelves {
		if s.List.Complete && (len(opt.Only) == 0 || contains(opt.Only, s.List.Name)) {
			shelve = append(shelve, s)
		}
	}
	both(func() { px.reconcile(ctx, shelve) }, func() { jf.reconcile(ctx, shelve) })
	rep.Count("plex_writes", px.w.writes)
	rep.Count("jellyfin_writes", jf.w.writes)
	rep.Count("name_taken", px.taken+jf.taken) // lists not shelved because one of the owner's collections has the title
	rep.Summary = fmt.Sprintf("%s; plex %d writes%s, jellyfin %d writes%s", summary, px.w.writes, deferred(px.w),
		jf.w.writes, deferred(jf.w))
	if px.taken+jf.taken > 0 {
		rep.Summary += fmt.Sprintf("; %d shelves skipped: title taken by a hand-made collection", px.taken+jf.taken)
	}
	return ctx.Err()
}

func both(a, b func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a() }()
	b()
	wg.Wait()
}

func deferred(w *writer) string {
	if w.stopped == "" {
		return ""
	}
	return " (rest deferred: " + w.stopped + ")"
}

// blockedRuns alerts when Letterboxd has refused us three runs in a row while films were still unresolved: one
// refusal heals by itself, a lasting one needs the owner.
func blockedRuns(ctx context.Context, env *stage.Env, rep *stage.Report, blocked bool, unresolved int) {
	var n int
	env.Store.GetJSON(ctx, name, "blocked_runs", &n)
	if blocked {
		n++
		env.Log.Warn("letterboxd refused requests", "event", "letterboxd_blocked", "runs_in_a_row", n, "unresolved", unresolved)
	} else {
		n = 0
	}
	if err := env.Store.PutJSON(ctx, name, "blocked_runs", n); err != nil {
		env.Log.Warn("state write failed", "event", "store_error", "err", err)
	}
	if n >= 3 && unresolved > 0 {
		rep.Alert("Letterboxd has refused requests %d runs in a row; %d films are still unresolved", n, unresolved)
	}
}

// gather builds every list: the canon from config, then the owner's public lists (his lists page 1 plus the lists
// in his export, which name lists by code).
func gather(ctx context.Context, env *stage.Env, opt Options, lb *letterboxd, rep *stage.Report, listLIDs []string) ([]list, error) {
	var out []list
	for i, src := range opt.Canon {
		u, err := url.Parse(src.URL)
		if err != nil {
			return nil, fmt.Errorf("canon %q: %w", src.Name, err)
		}
		_, entries, complete, err := lb.list(ctx, u.Path, opt.CanonRefresh, src.Limit)
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		if err != nil {
			env.Log.Warn("canon list unavailable", "event", "list_unavailable", "list", src.Name, "err", err)
		}
		if src.Reverse {
			entries = reversed(entries)
		}
		key := src.URL
		if src.Limit > 0 {
			key += "#" + strconv.Itoa(src.Limit)
		}
		out = append(out, list{Key: key, Name: src.Name, URL: src.URL, Canon: true, Order: i, Poster: src.Poster,
			Entries: entries, Complete: complete && err == nil})
	}
	if !opt.OwnLists {
		return out, nil
	}
	own, err := ownLists(ctx, env, opt, lb, rep, listLIDs)
	return append(out, own...), err
}

func ownLists(ctx context.Context, env *stage.Env, opt Options, lb *letterboxd, rep *stage.Report, listLIDs []string) ([]list, error) {
	var paths []string
	index, ok, err := lb.cachedPage(ctx, "/"+opt.User+"/lists/", opt.OwnRefresh)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	if ok {
		paths = append(paths, index.Lists...)
	}
	for _, lid := range listLIDs {
		p, err := lb.listPath(ctx, lid)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			env.Log.Warn("list code unresolved", "event", "list_unavailable", "lid", lid, "err", err)
			continue
		}
		if p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []list
	private, keys := 0, map[string]bool{}
	for i, path := range paths {
		if i > 0 && path == paths[i-1] {
			continue
		}
		slug := strings.TrimSuffix(path[strings.LastIndex(strings.TrimSuffix(path, "/"), "/")+1:], "/")
		first, entries, complete, err := lb.list(ctx, path, opt.OwnRefresh, 0)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, err
		case err != nil: // 404: private or deleted; a private list stays off the shared library
			private++
			env.Log.Info("own list not public", "event", "own_list_skipped", "path", path)
			continue
		case first.Title == "" && !complete:
			continue // never read and unreadable now
		}
		if contains(opt.SkipOwn, first.Title) || contains(opt.SkipOwn, slug) {
			continue
		}
		key := "own:" + first.LID
		if first.LID == "" {
			key = "own:" + path
		}
		if keys[key] { // a renamed list reached by its old path too (the code → path cache lags a rename)
			continue
		}
		keys[key] = true
		out = append(out, list{Key: key, Name: first.Title, URL: lb.base + path, Slug: slug, Entries: entries,
			Complete: complete, about: first.About})
	}
	rep.Count("own_lists_private_or_gone", private)
	disambiguate(out)
	return out, nil
}

// disambiguate gives own lists that share a name (13 are called "what is this genre called") the start of their
// description, or their slug, after the name.
func disambiguate(ls []list) {
	count := map[string]int{}
	for _, l := range ls {
		count[strings.ToLower(strings.TrimSpace(l.Name))]++
	}
	for i := range ls {
		l := &ls[i]
		l.Name = strings.TrimSpace(l.Name)
		if count[strings.ToLower(l.Name)] < 2 {
			continue
		}
		tag := firstLine(l.about, 40)
		if tag == "" {
			tag = l.Slug
		}
		l.Name += " · " + tag
	}
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max])) + "…"
}

func reversed(es []entry) []entry {
	out := make([]entry, len(es))
	for i, e := range es {
		out[len(es)-1-i] = e
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

var tmdbFolder = regexp.MustCompile(`\{tmdb-(\d+)\}$`)

// ownedMovies reads the TMDB ids of the movie folders ("Title (Year) {tmdb-N}").
func ownedMovies(dir string) (map[int]bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("movies: %w", err)
	}
	owned := map[int]bool{}
	for _, e := range ents {
		if m := tmdbFolder.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			n, _ := strconv.Atoi(m[1])
			owned[n] = true
		}
	}
	return owned, nil
}

// seenSet answers "has the owner seen this film" from every source: his Letterboxd (export, films page, diary feed)
// and the watched flags Plex and Jellyfin return with the addressing maps.
type seenSet struct {
	lb       map[string]bool // slug:<s>, tmdb:<n>
	lids     map[string]bool // boxd.it codes from watched.csv
	plex, jf map[int]bool
}

func (s seenSet) has(e entry, f film) bool {
	if s.lb["slug:"+e.Slug] || (e.LID != "" && s.lids[e.LID]) {
		return true
	}
	if f.Kind != "movie" || f.TMDB == 0 {
		return false // a TV id shares numbers with films: only a movie id may match
	}
	return s.lb["tmdb:"+strconv.Itoa(f.TMDB)] || s.plex[f.TMDB] || s.jf[f.TMDB]
}

// shelf is the desired state of one list in both apps.
type shelf struct {
	List                         list
	SortTitle, Summary           string
	Members                      []int // owned movie TMDB ids, list order, no repeats
	Missing                      []missing
	Total, Resolved, Owned, Seen int
}

type missing struct {
	Pos   int
	Title string
	Note  string // "TV", "not checked yet"
}

func plan(l list, films map[string]film, owned map[int]bool, seen seenSet) shelf {
	s := shelf{List: l, SortTitle: sortTitle(l)}
	slugs, ids := map[string]bool{}, map[int]bool{}
	pos := 0
	for _, e := range l.Entries {
		if slugs[e.Slug] {
			continue
		}
		slugs[e.Slug] = true
		pos++
		f := films[e.Slug]
		s.Total++
		if f.Checked {
			s.Resolved++
		}
		if seen.has(e, f) {
			s.Seen++
		}
		switch {
		case f.Kind == "movie" && owned[f.TMDB]:
			s.Owned++
			if !ids[f.TMDB] {
				ids[f.TMDB] = true
				s.Members = append(s.Members, f.TMDB)
			}
		case !f.Checked:
			s.Missing = append(s.Missing, missing{pos, e.Title, "not checked yet"})
		case f.Kind == "tv":
			s.Missing = append(s.Missing, missing{pos, e.Title, "TV"})
		default:
			s.Missing = append(s.Missing, missing{pos, e.Title, ""})
		}
	}
	s.Summary = fmt.Sprintf("On the NAS: %d of %d · seen %d · %s", s.Owned, s.Total, s.Seen, l.URL)
	return s
}

// summaryMarker starts every summary the stage writes; it is how a collection made here is recognised after the
// state database is lost, and why a hand-made collection (no such summary) is never adopted.
const summaryMarker = "On the NAS: "

// sortTitle groups the canon shelves, then the owner's lists, ahead of every other collection ("~" sorts first).
func sortTitle(l list) string {
	if l.Canon {
		return fmt.Sprintf("~ Canon %02d %s", l.Order+1, l.Name)
	}
	return "~ Mine " + l.Name
}

// writeMissing rewrites the missing-films file only when its text changed, atomically.
func writeMissing(path string, shelves []shelf) error {
	var b strings.Builder
	b.WriteString("Films on the shelved Letterboxd lists that are not on the NAS (written by projectionist, stage lists).\n")
	for _, s := range shelves {
		fmt.Fprintf(&b, "\n== %s · %d of %d not on the NAS · %s\n", s.List.Name, len(s.Missing), s.Total, s.List.URL)
		for _, m := range s.Missing {
			line := fmt.Sprintf("%5d. %s", m.Pos, m.Title)
			if m.Note != "" {
				line += "  (" + m.Note + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	text := b.String()
	if old, err := os.ReadFile(path); err == nil && string(old) == text {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
