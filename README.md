# projectionist

A home media server's upkeep, done by one Go service. projectionist runs in one container on a Synology NAS beside
Plex and Jellyfin. It keeps the film and TV library tidy: it names files, finds and times subtitles, picks posters,
builds collections, keeps watch history in step across both apps, and checks the apps' health. Before it, the same work
was a set of Python scripts: DSM's task scheduler ran some of them, and the others ran by hand. Each script is ported as
a stage, checked against the script's own output, and deleted at its cutover. Some stages are new.

The name comes from the person in the booth: a projectionist runs the show and stays out of the audience's way. Here
that is a hard rule. A job that moves or re-encodes files pauses while anyone but the owner is watching.

## The stages

A stage is one job with one interface (`Name()`, `Run(ctx, env, *Report) error`). It must be idempotent: a rerun
after a crash finishes the work and never repeats a side effect. Each stage runs on its own schedule. A stage runs only
when its `[stages.<name>]` table in the config enables it.

| Stage | What it does |
|---|---|
| `watcher` | Watches the library folders with inotify. When a new folder has been quiet for 3 minutes, it triggers the stages that act on it. A 15-minute sweep catches lost events. |
| `rename` | Keeps `Movies/` and `TV/` in one naming scheme. Identity comes from the file names and TMDB, never from Plex or Jellyfin. Every move is one atomic `renameat2(RENAME_NOREPLACE)`, written to an undo log before it happens. |
| `details` | Keeps a small text note beside each video (codec, HDR, audio and subtitle tracks), from ffprobe. |
| `subtitles` | Finds English subtitles for videos that have none. The cheap routes come first: an embedded English track, or a subtitle made for the same release. Next, a subtitle is timed against four short windows of the video. Full alignment ([alass](https://github.com/kaegi/alass)) runs only at night, and only while nobody else is watching. OCR reads subtitle tracks that are only images. |
| `posters` | Picks a poster for each title from TMDB and fanart.tv. A vision model judges the candidates, under a hard spending cap. The owner's own uploads are never replaced. |
| `collections` | Keeps film collections (franchises, directors) the same in Plex and Jellyfin. |
| `lists` | Mirrors film canon lists (for example, Letterboxd's Top 500, the Criterion Collection, Sight and Sound) as collections, in list order. |
| `letterboxd` | Reads Letterboxd diaries into the watch record, and Letterboxd watchlists into each user's Plex watchlist. |
| `jfsync` | Keeps watched state, progress and ratings the same in Plex and Jellyfin. It writes from projectionist's own watch record, which webhooks and one-time imports fill. |
| `tonight` | Each day, gives each person a "Tonight" shelf of five films from the library they have not seen: films over a Letterboxd and Rotten Tomatoes bar (MDBList's ratings), shuffled, or (`mode = "recommend"`) ranked by their taste. No model call. |
| `remote4k` | Makes small 4K copies (hardware HEVC) of the films on a "Remote 4K" list, for streaming away from home. The encode is paused with `SIGSTOP` while someone else watches. |
| `gpu-guard` | Keeps Plex's hardware transcoding on the integrated GPU. It repairs drift, alerts once on each CPU transcode, and runs a GPU self-test once a day when the box is idle. |
| `health` | Probes Plex, Jellyfin and the containers every 5 minutes. It restarts a hung Jellyfin only when nobody is playing on it. When Plex's Movies tab gets slow, it runs Plex's database optimize at night. |
| `audit` | A read-only library audit: names outside the scheme, unreadable videos, orphan sidecar files, identical copies. |
| `appbackup` | Makes nightly versions of the app databases on the NVMe volume: SQLite's online backup, plus hardlinks for unchanged files. |

## Architecture

```
                         Synology NAS (DSM 7.4, Intel J4125 + UHD 600)
 ┌──────────────────────── container "projectionist" (host network) ────────────────────────┐
 │                                                                                          │
 │  scheduler ── one goroutine per stage; cadence + optional window (e.g. 01:00-07:00);     │
 │      │        never two copies of a stage at once                                        │
 │      ▼                                                                                   │
 │  Runner ── StartRun ─► stage.Run(ctx, env, &report) ─► FinishRun(outcome) ─► logs        │
 │      │                       │                                                           │
 │      │     watcher · rename · details · subtitles · posters · collections · lists        │
 │      │     letterboxd · jfsync · tonight · remote4k · gpu-guard · health · audit         │
 │      │     appbackup                                                                     │
 │      │                                                                                   │
 │      │   env: Plex · Jellyfin · TMDB · fanart.tv · LLM clients · viewers.Gate (pause     │
 │      │        rule) · probe (ffprobe cache) · fsx (renameat2, FICLONE, dir fsync)        │
 │      ▼                                                                                   │
 │  SQLite /state/projectionist.db (WAL): runs · kv · events · probe_cache · <stage>_*      │
 │  HTTP on loopback :8097: GET /health (503 on an alert) · GET /metrics (Prometheus)       │
 └──────┬──────────────────┬──────────────────────┬──────────────────────┬──────────────────┘
        │ /dev/dri         │ HTTP, loopback       │ /volume1/media       │ read-only:
        │ renderD128       │                      │ (the only rw mount)  │ Plex app data
        ▼                  ▼                      ▼                      ▼
   GPU (QSV/VAAPI)    Plex · Jellyfin       Movies/ TV/ _archive/    Preferences.xml (token)

   DSM scheduled task: curl -fsS <loopback>:8097/health every 15 min → DSM's failure e-mail
```

The design rules that shape the code:

- **Plex and Jellyfin are sinks, not sources.** Facts come from the files (ffprobe, names, rename logs) and from TMDB.
  The apps receive writes and report who is watching. Both apps stall during library scans, so every call to them has
  a deadline, and a slow app never fails a run.
- **State lives in SQLite.** Every run is a row. A stage that a restart cut off resumes from its own state. The next run
  time comes from the last start time, so a restart does not run a stage early.
- **Measured file system facts.** On the NAS's btrfs, `renameat2(RENAME_NOREPLACE)` and `FICLONE` reflinks work, also
  across shares. `chmod` strips a Synology ACL, but `chown` keeps it. So no stage ever calls chmod. The rename executor
  does one fsync per unit, not one per operation.
- **Port, compare, delete.** A stage replaced a script only when it matched the script's output on a gate. For
  example, the rename planner matched the Python planner on 21,333 of 21,334 recorded rows, and the one difference was
  intended. The old script went away in the same cutover.

[docs/architecture.md](docs/architecture.md) has the full design, each stage's gate and the measurements.
[docs/STAGES.md](docs/STAGES.md) records the status of each stage and its evidence.

## How it runs on the NAS

- **One image, no registry.** `make save` builds a linux/amd64 image and writes a tarball. On the NAS, `docker load`
  reads it, and a Container Manager project runs [compose.yaml](compose.yaml). The image holds the static Go binary
  (about 16 MB, stripped), the same ffmpeg build that Jellyfin uses (with the Intel QSV/VAAPI drivers), alass, and a
  two small helpers on ONNX Runtime: `cmd/subocr` (OCR, a PP-OCR text model) and `cmd/subvad` (speech, Silero VAD).
- **Host network, hardware encode.** The container shares the host network to reach Plex and Jellyfin on loopback.
  `/dev/dri/renderD128` is passed through for QSV encodes.
- **Least privilege.** The service runs as root inside the container, so that files it moves keep their owners. All
  capabilities are dropped except six: CHOWN, DAC_OVERRIDE and FOWNER for the library files, and SETUID, SETGID and
  KILL to run (and stop) Plex's transcoder as the Plex user for the GPU self-test. The root file system is read-only, `no-new-privileges` is set, memory is capped at
  1 GB, and `cpu_shares: 256` lets Plex and Jellyfin win any contention. The media share is the only writable media
  mount.
- **Secrets are files.** API keys arrive as compose secrets under `/run/secrets/` and are read on each request, so a
  rotated key needs no restart. Plex's token is read from its `Preferences.xml` (a read-only mount). No secret is in an
  environment variable, the config, a URL or a log line.
- **Alerts use what was already there.** `/health` answers 503 while any stage has an open alert. A one-line DSM task
  curls it every 15 minutes, and DSM's own failure e-mail is the alert channel. `/metrics` exports runs, alerts, items
  and durations per stage.
- **One stage by hand.** `projectionist run <stage> -config FILE` runs one stage once and exits 1 on an alert or an
  error. `projectionist check -config FILE` validates the config as `serve` would and starts nothing: run it
  before a restart.

## Configure it for your home

Everything that belongs to one household lives in one config file, never in the code: the people and their Plex,
Jellyfin and Letterboxd accounts, the NAS paths, the Plex library ids, the collection and playlist names, the hours
the heavy work may run. To run projectionist at home, copy [config.example.toml](config.example.toml) to
`/config/projectionist.toml`, replace its placeholders (`your-plex-username`, `your-letterboxd-username`,
`media.example.com`, `/volume1/media`, ...), delete the tables of the stages you do not want, and put your API keys in
the secret files compose mounts at `/run/secrets/`. Then build the image (`make image`, or `make save` for a
tarball to `docker load`), run `projectionist check -config /config/projectionist.toml` in it, and start the
Container Manager project. The check names every missing or wrong key as `section.key` with what to set, and `serve`
refuses to start until there are none. [docs/configuration.md](docs/configuration.md) lists every parameter, its
default and an example, the secret files, and a minimal working config. A deployment keeps its own config outside
git; in a checkout, `deploy/nas/` holds a copy and git ignores it.

## Build and test

Requires Go 1.26. Docker is needed only for `test-linux` and the image.

```
make test        # go test ./...
make vet         # go vet for the host and for linux/amd64
make test-linux  # the tests in a golang:1.26 container (the renameat2 and FICLONE paths)
make build       # static linux/amd64 binary in bin/
make image       # the container image (docker buildx, linux/amd64)
make save        # the image as a tarball, for docker load on the NAS
```

The tests use fake Plex, Jellyfin, TMDB and model servers (`internal/fake`, `internal/llm/llmtest`), stand-in ffmpeg
and ffprobe scripts, and a fake clock, so they need no accounts, keys or media. Harnesses that replay real NAS data are
behind build tags (`replay`, `calibrate`, `gate`) and do not run by default.

To add a stage, see [docs/SKELETON_READY.md](docs/SKELETON_READY.md).

## Layout

| Path | Contents |
|---|---|
| `cmd/projectionist` | The service: `serve`, `run <stage>`, `check`, `align-worker` |
| `cmd/subocr` | OCR helper for image subtitle tracks |
| `cmd/subvad` | Speech helper: the stretches Silero VAD hears as speech, for subtitle alignment |
| `cmd/bakeoff` | Compares vision models as poster judges, to pick the one the posters stage uses |
| `internal/stage`, `internal/scheduler`, `internal/store` | The stage interface, the scheduler and the SQLite state |
| `internal/stages/<name>` | One package per stage |
| `internal/plex`, `jellyfin`, `tmdb`, `fanart`, `llm` | The API clients |
| `internal/viewers` | The pause rule |
| `internal/fsx`, `internal/probe` | Atomic file operations and the ffprobe cache |
| `config/` | The collection lists the collections stage reads |
| `testdata/` | The synthetic config the tests share (roles and example.com hosts, no real people) |
| `deploy/nas/` | Not in git: the NAS's own compose file and config, copied after each deploy |

## Licence

MIT. See [LICENSE](LICENSE). The container image also bundles third-party binaries (jellyfin-ffmpeg, alass, ONNX
Runtime and a PP-OCR model), each under its own licence.
