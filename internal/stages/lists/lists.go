// Package lists shelves Letterboxd lists in Plex and Jellyfin: the canon (Letterboxd's Top 500, the Criterion
// Collection, Sight and Sound, TSPDT, Best Picture, the Palme d'Or) and the owner's own public lists. Each list
// becomes a Plex collection and a Jellyfin BoxSet holding the films on the NAS, in list order, with a summary
// ("On the NAS: 312 of 500 · seen 201 · <list URL>") and a generated poster; chosen lists get a Plex Home row.
//
// Sources: Letterboxd list pages, the owner's export, the movie folder names ({tmdb-N}). Plex and Jellyfin are
// sinks: they are read only to address items (TMDB id → item id; that same answer carries the owner's watched
// flags) and to see the collections. Each app loads and reconciles on its own, so a slow one never holds up the
// other; writes are retried with backoff, bounded by a per-app budget, and never fail the run. A collection the
// stage did not make is never touched; one it made for a list that is gone (private or deleted on Letterboxd, left
// out by skip_own, dropped from the config) is deleted, unless the owner rewrote its summary. A shelf deleted by
// hand in an app is made again on the next run: skip_own, only and the canon config are the off switches.
package lists

import (
	"context"
	"errors"
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

	"projectionist/internal/config"
	"projectionist/internal/lbpace"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

const name = "lists"

// Source is one canon list.
type Source struct {
	Name   string `toml:"name" required:"the shelf's title"`
	URL    string `toml:"url" required:"the Letterboxd list's address"`
	Limit  int    `toml:"limit"`  // keep the first N entries (a Top 250 is the Top 500 with limit 250)
	Poster string `toml:"poster"` // an image file to use instead of the generated poster
}

type Options struct {
	DryRun         bool          `toml:"dry_run"` // read and count everything, write nothing
	Only           []string      `toml:"only"`    // shelve only these lists (by name); empty = all
	User           string        `toml:"letterboxd_user" required:"your Letterboxd username"`
	LetterboxdURL  string        `toml:"letterboxd_url"`
	ShortURL       string        `toml:"shortlink_url"`
	UserAgent      string        `toml:"user_agent"`
	CanonRefresh   time.Duration `toml:"canon_refresh"` // canon list pages are read again after this
	OwnRefresh     time.Duration `toml:"own_refresh"`   // the owner's lists, films page and diary feed
	ExportDir      string        `toml:"export_dir"`    // holds your unzipped Letterboxd export; "" = none
	OwnLists       bool          `toml:"own_lists"`
	SkipOwn        []string      `toml:"skip_own"` // the owner's lists to leave out: names or slugs, "*" matches any text
	MoviesDir      string        `toml:"movies_dir" required:"the folder that holds the films"`
	MissingFile    string        `toml:"missing_file"` // the shelved lists' films not on the NAS; "" = none
	PlexSection    int           `toml:"plex_section" required:"the id of the Plex Movies library"`
	PlexRefresh    time.Duration `toml:"plex_refresh"` // the TMDB id → ratingKey map is read at most this often
	PlexBudget     time.Duration `toml:"plex_budget"`
	JellyfinUser   string        `toml:"jellyfin_user" required:"your Jellyfin user name, whose library gets the shelves"`
	JellyfinBudget time.Duration `toml:"jellyfin_budget"`
	Home           []string      `toml:"home"`       // lists promoted to your Plex Home, in row order; none = no promotion
	HomeAfter      string        `toml:"home_after"` // the Plex hub the first promoted row follows
	Canon          []Source      `toml:"canon"`      // the Letterboxd lists to shelve, in shelf order; none = only your own
}

func defaults() Options {
	return Options{
		LetterboxdURL:  "https://letterboxd.com",
		ShortURL:       "https://boxd.it",
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 projectionist/1.0 (+personal media server)",
		CanonRefresh:   20 * time.Hour, // read on every daily run: the Top 500 changes as members rate
		OwnRefresh:     20 * time.Hour,
		OwnLists:       true,
		PlexRefresh:    24 * time.Hour,
		PlexBudget:     30 * time.Minute, // the first build of ~70 shelves is ~2,000 calls; later runs send a handful
		JellyfinBudget: 15 * time.Minute,
		HomeAfter:      "movie.recentlyadded",
	}
}

// defaultSkipOwn is applied after decoding, never decoded into: TOML fills an existing slice element by element. It
// keeps lists like "private reviews and notes" off a library every shared account sees, though Letterboxd shows them
// publicly.
var defaultSkipOwn = []string{"*private*"}

// list is one Letterboxd list in shelf order.
type list struct {
	Key      string // stable id: the canon URL (+ limit), or "own:<boxd.it code>"
	Name     string // shelf title
	URL      string
	Canon    bool
	Order    int // canon position, for the sort title
	Limit    int // the shelf holds the list's first Limit entries
	Slug     string
	Poster   string
	Entries  []entry
	Complete bool // every page was read (or is cached); an incomplete list is counted but not shelved
	about    string
}

func (Stage) Check(decode func(any) error) []config.Problem {
	opt := defaults()
	ps, _ := config.CheckStage(name, decode, &opt)
	return ps
}

func options(env *stage.Env) (Options, error) {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return opt, err
	}
	if opt.SkipOwn == nil {
		opt.SkipOwn = defaultSkipOwn
	}
	return opt, nil
}

type Stage struct{}

func (Stage) Name() string { return name }

// limiter paces every Letterboxd request; tests replace it with one on a fake clock.
var limiter = lbpace.Shared

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt, err := options(env)
	if err != nil {
		return err
	}
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, cacheSchema+sinkSchema); err != nil {
		return fmt.Errorf("lists tables: %w", err)
	}
	start := env.Now()
	lb := &letterboxd{base: strings.TrimRight(opt.LetterboxdURL, "/"), short: strings.TrimRight(opt.ShortURL, "/"),
		user: opt.User, f: newFetcher(opt.UserAgent, limiter, env.Log), db: db, now: env.Now, log: env.Log}

	listLIDs, watched, err := readExport(opt.ExportDir)
	if err != nil { // the run goes on without it: the export only adds list codes and older watched films
		env.Log.Warn("letterboxd export unreadable", "event", "export_error", "err", err)
		rep.Alert("letterboxd export not read (mount it read-only, or set export_dir = \"\"): %v", err)
	}
	made, err := shelvedKeys(ctx, db)
	if err != nil {
		return err
	}
	lists, retire, err := gather(ctx, env, opt, lb, rep, listLIDs, made)
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

	// Each app loads and reconciles in its own lane. What the shelves take from the apps (their watched flags for
	// "seen", the titles of collections the stage did not make) comes from each app's report of its previous run,
	// so a slow app never holds up the other; only an app that has never reported is waited for.
	px := &plexSink{env: env, opt: opt, log: env.Log, start: start}
	jf := &jellyfinSink{env: env, opt: opt, log: env.Log}
	lanes := []*lane{{sink: px, app: "plex"}, {sink: jf, app: "jellyfin"}}
	var shelves, shelve []shelf
	planned := make(chan struct{})
	var wg sync.WaitGroup
	for _, l := range lanes {
		l.known, _ = env.Store.GetJSON(ctx, name, l.app+"_report", &l.report) // read before the lane overwrites it
		l.loaded = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			fresh := l.sink.load(ctx)
			if !l.known {
				l.report = fresh
			}
			close(l.loaded)
			<-planned
			if !opt.DryRun {
				l.sink.reconcile(ctx, shelve, retire)
			}
		}()
	}
	taken := map[string]bool{}
	for _, l := range lanes {
		if !l.known {
			<-l.loaded
		}
		for _, t := range l.report.Taken {
			taken[t] = true
		}
	}
	seen.plex, seen.jf = lanes[0].report.watched(), lanes[1].report.watched()

	unresolved := 0
	canonTitles := map[string]bool{}
	for _, l := range lists { // the canon comes first
		s := plan(l, known, owned, seen)
		if s.Art, err = newArt(l); err != nil {
			env.Log.Warn("poster unavailable", "event", "poster_error", "list", l.Name, "err", err)
		}
		s.Title = shelfTitle(l.Name, taken)
		if l.Canon {
			canonTitles[fold(s.Title)] = true
		} else if canonTitles[fold(s.Title)] { // Plex would file both shelves under one tag
			s.Title += " · my list"
		}
		shelves = append(shelves, s)
		unresolved += s.Total - s.Resolved
		env.Log.Info("list counted", "event", "list_counts", "list", l.Name, "complete", l.Complete, "total", s.Total,
			"resolved", s.Resolved, "owned", s.Owned, "seen", s.Seen)
		if s.List.Complete && (len(opt.Only) == 0 || contains(opt.Only, l.Name)) {
			shelve = append(shelve, s)
		}
	}
	if !opt.DryRun && opt.MissingFile != "" {
		if err := writeMissing(opt.MissingFile, shelves); err != nil {
			env.Log.Warn("missing-films file not written", "event", "missing_file_error", "path", opt.MissingFile, "err", err)
		}
	}
	close(planned)
	wg.Wait()

	if lb.f.blocked {
		env.Log.Warn("letterboxd refused requests", "event", "letterboxd_blocked", "unresolved", unresolved)
	}
	lasting(ctx, env, rep, "blocked_runs", lb.f.blocked, "Letterboxd refuses requests; %d films are unresolved", unresolved)
	rep.Count("letterboxd_requests", lb.f.requests)
	rep.Count("lists", len(lists))
	summary := fmt.Sprintf("%d lists, %d films unresolved, %d Letterboxd requests in %s", len(lists), unresolved,
		lb.f.requests, env.Now().Sub(start).Round(time.Second))
	if opt.DryRun {
		rep.Summary = fmt.Sprintf("dry run: %s; %d shelves to retire, %d strays", summary, len(retire), len(px.strays)+len(jf.strays))
		return nil
	}
	for _, w := range []*writer{px.w, jf.w} {
		lasting(ctx, env, rep, w.app+"_deferred_runs", w.stopped != "", "%s shelf upkeep deferred: %s", w.app, w.stopped)
	}
	// one run heals itself (the section is listed again); a lasting one means Plex's listing and its collections disagree
	lasting(ctx, env, rep, "plex_stale_runs", px.notTaken+px.w.refused > 0,
		"Plex took %d shelf members under no key its section listing gives, and refused %d calls", px.notTaken, px.w.refused)
	rep.Count("plex_not_taken", px.notTaken)
	rep.Count("plex_refused", px.w.refused)
	rep.Count("plex_writes", px.w.writes)
	rep.Count("jellyfin_writes", jf.w.writes)
	rep.Count("shelves_retired", px.retired+jf.retired)
	rep.Count("name_taken", px.taken+jf.taken) // shelves held back: a collection the owner made since the last run has the title
	rep.Summary = fmt.Sprintf("%s; plex %d writes%s, jellyfin %d writes%s", summary, px.w.writes, deferred(px.w),
		jf.w.writes, deferred(jf.w))
	if px.retired+jf.retired > 0 {
		rep.Summary += fmt.Sprintf("; %d shelves retired", px.retired+jf.retired)
	}
	if px.taken+jf.taken > 0 {
		rep.Summary += fmt.Sprintf("; %d shelves held back: title taken by a hand-made collection", px.taken+jf.taken)
	}
	if px.notTaken+px.w.refused > 0 {
		rep.Summary += fmt.Sprintf("; plex took %d members under no key it holds (%d calls refused): the section is listed again next run",
			px.notTaken, px.w.refused)
	}
	return ctx.Err()
}

// sink is one app's side of the shelves.
type sink interface {
	// load reads what the app holds (the addressing map, the collections) and returns its report, which it also
	// keeps for the next run.
	load(ctx context.Context) appReport
	// reconcile deletes the stage's collections for the retired list keys, then brings the shelves into the app.
	reconcile(ctx context.Context, shelves []shelf, retire []string)
}

type lane struct {
	sink   sink
	app    string
	known  bool // a report from an earlier run was found
	report appReport
	loaded chan struct{}
}

// appReport is what one app contributes to the shelves.
type appReport struct {
	Watched []int    `json:"watched"` // the owner's watched films, by TMDB id
	Taken   []string `json:"taken"`   // folded titles of the collections the stage did not make
}

func (r appReport) watched() map[int]bool {
	out := make(map[int]bool, len(r.Watched))
	for _, t := range r.Watched {
		out[t] = true
	}
	return out
}

// shelfTitle is the list's name, unless a collection the stage did not make has that title in either app, ignoring
// case: Plex files a new collection under an existing tag that differs only in case (measured on 1.43: a second
// "zz projectionist probe" joined "zz Projectionist Probe" and took its title), and the collections stage matches
// titles the same way. Using one title in both apps keeps a later mirror of the owner's collection out of the shelf.
func shelfTitle(name string, taken map[string]bool) string {
	if taken[fold(name)] {
		return name + takenSuffix
	}
	return name
}

const takenSuffix = " · Letterboxd"

// stray tells whether a collection is a shelf the stage made for a canon list the config no longer names, though no
// record says so: the state database was lost, or another database's run made it (the NAS gate's Top 250). It
// carries the stage's summary, that summary links no list of the owner's (an own list merely unreadable now is never
// retired), and no configured canon list would adopt it by its title. The owner's own collections carry no such
// summary, and one whose summary he rewrote is his.
func stray(title, summary string, opt Options) bool {
	i := strings.LastIndex(summary, " · ")
	if !strings.HasPrefix(summary, summaryMarker) || i < 0 {
		return false
	}
	link := summary[i+len(" · "):]
	if _, whole, ok := strings.Cut(link, " of "); ok && strings.HasPrefix(link, "the first ") {
		link = whole
	}
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || strings.HasPrefix(u.Path, "/"+opt.User+"/") {
		return false
	}
	for _, src := range opt.Canon {
		if sameTitle(title, src.Name) || sameTitle(title, src.Name+takenSuffix) {
			return false
		}
	}
	return true
}

func fold(title string) string { return strings.ToLower(strings.Trim(title, "\u200e ")) }

func deferred(w *writer) string {
	if w.stopped == "" {
		return ""
	}
	return " (rest deferred: " + w.stopped + ")"
}

// lasting counts, in the store under key, the runs in a row on which bad held, and alerts from the third: one bad run
// heals by itself (a Cloudflare hiccup, a busy app), a lasting one needs the owner.
func lasting(ctx context.Context, env *stage.Env, rep *stage.Report, key string, bad bool, format string, args ...any) {
	var n int
	env.Store.GetJSON(ctx, name, key, &n)
	if bad {
		n++
	} else {
		n = 0
	}
	if err := env.Store.PutJSON(ctx, name, key, n); err != nil {
		env.Log.Warn("state write failed", "event", "store_error", "err", err)
	}
	if n >= 3 {
		rep.Alert(format+" (%d runs in a row)", append(args, n)...)
	}
}

// gather builds every list: the canon from config, then the owner's public lists (his lists page 1, the lists in his
// export, which name lists by code, and every own list shelved before). retire holds the keys of shelved lists that
// are positively gone: dropped from the canon config, own lists turned off, made private or deleted (404), or left
// out by skip_own. A list that is merely unreadable this run is never retired.
func gather(ctx context.Context, env *stage.Env, opt Options, lb *letterboxd, rep *stage.Report, listLIDs, made []string) (out []list, retire []string, err error) {
	canon := map[string]bool{}
	for i, src := range opt.Canon {
		u, err := url.Parse(src.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("canon %q: %w", src.Name, err)
		}
		_, entries, complete, err := lb.list(ctx, u.Path, opt.CanonRefresh, src.Limit)
		if err != nil && ctx.Err() != nil {
			return nil, nil, err
		}
		if err != nil {
			env.Log.Warn("canon list unavailable", "event", "list_unavailable", "list", src.Name, "err", err)
		}
		key := src.URL
		if src.Limit > 0 {
			key += "#" + strconv.Itoa(src.Limit)
		}
		canon[key] = true
		out = append(out, list{Key: key, Name: src.Name, URL: src.URL, Canon: true, Order: i, Limit: src.Limit, Poster: src.Poster,
			Entries: entries, Complete: complete && err == nil})
	}
	for _, key := range made {
		if own := strings.HasPrefix(key, "own:"); (!own && !canon[key]) || (own && !opt.OwnLists) {
			retire = append(retire, key)
		}
	}
	if !opt.OwnLists {
		return out, retire, nil
	}
	own, gone, err := ownLists(ctx, env, opt, lb, rep, listLIDs, made)
	return append(out, own...), append(retire, gone...), err
}

func ownLists(ctx context.Context, env *stage.Env, opt Options, lb *letterboxd, rep *stage.Report, listLIDs, made []string) (out []list, retire []string, err error) {
	var paths []string
	codes := append([]string(nil), listLIDs...)
	for _, key := range made { // shelved before: always looked at again, so a list gone private is retired
		if ref, ok := strings.CutPrefix(key, "own:"); ok && strings.HasPrefix(ref, "/") {
			paths = append(paths, ref)
		} else if ok {
			codes = append(codes, ref)
		}
	}
	index, ok, err := lb.cachedPage(ctx, "/"+opt.User+"/lists/", opt.OwnRefresh)
	if err != nil && ctx.Err() != nil {
		return nil, nil, err
	}
	if ok {
		paths = append(paths, index.Lists...)
	}
	via := map[string][]string{} // path → the list codes that lead there
	for _, lid := range codes {
		p, err := lb.listPath(ctx, lid)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			env.Log.Warn("list code unresolved", "event", "list_unavailable", "lid", lid, "err", err)
			continue
		}
		if p != "" {
			paths = append(paths, p)
			via[p] = append(via[p], lid)
		}
	}
	keysOf := func(path string) []string { // every key a list at path may have been shelved under
		keys := []string{"own:" + path}
		for _, lid := range via[path] {
			keys = append(keys, "own:"+lid)
		}
		return keys
	}
	sort.Strings(paths)
	private, keys := 0, map[string]bool{}
	for i, path := range paths {
		if i > 0 && path == paths[i-1] {
			continue
		}
		slug := strings.TrimSuffix(path[strings.LastIndex(strings.TrimSuffix(path, "/"), "/")+1:], "/")
		first, entries, complete, err := lb.list(ctx, path, opt.OwnRefresh, 0)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, nil, err
		case errors.Is(err, errNotFound): // private or deleted; it leaves the shared library
			private++
			retire = append(retire, keysOf(path)...)
			env.Log.Info("own list not public", "event", "own_list_skipped", "path", path)
			continue
		case err != nil: // unreadable this run (the state database): the shelf stays as it is
			env.Log.Warn("own list unreadable", "event", "list_unavailable", "path", path, "err", err)
			continue
		case first.Title == "" && !complete:
			continue // never read and unreadable now
		}
		key := "own:" + path
		if first.LID != "" {
			key = "own:" + first.LID
			// its code → path, so the list is still found once it drops off his lists page 1
			if _, err := lb.db.ExecContext(ctx, `INSERT OR IGNORE INTO lists_links(lid, path, checked_at) VALUES (?, ?, ?)`,
				first.LID, path, lb.now().UTC().Format(store.TimeFormat)); err != nil {
				env.Log.Warn("state write failed", "event", "store_error", "err", err)
			}
		}
		if skipped(opt.SkipOwn, first.Title) || skipped(opt.SkipOwn, slug) {
			retire = append(retire, append(keysOf(path), key)...)
			continue
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
	return out, retire, nil
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

// skipped tells whether name matches one of the skip_own patterns, ignoring case; "*" matches any text.
func skipped(patterns []string, name string) bool {
	for _, p := range patterns {
		expr := strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSpace(p)), `\*`, ".*")
		if regexp.MustCompile(`(?i)^` + expr + `$`).MatchString(strings.TrimSpace(name)) {
			return true
		}
	}
	return false
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
	Title, SortTitle, Summary    string // Title: the collection's, the list's name unless that is taken
	Members                      []int  // owned movie TMDB ids, list order, no repeats
	Art                          *art
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
	link := l.URL
	if l.Limit > 0 { // a limited shelf has no list of its own on Letterboxd
		link = fmt.Sprintf("the first %d of %s", l.Limit, l.URL)
	}
	s.Summary = fmt.Sprintf("On the NAS: %d of %d · seen %d · %s", s.Owned, s.Total, s.Seen, link)
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
