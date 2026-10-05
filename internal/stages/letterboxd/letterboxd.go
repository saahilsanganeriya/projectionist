// Package letterboxd syncs the owner's Letterboxd diary into Plex and Jellyfin: watched state, play counts, the
// last-played date, his star ratings, and his watchlist as a "Watchlist" collection promoted to Plex Home.
//
// Letterboxd is the truth; Plex and Jellyfin are sinks. Inputs, all public and fetched politely (one request per
// 2 s, every answer that can be cached is): the RSS feed (every run), the watchlist and watched-films grid pages
// (daily), the film pages (TMDB id, cached forever), and the account export unzipped on the NAS (the backfill).
// Films join the library on the TMDB id in the folder names. Per app the stage keeps a snapshot of what the app
// holds (Plex: one paged listing, re-read daily and before a run that writes to it, at most once a day per film;
// Jellyfin: the same, every jellyfin_refresh, whose default 0 re-reads it every run while its first library scan
// adds films) and updates it after each write, so a rerun writes nothing. Watched in an app is its own mark (a
// Jellyfin play count rises when playback starts). A film whose writes fail in 3 runs in a row is parked, and the
// owner told once. Diary viewings count once each, by the viewing id that the feed guid and the export's boxd.it
// code both carry; an entry he deletes is dropped, and he is told, since a view already written stays.
//
// Never: unwatch, lower a play count, clear a rating, or remove anything this stage did not add. Plex takes no
// last-played date (a scrobble stamps "now"). Reviews are not synced: a Plex review is a community post whose
// privacy the poster cannot set (createReview takes none; other members' reviews carry ANYONE or PLEX and are
// listed to non-friends), and Jellyfin has no per-user text field (UpdateUserItemDataDto).
package letterboxd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

const name = "letterboxd"

type Options struct {
	User          string        `toml:"user"`
	LetterboxdURL string        `toml:"letterboxd_url"`
	BoxdURL       string        `toml:"boxd_url"`
	UserAgent     string        `toml:"user_agent"`
	Interval      time.Duration `toml:"interval"`    // between two Letterboxd requests; at least minInterval
	MaxFetches    int           `toml:"max_fetches"` // Letterboxd requests per run; the backfill resumes next run
	PagesEvery    time.Duration `toml:"pages_every"` // watchlist + watched grid crawl
	ExportDir     string        `toml:"export_dir"`  // holds letterboxd-<user>-<date>-utc/ folders; newest wins
	MoviesDir     string        `toml:"movies_dir"`
	MissingFile   string        `toml:"missing_file"`     // watchlist films not on the NAS; "" = none
	SinkRefresh   time.Duration `toml:"sink_refresh"`     // re-read what Plex and plex.tv hold this often (Plex also before writing)
	JFRefresh     time.Duration `toml:"jellyfin_refresh"` // 0 = every run, so films Jellyfin indexes are picked up within one run

	PlexSection   int    `toml:"plex_section"`
	JellyfinUser  string `toml:"jellyfin_user"`
	Collection    string `toml:"collection"`     // "" = no Watchlist collection
	PlexWatchlist bool   `toml:"plex_watchlist"` // mirror onto the owner's plex.tv watchlist (visible per his Plex privacy)
	DiscoverURL   string `toml:"discover_url"`

	RunBudget  time.Duration `toml:"run_budget"`  // no app call starts later than this into a run; keep it under "every"
	DryRun     bool          `toml:"dry_run"`     // plan and count, write nothing to the apps or the missing file
	FirstFilms int           `toml:"first_films"` // per-film writes only for the first N library films in diary order; 0 = all
}

func defaults() Options {
	return Options{
		User:          "saahilsan",
		LetterboxdURL: "https://letterboxd.com",
		BoxdURL:       "https://boxd.it",
		UserAgent:     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 (projectionist; personal sync of one member's own public diary)",
		Interval:      minInterval,
		MaxFetches:    400,
		PagesEvery:    24 * time.Hour,
		ExportDir:     "/volume1/docker/projectionist/data/letterboxd",
		MoviesDir:     "/volume1/media/Movies",
		MissingFile:   "/volume1/media/_watchlist-missing.txt",
		SinkRefresh:   24 * time.Hour,
		PlexSection:   4,
		JellyfinUser:  "saahil",
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

// minInterval is the politeness floor: never more than one Letterboxd request per 2 s.
const minInterval = 2 * time.Second

var errTooFast = errors.New("letterboxd: interval below the 2 s politeness floor")

// Stage is registered as letterboxd.Stage{}; tests set sleep so nothing waits on the wall clock.
type Stage struct {
	sleep func(context.Context, time.Duration) error
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
	if opt.Interval < minInterval {
		return fmt.Errorf("%w: %v", errTooFast, opt.Interval)
	}
	if _, err := env.Store.DB().ExecContext(ctx, schema+foldFeedIDs); err != nil {
		return err
	}
	sleep := s.sleep
	if sleep == nil {
		sleep = realSleep
	}
	r := &run{env: env, opt: opt, db: env.Store.DB(), until: env.Now().Add(opt.RunBudget), sleep: sleep, rep: rep, fails: map[string]map[int]bool{}, fetch: &fetcher{
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		ua: opt.UserAgent, interval: opt.Interval, budget: opt.MaxFetches, now: env.Now, sleep: sleep,
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
	scope := diaryScope(wants, opt.FirstFilms)

	var wg sync.WaitGroup
	var storeFailure error
	for _, syncApp := range []func(context.Context, []want, map[int]bool) error{r.syncPlex, r.syncJellyfin, r.syncPlexTV} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := syncApp(ctx, wants, scope)
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
	rep.Summary = fmt.Sprintf("%s: %d Letterboxd films in the library (%d watched), %d unresolved, %d watchlist films not on the NAS",
		mode, inLib, watched, unresolved, len(missing))
	return nil
}

// ---- ingest: feed, grid crawls ----

type backoff struct {
	Until time.Time `json:"until"`
	Step  int       `json:"step"`
}

// guarded runs one Letterboxd fetch job of a kind, honouring and updating that kind's challenge backoff
// (1 h doubling to 24 h). Budget exhaustion and challenges end the job quietly; it resumes next run.
func (r *run) guarded(ctx context.Context, kind string, job func() error) error {
	var b backoff
	if _, err := r.env.Store.GetJSON(ctx, name, "backoff."+kind, &b); err != nil {
		return err
	}
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
		return r.env.Store.PutJSON(ctx, name, "backoff."+kind, b)
	case errors.Is(err, errBudget):
		r.count("budget_spent", 1)
		return nil
	case err != nil && ctx.Err() == nil && !isStoreErr(err):
		r.env.Log.Warn("letterboxd fetch failed", "event", "letterboxd_error", "kind", kind, "err", err)
		r.count("errors."+kind, 1)
		return nil
	case err != nil:
		return err
	}
	if b.Step != 0 {
		return r.env.Store.PutJSON(ctx, name, "backoff."+kind, backoff{})
	}
	return nil
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
	// A rating's time decides which source's rating is his latest (see wants): an entry's is when it was logged,
	// until a read finds the rating changed, which can only be after a films grid page showed the old one.
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
// deleted; an older one has only fallen off the end. A view a run already wrote for it stays: play counts are
// never lowered, so he is told.
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
		r.env.Log.Info("diary entry deleted on Letterboxd", "event", "rss_retracted", "id", e.ID, "item", e.Title, "watched", e.Watched)
		names = append(names, fmt.Sprintf("%s (watched %s)", e.Title, e.Watched))
	}
	if len(names) > 0 {
		r.count("rss_retracted", len(names))
		r.rep.Alert("Letterboxd diary entries deleted: %s. Plex and Jellyfin play counts are never lowered: a view a run already wrote for one stays until you change it by hand",
			strings.Join(names, ", "))
	}
	return nil
}

type crawlState struct {
	Next    int        `json:"next"` // next page; 0 = idle
	Started time.Time  `json:"started"`
	Done    time.Time  `json:"done"`
	Films   []gridFilm `json:"films,omitempty"` // watchlist only: pages read so far
}

// crawl reads /<user>/<kind>/ page by page, at most once per PagesEvery, resuming where an earlier run stopped
// that day (a crawl older than PagesEvery starts over, so page 1, the newest films, is read daily even when
// Cloudflare blocks the deeper pages). "films" pages record watched films and their current ratings; a complete
// "watchlist" crawl replaces the watchlist snapshot (an incomplete one never does, so a blocked page cannot
// shrink the watchlist). A page that lists no films without saying the list is empty is backed off as a
// challenge: an interstitial answered with 200, a redirect or new markup must not end a crawl, and so empty both
// Watchlist collections.
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
		if err == nil && len(films) == 0 && !bytes.Contains(p.body, []byte(`data-num-entries="0"`)) {
			err = fmt.Errorf("%w: GET %s: HTTP %d lists no films and does not say the list is empty", errChallenged, url, p.status)
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

// resolve looks up what the TMDB join lacks; learned reports whether any answer was stored.
func (r *run) resolve(ctx context.Context, f facts) (learned bool, err error) {
	refs := f.resolveNeeds()
	if len(refs) == 0 {
		return false, nil
	}
	err = r.guarded(ctx, "film", func() error {
		done := map[string]bool{}
		for _, ref := range refs {
			slug, isSlug := strings.CutPrefix(ref, "slug:")
			if !isSlug {
				lid := strings.TrimPrefix(ref, "lid:")
				p, err := r.fetch.get(ctx, r.opt.BoxdURL+"/"+lid, nil)
				if err != nil && p.status != http.StatusNotFound {
					return err
				}
				slug = slugOf(p.header.Get("Location")) // "" when the code is not a film: cached as such
				if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_lid(lid, slug) VALUES (?, ?)`, lid, slug); err != nil {
					return storeErr{err}
				}
				learned = true
				r.count("resolved_lids", 1)
			}
			if _, known := f.films[slug]; slug == "" || known || done[slug] {
				continue
			}
			done[slug] = true
			tmdb, kind, err := r.filmPage(ctx, slug)
			if err != nil {
				return err
			}
			if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_film(slug, tmdb, kind) VALUES (?, ?, ?)`, slug, tmdb, kind); err != nil {
				return storeErr{err}
			}
			learned = true
			r.count("resolved_films", 1)
		}
		return nil
	})
	return learned, err
}

// filmPage reads one film page, following one slug rename (Letterboxd 301s an old slug to the new one).
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
		if p.status/100 == 3 {
			next := slugOf(p.header.Get("Location"))
			if next == "" {
				return 0, "", nil
			}
			url = fmt.Sprintf("%s/film/%s/", r.opt.LetterboxdURL, next)
			continue
		}
		tmdb, kind := parseFilmPage(p.body)
		return tmdb, kind, nil
	}
	return 0, "", nil
}

// ---- facts from the store, the export and the library ----

func (r *run) loadFacts(ctx context.Context) (facts, error) {
	f := facts{seen: map[string]seenFilm{}, lidSlug: map[string]string{}, films: map[string]filmFacts{}}
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
		err = query(ctx, r.db, `SELECT slug, rating, seen_at FROM letterboxd_seen`, func(rows *sql.Rows) error {
			var slug, at string
			var s seenFilm
			if err := rows.Scan(&slug, &s.Rating, &at); err != nil {
				return err
			}
			s.At, err = time.Parse(store.TimeFormat, at)
			f.seen[slug] = s
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
