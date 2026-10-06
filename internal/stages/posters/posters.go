// Package posters picks the most artistic poster for every movie, show and season and puts it on Jellyfin and Plex
// (port of posters/posters.py, which ran on the Mac), and gives every Movies collection without a poster of the
// owner's one (collections.go), and dresses each franchise shelf's films in one MediUX poster set (sets.go).
//
// Each run reconciles the library against the posters_items table, so an early trigger or a crash only resumes:
//
//	inventory   folder names under the Movies and TV roots → titles (identity from {tmdb-N}; no app is asked)
//	owner       per export: which poster Plex showed at a Plex DB backup and whether the owner uploaded or picked it
//	candidates  the owner's poster + TMDB's (every language, from env.TMDB) + fanart.tv's (env.Fanart), filtered to
//	            English, textless or the title's own language, best voted first, near-duplicates dropped by dHash
//	judge       one vision call per title through the AI gateway (env.LLM: the posters task's model, the collections
//	            task's for a collection), as many at once as the client allows, under a hard budget. The owner's
//	            UPLOAD and PICK are never replaced, Plex's DEFAULT only by a candidate scored at least 2 points higher;
//	            a pool judged before is decided again from its stored scores when these rules change
//	apply       an outbox per app (Plex, Jellyfin are sinks), retried with backoff; Plex's lock is its own step
//
// While anyone but the owner watches, the heavy steps wait for a later run: downloads and resizes, judging and poster
// uploads. The gate is asked before each title's download, its judge call and its uploads (at most once a minute).
package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"projectionist/internal/fanart"
	"projectionist/internal/llm"
	"projectionist/internal/mediux"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const name = "posters"

type Options struct {
	MoviesRoot       string        `toml:"movies_root"`
	TVRoot           string        `toml:"tv_root"`
	Only             []string      `toml:"only"`    // title keys ("movie:tmdb-422", "season:tmdb-1400:2", "collection:scary movie") to limit a run to
	DryRun           bool          `toml:"dry_run"` // titles: candidates and judge only, no poster uploaded
	CacheDir         string        `toml:"cache_dir"`
	OwnerItems       string        `toml:"owner_items"`     // customizations/items.json: ratingKey, guid, external ids per item
	OwnerPosters     string        `toml:"owner_posters"`   // customizations/posters/<kind>/<ratingKey>.jpg
	OwnerBackupDB    string        `toml:"owner_backup_db"` // the Plex DB .backup taken with that export
	MaxTokens        int           `toml:"max_tokens"`      // reasoning included
	BudgetUSD        float64       `toml:"budget_usd"`      // every judge call ever, not per run
	FetchWorkers     int           `toml:"fetch_workers"`
	ApplyWorkers     int           `toml:"apply_workers"`
	PlexMovieSection int           `toml:"plex_movie_section"`
	PlexTVSection    int           `toml:"plex_tv_section"`
	SinkIndexEvery   time.Duration `toml:"sink_index_every"` // how often each app's item list is fetched
	SinkAlertAfter   time.Duration `toml:"sink_alert_after"` // an app whose listing fails this long raises an alert
	Mark             string        `toml:"mark"`             // EXIF ImageDescription on the images the stage chose
	TMDBImages       string        `toml:"tmdb_images"`
	// Collections: both files set (the collections stage's) turn them on.
	CollectionsOwner    string  `toml:"collections_owner_file"`    // the owner's collections: never given a poster here
	CollectionsApproved string  `toml:"collections_approved_file"` // with the arrivals the collections stage stored
	CollectionDryRun    bool    `toml:"collection_dry_run"`        // collections: candidates and judge only (default)
	CollectionBudgetUSD float64 `toml:"collection_budget_usd"`     // every collection judge call ever, apart from budget_usd
	DirectorMinScore    float64 `toml:"director_min_score"`        // a director's photo needs this judge score or no poster
	CommonsAPI          string  `toml:"commons_api"`
	CommonsContact      string  `toml:"commons_contact"` // a URL or e-mail for the User-Agent: Wikimedia then allows 20× the rate
	// The owner's own posters for other films (title keys of his uploads and picks), shown to the judge of every title
	// and franchise as his taste; none = the prompt alone.
	TasteReferences []string `toml:"taste_references"`
	// Poster sets (sets.go), with collections on: MediUX's site and image host; mediux = "" turns them off.
	MediUX       string `toml:"mediux"`
	MediUXAssets string `toml:"mediux_assets"`
}

func defaults() Options {
	backups := "/volume1/PlexMediaServer/backups"
	return Options{
		MoviesRoot:       "/volume1/media/Movies",
		TVRoot:           "/volume1/media/TV",
		CacheDir:         "/state/posters",
		OwnerItems:       backups + "/customizations/items.json",
		OwnerPosters:     backups + "/customizations/posters",
		OwnerBackupDB:    backups + "/pre-rename-20261005T0328Z/com.plexapp.plugins.library.db",
		MaxTokens:        1024,
		BudgetUSD:        12,
		FetchWorkers:     3,
		ApplyWorkers:     2,
		PlexMovieSection: 4,
		PlexTVSection:    2,
		SinkIndexEvery:   24 * time.Hour,
		SinkAlertAfter:   72 * time.Hour,
		Mark:             "poster-curator 2026-10",
		TMDBImages:       "https://image.tmdb.org/t/p",

		CollectionDryRun:    true,
		CollectionBudgetUSD: 5,
		DirectorMinScore:    4,
		CommonsAPI:          "https://commons.wikimedia.org/w/api.php",
		MediUX:              "https://mediux.pro",
		MediUXAssets:        "https://api.mediux.pro/assets",
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS posters_owner (
  key   TEXT PRIMARY KEY, -- title key
  file  TEXT NOT NULL,    -- the export's poster: <owner_posters>/<kind>/<ratingKey>.jpg
  state TEXT NOT NULL,    -- upload | pick | default
  guid  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_items (
  key        TEXT PRIMARY KEY,
  title      TEXT NOT NULL, -- JSON Title
  cands      TEXT,          -- JSON []Candidate (null: none found); NULL until gathered
  sig        TEXT NOT NULL, -- the sources and owner poster the pool was gathered from
  verdict    TEXT,          -- JSON Verdict; NULL until judged
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_calls (
  id            INTEGER PRIMARY KEY,
  at            TEXT NOT NULL,
  item          TEXT NOT NULL,
  of            TEXT NOT NULL, -- the candidate set judged
  model         TEXT NOT NULL,
  mode          TEXT NOT NULL, -- gateway (sync | batch: Anthropic's API, before the gateway)
  batch_id      TEXT NOT NULL, -- an Anthropic Message Batch's
  state         TEXT NOT NULL, -- pending | done | failed (no answer) | unusable (an answer with no scores)
  input_tokens  INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  reserved_usd  REAL NOT NULL, -- worst case, held against the budget while pending
  cost_usd      REAL NOT NULL,
  llm_ms        INTEGER NOT NULL,
  error         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS posters_calls_state ON posters_calls(state, batch_id);
CREATE TABLE IF NOT EXISTS posters_sinks (
  item       TEXT NOT NULL,
  sink       TEXT NOT NULL, -- jellyfin | plex
  url        TEXT NOT NULL, -- the candidate to show there
  state      TEXT NOT NULL, -- pending | uploaded (Plex: the lock is still to set) | done
  target     TEXT NOT NULL, -- the app's item id the url went to ('' while pending)
  attempts   INTEGER NOT NULL,
  next_at    TEXT NOT NULL,
  last_error TEXT NOT NULL,
  PRIMARY KEY (item, sink)
);`

// Stage needs no state between runs: everything lives in the store.
type Stage struct{}

func (Stage) Name() string { return name }

// row is a title with its stored state.
type row struct {
	Title      Title
	Gathered   bool      // Cands holds the pool, which may be empty
	GatheredAt time.Time // when it was gathered (an empty pool is gathered again a day later)
	Cands      []Candidate
	Sig        string
	Verdict    *Verdict
}

const (
	emptyRegather = 24 * time.Hour     // an empty pool is gathered again after this: a source may have gained art
	emptyAlert    = 3 * 24 * time.Hour // a title or collection empty this long raises an alert
)

// refs is the message of the owner's taste references (nil: none) and an upper estimate of its tokens.
type refs struct {
	message *llm.Message
	tokens  int
}

// run carries one Run's clients and counters; the steps fan out, so shared state is guarded.
type run struct {
	env    *stage.Env
	opt    Options
	http   *http.Client // image downloads
	owners map[string]ownerPoster

	mu            sync.Mutex
	rep           *stage.Report
	budgetAlerted map[bool]bool // titles (false), collections (true)

	gateMu   sync.Mutex
	gateAt   time.Time
	gateBusy bool

	collections map[string]bool // this run's collections (keys): only their outbox rows are sent
	refs        refs            // the owner's taste references, set before judging fans out
	ownerKeys   map[string]bool // the owner's collections' Plex ratingKeys: never a target

	mx         *mediux.Client
	setPosters map[string][]Candidate // title key → the worn sets' posters its pool holds (sets.go)
	bonus      map[string]string      // film title key → its worn set's poster, which gets the shelf bonus
	credits    map[string]string      // a set poster's URL → its artist's credit, for the EXIF mark
	worn       []wornSet

	wikiMu    sync.Mutex // requests to Wikimedia go one at a time, WikimediaPace apart (wikimediaTurn)
	wikiLast  time.Time
	wikiPause time.Time // no Wikimedia request before this: the last Retry-After
}

// guestsWatching is the viewer gate, asked at most once a minute; a gate that cannot tell counts as busy.
func (r *run) guestsWatching(ctx context.Context) bool {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if now := r.env.Now(); now.Sub(r.gateAt) >= time.Minute {
		n, err := r.env.Viewers.Count(ctx)
		r.gateAt, r.gateBusy = now, err != nil || n > 0
		if r.gateBusy {
			r.env.Log.Info("guests watching; heavy work waits", "event", "viewer_wait", "others", n, "err", err)
		}
	}
	return r.gateBusy
}

func (r *run) count(kind string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count(kind, n)
}

func (r *run) alert(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Alert(format, args...)
}

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	r := &run{env: env, opt: opt, http: &http.Client{Timeout: 2 * time.Minute}, rep: rep, budgetAlerted: map[bool]bool{}}

	// An export (items.json, its posters and the Plex DB backup taken with it) is imported once; pointing the
	// options at a newer export merges it in (importOwners says how).
	var imported struct {
		Export string    `json:"export"`
		At     time.Time `json:"at"`
		Rows   int       `json:"rows"`
	}
	if _, err := env.Store.GetJSON(ctx, name, "owner_import", &imported); err != nil {
		return err
	}
	if export := strings.Join([]string{opt.OwnerItems, opt.OwnerPosters, opt.OwnerBackupDB}, "\n"); imported.Export != export {
		n, err := importOwners(ctx, db, opt.OwnerItems, opt.OwnerPosters, opt.OwnerBackupDB, opt.Mark)
		if err != nil {
			return fmt.Errorf("owner posters import: %w", err)
		}
		imported.Export, imported.At, imported.Rows = export, env.Now(), n
		if err := env.Store.PutJSON(ctx, name, "owner_import", imported); err != nil {
			return err
		}
		env.Log.Info("owner posters imported", "event", "owner_import", "titles", n, "backup", opt.OwnerBackupDB)
	}
	owners, err := loadOwners(ctx, db)
	if err != nil {
		return err
	}
	r.owners = owners

	titles, err := inventory(opt.MoviesRoot, opt.TVRoot)
	if err != nil {
		return err
	}
	dressShelves := false
	if opt.CollectionsApproved != "" {
		// A collections file that cannot be read stops the collections only; the titles go on.
		if cs, keys, err := collectionTitles(ctx, db, opt); err != nil {
			rep.Alert("posters: collections skipped this run: %v", err)
		} else {
			titles, r.ownerKeys = append(titles, cs...), keys
			rep.Count("collections", len(cs))
			dressShelves = opt.MediUX != ""
		}
	}
	if len(opt.Only) > 0 {
		titles = slices.DeleteFunc(titles, func(t Title) bool { return !slices.Contains(opt.Only, t.Key) })
	}
	r.collections = map[string]bool{}
	for _, t := range titles {
		if t.Kind.collection() {
			r.collections[t.Key] = true
		}
	}
	rows, err := loadRows(ctx, db, titles, env.Now())
	if err != nil {
		return err
	}
	rep.Count("titles", len(rows))
	r.setPosters, r.bonus, r.credits = map[string][]Candidate{}, map[string]string{}, map[string]string{}
	if dressShelves {
		r.mx = mediux.New(opt.MediUX, opt.MediUXAssets, env.Store, env.Now, mediuxSleep)
		if err := r.sets(ctx, titles); err != nil {
			return err
		}
	}

	var stale []*row
	for _, it := range rows {
		empty := it.Gathered && len(it.Cands) == 0 && env.Now().Sub(it.GatheredAt) >= emptyRegather
		if !it.Gathered || it.Sig != r.poolSig(it.Title) || empty {
			stale = append(stale, it)
		}
	}
	// A missing TMDB or fanart.tv key, or a thumbnail cache that cannot be written, fails every title alike: the run
	// stops on the first one.
	gctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	parallel(gctx, opt.FetchWorkers, stale, func(it *row) {
		if r.guestsWatching(gctx) {
			r.count("gather_deferred", 1)
			return
		}
		if err := r.gather(gctx, it); err != nil {
			if errors.Is(err, tmdb.ErrNoCredentials) || errors.Is(err, fanart.ErrNoKey) || errors.Is(err, errThumbCache) {
				stop(err)
			}
			env.Log.Warn("candidates", "event", "gather_failed", "item", it.Title.Key, "err", err)
			r.count("gather_failed", 1)
			return
		}
		it.Gathered, it.Sig = true, r.poolSig(it.Title)
		if err := r.saveCandidates(ctx, it); err != nil {
			env.Log.Error("save candidates", "event", "store_error", "item", it.Title.Key, "err", err)
			return
		}
		r.count("gathered", 1)
	})
	if err := context.Cause(gctx); err != nil {
		return err
	}
	if err := r.emptyPools(ctx, rows); err != nil {
		return err
	}
	if err := r.judge(ctx, rows); err != nil {
		return err
	}
	if err := r.dropUnworn(ctx, rows); err != nil {
		return err
	}
	r.noPoster(rows)
	if live := r.live(); live[false] || live[true] {
		var send []*row
		for _, it := range rows {
			if live[it.Title.Kind.collection()] {
				send = append(send, it)
			}
		}
		if err := r.enqueue(ctx, send); err != nil {
			return err
		}
		if err := r.apply(ctx); err != nil {
			return err
		}
	}
	spent, err := spentUSD(ctx, db, false)
	if err != nil {
		return err
	}
	rep.Summary = fmt.Sprintf("%d titles; %d judged this run; $%.4f of $%.2f spent or held", len(rows), rep.Counts["judged"], spent, opt.BudgetUSD)
	if opt.CollectionsApproved != "" {
		if spent, err = spentUSD(ctx, db, true); err != nil {
			return err
		}
		rep.Summary += fmt.Sprintf("; collections $%.4f of $%.2f", spent, opt.CollectionBudgetUSD)
	}
	return ctx.Err()
}

// live is, for titles (false) and collections (true), whether their posters are uploaded this run.
func (r *run) live() map[bool]bool {
	return map[bool]bool{false: !r.opt.DryRun, true: r.opt.CollectionsApproved != "" && !r.opt.CollectionDryRun}
}

// ---- store ----

// loadRows upserts this run's titles and returns them with their stored candidates and verdicts.
func loadRows(ctx context.Context, db *sql.DB, titles []Title, now time.Time) ([]*row, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]*row, 0, len(titles))
	for _, t := range titles {
		enc, _ := json.Marshal(t)
		if _, err := tx.ExecContext(ctx, `INSERT INTO posters_items(key, title, sig, updated_at) VALUES (?, ?, '', ?)
			ON CONFLICT(key) DO UPDATE SET title=excluded.title WHERE title != excluded.title`, t.Key, string(enc), stamp(now)); err != nil {
			return nil, err
		}
		var cands, verdict sql.NullString
		var at string
		it := &row{Title: t}
		if err := tx.QueryRowContext(ctx, `SELECT cands, sig, verdict, updated_at FROM posters_items WHERE key=?`, t.Key).
			Scan(&cands, &it.Sig, &verdict, &at); err != nil {
			return nil, err
		}
		it.GatheredAt, _ = time.Parse(store.TimeFormat, at)
		if it.Gathered = cands.Valid; cands.Valid {
			if err := json.Unmarshal([]byte(cands.String), &it.Cands); err != nil {
				return nil, fmt.Errorf("%s candidates: %w", t.Key, err)
			}
		}
		if verdict.Valid {
			it.Verdict = new(Verdict)
			if err := json.Unmarshal([]byte(verdict.String), it.Verdict); err != nil {
				return nil, fmt.Errorf("%s verdict: %w", t.Key, err)
			}
		}
		out = append(out, it)
	}
	return out, tx.Commit()
}

// poolSig names what a title's pool is gathered from: a new owner poster (a newer export) or another worn set gathers
// it again.
func (r *run) poolSig(t Title) string {
	switch t.Kind {
	case franchise:
		return "tmdb-collection+fanart+film" + r.setSig(t.Key)
	case director:
		return "commons+tmdb-person"
	}
	sig := "tmdb+fanart"
	if p, ok := r.owners[t.Key]; ok {
		sig += " owner=" + p.State + ":" + p.File
	}
	return sig + r.setSig(t.Key)
}

// Stores run detached from ctx: finished work is recorded even while the run is being cancelled.
// emptyPools keeps, per title, since when its pool has been empty, and alerts those empty for emptyAlert or longer.
func (r *run) emptyPools(ctx context.Context, rows []*row) error {
	since := map[string]time.Time{}
	if _, err := r.env.Store.GetJSON(ctx, name, "empty_since", &since); err != nil {
		return err
	}
	var long []string
	for _, it := range rows {
		k := it.Title.Key
		switch _, known := since[k]; {
		case !it.Gathered:
		case len(it.Cands) > 0:
			delete(since, k)
		case !known:
			since[k] = r.env.Now()
		case r.env.Now().Sub(since[k]) >= emptyAlert:
			long = append(long, k)
		}
	}
	if len(long) > 0 {
		slices.Sort(long)
		r.alert("posters: %d title(s) have had no poster candidate for %v or more (gathered again daily); first: %s", len(long), emptyAlert, long[0])
	}
	return r.env.Store.PutJSON(context.WithoutCancel(ctx), name, "empty_since", since)
}

func (r *run) saveCandidates(ctx context.Context, it *row) error {
	enc, _ := json.Marshal(it.Cands)
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_items SET cands=?, sig=?, updated_at=? WHERE key=?`,
		string(enc), it.Sig, stamp(r.env.Now()), it.Title.Key)
	return err
}

func (r *run) saveVerdict(ctx context.Context, it *row, v Verdict) error {
	v.Prompt = promptVersion(it.Title.Kind)
	it.Verdict = &v
	enc, _ := json.Marshal(v)
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_items SET verdict=?, updated_at=? WHERE key=?`,
		string(enc), stamp(r.env.Now()), it.Title.Key)
	return err
}

// stamp is fixed-width (store.TimeFormat), so text order is time order in next_at comparisons.
func stamp(t time.Time) string { return t.UTC().Format(store.TimeFormat) }

// parallel runs fn over items on at most n goroutines and stops handing out work once ctx is done.
func parallel[T any](ctx context.Context, n int, items []T, fn func(T)) {
	sem := make(chan struct{}, max(1, n))
	var wg sync.WaitGroup
	for _, it := range items {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				fn(it)
			}()
			continue
		}
		break
	}
	wg.Wait()
}
