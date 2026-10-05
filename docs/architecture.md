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
 │      │        gpu-guard   remote4k    details*   subtitles*   rename*  ingest*  jf-sync*  ssd*  │
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
| State | SQLite in WAL mode with `synchronous=NORMAL`: at most one fsync per transaction commit. Shared tables: `runs`, `kv` (per-stage JSON state), `events` (per-item history), `probe_cache`. A stage owns its own `<stage>_*` tables. Pure Go driver (modernc), so the binary stays static. | `internal/store` |
| Resume | Every run is a row. A row without `finished_at` means the process died mid-run, and `/health` shows that run as "interrupted". The stage resumes from its own state, e.g. remote4k redoes the film because no copy exists yet. | |
| Logs | `log/slog` on stderr, key=value (or JSON), UTC RFC 3339 timestamps, always `event=<snake_case>` plus fields (`stage=`, `item=`, `duration_ms=`, `err=`). Docker's json-file driver rotates them at 10 MB × 5. | `internal/obs` |
| Health | `/health` answers 503 while any stage's latest run raised an alert or failed. A one-line DSM task (`curl -fsS .../health`, every 15 min, "e-mail on failure") keeps DSM's existing e-mail as the alert channel, with no new credentials. `/metrics` exports runs, alerts, items, last-run time and duration per stage. | `internal/server` |
| Secrets | Never in env vars and never in the config. The Plex token is read from `Preferences.xml` (read-only mount) on every request and sent as a header, never in a URL. The Jellyfin key arrives as a compose secret at `/run/secrets/jellyfin_key` (read-only) and is read per request. A rotated secret therefore needs no restart, and nothing logs it. | `internal/plex`, `internal/jellyfin` |
| Plex API | Plex's API shares one SQLite database with its scanner and times out during scans. Use it only where no file source exists: live sessions, preference writes, scan requests, and playlist membership (the owner's "Remote 4K" list exists only in Plex). Settings come from `Preferences.xml`, technical facts from ffprobe, and identity from the folder names (`{tmdb-N}`). | `internal/library`, `internal/probe` |
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
| Durability | | **One fsync per unit, not per op.** The rename executor writes all intended moves of a unit in one SQLite transaction (the "log before acting" rule, one commit). It then performs the renames, fsyncs each touched directory once (`fsx.SyncDir`), and commits `unit-done`. On restart, a unit with intents but no `unit-done` is resolved per op from the filesystem: src present and dst absent means not done, the reverse means done. The old executor's ~18 ops/s came from a per-op fsync of `undo.tsv` plus a `synoacltool` exec per new folder. Both costs disappear here. |

## Where Rust would earn its place: not yet

Every stage so far is bound by I/O or subprocesses: ffmpeg/ffprobe, Plex and Jellyfin HTTP, renames on a RAID5
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
| 1 | gpu-guard | `scripts/gpu_guard.py`, DSM task 8 | **Ported.** Table tests on every decision, plus a fake-server run: drift repaired, each transcode alerted once, log offset kept across rotation, self-test only when idle and once a day. On the NAS: `projectionist run gpu-guard` gives the same alert set as the Python run in the same window (none expected), and both GPU self-tests pass (>100 frames). |
| 2 | remote4k | `scripts/remote4k.py`, DSM task 9 | **Ported.** Table tests (selection, encode arguments, quoting) and an end-to-end fake run (real SIGSTOP/SIGCONT, DV5 skip, rerun does nothing). On the NAS: for one playlist film, the Go ffmpeg argument list equals the Python's, the copy's ffprobe colour tags and HDR10 match, and a guest session pauses the encode. |
| 3 | details | `rename/write_details.py` | For every video, the `.details` text equals the Python output on every field from the rename log. Probe-derived fields (codec, bit depth, HDR, audio, subtitles) agree on ≥99.5 %, and every difference is listed with its cause (Plex analysis vs ffprobe). |
| 4 | subtitles | `scripts/sub_stats.py`, `scripts/subfetch2.py` | `sub_stats` TSV byte-identical on the full library. subfetch: identical keep/reject decisions and offsets on a 50-title sample replayed from recorded Plex search responses. Never overwrites (`O_EXCL`). |
| 5 | rename planner + executor | `rename_engine.py`, `apply_ops.py` | Planner: **byte-identical `ops.tsv` on the full library listing** (same input TSVs). Executor: sandbox run + undo restores 89/89 like the Python did; `undo.tsv` stays readable for the old undo path; then a 12-unit pilot with the fingerprint compare showing no lost customizations; throughput measured against 18 ops/s. |
| 6 | autorename | `rename/autorename.py` | GOAL item 7: one dropped folder is renamed, gets `.details` and a subtitle queue entry, and both servers rescan. An unmatched folder goes to `_needs-review.txt`. |
| 7 | ingest | qBittorrent hook (in progress) | Hardlink within the `Plex Media` share, reflink across shares. Same bytes and inode/extent sharing verified, zero extra space used, the torrent keeps seeding. |
| 8 | jfsync | `jellyfin/jf_mirror.py` (watch half; collections and posters are their own stages) | Watch state is projectionist's own record (`internal/watch`: Plex webhook, Jellyfin Webhook plugin, a one-time import of the newest Plex DB backup copy and of Jellyfin's played items), written to both apps, last writer wins. On the NAS: dry-run counts per user and direction, an apply for the owner, a second run writes nothing, items spot-checked in both apps. |
| 9 | ssd-mirror | `ssd-copy/fill.py` | The same A→Z plan for the same free space. Resumes after a kill. Runs at idle I/O priority. |
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
