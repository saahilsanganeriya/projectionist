// Package letterboxd records the owner's Letterboxd diary in projectionist's watch record, and keeps his watchlist
// as a "Watchlist" collection in Plex (promoted to his Home) and Jellyfin.
//
//	feed (every run) ───┐                              ┌─► watch_events ──► jfsync ──► Plex, Jellyfin watch state
//	export (on the NAS) ┼─► letterboxd_* ─► TMDB join ─┤   (logged viewings, ratings)
//	grid pages (daily) ─┘                              └─► Watchlist collections, _watchlist-missing.txt
//
// It writes no watch state itself: jfsync is the one writer to both apps, so a "mark unplayed" newer than his diary
// entry stands. Each diary viewing is one Logged event (at the time he logged it, on its watched date, with at least
// as many plays as viewings of the film logged by then) and each rating one Rated event (at the time it was set), each
// recorded once: a viewing by its id, which the feed guid and the export's boxd.it code both carry, a rating by its
// time. With history false (the default) only the entries he logs after this stage first runs become events; history
// true imports the export and the feed's backlog, which Plex then shows his friends (his watch history is visible to
// friends of friends, his ratings to friends). An entry he deletes is dropped, and he is told when its play is
// already in the record (marking the film unplayed undoes it).
//
// Inputs, all public and fetched politely (one request per 2 s for the whole process, shared with every stage through
// lbpace; every answer that can be cached is): the RSS feed (every run), the watchlist and watched-films grid pages
// (daily), the film pages (TMDB id, cached forever), and the account export unzipped on the NAS. An answer the stage
// cannot read as Letterboxd's is never cached, so new markup cannot shrink the watchlist or mark a film unresolvable:
// a watchlist page not stating its size, or a crawl whose films do not add up to it, is a challenge and backs the
// crawl off; a film page naming no TMDB type, or a boxd.it answer that is not a redirect to a film page, holds up that
// one film for a day (a second in a run backs the kind off). A kind failing for 3 days, or a film unreadable for 3
// days, is an alert each run. Films join the library on the TMDB id in the folder names.
//
// For the collections the stage keeps a snapshot of what each app holds (one paged listing, re-read daily and before a
// run that changes a collection, at most once a day per film) and updates it after each write, so a rerun writes
// nothing. A film watched on Letterboxd or in the app leaves them; only films this stage added are ever taken out. A
// film whose collection writes fail in 3 runs in a row is parked, and the owner told once. Reviews are not synced: a
// Plex review is a community post whose privacy the poster cannot set (createReview takes none; other members'
// reviews carry ANYONE or PLEX and are listed to non-friends), and Jellyfin has no per-user text field
// (UpdateUserItemDataDto).
package letterboxd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"projectionist/internal/lbpace"
	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/watch"
)

const name = "letterboxd"

type Options struct {
	User          string        `toml:"user"`
	LetterboxdURL string        `toml:"letterboxd_url"`
	BoxdURL       string        `toml:"boxd_url"`
	UserAgent     string        `toml:"user_agent"`
	MaxFetches    int           `toml:"max_fetches"` // Letterboxd requests per run; the backfill resumes next run
	PagesEvery    time.Duration `toml:"pages_every"` // watchlist + watched grid crawl
	ExportDir     string        `toml:"export_dir"`  // holds letterboxd-<user>-<date>-utc/ folders; newest wins
	MoviesDir     string        `toml:"movies_dir"`
	MissingFile   string        `toml:"missing_file"`     // watchlist films not on the NAS; "" = none
	SinkRefresh   time.Duration `toml:"sink_refresh"`     // re-read what Plex and plex.tv hold this often (Plex also before writing)
	JFRefresh     time.Duration `toml:"jellyfin_refresh"` // re-read what Jellyfin holds this often (also before writing)

	PlexSection   int    `toml:"plex_section"`
	JellyfinUser  string `toml:"jellyfin_user"`
	Person        string `toml:"person"`         // the owner in the watch record: his name in jfsync's users (jfsync alerts otherwise)
	History       bool   `toml:"history"`        // also record the export and the feed's backlog, not just entries logged from now on
	Collection    string `toml:"collection"`     // "" = no Watchlist collection
	PlexWatchlist bool   `toml:"plex_watchlist"` // mirror onto the owner's plex.tv watchlist (visible per his Plex privacy)
	DiscoverURL   string `toml:"discover_url"`

	RunBudget time.Duration `toml:"run_budget"` // no app call starts later than this into a run; keep it under "every"
	DryRun    bool          `toml:"dry_run"`    // count events and writes, record and write nothing (nor the missing file)
}

func defaults() Options {
	return Options{
		User:          "saahilsan",
		LetterboxdURL: "https://letterboxd.com",
		BoxdURL:       "https://boxd.it",
		UserAgent:     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 (projectionist; personal sync of one member's own public diary)",
		MaxFetches:    400,
		PagesEvery:    24 * time.Hour,
		ExportDir:     "/volume1/docker/projectionist/data/letterboxd",
		MoviesDir:     "/volume1/media/Movies",
		MissingFile:   "/volume1/media/_watchlist-missing.txt",
		SinkRefresh:   24 * time.Hour,
		JFRefresh:     24 * time.Hour,
		PlexSection:   4,
		JellyfinUser:  "saahil",
		Person:        "saahil",
		Collection:    "Watchlist",
		DiscoverURL:   "https://discover.provider.plex.tv",
		RunBudget:     25 * time.Minute,
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS letterboxd_lid (lid TEXT PRIMARY KEY, slug TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS letterboxd_film (slug TEXT PRIMARY KEY, tmdb INTEGER NOT NULL, kind TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS letterboxd_entry (id TEXT PRIMARY KEY, slug TEXT NOT NULL, tmdb INTEGER NOT NULL,
  title TEXT NOT NULL, year INTEGER NOT NULL, watched TEXT NOT NULL, rating INTEGER NOT NULL, logged_at TEXT NOT NULL,
  rated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS letterboxd_seen (slug TEXT PRIMARY KEY, rating INTEGER NOT NULL, seen_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS letterboxd_sink (app TEXT NOT NULL, tmdb INTEGER NOT NULL, item TEXT NOT NULL,
  plays INTEGER NOT NULL, played INTEGER NOT NULL, rating INTEGER NOT NULL, last TEXT NOT NULL, member INTEGER NOT NULL,
  added INTEGER NOT NULL, PRIMARY KEY (app, tmdb));
CREATE TABLE IF NOT EXISTS letterboxd_retry (app TEXT NOT NULL, tmdb INTEGER NOT NULL, item TEXT NOT NULL,
  reread_at TEXT NOT NULL, tries INTEGER NOT NULL, PRIMARY KEY (app, tmdb));
`

// foldFeedIDs re-keys feed rows stored under the whole guid onto the viewing id. Where one viewing was stored
// twice (as watch, then as review), the most recently logged row stays.
const foldFeedIDs = `
INSERT INTO letterboxd_entry(id, slug, tmdb, title, year, watched, rating, logged_at, rated_at)
  SELECT substr(id, length(rtrim(id, '0123456789')) + 1), slug, tmdb, title, year, watched, rating, logged_at, rated_at
  FROM letterboxd_entry WHERE id GLOB 'letterboxd-*'
  ON CONFLICT(id) DO UPDATE SET slug=excluded.slug, tmdb=excluded.tmdb, title=excluded.title, year=excluded.year,
    watched=excluded.watched, rating=excluded.rating, logged_at=excluded.logged_at, rated_at=excluded.rated_at
  WHERE excluded.logged_at >= letterboxd_entry.logged_at;
DELETE FROM letterboxd_entry WHERE id GLOB 'letterboxd-*';
`

// addedColumns are the columns added to a table after state DBs held it: CREATE TABLE IF NOT EXISTS leaves an
// older table as it is, so each is added, then filled, when missing. A Plex row's watched mark is a view; any other
// app's row is corrected by its next read, which any write planned from the row forces first.
var addedColumns = []struct{ table, column, def, fill string }{
	{"letterboxd_sink", "played", "INTEGER NOT NULL DEFAULT 0", "UPDATE letterboxd_sink SET played = (app = 'plex' AND plays > 0)"},
	{"letterboxd_entry", "rated_at", "TEXT NOT NULL DEFAULT ''", "UPDATE letterboxd_entry SET rated_at = logged_at"},
}

func migrate(ctx context.Context, db *sql.DB) error {
	for _, c := range addedColumns {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil) // added and filled together: a column left unfilled would never be filled
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+c.table+" ADD COLUMN "+c.column+" "+c.def+"; "+c.fill); err != nil {
			tx.Rollback()
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Stage is registered as letterboxd.Stage{}; tests set sleep and pace so nothing waits on the wall clock.
type Stage struct {
	sleep func(context.Context, time.Duration) error
	pace  *lbpace.Limiter // nil = lbpace.Shared
}

func (Stage) Name() string { return name }

type run struct {
	env   *stage.Env
	opt   Options
	db    *sql.DB
	fetch *fetcher
	until time.Time // run start + run_budget
	sleep func(context.Context, time.Duration) error
	mu    sync.Mutex
	rep   *stage.Report
	fails map[string]map[int]bool // app → films with a write that failed or went unconfirmed this run
}

func (r *run) count(kind string, n int) {
	if n == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count(kind, n)
}

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := migrate(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, foldFeedIDs); err != nil {
		return err
	}
	if err := watch.Open(ctx, db); err != nil {
		return err
	}
	// From when entries become events with history false: this stage's first run (its deploy).
	var since time.Time
	if found, err := env.Store.GetJSON(ctx, name, "events_since", &since); err != nil {
		return err
	} else if !found {
		since = env.Now()
		if err := env.Store.PutJSON(ctx, name, "events_since", since); err != nil {
			return err
		}
	}
	sleep, pace := s.sleep, s.pace
	if sleep == nil {
		sleep = realSleep
	}
	if pace == nil {
		pace = lbpace.Shared
	}
	r := &run{env: env, opt: opt, db: db, until: env.Now().Add(opt.RunBudget), sleep: sleep, rep: rep, fails: map[string]map[int]bool{}, fetch: &fetcher{
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		ua: opt.UserAgent, pace: pace, budget: opt.MaxFetches,
	}}

	if err := r.ingest(ctx); err != nil {
		return err
	}
	var feed feedCache
	if _, err := env.Store.GetJSON(ctx, name, "rss", &feed); err != nil {
		return err
	}
	if env.Now().Sub(feed.ReadAt) > feedStale {
		rep.Alert("Letterboxd feed unread since %s: new diary entries are not reaching Plex or Jellyfin", feed.ReadAt.Format(time.RFC3339))
	}
	f, err := r.loadFacts(ctx)
	if err != nil {
		return err
	}
	learned, err := r.resolve(ctx, f)
	if err != nil {
		return err
	}
	if learned {
		if f, err = r.loadFacts(ctx); err != nil {
			return err
		}
	}
	wants, missing, unresolved, other := f.wants()
	r.count("letterboxd_fetches", r.fetch.fetches)
	r.count("unresolved", unresolved)
	r.count("not_a_movie", other)
	r.count("missing_films", len(missing))
	if err := r.writeMissing(missing); err != nil {
		env.Log.Warn("missing-films file not written", "event", "missing_file_error", "err", err)
	}
	if err := r.record(ctx, f.events(opt.Person, env.Loc, opt.History, since)); err != nil {
		return err
	}

	var wg sync.WaitGroup
	var storeFailure error
	for _, syncApp := range []func(context.Context, []want) error{r.syncPlex, r.syncJellyfin, r.syncPlexTV} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := syncApp(ctx, wants)
			switch {
			case err == nil || ctx.Err() != nil:
			case isStoreErr(err):
				r.mu.Lock()
				storeFailure = err
				r.mu.Unlock()
			default:
				// An app being slow or down never fails the run: its writes stay in the diff for the next run.
				env.Log.Warn("sink skipped this run", "event", "sink_error", "err", err)
				r.count("sink_errors", 1)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if storeFailure != nil {
		return storeFailure
	}
	if err := r.recordFails(ctx, wants); err != nil {
		return err
	}
	inLib, watched := 0, 0
	for _, w := range wants {
		if w.InLibrary {
			inLib++
			if w.Watched {
				watched++
			}
		}
	}
	mode := "applied"
	if opt.DryRun {
		mode = "dry run"
	}
	rep.Summary = fmt.Sprintf("%s: %d new diary events, %d Letterboxd films in the library (%d watched), %d unresolved, %d watchlist films not on the NAS",
		mode, rep.Counts["events.logged"]+rep.Counts["events.rated"], inLib, watched, unresolved, len(missing))
	return nil
}

// record appends to the watch record the events it lacks: a Logged event once per viewing (a date he corrects later
// is not sent again), a Rated event once per rating and time. A dry run counts them.
func (r *run) record(ctx context.Context, events []watch.Event) error {
	key := func(kind watch.Kind, ref string, at time.Time, rating *float64) string {
		if kind == watch.Logged {
			return "logged " + ref
		}
		return fmt.Sprintf("%s %s %s %v", kind, ref, at.UTC().Format(store.TimeFormat), *rating)
	}
	recorded := map[string]bool{}
	err := query(ctx, r.db, `SELECT kind, ref, at, rating FROM watch_events WHERE source = ? AND person = ?`, func(rows *sql.Rows) error {
		var kind watch.Kind
		var ref, at string
		var rating *float64
		if err := rows.Scan(&kind, &ref, &at, &rating); err != nil {
			return err
		}
		t, err := time.Parse(store.TimeFormat, at)
		if kind == watch.Logged || rating != nil {
			recorded[key(kind, ref, t, rating)] = true
		}
		return err
	}, name, r.opt.Person)
	if err != nil {
		return storeErr{err}
	}
	var fresh []watch.Event
	for _, e := range events {
		if k := key(e.Kind, e.Ref, e.At, e.Rating); !recorded[k] {
			recorded[k] = true
			fresh = append(fresh, e)
		}
	}
	for _, e := range fresh {
		r.count("events."+string(e.Kind), 1)
		r.env.Log.Info("diary event", "event", "watch_event_planned", "kind", e.Kind, "item", e.Item, "ref", e.Ref,
			"at", e.At.Format(time.RFC3339), "plays", e.Plays, "dry_run", r.opt.DryRun)
	}
	if r.opt.DryRun || len(fresh) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return storeErr{err}
	}
	defer tx.Rollback()
	for _, e := range fresh {
		if err := watch.Add(ctx, tx, e); err != nil {
			return storeErr{err}
		}
	}
	if err := tx.Commit(); err != nil {
		return storeErr{err}
	}
	return nil
}

// ---- ingest: feed, grid crawls ----

type backoff struct {
	Until time.Time `json:"until"`
	Step  int       `json:"step"`
	Since time.Time `json:"since,omitzero"` // first failure, challenged or not, since the kind last read well
	Err   string    `json:"err,omitempty"`  // the latest failure
}

// failing names each kind's pages and what the owner loses while they keep failing. A refusal only backs off, so
// that new markup can never shrink anything, which leaves a lasting one silent: past failingAlert he is told, each
// run until they read well again. The feed has its own, earlier alarm (feedStale).
var failing = map[string]struct{ pages, news string }{
	"watchlist": {"watchlist pages", "both Watchlist collections and the missing-films file keep his watchlist as last read in full"},
	"films":     {"watched-films pages", "films he marks watched or rates outside the diary are not picked up"},
	"film":      {"film pages", "films new to the stage are not matched to TMDB, so nothing is written for them"},
}

const failingAlert = 72 * time.Hour

// guarded runs one Letterboxd fetch job of a kind, honouring and updating that kind's challenge backoff
// (1 h doubling to 24 h). Budget exhaustion, challenges and failures end the job quietly; it resumes next run.
func (r *run) guarded(ctx context.Context, kind string, job func() error) error {
	var b backoff
	if _, err := r.env.Store.GetJSON(ctx, name, "backoff."+kind, &b); err != nil {
		return err
	}
	defer func() {
		if f, ok := failing[kind]; ok && !b.Since.IsZero() && r.env.Now().Sub(b.Since) >= failingAlert {
			r.rep.Alert("Letterboxd %s failing since %s (latest: %s): %s", f.pages, b.Since.Format(time.RFC3339), b.Err, f.news)
		}
	}()
	if r.env.Now().Before(b.Until) {
		r.count("backed_off."+kind, 1)
		return nil
	}
	err := job()
	switch {
	case errors.Is(err, errChallenged):
		b.Step = min(b.Step+1, 6) // 1, 2, 4 ... 32 h, capped at 24 h below; the cap also keeps the shift from overflowing
		b.Until = r.env.Now().Add(min(time.Hour<<(b.Step-1), 24*time.Hour))
		r.env.Log.Warn("letterboxd challenged; backing off", "event", "letterboxd_challenged", "kind", kind,
			"until", b.Until.Format(time.RFC3339), "err", err)
		r.count("challenged."+kind, 1)
	case errors.Is(err, errBudget):
		r.count("budget_spent", 1)
		return nil
	case err != nil && ctx.Err() == nil && !isStoreErr(err):
		r.env.Log.Warn("letterboxd fetch failed", "event", "letterboxd_error", "kind", kind, "err", err)
		r.count("errors."+kind, 1)
	case err != nil:
		return err
	case b.Step == 0 && b.Since.IsZero():
		return nil
	default:
		b = backoff{}
		return r.env.Store.PutJSON(ctx, name, "backoff."+kind, b)
	}
	if b.Since.IsZero() {
		b.Since = r.env.Now()
	}
	b.Err = err.Error()
	return r.env.Store.PutJSON(ctx, name, "backoff."+kind, b)
}

// storeErr marks a local SQLite failure, which (unlike any network trouble) fails the run.
type storeErr struct{ error }

func (e storeErr) Unwrap() error { return e.error }

func isStoreErr(err error) bool {
	var se storeErr
	return errors.As(err, &se)
}

func (r *run) ingest(ctx context.Context) error {
	if err := r.guarded(ctx, "rss", func() error { return r.readFeed(ctx) }); err != nil {
		return err
	}
	if err := r.guarded(ctx, "watchlist", func() error { return r.crawl(ctx, "watchlist") }); err != nil {
		return err
	}
	return r.guarded(ctx, "films", func() error { return r.crawl(ctx, "films") })
}

type feedCache struct {
	ETag         string    `json:"etag"`
	LastModified string    `json:"last_modified"`
	ReadAt       time.Time `json:"read_at"` // last successful read (200 or 304)
}

// feedStale is how long the feed may go unread (blocked, down) before the owner hears: past it, new diary
// entries stop reaching Plex and Jellyfin.
const feedStale = 48 * time.Hour

func (r *run) readFeed(ctx context.Context) error {
	var c feedCache
	if _, err := r.env.Store.GetJSON(ctx, name, "rss", &c); err != nil {
		return storeErr{err}
	}
	hdr := map[string]string{"Accept": "application/rss+xml"}
	if c.ETag != "" {
		hdr["If-None-Match"] = c.ETag
	}
	if c.LastModified != "" {
		hdr["If-Modified-Since"] = c.LastModified
	}
	p, err := r.fetch.get(ctx, fmt.Sprintf("%s/%s/rss/", r.opt.LetterboxdURL, r.opt.User), hdr)
	if err != nil {
		return err
	}
	c.ReadAt = r.env.Now()
	if p.status == http.StatusNotModified {
		r.count("rss_not_modified", 1)
		return r.env.Store.PutJSON(ctx, name, "rss", c)
	}
	entries, err := parseRSS(p.body)
	if err != nil {
		return err
	}
	// A rating's time orders it against his other ratings of the film in the watch record (see events): an entry's
	// is when it was logged, until a read finds its rating changed; then it is that read's, a new Rated event.
	now := r.env.Now().Format(store.TimeFormat)
	for _, e := range entries {
		if e.Slug == "" {
			continue
		}
		res, err := r.db.ExecContext(ctx, `INSERT INTO letterboxd_entry(id, slug, tmdb, title, year, watched, rating, logged_at, rated_at)
			VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?8) ON CONFLICT(id) DO UPDATE SET watched=excluded.watched, rating=excluded.rating,
			rated_at=CASE WHEN rating != excluded.rating THEN ?9 ELSE rated_at END
			WHERE watched != excluded.watched OR rating != excluded.rating`,
			e.ID, e.Slug, e.TMDB, e.Title, e.Year, e.Watched, e.Rating, e.LoggedAt.Format(store.TimeFormat), now)
		if err != nil {
			return storeErr{err}
		}
		if n, _ := res.RowsAffected(); n > 0 {
			r.count("rss_entries", 1)
		}
		if e.TMDB > 0 { // the feed names the TMDB movie, so this film needs no page fetch
			if _, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO letterboxd_film(slug, tmdb, kind) VALUES (?, ?, 'movie')`, e.Slug, e.TMDB); err != nil {
				return storeErr{err}
			}
		}
	}
	if err := r.retract(ctx, entries); err != nil {
		return err
	}
	return r.env.Store.PutJSON(ctx, name, "rss", feedCache{ETag: p.header.Get("ETag"), LastModified: p.header.Get("Last-Modified"), ReadAt: c.ReadAt})
}

// retract drops the stored diary entries he has deleted on Letterboxd. The feed holds his latest 50 entries in the
// order he logged them (then his lists), so a stored entry logged after the oldest of them and missing from it was
// deleted; an older one has only fallen off the end. The record is append-only, so the play of one already recorded
// stays there: he is told, and marking the film unplayed in Plex or Jellyfin undoes it.
func (r *run) retract(ctx context.Context, entries []rssEntry) error {
	if len(entries) == 0 {
		return nil
	}
	inFeed, oldest := map[string]bool{}, entries[0].LoggedAt
	for _, e := range entries {
		inFeed[e.ID] = true
		if e.LoggedAt.Before(oldest) {
			oldest = e.LoggedAt
		}
	}
	var gone []rssEntry
	err := query(ctx, r.db, `SELECT id, title, watched FROM letterboxd_entry WHERE logged_at > ?`, func(rows *sql.Rows) error {
		var e rssEntry
		err := rows.Scan(&e.ID, &e.Title, &e.Watched)
		if !inFeed[e.ID] {
			gone = append(gone, e)
		}
		return err
	}, oldest.Format(store.TimeFormat))
	if err != nil {
		return storeErr{err}
	}
	var names []string
	for _, e := range gone {
		if _, err := r.db.ExecContext(ctx, `DELETE FROM letterboxd_entry WHERE id=?`, e.ID); err != nil {
			return storeErr{err}
		}
		var recorded int
		if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM watch_events WHERE source = ? AND ref = ? AND kind = ?`,
			name, name+":"+e.ID, watch.Logged).Scan(&recorded); err != nil {
			return storeErr{err}
		}
		r.env.Log.Info("diary entry deleted on Letterboxd", "event", "rss_retracted", "id", e.ID, "item", e.Title, "watched", e.Watched,
			"recorded", recorded > 0)
		r.count("rss_retracted", 1)
		if recorded > 0 {
			names = append(names, fmt.Sprintf("%s (watched %s)", e.Title, e.Watched))
		}
	}
	if len(names) > 0 {
		r.rep.Alert("Letterboxd diary entries deleted: %s. Their plays stay in the watch record, so Plex and Jellyfin keep them: mark the film unplayed in either app to undo one",
			strings.Join(names, ", "))
	}
	return nil
}

type crawlState struct {
	Next    int        `json:"next"` // next page; 0 = idle
	Started time.Time  `json:"started"`
	Done    time.Time  `json:"done"`
	Stated  int        `json:"stated,omitempty"` // watchlist only: the size page 1 stated
	Films   []gridFilm `json:"films,omitempty"`  // watchlist only: pages read so far
}

// crawl reads /<user>/<kind>/ page by page, at most once per PagesEvery, resuming where an earlier run stopped
// that day (a crawl older than PagesEvery starts over, so page 1, the newest films, is read daily).
//
// A complete "watchlist" crawl replaces the watchlist snapshot, which decides both Watchlist collections, so every
// page must state the size page 1 stated (data-num-entries), and the crawl completes only when its films add up to
// it. A page that states none (an interstitial answered with 200, a redirect's body, new markup), another size (he
// edited the list mid-crawl, shifting films across pages) or a last page that leaves the count short (a pagination
// link it cannot read) is backed off as a challenge, and the crawl starts over at page 1. "films" pages only add (watched films, current ratings), so a page listing no films is the
// one refusal there; and since Cloudflare refuses every anonymous /films/page/N/ past 1 (403 since 2026-10-05, as
// for lists), the films crawl ends that day at the first refused page past 1, logged once, instead of backing off.
func (r *run) crawl(ctx context.Context, kind string) error {
	key := "crawl." + kind
	var c crawlState
	if _, err := r.env.Store.GetJSON(ctx, name, key, &c); err != nil {
		return storeErr{err}
	}
	now := r.env.Now()
	if c.Next == 0 || now.Sub(c.Started) >= r.opt.PagesEvery {
		if c.Next == 0 && now.Sub(c.Done) < r.opt.PagesEvery {
			return nil
		}
		c = crawlState{Next: 1, Started: now, Done: c.Done}
	}
	for {
		url := fmt.Sprintf("%s/%s/%s/", r.opt.LetterboxdURL, r.opt.User, kind)
		if c.Next > 1 { // page 1 without /page/1/: that is the URL Letterboxd links, and its edge cache answers it
			url += fmt.Sprintf("page/%d/", c.Next)
		}
		p, err := r.fetch.get(ctx, url, map[string]string{"Accept": "text/html"})
		films, hasNext := parseGrid(p.body)
		stated, states := statedCount(p.body)
		if c.Next == 1 {
			c.Stated = stated
		}
		switch {
		case err != nil:
		case kind == "films" && len(films) == 0:
			err = fmt.Errorf("%w: GET %s: HTTP %d lists no films", errChallenged, url, p.status)
		case kind == "watchlist" && !states:
			err = fmt.Errorf("%w: GET %s: HTTP %d does not state data-num-entries", errChallenged, url, p.status)
		case kind == "watchlist" && (stated != c.Stated || !hasNext && distinctFilms(c.Films, films) != stated):
			err = fmt.Errorf("%w: GET %s: states %d films, page 1 stated %d, the pages so far list %d", errChallenged, url,
				stated, c.Stated, distinctFilms(c.Films, films))
			c = crawlState{Next: 1, Started: c.Started, Done: c.Done}
		}
		if kind == "films" && c.Next > 1 && errors.Is(err, errChallenged) {
			r.env.Log.Warn("letterboxd refuses the deeper films pages; this crawl ends here until tomorrow", "event", "letterboxd_films_partial",
				"pages", c.Next-1, "err", err)
			r.count("films_partial", 1)
			if err := r.env.Store.PutJSON(ctx, name, key, crawlState{Done: now}); err != nil {
				return storeErr{err}
			}
			return nil
		}
		if err != nil {
			if perr := r.env.Store.PutJSON(ctx, name, key, c); perr != nil {
				return storeErr{perr}
			}
			return err
		}
		if err := r.recordGrid(ctx, kind, films); err != nil {
			return err
		}
		if kind == "watchlist" {
			c.Films = append(c.Films, films...)
		}
		r.count("pages."+kind, 1)
		c.Next++
		if !hasNext {
			if kind == "watchlist" {
				if err := r.env.Store.PutJSON(ctx, name, "watchlist", watchlistSnapshot{At: c.Started, Films: c.Films}); err != nil {
					return storeErr{err}
				}
			}
			c = crawlState{Done: r.env.Now()}
		}
		if err := r.env.Store.PutJSON(ctx, name, key, c); err != nil {
			return storeErr{err}
		}
		if c.Next == 0 {
			return nil
		}
	}
}

// distinctFilms counts the films in a crawl's pages (one shifted across a page boundary is listed twice).
func distinctFilms(pages ...[]gridFilm) int {
	slugs := map[string]bool{}
	for _, films := range pages {
		for _, g := range films {
			slugs[g.Slug] = true
		}
	}
	return len(slugs)
}

func (r *run) recordGrid(ctx context.Context, kind string, films []gridFilm) error {
	for _, g := range films {
		if g.LID != "" {
			if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_lid(lid, slug) VALUES (?, ?)`, g.LID, g.Slug); err != nil {
				return storeErr{err}
			}
		}
		if kind == "films" {
			if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_seen(slug, rating, seen_at) VALUES (?, ?, ?)`,
				g.Slug, g.Rating, r.env.Now().Format(store.TimeFormat)); err != nil {
				return storeErr{err}
			}
		}
	}
	return nil
}

// ---- resolve: boxd.it codes → slugs, slugs → TMDB (each answer cached forever) ----

// unreadable is a page that answered without the markup the stage reads; it is asked again a day after Last.
type unreadable struct {
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// resolve looks up what the TMDB join lacks; learned reports whether any answer was stored. One answer it cannot
// read (an interstitial with 200, a redirect to a sign-in) holds up only its own film, for a day, never as a cached
// negative; a second in a run is most likely new markup on every page, so the whole kind backs off.
func (r *run) resolve(ctx context.Context, f facts) (learned bool, err error) {
	refs := f.resolveNeeds()
	if len(refs) == 0 {
		return false, nil
	}
	bad := map[string]unreadable{} // "lid:<code>" or "slug:<slug>" →
	if _, err := r.env.Store.GetJSON(ctx, name, "unreadable", &bad); err != nil {
		return false, err
	}
	for key := range bad { // a film no longer needed (taken off the watchlist) is forgotten
		if !slices.Contains(refs, key) {
			delete(bad, key)
		}
	}
	now := r.env.Now()
	waiting := func(key string) bool { u, ok := bad[key]; return ok && now.Sub(u.Last) < rereadEvery }
	strikes := 0
	refused := func(key string, err error) error {
		u := bad[key]
		if u.First.IsZero() {
			u.First = now
		}
		u.Last = now
		bad[key] = u
		r.count("unreadable", 1)
		r.env.Log.Warn("letterboxd answer unreadable; this film is asked again tomorrow", "event", "letterboxd_unreadable", "ref", key, "err", err)
		if strikes++; strikes == 2 {
			return fmt.Errorf("%w: a second unreadable answer this run: %w", errChallenged, err)
		}
		return nil
	}
	err = r.guarded(ctx, "film", func() error {
		done := map[string]bool{}
		for _, ref := range refs {
			if waiting(ref) {
				continue
			}
			slug, isSlug := strings.CutPrefix(ref, "slug:")
			if !isSlug {
				lid := strings.TrimPrefix(ref, "lid:")
				p, err := r.fetch.get(ctx, r.opt.BoxdURL+"/"+lid, nil)
				if err != nil && p.status != http.StatusNotFound {
					return err
				}
				// A code with no film behind it answers 404, cached as such; a redirect anywhere but a film page is no answer.
				if slug = slugOf(p.header.Get("Location")); slug == "" && p.status != http.StatusNotFound {
					if err := refused(ref, fmt.Errorf("boxd.it/%s: HTTP %d to %q, not a film page", lid, p.status, p.header.Get("Location"))); err != nil {
						return err
					}
					continue
				}
				if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_lid(lid, slug) VALUES (?, ?)`, lid, slug); err != nil {
					return storeErr{err}
				}
				delete(bad, ref)
				learned = true
				r.count("resolved_lids", 1)
			}
			if _, known := f.films[slug]; slug == "" || known || done[slug] || waiting("slug:"+slug) {
				continue
			}
			done[slug] = true
			tmdb, kind, err := r.filmPage(ctx, slug)
			if errors.Is(err, errUnreadable) {
				if err := refused("slug:"+slug, err); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_film(slug, tmdb, kind) VALUES (?, ?, ?)`, slug, tmdb, kind); err != nil {
				return storeErr{err}
			}
			delete(bad, "slug:"+slug)
			learned = true
			r.count("resolved_films", 1)
		}
		return nil
	})
	if perr := r.env.Store.PutJSON(ctx, name, "unreadable", bad); err == nil {
		err = perr
	}
	var stuck []string
	for _, key := range sortedKeys(bad) {
		if now.Sub(bad[key].First) >= failingAlert {
			stuck = append(stuck, key)
		}
	}
	if len(stuck) > 0 {
		r.rep.Alert("Letterboxd pages unreadable for %s or more, so these films are not matched to TMDB: %s", failingAlert, strings.Join(stuck, ", "))
	}
	return learned, err
}

// filmPage reads one film page, following one slug rename (Letterboxd 301s an old slug to the new one). A 404, a
// page naming its TMDB type and a second rename are answers; a page naming no type is unreadable, never cached.
func (r *run) filmPage(ctx context.Context, slug string) (int, string, error) {
	url := fmt.Sprintf("%s/film/%s/", r.opt.LetterboxdURL, slug)
	for hop := 0; hop < 2; hop++ {
		p, err := r.fetch.get(ctx, url, map[string]string{"Accept": "text/html"})
		if p.status == http.StatusNotFound {
			return 0, "", nil
		}
		if err != nil {
			return 0, "", err
		}
		if next := slugOf(p.header.Get("Location")); p.status/100 == 3 && next != "" {
			url = fmt.Sprintf("%s/film/%s/", r.opt.LetterboxdURL, next)
			continue
		}
		if tmdb, kind := parseFilmPage(p.body); kind != "" {
			return tmdb, kind, nil
		}
		return 0, "", fmt.Errorf("%w: GET %s: HTTP %d names no data-tmdb-type", errUnreadable, url, p.status)
	}
	return 0, "", nil
}

// ---- facts from the store, the export and the library ----

func (r *run) loadFacts(ctx context.Context) (facts, error) {
	f := facts{seen: map[string]bool{}, lidSlug: map[string]string{}, films: map[string]filmFacts{}}
	ex, ok, err := readExport(r.opt.ExportDir)
	if err != nil {
		return f, err
	}
	if ok {
		f.ex = &ex
	}
	var wl watchlistSnapshot
	found, err := r.env.Store.GetJSON(ctx, name, "watchlist", &wl)
	if err != nil {
		return f, err
	}
	if found {
		f.watchlist = &wl
	}
	if f.library, err = readLibrary(r.opt.MoviesDir); err != nil {
		return f, err
	}
	err = query(ctx, r.db, `SELECT lid, slug FROM letterboxd_lid`, func(rows *sql.Rows) error {
		var lid, slug string
		err := rows.Scan(&lid, &slug)
		f.lidSlug[lid] = slug
		return err
	})
	if err == nil {
		err = query(ctx, r.db, `SELECT slug, tmdb, kind FROM letterboxd_film`, func(rows *sql.Rows) error {
			var slug string
			var ff filmFacts
			err := rows.Scan(&slug, &ff.TMDB, &ff.Kind)
			f.films[slug] = ff
			return err
		})
	}
	if err == nil {
		err = query(ctx, r.db, `SELECT slug FROM letterboxd_seen`, func(rows *sql.Rows) error {
			var slug string
			err := rows.Scan(&slug)
			f.seen[slug] = true
			return err
		})
	}
	if err == nil {
		err = query(ctx, r.db, `SELECT id, slug, tmdb, title, year, watched, rating, logged_at, rated_at FROM letterboxd_entry ORDER BY id`, func(rows *sql.Rows) error {
			var e rssEntry
			var logged, rated string
			if err := rows.Scan(&e.ID, &e.Slug, &e.TMDB, &e.Title, &e.Year, &e.Watched, &e.Rating, &logged, &rated); err != nil {
				return err
			}
			e.LoggedAt, err = time.Parse(store.TimeFormat, logged)
			if err == nil {
				e.RatedAt, err = time.Parse(store.TimeFormat, rated)
			}
			f.entries = append(f.entries, e)
			return err
		})
	}
	return f, err
}

func query(ctx context.Context, db *sql.DB, q string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// readLibrary lists the movie folders on disk: the library is the files, not what Plex or Jellyfin indexed.
func readLibrary(dir string) (map[int]libraryFilm, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[int]libraryFilm, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// ParseMovie reads a file path; the folder's own main-file name puts the folder through the same parser.
		m, ok := library.ParseMovie(filepath.Join(dir, e.Name(), e.Name()+".mkv"))
		if !ok || m.ID.Source != "tmdb" {
			continue
		}
		var id int
		if _, err := fmt.Sscan(m.ID.Value, &id); err == nil {
			out[id] = libraryFilm{Title: m.Title, Year: m.Year}
		}
	}
	return out, nil
}

// writeMissing rewrites the owner's list of watchlist films not on the NAS, only when its text changes.
func (r *run) writeMissing(missing []missingFilm) error {
	if r.opt.MissingFile == "" {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Letterboxd watchlist films not on the NAS (%d). Written by projectionist; edits are overwritten.\n", len(missing))
	for _, m := range missing {
		fmt.Fprintf(&b, "%s (%d)\t%s/film/%s/\n", m.Title, m.Year, r.opt.LetterboxdURL, m.Slug)
	}
	old, err := os.ReadFile(r.opt.MissingFile)
	if err == nil && string(old) == b.String() {
		return nil
	}
	r.count("missing_file_writes", 1)
	if r.opt.DryRun {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(r.opt.MissingFile), "."+filepath.Base(r.opt.MissingFile)+".tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil { // no chmod afterwards: it would strip the ACL
		return err
	}
	return os.Rename(tmp, r.opt.MissingFile)
}
