// Package posters picks the most artistic poster for every movie, show and season and puts it on Jellyfin and Plex
// (port of posters/posters.py, which ran on the Mac).
//
// Each run reconciles the library against the posters_items table, so an early trigger or a crash only resumes:
//
//	inventory   folder names under the Movies and TV roots → titles (identity from {tmdb-N}; no app is asked)
//	owner       once: which poster Plex showed at the pre-rename backup and whether the owner uploaded or picked it
//	candidates  the owner's poster + TMDB's (every language, from TMDB directly) + fanart.tv's, filtered to English,
//	            textless or the title's own language, best voted first, near-duplicates dropped by perceptual hash
//	judge       one vision call per title on the owner's Anthropic key, under a hard budget; a backlog goes through
//	            the Message Batches API at half price. The owner's UPLOAD is never replaced, the owner's PICK only by
//	            a candidate the judge scores at least 2 points higher
//	apply       an outbox per app (Plex, Jellyfin are sinks), retried with backoff
//
// While anyone but the owner watches, the heavy steps wait for a later run: downloads and resizes, batch uploads and
// poster uploads. Collecting a finished batch's results (a small download) goes on.
package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"projectionist/internal/llm"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const name = "posters"

type Options struct {
	MoviesRoot       string        `toml:"movies_root"`
	TVRoot           string        `toml:"tv_root"`
	Only             []string      `toml:"only"`    // title keys ("movie:tmdb-422", "season:tmdb-1400:2") to limit a run to
	DryRun           bool          `toml:"dry_run"` // candidates and judge only: Plex and Jellyfin are not touched
	CacheDir         string        `toml:"cache_dir"`
	SecretsDir       string        `toml:"secrets_dir"`     // tmdb_read_token (or tmdb_api_key), fanart_api_key, anthropic_api_key
	OwnerItems       string        `toml:"owner_items"`     // customizations/items.json: ratingKey, guid, external ids per item
	OwnerPosters     string        `toml:"owner_posters"`   // customizations/posters/<kind>/<ratingKey>.jpg
	OwnerBackupDB    string        `toml:"owner_backup_db"` // the Plex DB .backup taken with that export
	Model            string        `toml:"model"`           // "haiku:latest": the newest Haiku, checked daily
	MaxTokens        int           `toml:"max_tokens"`
	BudgetUSD        float64       `toml:"budget_usd"` // every judge call ever, not per run
	BatchMin         int           `toml:"batch_min"`  // this many titles to judge or more go through Message Batches
	BatchSize        int           `toml:"batch_size"`
	FetchWorkers     int           `toml:"fetch_workers"`
	JudgeWorkers     int           `toml:"judge_workers"`
	ApplyWorkers     int           `toml:"apply_workers"`
	PlexMovieSection int           `toml:"plex_movie_section"`
	PlexTVSection    int           `toml:"plex_tv_section"`
	SinkIndexEvery   time.Duration `toml:"sink_index_every"` // how often each app's item list is fetched
	Mark             string        `toml:"mark"`             // EXIF ImageDescription on every uploaded image
	AnthropicAPI     string        `toml:"anthropic_api"`
	TMDBAPI          string        `toml:"tmdb_api"`
	TMDBImages       string        `toml:"tmdb_images"`
	FanartAPI        string        `toml:"fanart_api"`
}

func defaults() Options {
	backups := "/volume1/PlexMediaServer/backups"
	return Options{
		MoviesRoot:       "/volume1/media/Movies",
		TVRoot:           "/volume1/media/TV",
		CacheDir:         "/state/posters",
		SecretsDir:       "/run/secrets",
		OwnerItems:       backups + "/customizations/items.json",
		OwnerPosters:     backups + "/customizations/posters",
		OwnerBackupDB:    backups + "/pre-rename-20261005T0328Z/com.plexapp.plugins.library.db",
		Model:            "haiku:latest",
		MaxTokens:        400,
		BudgetUSD:        5,
		BatchMin:         25,
		BatchSize:        200,
		FetchWorkers:     3,
		JudgeWorkers:     4,
		ApplyWorkers:     2,
		PlexMovieSection: 4,
		PlexTVSection:    2,
		SinkIndexEvery:   24 * time.Hour,
		Mark:             "poster-curator 2026-10",
		AnthropicAPI:     "https://api.anthropic.com",
		TMDBAPI:          "https://api.themoviedb.org/3",
		TMDBImages:       "https://image.tmdb.org/t/p",
		FanartAPI:        "https://webservice.fanart.tv/v3",
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS posters_owner (
  key   TEXT PRIMARY KEY, -- title key
  file  TEXT NOT NULL,    -- under owner_posters: <kind>/<ratingKey>.jpg
  state TEXT NOT NULL,    -- upload | pick | default
  guid  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_items (
  key        TEXT PRIMARY KEY,
  title      TEXT NOT NULL, -- JSON Title
  cands      TEXT,          -- JSON []Candidate; NULL until gathered
  sig        TEXT NOT NULL, -- the sources the pool was gathered from
  verdict    TEXT,          -- JSON Verdict; NULL until judged
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_calls (
  id            INTEGER PRIMARY KEY,
  at            TEXT NOT NULL,
  item          TEXT NOT NULL,
  of            TEXT NOT NULL, -- the candidate set judged
  model         TEXT NOT NULL,
  mode          TEXT NOT NULL, -- sync | batch
  batch_id      TEXT NOT NULL,
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
  state      TEXT NOT NULL, -- pending | done
  attempts   INTEGER NOT NULL,
  next_at    TEXT NOT NULL,
  last_error TEXT NOT NULL,
  PRIMARY KEY (item, sink)
);`

// Stage needs no state between runs: everything lives in the store.
type Stage struct {
	HTTP   *http.Client                      // TMDB, fanart.tv, image downloads, Anthropic; nil = 2-minute timeout
	Secret func(name string) (string, error) // nil = read <secrets_dir>/<name> on every use
}

func (Stage) Name() string { return name }

// row is a title with its stored state.
type row struct {
	Title   Title
	Cands   []Candidate
	Sig     string
	Verdict *Verdict
}

// run carries one Run's clients and counters; the steps fan out, so shared state is guarded.
type run struct {
	env       *stage.Env
	opt       Options
	http      *http.Client
	tmdb      *tmdb.Client
	llm       *llm.Client
	fanartKey string
	owners    map[string]ownerPoster
	memo      sync.Map // once()'s answers, per run

	mu            sync.Mutex
	rep           *stage.Report
	budgetAlerted bool

	gateMu   sync.Mutex
	gateAt   time.Time
	gateBusy bool
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

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	secret := s.Secret
	if secret == nil {
		secret = func(n string) (string, error) {
			b, err := os.ReadFile(filepath.Join(opt.SecretsDir, n))
			return strings.TrimSpace(string(b)), err
		}
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	r := &run{env: env, opt: opt, http: client, rep: rep,
		tmdb: &tmdb.Client{API: opt.TMDBAPI, HTTP: client, Key: func() (string, error) {
			if k, err := secret("tmdb_read_token"); err == nil {
				return k, nil
			}
			return secret("tmdb_api_key")
		}},
		llm: &llm.Client{API: opt.AnthropicAPI, HTTP: client, Key: func() (string, error) { return secret("anthropic_api_key") }},
	}
	if _, err := r.tmdb.Key(); err != nil {
		return fmt.Errorf("no TMDB key: %w", err)
	}
	if k, err := secret("fanart_api_key"); err == nil {
		r.fanartKey = k
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	var imported struct {
		At   time.Time `json:"at"`
		Rows int       `json:"rows"`
	}
	found, err := env.Store.GetJSON(ctx, name, "owner_import", &imported)
	if err != nil {
		return err
	}
	if !found {
		n, err := importOwners(ctx, db, opt.OwnerItems, opt.OwnerPosters, opt.OwnerBackupDB)
		if err != nil {
			return fmt.Errorf("owner posters import: %w", err)
		}
		imported.At, imported.Rows = env.Now(), n
		if err := env.Store.PutJSON(ctx, name, "owner_import", imported); err != nil {
			return err
		}
		env.Log.Info("owner posters imported", "event", "owner_import", "titles", n)
	}
	if r.owners, err = loadOwners(ctx, db); err != nil {
		return err
	}

	titles, err := inventory(opt.MoviesRoot, opt.TVRoot)
	if err != nil {
		return err
	}
	if len(opt.Only) > 0 {
		titles = slices.DeleteFunc(titles, func(t Title) bool { return !slices.Contains(opt.Only, t.Key) })
	}
	rows, err := loadRows(ctx, db, titles, env.Now())
	if err != nil {
		return err
	}
	rep.Count("titles", len(rows))

	sig := "tmdb"
	if r.fanartKey != "" {
		sig += "+fanart"
	}
	var stale []*row
	for _, it := range rows {
		if it.Cands == nil || it.Sig != sig {
			stale = append(stale, it)
		}
	}
	parallel(ctx, opt.FetchWorkers, stale, func(it *row) {
		if r.guestsWatching(ctx) {
			r.count("gather_deferred", 1)
			return
		}
		if err := r.gather(ctx, it); err != nil {
			env.Log.Warn("candidates", "event", "gather_failed", "item", it.Title.Key, "err", err)
			r.count("gather_failed", 1)
			return
		}
		it.Sig = sig
		if err := r.saveCandidates(ctx, it); err != nil {
			env.Log.Error("save candidates", "event", "store_error", "item", it.Title.Key, "err", err)
			return
		}
		r.count("gathered", 1)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.judge(ctx, rows); err != nil {
		return err
	}
	if !opt.DryRun {
		if err := r.enqueue(ctx, rows); err != nil {
			return err
		}
		if err := r.apply(ctx); err != nil {
			return err
		}
	}
	spent, err := spentUSD(ctx, db)
	if err != nil {
		return err
	}
	rep.Summary = fmt.Sprintf("%d titles; %d judged this run; $%.4f of $%.2f spent or held", len(rows), rep.Counts["judged"], spent, opt.BudgetUSD)
	return ctx.Err()
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
		it := &row{Title: t}
		if err := tx.QueryRowContext(ctx, `SELECT cands, sig, verdict FROM posters_items WHERE key=?`, t.Key).Scan(&cands, &it.Sig, &verdict); err != nil {
			return nil, err
		}
		if cands.Valid {
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

// Stores run detached from ctx: finished work is recorded even while the run is being cancelled.
func (r *run) saveCandidates(ctx context.Context, it *row) error {
	enc, _ := json.Marshal(it.Cands)
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_items SET cands=?, sig=?, updated_at=? WHERE key=?`,
		string(enc), it.Sig, stamp(r.env.Now()), it.Title.Key)
	return err
}

func (r *run) saveVerdict(ctx context.Context, it *row, v Verdict) error {
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
