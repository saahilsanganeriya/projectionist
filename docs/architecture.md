# projectionist — the NAS media pipeline as one service

Today the pipeline is a set of Python 3.8 scripts on the DS423+. DSM Task Scheduler runs some of them, and the
owner runs the others by hand. projectionist replaces them, one at a time, with **one Go service in one
container**. The service runs each step ("stage") on its own schedule, keeps its state in SQLite, and is
observable over HTTP. A projectionist runs the show from the booth and stays out of the audience's way. Here,
that means every disruptive job pauses while anyone but the owner is watching.

```
                          DS423+ (DSM 7.4.1, kernel 4.4, J4125 + UHD 600)
 ┌──────────────────────────── container "projectionist" (host network) ───────────────────────────┐
 │                                                                                                 │
 │  scheduler ── one goroutine per stage; cadence + optional window ("02:00-08:00" Pacific);      │
 │      │        never two copies of a stage at once                                               │
 │      ▼                                                                                          │
 │  Runner ── StartRun ─► stage.Run(ctx, env, &report) ─► FinishRun(outcome) ─► logs + metrics    │
 │      │                       │                                                                  │
 │      │            ┌──────────┼──────────────┬───────────────┬──────────────┐                    │
 │      │            ▼          ▼              ▼               ▼              ▼                    │
 │      │        gpu-guard   remote4k    details*   subtitles*   rename   ingest*  jf-sync*  ssd*  │
 │      │            │          │                                                                  │
 │      │   env: Plex client · Jellyfin client · viewers.Gate (pause rule) · probe (ffprobe cache)│
 │      │        fsx (renameat2 NOREPLACE, FICLONE, dir fsync) · store · FFmpeg paths              │
 │      ▼                                                                                          │
 │  SQLite /state/projectionist.db (WAL): runs · kv · events · probe_cache · <stage>_* tables      │
 │  HTTP 127.0.0.1:8097: GET /health (503 on alert/error) · GET /metrics (Prometheus text)         │
 └──────┬──────────────────┬─────────────────────┬───────────────────────┬─────────────────────────┘
        │ /dev/dri/        │ http 127.0.0.1      │ /volume1/media   │ ro: Plex AppData,
        │ renderD128       │ :32400  :8096       │ (only rw media mount) │ Plex package, scripts/
        ▼                  ▼                     ▼                       ▼
   GPU (QSV/VAAPI)    Plex · Jellyfin      Movies/ TV/ _archive/    Preferences.xml (token),
                                            .projectionist/ (work)   Plex log, transcoder
   DSM task "projectionist health": curl -fsS http://127.0.0.1:8097/health  → DSM failure e-mail
                                                                    (* = stage not ported yet)
```

## The pieces

| Concern | Design | Code |
|---|---|---|
| Stages | One interface: `Name()`, `Run(ctx, env, *Report) error`. A stage must be idempotent: a rerun after a crash finishes the work and never repeats a side effect. A stage stops when ctx is cancelled. `Report.Alert` and `Report.Count` give the outcome and the metrics. | `internal/stage` |
| Scheduler | Per stage: `every`, optional `window` in the config time zone (Pacific; it may wrap past midnight). The next run is computed from the last run's start time read from SQLite, so a restart does not rerun a stage early. Each stage has its own goroutine, so a 6 h encode delays only itself. | `internal/scheduler` |
| Trigger | `Env.Trigger(name)` runs a stage soon (inside its window); triggers that arrive while it runs or waits give exactly one more run. The `watcher` stage calls it: inotify on the library and finished downloads, a top-level folder that stayed quiet for 3 min triggers the reconcilers once, and a 15 min mtime sweep catches lost events. Plex and Jellyfin are not involved. | `internal/scheduler`, `internal/stages/watcher` |
| State | SQLite in WAL mode with `synchronous=NORMAL`: at most one fsync per transaction commit. Shared tables: `runs`, `kv` (per-stage JSON state), `events` (per-item history), `probe_cache`. A stage owns its own `<stage>_*` tables. Pure Go driver (modernc), so the binary stays static. | `internal/store` |
| Resume | Every run is a row. A row without `finished_at` means the process died mid-run, and `/health` shows that run as "interrupted". The stage resumes from its own state, e.g. remote4k redoes the film because no copy exists yet. | |
| Logs | `log/slog` on stderr, key=value (or JSON), UTC RFC 3339 timestamps, always `event=<snake_case>` plus fields (`stage=`, `item=`, `duration_ms=`, `err=`). Docker's json-file driver rotates them at 10 MB × 5. | `internal/obs` |
| Health | `/health` answers 503 while any stage's latest run raised an alert or failed. A one-line DSM task (`curl -fsS .../health`, every 15 min, "e-mail on failure") keeps DSM's existing e-mail as the alert channel, with no new credentials. `/metrics` exports runs, alerts, items, last-run time and duration per stage. | `internal/server` |
| Secrets | Never in env vars and never in the config. The Plex token is read from `Preferences.xml` (read-only mount) on every request and sent as a header, never in a URL. The Jellyfin key, the TMDB read token and v3 key, and the fanart.tv key arrive as compose secrets under `/run/secrets/` (read-only) and are read per request; the TMDB token and the fanart.tv key travel in headers. A rotated secret therefore needs no restart, and nothing logs it. | `internal/plex`, `internal/jellyfin`, `internal/tmdb`, `internal/fanart` |
| Sources | Facts come from the files: ffprobe, folder and file names, the rename logs. Identity, episode lists, specials, collections, external ids and images come from TMDB v3 directly (one client per process: 40 req/s token bucket, 10 s timeout, 429/5xx retried twice, 24 h response cache in `http_cache`), artwork also from fanart.tv. Subtitles come from subtitle providers. | `internal/probe`, `internal/library`, `internal/tmdb`, `internal/fanart` |
| Plex and Jellyfin | Sinks only; both APIs stall during library scans. Allowed: writes of state that lives in the app (preferences, posters, collections, watched state), mapping a TMDB id to the app's item id, the viewer gate's live sessions, the webhooks/events the apps push, and lists the owner keeps there as requests (remote4k's "Remote 4K"). Every call is time-bounded and a slow app never fails a run. Never a data source: no Jellyfin RemoteSearch/RemoteImages/ProviderIds, no Plex metadata or subtitle search, no live app DB, no rescan requests (Plex watches the disk; Jellyfin gets one `/Library/Media/Updated`). | `internal/plex`, `internal/jellyfin` |
| ffmpeg | The image carries `/usr/lib/jellyfin-ffmpeg` (8.1.2-Jellyfin, with the iHD/i965 VA drivers), copied from the same Jellyfin image tag the NAS runs. That is the same build Jellyfin itself uses. `/dev/dri/renderD128` is passed through, and the container joins group 937 (videodriver). Encodes use QSV (`-init_hw_device vaapi=va:/dev/dri/renderD128 -init_hw_device qsv=qs@va`, `hevc_qsv`/`h264_qsv`). The GPU self-test also runs Plex's own transcoder (read-only mount) as the Plex user, via VAAPI. | Dockerfile, gpu-guard |
| Pause rule | `viewers.Gate`: count Plex and Jellyfin sessions not owned by `owners` (the owner's viewing never blocks a job). Use `WaitIdle` before moving files and poll `Count` to SIGSTOP/SIGCONT a long encode. If the count cannot be read, the gate treats it as busy. | `internal/viewers` |
| Least privilege | Root inside the container, because files must keep their owners. All capabilities are dropped except CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETUID and SETGID. Also: `no-new-privileges`, read-only root filesystem, 1 GB memory cap, `cpu_shares: 256` (Plex and Jellyfin win any contention). The only writable media mount is `/volume1/media`. | compose.yaml |

## File-level operations (measured on this NAS, 2026-10-05)

A throwaway probe in scratch folders of two non-media shares (deleted afterwards) gave these results:

| Operation | Result on DSM kernel 4.4.302+, btrfs | Consequence |
|---|---|---|
| `renameat2(RENAME_NOREPLACE)` | Works: `EEXIST` onto an existing file or folder, ok otherwise. It also works on ext4. | Every library move is one atomic syscall that cannot overwrite (`fsx.RenameNoReplace`). The old check-then-rename race is gone. A `link`+`unlink` fallback exists for filesystems without the flag. |
| `FICLONE` reflink | Works within a share and **across shares**. Fails with `EXDEV` onto ext4 `/tmp`. | Copies on the volume are instant and use no space (`fsx.Clone`, with a byte-copy fallback). |
| hardlink / rename across shares | `EXDEV` (each share is its own btrfs subvolume: `st_dev` 42/40/43). Within a share, it works. | Hardlink ingest works only if qBittorrent downloads into the `Plex Media` share. From another share, ingest must use a reflink clone instead (just as instant, and the copy is independent of the torrent's file). Work directories live in the destination's share. remote4k publishes with one rename from `Plex Media/.projectionist/remote4k`. |
| Synology ACL | A plain `mkdir` or create under a folder that has its ACL **inherits it**. **`chmod` strips it**; `chown` keeps it. | This is the root cause of the pilot bug ("Plex marked items deleted"). The executor did `chown`+`chmod` on each new folder, then repaired the damage with one `synoacltool` subprocess per folder. The Go stages never chmod, so they need no ACL work and no `synoacltool`, which the container does not have anyway. |
| Durability | | **One fsync per unit, not per op.** The rename executor appends all intended ops of a unit to its undo log (`undo.tsv`, apply_ops.py's TSV format) and fsyncs it once (the "log before acting" rule). It then performs the renames, fsyncs each folder that unit touched once (`fsx.SyncDir`), and appends `unit-done`. A unit stopped half-way is finished by the next run, which re-plans from the disk (src gone and dst present means done). Undo logs its `undone` rows the same way, one fsync per unit. The old executor's ~18 ops/s came from a per-op fsync of `undo.tsv` plus a `synoacltool` exec per new folder. Both costs disappear here. |

## Where Rust would earn its place: not yet

Every stage so far is bound by I/O or subprocesses: ffmpeg/ffprobe, HTTP (TMDB, Plex, Jellyfin), renames on a RAID5
volume. Go reaches the needed syscalls directly (`renameat2`, `FICLONE`, `setpriority` through `x/sys`) and
ships as a single 12 MB static binary (stripped). The CPU-heavy candidates are small:
- subtitle alignment: cue-time fitting over 7 frame-rate ratios, milliseconds per title;
- hashing for deduplication: Go's SHA-256 uses the J4125's SHA extensions.

Rust earns a crate when a stage needs a library only Rust has, or when a profile on the J4125 shows a ≥2× wall-time
win on a stage's critical path. One example: voice-activity-based subtitle sync (what ffsubsync does) without
spawning Python. It would ship as a static binary that the Go stage execs, measured before and after.

## Migration: one script at a time, deleted at its cutover

A Go stage replaces a Python script only when it matches that script on the gate below. **The old script and its
DSM task are deleted in the same cutover**, so there is never a window where both act. The same cutover deploys the
image, enables the stage's `[stages.<name>]` table, and imports any legacy state (gpu-guard reads
`gpu_guard.state.json` once, so the first run does not re-alert on old log lines).

| # | Stage | Replaces | Acceptance gate |
|---|---|---|---|
| 1 | gpu-guard | `scripts/gpu_guard.py`, DSM task 8 | **Ported.** Table tests on every decision, plus a fake-server run: drift repaired, each transcode alerted once, log offset kept across rotation, self-test only when idle and once a day, a hung Plex costs 5 s per call and only a pin that cannot be written back alerts ("GPU pin lost"). On the NAS: `projectionist run gpu-guard` gives the same alert set as the Python run in the same window (none expected), and both GPU self-tests pass (>100 frames), also with Plex unreachable. |
| 2 | remote4k | `scripts/remote4k.py`, DSM task 9 | **Ported.** Requests are the union of the Plex playlist and Jellyfin playlists/collections named "Remote 4K"; source facts come from ffprobe only. Table tests (selection, encode arguments) and an end-to-end fake run (real SIGSTOP/SIGCONT, DV5 skip, union deduped, no Plex rescan, rerun does nothing, either app hung leaves its half empty). On the NAS: an empty list does nothing (also with Plex unreachable); for one playlist film, the Go ffmpeg argument list equals the Python's, the copy's ffprobe colour tags and HDR10 match, and a guest session pauses the encode. |
| 3 | details | `rename/write_details.py` | For every video, the `.details` text equals the Python output on every field from the rename log. Probe-derived fields (codec, bit depth, HDR, audio, subtitles) agree on ≥99.5 %, and every difference is listed with its cause (Plex analysis vs ffprobe). |
| 4 | subtitles | `scripts/sub_stats.py`, `scripts/subfetch2.py` | `sub_stats` TSV byte-identical on the full library. subfetch: identical keep/reject decisions and offsets on a 50-title sample replayed from recorded Plex search responses. Never overwrites (`O_EXCL`). |
| 5 | rename planner + executor | `rename_engine.py`, `apply_ops.py` | **Ported** (also replaces autorename's matching). Identity from the names and TMDB directly (a tag in the name, then TMDB search under the acceptance rule, then `_needs-review.txt`); never from Plex, and never from the library's own folder names. Port check: on the recorded input TSVs, op/src/dst/reason equal the Python's for 21333/21334 rows; the other is intended: a subtitle of a special left for review stays beside it, where the Python archived it. A TMDB failure (identity, episode list or show details) leaves the show's unnumbered videos in place for the next run. History (`undo.tsv`, 13085 moves; original path in, identity from each destination's `{tmdb-N}` folder): 13084 byte-identical with the episode titles the Python planner had (the other row moved *Ad Astra* back out of `_archive/samples`, where a plan from before the is_main guard had put it); 13005 (99.389 %) with TMDB's episode titles, which production reads. The 79 extra differences are only TMDB's spelling of the episode title: 37 case or punctuation, 35 other words, 7 placeholder titles the planner drops. Episodes already named keep their own titles, so this renames nothing already in the library; it decides how new arrivals are spelled. Subtitle facts equal `sub_stats.py` on 3459/3459 real files. NAS sandbox (20 real folders, sparse files): apply; a second apply changes nothing; undo restores the tree exactly; a second undo writes nothing; apply after undo leaves what undo put back. Replay harness: `internal/stages/rename/replay_test.go` (`-tags replay`). |
| 6 | autorename | `rename/autorename.py` | Folded into rename: a new arrival is renamed once its folder has been quiet for `quiet` (10 min); an unmatched one goes to `_needs-review.txt` with TMDB's top 3 candidates. Its `.details` and subtitle queue belong to the details and subtitles stages, which read the disk. |
| 7 | ingest | qBittorrent hook (in progress) | Hardlink within the `Plex Media` share, reflink across shares. Same bytes and inode/extent sharing verified, zero extra space used, the torrent keeps seeding. |
| 8 | jf-sync | `jellyfin/jf_mirror.py` | Watch state for both users equals `plex_watch.tsv`. Collections match `collections.json`. A second run changes nothing. |
| 9 | ssdmirror | `ssd-copy/fill.py` | **Ported.** On the NAS, read-only: the dry run plans the same 640 films A→Z as fill.py's own planning code on the same free space. In scratch: SIGKILL mid-file, and the rerun resumes from the partial (same bytes, no second file). Copies run at the idle I/O class; `io_class = "best-effort"` gives fill.py's nice 19 instead. |
| 10 | backups | `backup_plex_db.sh`, `export_customizations.py` | `PRAGMA integrity_check` = ok on the copy. The exported file set equals the Python's. |
| 11 | posters | `posters/posters.py` (runs on the Mac) | Last. It needs AI gateway keys, which would become a compose secret. Replay of `decisions.json` gives identical choices. |

Order: small and independent stages first, so the pattern is proven before the rename stages. The shared
probe/details work comes before the rename planner, because the planner consumes subtitle facts. The rename
executor carries the most risk, so it waits until everything around it is in Go.

## Build, ship, run

```
make test vet test-linux     # macOS + Linux; Linux exercises renameat2/FICLONE
make image                   # linux/amd64; Go cross-compiles natively, the runtime stage is emulated
make save                    # bin/projectionist-<ver>.tar.gz → NAS: docker load; Container Manager project = compose.yaml
projectionist run <stage>    # one run on the NAS (exit 1 on alert or error), for the gates above
```
