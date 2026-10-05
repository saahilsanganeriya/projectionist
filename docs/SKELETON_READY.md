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
	FFmpeg   config.FFmpeg // .FFmpegPath(), .FFprobePath(), .Device
	Now      func() time.Time // UTC
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
| `internal/store` | SQLite (WAL): `StartRun/FinishRun/LastRun`, `GetJSON/PutJSON(stage, key)`, `AddEvent/LastEvent(stage, item)`, `DB()` for your own tables. Name them `<stage>_*` and create them with `CREATE TABLE IF NOT EXISTS` |
| `internal/scheduler` | per-stage cadence + window, `Runner.RunOnce` (also used by `projectionist run <stage>`) |
| `internal/viewers` | the pause rule: `Gate.Count`, `Gate.WaitIdle(ctx)` before moving files, `Gate.Watch(ctx, func(busy bool))` to SIGSTOP/SIGCONT a long process. Owners never block; an error counts as busy |
| `internal/plex` | `ReadPrefs(path)` (file, no API), `Sessions`, `GetJSON` (unwraps MediaContainer), `Do(method, path)`. Token read from Preferences.xml on every request and sent as a header. Use the API only where no file source exists |
| `internal/jellyfin` | `GetJSON`, `PostJSON`, `Sessions(activeWithin)`, `MediaUpdated(paths...)`. Key read from a file on every request |
| `internal/probe` | `Prober.Probe(ctx, path) (Info, error)`: ffprobe JSON → codec, profile, bit depth, HDR10/HLG/Dolby Vision + dv_profile, size, fps, bitrate, audio and subtitle tracks, duration. Cached in `probe_cache` keyed by (path, size, mtime_ns). `Parse(raw)` is pure, for tests |
| `internal/library` | `ParseMovie(path)` / `ParseEpisode(path)`: identity (`{tmdb-N}`, `{imdb-ttN}`, `{tvdb-N}`), year, label, season, episode range, read from the naming scheme |
| `internal/fsx` | `RenameNoReplace` (renameat2 RENAME_NOREPLACE; link+unlink fallback for files), `Clone` (FICLONE reflink, byte-copy fallback, never overwrites), `SyncDir` (one fsync per unit) |
| `internal/obs` | `NewLogger` (UTC, key=value or JSON), `Metrics` (Prometheus text) |
| `internal/server` | `/health` (503 while any stage's latest run is alert/error) and `/metrics` |
| `internal/fake` | `fake.Plex(t, prefs)` → (server, client, prefsPath); `fake.Jellyfin(t)`; `srv.Handle(method, path, status, body)`, `srv.Calls(method, path)` |

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
