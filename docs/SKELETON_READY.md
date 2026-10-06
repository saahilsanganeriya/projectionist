# Skeleton ready — projectionist

Module `projectionist` (Go 1.26, no remote). Branch `main`; the skeleton is the commit titled
"Skeleton: stage interface, scheduler, SQLite state, clients, probe, image". `go test ./...` passes on macOS.

## Adding a stage (what each builder does)

1. Create `internal/stages/<name>/` with a type that implements `stage.Stage`.
2. Add ONE line to `cmd/projectionist/stages.go` (keep the list sorted, so merges stay trivial).
3. Add a `[stages.<name>]` table to `config.example.toml` (`every`, optional `window`, then your own keys).
4. Tests: table tests for the decision logic; `internal/fake` for Plex/Jellyfin HTTP.

```go
// internal/stage/stage.go
type Stage interface {
	Name() string
	Run(ctx context.Context, env *Env, rep *Report) error // idempotent; return promptly on ctx.Done()
}
type Env struct {
	Log      *slog.Logger // tagged stage=<name>
	Store    *store.Store
	Plex     *plex.Client
	Jellyfin *jellyfin.Client
	Viewers  *viewers.Gate
	Probe    *probe.Prober
	TMDB     *tmdb.Client   // direct TMDB v3; one per process (shared rate limit and cache)
	Fanart   *fanart.Client // direct fanart.tv
	FFmpeg   config.FFmpeg // .FFmpegPath(), .FFprobePath(), .Device
	Now      func() time.Time // UTC
	Loc      *time.Location   // config timezone, only for owner-facing text (status files)
	Options  func(v any) error // decodes your [stages.<name>] keys into v (toml tags; durations as "15m")
}
type Report struct{ Alerts []string; Counts map[string]int; Summary string }
func (r *Report) Alert(format string, args ...any) // outcome "alert"; /health 503 until a clean run
func (r *Report) Count(kind string, n int)         // metric projectionist_stage_items_total{stage,kind}
```

The scheduler never runs two copies of one stage at once. A returned error makes the outcome "error". Alerts
raised before the error are kept.

## Packages

| Path | Use it for |
|---|---|
| `internal/stage` | the interface above |
| `internal/config` | TOML loader; `Schedule(name)`, `StageOptions(name, &v)`, `ParseWindow` |
| `internal/store` | SQLite (WAL): `StartRun/FinishRun/LastRun`, `GetJSON/PutJSON(stage, key)`, `AddEvent/LastEvent(stage, item)`, `CacheGet/CachePut/CachePrune` (the `http_cache` the TMDB and fanart.tv clients use), `DB()` for your own tables. Name them `<stage>_*` and create them with `CREATE TABLE IF NOT EXISTS` |
| `internal/scheduler` | per-stage cadence + window, `Runner.RunOnce` (also used by `projectionist run <stage>`) |
| `internal/viewers` | the pause rule: `Gate.Count`, `Gate.WaitIdle(ctx)` before moving files; poll `Gate.Count` every `Gate.Poll` to SIGSTOP/SIGCONT a long process (see remote4k). Owners never block; an error counts as busy |
| `internal/plex` | `Prefs()` / `ReadPrefs(path)` (file, no API), `Sessions`, `GetJSON` (unwraps MediaContainer), `Do(method, path)`, `Send` (with a body), `DoAs(token, …)` (a shared user's own watch-state writes), `Account(method, plex.tv URL, form)` (the owner's plex.tv account: webhooks, shared users' server tokens), `WithTimeout(d)`. Token read from Preferences.xml on every request and sent as a header. A sink only (see architecture.md, "Plex and Jellyfin"): bound every call with a short context deadline and never fail a run on it |
| `internal/tmdb` | `Search(ctx, tmdb.Movie\|tmdb.TV, query, year)`, `Details(ctx, kind, id)` (original language, collection, external ids, seasons), `Find(ctx, id, "imdb_id"\|"tvdb_id")`, `Episodes(ctx, showID, season)` (0 = specials), `Images` / `SeasonImages` (language, votes; optional language filter), `Directors(ctx, movieID)`, `Directed(ctx, personID)` (film ids), `ImageURL(path, size)`. `ErrNotFound`, `ErrNoCredentials`. Rate limit, retries and the 24 h cache are inside; use `env.TMDB`, never a second client |
| `internal/fanart` | `Movie(ctx, tmdbOrIMDbID)`, `TV(ctx, tvdbID)`: one flat list of images with type, language, likes, season and size; no art is an empty list |
| `internal/jellyfin` | `GetJSON`, `PostJSON`, `PostBody(path, contentType, body)`, `Sessions(activeWithin)`, `MediaUpdated(paths...)`, `WithTimeout(d)`; a 404 is `ErrNotFound`. Key read from a file on every request |
| `internal/probe` | `Prober.Probe(ctx, path) (Info, error)`: ffprobe JSON → codec, profile, bit depth, HDR10/HLG/Dolby Vision + dv_profile, size, fps, bitrate, audio and subtitle tracks, duration. Cached in `probe_cache` keyed by (path, size, mtime_ns). `Parse(raw)` is pure, for tests |
| `internal/watch` | who watched what, as projectionist's own record: `Add` an event (finished, stopped, state, rated) per person and item (`movie/tmdb-N`, `tv/tmdb-N/s01e02`, from `ItemsForPath`), an identical event is recorded once; `States` folds them, last writer wins. Fed by jfsync's hooks and imports; read it rather than asking an app |
| `internal/library` | `ParseMovie(path)` / `ParseEpisode(path)` (and `ParseMovieFolder` / `ParseShowFolder` / `ParseSeasonFolder` for folder names): identity (`{tmdb-N}`, `{imdb-ttN}`, `{tvdb-N}`), year, label, season, episode range, read from the naming scheme |
| `internal/llm` | Every model call, through the Vercel AI Gateway (OpenAI-compatible Chat Completions; `[llm]` in the config). `env.LLM.Session(task)` per stage run: `Chat(ctx, req)` sends `req` as the task's model with its fallbacks and reasoning, `WorstCase`/`Price` for a budget's reservation, `Parked()` for the run's one alert. `Result.CostUSD` is the gateway's own figure (else usage at the dearest price the gateway's model list gives, cached a day) plus every attempt whose answer was lost at its worst case. `llmtest` is the fake gateway the tests share. Key read per request |
| `internal/fsx` | `RenameNoReplace` (renameat2 RENAME_NOREPLACE; link+unlink fallback for files), `Clone` (FICLONE reflink, byte-copy fallback, never overwrites), `SyncDir` (one fsync per unit) |
| `internal/obs` | `NewLogger` (UTC, key=value or JSON), `Metrics` (Prometheus text) |
| `internal/server` | `/health` (503 while any stage's latest run is alert/error) and `/metrics` |
| `internal/fake` | `fake.NewEnv(t, prefs)` → a full `stage.Env` (fake Plex + Jellyfin with no sessions, fake TMDB `h.TMDB` and fanart.tv `h.Fanart` answering 404 until a route is registered, temp store, 5 ms viewer poll); `h.SetOptions(toml)`, `h.Script(t, "ffprobe"|"ffmpeg", body)` for stand-in binaries, `h.Now` to move the clock. Lower level: `fake.Plex(t, prefs)`, `fake.Jellyfin(t)`, `srv.Handle(method, path, status, body)`, `srv.HandleFunc`, `srv.Calls(method, path)`. A test in package X that imports `fake` must be `package X_test` when `fake` imports X |

## Build and run

- `make test`, `make vet` (darwin + linux), `make test-linux` (tests in a linux/amd64 golang container).
- `make image`: linux/amd64 image `projectionist:<git describe>`. The runtime is debian:trixie-slim plus
  `/usr/lib/jellyfin-ffmpeg`, copied from the same Jellyfin image tag the NAS runs.
- `projectionist run <stage> -config FILE` runs one stage once and exits 1 on an alert or an error.
- `compose.yaml`: host network, `/dev/dri/renderD128`, the same paths inside the container as on the host. The
  only writable media mount is `/volume1/media`. Plex AppData and the Plex package are read-only. The
  Jellyfin key comes in as a compose secret.

## Logging conventions

Always pass `"event", "<snake_case>"`, then fields (`item=`, `path=`, `duration_ms=`, `err=`). Never log tokens,
keys or full payloads.

## File-system facts measured on the NAS (DSM 7.4.1, kernel 4.4.302+), 2026-10-05

- renameat2 RENAME_NOREPLACE works on btrfs: EEXIST onto an existing file or directory.
- FICLONE (reflink) works within a share AND across shares. Hardlink and rename across shares fail with EXDEV:
  each share is its own subvolume (`st_dev` differs). Keep work files in the same share as their destination.
- Synology ACL: a plain `mkdir`/create under a folder that has its ACL inherits it. **`chmod` strips it**, `chown`
  keeps it. Never chmod in the library; no `synoacltool` is needed (and it does not exist in the container).
