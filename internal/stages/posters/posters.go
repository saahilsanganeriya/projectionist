// Package posters picks the most artistic poster for every movie, show and season, and puts it on Jellyfin and
// Plex (port of posters/posters.py, which ran on the Mac).
//
// Each run reconciles the library against the posters_items table, so an early trigger or a crash only resumes:
//
//	inventory   folder names under the Movies and TV roots → titles (identity from {tmdb-N}; no Plex)
//	candidates  the owner's backed-up poster + TMDB's posters (through Jellyfin, or the TMDB API once a key file
//	            exists) + fanart.tv's (with a key), filtered by size and language, ranked by votes, near-duplicates
//	            dropped by perceptual hash; a title Jellyfin has not scanned yet stays pending
//	judge       one vision call per title through the AI gateway, under a hard budget; the owner's UPLOAD is never
//	            replaced, the owner's PICK only by a candidate the judge scores >= 2 points higher
//	apply       an outbox per sink, drained while no guest watches: Jellyfin always; Plex (a best-effort sink with
//	            backoff that never fails the run) only when the choice is not the poster Plex already shows
package posters

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/tmdb"
)

const name = "posters"

type Options struct {
	MoviesRoot    string   `toml:"movies_root"`
	TVRoot        string   `toml:"tv_root"`
	Only          []string `toml:"only"`    // title keys ("movie:tmdb-422", "season:tmdb-1400:2") to limit a run to
	DryRun        bool     `toml:"dry_run"` // candidates and judge only; nothing is uploaded
	CacheDir      string   `toml:"cache_dir"`
	OwnerItems    string   `toml:"owner_items"`    // customizations/items.json: ratingKey, guid, TMDB ids per item
	OwnerPosters  string   `toml:"owner_posters"`  // customizations/posters/<kind>/<ratingKey>.jpg
	OwnerStateDB  string   `toml:"owner_state_db"` // Plex DB .backup taken with that export: upload or pick per guid
	PlexDBGlob    string   `toml:"plex_db_glob"`   // newest match maps a folder to its Plex ratingKey for uploads
	AIKeyFile     string   `toml:"ai_key_file"`
	TMDBKeyFile   string   `toml:"tmdb_key_file"`   // optional; empty or absent = ask Jellyfin
	FanartKeyFile string   `toml:"fanart_key_file"` // optional
	Gateway       string   `toml:"gateway"`
	Model         string   `toml:"model"`
	TMDBAPI       string   `toml:"tmdb_api"`
	TMDBImages    string   `toml:"tmdb_images"`
	FanartAPI     string   `toml:"fanart_api"`
	BudgetUSD     float64  `toml:"budget_usd"` // all judge calls ever, not per run
	JudgeWorkers  int      `toml:"judge_workers"`
	FetchWorkers  int      `toml:"fetch_workers"`
	Mark          string   `toml:"mark"` // EXIF ImageDescription on every image this stage uploads
}

func defaults() Options {
	backups := "/volume1/PlexMediaServer/backups"
	return Options{
		MoviesRoot:    "/volume1/media/Movies",
		TVRoot:        "/volume1/media/TV",
		CacheDir:      "/state/posters",
		OwnerItems:    backups + "/customizations/items.json",
		OwnerPosters:  backups + "/customizations/posters",
		OwnerStateDB:  backups + "/pre-rename-20261005T0328Z/com.plexapp.plugins.library.db",
		PlexDBGlob:    backups + "/*.db",
		AIKeyFile:     "/run/secrets/vercel_ai_key",
		TMDBKeyFile:   "/run/secrets/tmdb_api_key",
		FanartKeyFile: "/run/secrets/fanart_api_key",
		Gateway:       "https://ai-gateway.vercel.sh/v1",
		Model:         "google/gemini-2.5-flash-lite",
		TMDBAPI:       "https://api.themoviedb.org/3",
		TMDBImages:    "https://image.tmdb.org/t/p",
		FanartAPI:     "https://webservice.fanart.tv/v3",
		BudgetUSD:     5,
		JudgeWorkers:  50,
		FetchWorkers:  6,
		Mark:          "poster-curator 2026-10",
	}
}

const schema = `
CREATE TABLE IF NOT EXISTS posters_items (
  key        TEXT PRIMARY KEY,
  title      TEXT NOT NULL,
  cands      TEXT,          -- JSON []Candidate; NULL until gathered
  sig        TEXT NOT NULL, -- the candidate sources the list was gathered with
  verdict    TEXT,          -- JSON Verdict; NULL until judged
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_calls (
  id                INTEGER PRIMARY KEY,
  at                TEXT NOT NULL,
  item              TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL,
  completion_tokens INTEGER NOT NULL,
  cost_usd          REAL NOT NULL,
  images            INTEGER NOT NULL,
  llm_ms            INTEGER NOT NULL,
  error             TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS posters_sinks (
  item       TEXT NOT NULL,
  sink       TEXT NOT NULL,          -- jellyfin | plex
  url        TEXT NOT NULL,          -- the candidate to show there
  state      TEXT NOT NULL,          -- pending | done
  attempts   INTEGER NOT NULL,
  next_at    TEXT NOT NULL,
  last_error TEXT NOT NULL,
  PRIMARY KEY (item, sink)
);`

// Title is one movie, show or season found on disk.
type Title struct {
	Key    string     `json:"key"`
	Kind   tmdb.Kind  `json:"kind"`
	Name   string     `json:"name"` // as the judge reads it: "Seinfeld - Season 2"
	Year   int        `json:"year"`
	ID     library.ID `json:"id"` // the movie's or the show's
	Season int        `json:"season,omitempty"`
	Folder string     `json:"folder"`
}

func (t Title) ref() tmdb.Ref { return tmdb.Ref{Kind: t.Kind, ID: t.ID.Value, Season: t.Season} }

// Candidate is one poster in a title's pool. The owner's poster, when the backup has one, is always first.
type Candidate struct {
	URL    string  `json:"url"`   // full size: https://… or owner:<kind>/<ratingKey>.jpg
	Thumb  string  `json:"thumb"` // what the judge sees; also the thumbnail cache key
	Source string  `json:"source"`
	Owner  string  `json:"owner,omitempty"` // upload | pick | default, on the owner's poster only
	Lang   string  `json:"lang"`
	Votes  int     `json:"votes"`
	Rating float64 `json:"rating"`
	Hash   uint64  `json:"dhash"`
}

const (
	srcOwner  = "owner"
	srcTMDB   = "tmdb"
	srcFanart = "fanart"

	ownerUpload  = "upload"
	ownerPick    = "pick"
	ownerDefault = "default"
)

// Verdict is the decision for one candidate set; a different set (a new source) is judged again.
type Verdict struct {
	Choice   string             `json:"choice"` // Candidate.URL
	RunnerUp string             `json:"runner_up,omitempty"`
	Rule     string             `json:"rule"`
	Reason   string             `json:"reason,omitempty"`
	Scores   map[string]float64 `json:"scores,omitempty"` // by Candidate.URL
	Of       string             `json:"of"`               // candidateSetID of what was judged
	At       time.Time          `json:"at"`
}

// row is a title with its stored state.
type row struct {
	Title   Title
	Cands   []Candidate
	Sig     string
	Verdict *Verdict
}

func (r *row) choice() (Candidate, bool) {
	if r.Verdict == nil {
		return Candidate{}, false
	}
	i := slices.IndexFunc(r.Cands, func(c Candidate) bool { return c.URL == r.Verdict.Choice })
	if i < 0 {
		return Candidate{}, false
	}
	return r.Cands[i], true
}

// Stage: HTTP is used for TMDB, fanart.tv, image downloads and the AI gateway (nil = a client with a 2 min timeout).
type Stage struct {
	HTTP *http.Client
}

func (Stage) Name() string { return name }

// run carries one Run's shared state; its counters are guarded because the steps fan out.
type run struct {
	env  *stage.Env
	opt  Options
	http *http.Client
	jf   *tmdb.Jellyfin

	mu  sync.Mutex
	rep *stage.Report

	langOnce sync.Once
	langs    map[string]string // ISO 639-2 (every spelling) → 639-1
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
	if _, err := env.Store.DB().ExecContext(ctx, schema); err != nil {
		return err
	}
	titles, err := inventory(opt.MoviesRoot, opt.TVRoot)
	if err != nil {
		return err
	}
	if len(opt.Only) > 0 {
		titles = slices.DeleteFunc(titles, func(t Title) bool { return !slices.Contains(opt.Only, t.Key) })
	}
	rows, err := loadRows(ctx, env.Store.DB(), titles, env.Now())
	if err != nil {
		return err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	r := &run{env: env, opt: opt, http: client, jf: &tmdb.Jellyfin{JF: env.Jellyfin}, rep: rep}
	rep.Count("titles", len(rows))
	if err := r.gather(ctx, rows); err != nil {
		return err
	}
	if err := r.judge(ctx, rows); err != nil {
		return err
	}
	if !opt.DryRun {
		if err := r.apply(ctx, rows); err != nil {
			return err
		}
	}
	spent, err := spentUSD(ctx, env.Store.DB())
	if err != nil {
		return err
	}
	rep.Summary = fmt.Sprintf("%d titles; %d judged this run; $%.4f of $%.2f spent", len(rows), rep.Counts["judged"], spent, opt.BudgetUSD)
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

// Stores run detached from ctx: finished work is recorded even when the run is being cancelled.
func (r *run) saveCandidates(ctx context.Context, it *row) error {
	enc, _ := json.Marshal(it.Cands)
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_items SET cands=?, sig=?, updated_at=? WHERE key=?`,
		string(enc), it.Sig, stamp(r.env.Now()), it.Title.Key)
	return err
}

func (r *run) saveVerdict(ctx context.Context, it *row) error {
	enc, _ := json.Marshal(it.Verdict)
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_items SET verdict=?, updated_at=? WHERE key=?`,
		string(enc), stamp(r.env.Now()), it.Title.Key)
	return err
}

func spentUSD(ctx context.Context, db *sql.DB) (float64, error) {
	var spent float64
	err := db.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd), 0) FROM posters_calls`).Scan(&spent)
	return spent, err
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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
