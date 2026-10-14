# Configuration

projectionist reads one TOML file, `/config/projectionist.toml` inside the container (`-config FILE` names another).
Everything that belongs to one household is in that file: the people and their accounts, the NAS paths, the Plex
library ids, the collection and playlist names, the hours the heavy work may run. The code holds no such value.
Secrets are never in the file: it names the files that hold them (see [Secrets and environment](#secrets-and-environment)).

Start from [`config.example.toml`](../config.example.toml): every key is there, with a placeholder or its default
and a one-line comment. This page lists every key with its type, when it is required, its default and an example.

## How to configure it

1. Copy `config.example.toml` to the folder you mount at `/config` and name it `projectionist.toml`.
2. Replace every placeholder (`your-plex-username`, `your-letterboxd-username`, `Your Name`, `media.example.com`,
   `/volume1/media`) with your own values. Delete the `[stages.<name>]` table of each stage you do not want: a stage
   with no table does not run.
3. Check the file before you start the service:

   ```sh
   docker run --rm --entrypoint /usr/local/bin/projectionist \
     -v /volume1/docker/projectionist/config:/config:ro projectionist:latest check -config /config/projectionist.toml
   ```

   It prints `config ok: <file>: <n> stages enabled: <names>` and exits 0, or prints every problem and exits 2.
   It reads only the file: no mounts, secrets or network are needed.
4. Start the service (`compose.yaml`). `serve` and `run` do the same check first and refuse to start on any problem.

## The start-up check

One pass collects every problem, so one edit can fix them all. Each problem names its key as `section.key` and says
what to set:

```text
stages.letterboxd.user: required when stages.letterboxd is enabled; set your Letterboxd username
stages.subtitles.archive_dir: required when stages.subtitles.verify_per_run is above 0; set the folder where sidecars the verify pass replaces are kept (on the media disks)
/config/projectionist.toml: 2 problem(s); serve and run refuse to start until they are fixed
```

A list entry is named `key[n]`, counting from 1: `stages.jfsync.users[2].name` is the name of the second user.
Only enabled stages are checked (and the stage `run STAGE` runs, enabled or not). Keys the code does not know are
ignored, as TOML decoding ignores them.

## Secrets and environment

The service reads no environment variables. Each secret is a file, read on every request, so a rotated key needs no
restart. In `compose.yaml` each one is a Docker secret, mounted at `/run/secrets/<name>`:

| File | Read by | Set by key |
|---|---|---|
| Plex's `Preferences.xml` (its token) | every Plex call | `plex.preferences` |
| `jellyfin_key` | every Jellyfin call | `jellyfin.key_file` |
| `tmdb_read_token`, else `tmdb_api_key` | TMDB | `tmdb.read_token_file`, `tmdb.api_key_file` |
| `fanart_api_key` | fanart.tv | `fanart.api_key_file` |
| `ai_gateway_api_key` | every model call (posters, collections) | `llm.key_file` (a task with its own API: `llm.tasks.<task>.key_file`) |
| `mdblist_api_key` | tonight: each film's Letterboxd and Rotten Tomatoes scores (MDBList) | `stages.tonight.mdblist_key_file` |
| `subsource_api_key`, `subdl_api_key` | subtitles: SubSource, SubDL (a provider without its key is skipped) | in `stages.subtitles.secrets_dir` |
| `opensubtitles_api_key`, `opensubtitles_username`, `opensubtitles_password` | subtitles: OpenSubtitles.com | in `stages.subtitles.secrets_dir` |

The `bakeoff` tool (not the service) reads the gateway key from `AI_GATEWAY_API_KEY` when it is set, else from the
key file.

## Every parameter

Types: a duration is a string such as `"90s"`, `"15m"` or `"24h"`; a window is `"HH:MM-HH:MM"` in `timezone` and may
wrap past midnight. Required: blank means optional. Default: `—` means the key has no default; `none` means empty
(an empty list, an unset string or table, which turns the feature off). Every stage table also takes the scheduler's
three keys (`enabled`, `every`, `window`).

### `top level`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| (top) | `listen` | string |  | "127.0.0.1:8097" | "127.0.0.1:8097" | host network: keep it on 127.0.0.1 or the LAN address |
| (top) | `state_db` | string |  | "/state/projectionist.db" | "/state/projectionist.db" | SQLite file; must sit on a local disk (btrfs volume), never SMB/NFS |
| (top) | `timezone` | string |  | "UTC" | "Europe/London" | only for stage windows; logs and the DB are always UTC |
| (top) | `owners` | list of strings | always | — | ["your-plex-username", "your-jellyfin-username"] | your own Plex and Jellyfin account names: their viewing never pauses a job |

### `[release_sites]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| release_sites | `names` | list of strings |  | none | ["SITE"] | a release's site tag and a subtitle's advert line |
| release_sites | `junk_file_prefixes` | list of strings |  | none | ["site.com"] | a file named so is a site's bundled clip, never the title's video |

### `[log]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| log | `format` | string |  | "text" | "text" | "text" (key=value) or "json" |
| log | `level` | string |  | "info" | "info" | debug, info, warn, error |

### `[plex]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| plex | `url` | string |  | "http://127.0.0.1:32400" | "http://127.0.0.1:32400" | where Plex answers; the service runs on the host network |
| plex | `preferences` | string |  | none | "/volume1/PlexMediaServer/AppData/Plex Media Server/Preferences.xml" | Preferences.xml, mounted read-only; holds the token |
| plex | `timeout` | duration |  | "20s" | "20s" | bound on each Plex request |

### `[jellyfin]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| jellyfin | `url` | string |  | "http://127.0.0.1:8096" | "http://127.0.0.1:8096" | where Jellyfin answers |
| jellyfin | `key_file` | string |  | none | "/run/secrets/jellyfin_key" | API key file, mounted read-only |
| jellyfin | `timeout` | duration |  | "20s" | "20s" | bound on each Jellyfin request |

### `[ffmpeg]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| ffmpeg | `dir` | string |  | "/usr/lib/jellyfin-ffmpeg" | "/usr/lib/jellyfin-ffmpeg" | jellyfin-ffmpeg install: <dir>/ffmpeg, <dir>/ffprobe |
| ffmpeg | `device` | string |  | "/dev/dri/renderD128" | "/dev/dri/renderD128" | render node passed through from the host |

### `[tmdb]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| tmdb | `read_token_file` | string |  | "/run/secrets/tmdb_read_token" | "/run/secrets/tmdb_read_token" | v4 read-access token, preferred |
| tmdb | `api_key_file` | string |  | "/run/secrets/tmdb_api_key" | "/run/secrets/tmdb_api_key" | v3 key, used only when the token file is missing or empty |
| tmdb | `cache_ttl` | duration |  | "24h" | "24h" | how long a TMDB answer is reused from state_db |

### `[fanart]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| fanart | `api_key_file` | string |  | "/run/secrets/fanart_api_key" | "/run/secrets/fanart_api_key" | fanart.tv API key file, read on every request |
| fanart | `cache_ttl` | duration |  | "24h" | "24h" | how long a fanart.tv answer is reused from state_db |

### `[llm]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| llm | `api` | string |  | "https://ai-gateway.vercel.sh/v1" | "https://ai-gateway.vercel.sh/v1" | the AI gateway's OpenAI-compatible base URL |
| llm | `key_file` | string |  | "/run/secrets/ai_gateway_api_key" | "/run/secrets/ai_gateway_api_key" | the gateway's API key file, read on every request |
| llm | `in_flight` | integer |  | 8 | 8 | calls in the air across the process |
| llm | `timeout` | duration |  | "2m" | "2m" | per attempt |
| llm | `attempts` | integer |  | 4 | 4 | per call, the first included |
| llm | `park_after` | integer |  | 5 | 5 | failed calls in a row before a session parks |

### `[llm.tasks.<task>]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| llm.tasks.<task> | `model` | string |  | none | "provider/model-name" | the model a task (posters, collections) calls |
| llm.tasks.<task> | `fallbacks` | list of strings |  | none | ["provider/other-model"] | models the gateway tries, in order, when the first fails |
| llm.tasks.<task> | `reasoning` | string |  | none | "low" | reasoning effort sent with the call ("off", "none", "low", ...) |
| llm.tasks.<task> | `api` | string |  | none | "https://api.example.com/v1" | an API of the task's own instead of the gateway; then price_in and price_out are required |
| llm.tasks.<task> | `key_file` | string |  | none | "/run/secrets/task_api_key" | that API's key file |
| llm.tasks.<task> | `price_in` | number |  | none | 1.0 | USD per million input tokens |
| llm.tasks.<task> | `price_out` | number |  | none | 4.0 | USD per million output tokens |

### `[stages.appbackup]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.appbackup | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.appbackup | `every` | duration | when the stage is enabled |  | "24h" | how often the stage runs |
| stages.appbackup | `window` | string |  | none (any time) | "04:30-06:30" | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.appbackup | `databases` | list of strings | roots or databases, when enabled | none | ["/volume2/apps/jellyfin/jellyfin.db"] | SQLite files copied through the online backup API, never as plain files |
| stages.appbackup | `roots` | list of strings | roots or databases, when enabled | none | ["/volume2/apps"] | folders copied file by file |
| stages.appbackup | `exclude` | list of strings |  | none | ["/volume2/apps/transcode"] | paths never copied, wherever they sit |
| stages.appbackup | `target` | string | when the stage is enabled | — | "/volume1/projectionist/app-backups" | the folder that holds one version per day, on another volume than the roots |
| stages.appbackup | `keep` | integer |  | 7 | 7 | versions kept, today's included |

### `[stages.audit]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.audit | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.audit | `every` | duration | when the stage is enabled |  | "24h" | how often the stage runs |
| stages.audit | `window` | string |  | none (any time) | "04:00-07:00" | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.audit | `root` | string | when the stage is enabled | — | "/volume1/media" | the media share that holds Movies/ and TV/ |
| stages.audit | `report` | string |  | none |  | the one file this stage writes; default <root>/_audit.txt |
| stages.audit | `max_listed` | integer |  | 50 | 50 | examples per category in the report |
| stages.audit | `settle` | duration |  | "15m" | "15m" | a file or folder changed more recently may still be moving |
| stages.audit | `probe_workers` | integer |  | 2 | 2 | concurrent ffprobes on a cache miss |
| stages.audit | `quiet` | list of strings |  | ["missing_english_subs"] | ["missing_english_subs"] | categories whose growth never alerts |

### `[stages.collections]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.collections | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.collections | `every` | duration | when the stage is enabled |  | "6h" | how often the stage runs |
| stages.collections | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.collections | `movies_dir` | string | when the stage is enabled | — | "/volume1/media/Movies" | the folder that holds the films (one folder per film) |
| stages.collections | `owner_file` | string |  | "/config/collections.owner.json" | "/config/collections.owner.json" | your own collections, frozen once from Plex's customizations export (JSON) |
| stages.collections | `poster_dir` | string |  | "/config/collection-posters" | "/config/collection-posters" | <ratingKey>.jpg for each of your collections, copied once from the export |
| stages.collections | `approved_file` | string |  | "/config/collections.approved.json" | "/config/collections.approved.json" | the approved franchise and director collections (JSON) |
| stages.collections | `plex_section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library (the number in its URL: section=N) |
| stages.collections | `sinks` | list of strings |  | ["plex", "jellyfin"] | ["plex", "jellyfin"] | the apps reconciled, concurrently; a slow or down app never holds up the other |
| stages.collections | `canon_directors` | list of strings |  | none | ["Agnès Varda"] | such a director gets a collection at their third film on disk |
| stages.collections | `only` | list of strings |  | none |  | reconcile just these collections; empty means all |
| stages.collections | `dry_run` | boolean |  | false | false | log every write instead of sending it |
| stages.collections | `sink_budget` | duration |  | "20m" | "20m" | each app's share of a run; no request starts after it |
| stages.collections | `call_timeout` | duration |  | "2m" | "2m" | each request to an app, on this stage's own clients |

### `[stages.details]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.details | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.details | `every` | duration | when the stage is enabled |  | "15m" | how often the stage runs |
| stages.details | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.details | `library` | string | when the stage is enabled | — | "/volume1/media" | the media share whose sections hold the videos |
| stages.details | `sections` | list of strings |  | ["Movies", "TV"] | ["Movies", "TV"] | folders under Library that hold the scheme |
| stages.details | `undo_logs` | list of strings |  | none | ["/volume1/media/.projectionist/rename/undo.tsv"] | rename logs, oldest first (TSV): each video's original name for its note; none = no logs |
| stages.details | `max_probes` | integer |  | 400 | 400 | ffprobes per run (files already probed cost nothing) |
| stages.details | `max_peak_scans` | integer |  | 2 | 2 | bitrate peak scans per run |
| stages.details | `max_decode_checks` | integer |  | 60 | 60 | decode checks per run |
| stages.details | `peak_above_kbps` | integer |  | 20000 | 20000 | only files whose average bitrate is above this get a peak scan |
| stages.details | `retry_unreadable` | duration |  | "24h" | "24h" | an unreadable verdict is retried after this long |
| stages.details | `notes_root` | string |  | none |  | when set, notes go into a mirror of the library under it, not beside the videos (dry runs) |
| stages.details | `paths_file` | string |  | none |  | when set, limits the run to the videos it lists, one path per line (gates) |

### `[stages.gpu-guard]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.gpu-guard | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.gpu-guard | `every` | duration | when the stage is enabled |  | "15m" | how often the stage runs |
| stages.gpu-guard | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.gpu-guard | `plex_device` | string | when the stage is enabled | — | "8086:3185@0000:00:02.0" | Plex's id for the GPU it must use, as its HardwareDevicePath preference shows it |
| stages.gpu-guard | `plex_log` | string |  | "/volume1/PlexMediaServer/AppData/Plex Media Server/Logs/Plex Media Server.log" | "/volume1/PlexMediaServer/AppData/Plex Media Server/Logs/Plex Media Server.log" | Plex Media Server.log |
| stages.gpu-guard | `plex_app_data` | string |  | "/volume1/PlexMediaServer/AppData/Plex Media Server" | "/volume1/PlexMediaServer/AppData/Plex Media Server" | for the transcoder's Codecs and VA driver dirs |
| stages.gpu-guard | `plex_transcoder` | string |  | "/volume1/@appstore/PlexMediaServer/Plex Transcoder" | "/volume1/@appstore/PlexMediaServer/Plex Transcoder" | "Plex Transcoder" binary |
| stages.gpu-guard | `plex_uid` | integer | when the stage is enabled | — | 297536 | the uid of Plex's own user (the self-test runs as it, like Plex does) |
| stages.gpu-guard | `plex_gid` | integer | when the stage is enabled | — | 297536 | the gid of Plex's own user |
| stages.gpu-guard | `plex_groups` | list of integers | when the stage is enabled | — | [297536, 937] | Plex's user's groups, including the group that owns the render node |
| stages.gpu-guard | `sample` | string | when the stage is enabled | — | "/volume1/projectionist/gpu-sample.mkv" | a 5 s HEVC 10-bit 1080p clip the Plex user can read |
| stages.gpu-guard | `self_test_every` | duration |  | "24h" | "24h" | how often Plex's transcoder is run on the sample as a GPU self-test |
| stages.gpu-guard | `legacy_state` | string |  | none |  | gpu_guard.state.json; imported once when the store has no state |
| stages.gpu-guard | `plex_timeout` | duration |  | "5s" | "5s" | bound on each Plex API call |

### `[stages.health]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.health | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.health | `every` | duration | when the stage is enabled |  | "5m" | how often the stage runs |
| stages.health | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.health | `timeout` | duration |  | "30s" | "30s" | per probe; Plex and Jellyfin calls also stop at their client's timeout |
| stages.health | `fail_threshold` | integer |  | 3 | 3 | consecutive failed runs before an alert or a restart |
| stages.health | `restart_cooldown` | duration |  | "1h" | "1h" | at most one restart per container per cooldown; at least 1h |
| stages.health | `restartable` | list of strings |  | none | ["jellyfin"] | the only containers this stage may restart |
| stages.health | `docker_socket` | string |  | "/var/run/docker.sock" | "/var/run/docker.sock" | the Docker socket mounted into the container |
| stages.health | `jellyfin_container` | string |  | "jellyfin" | "jellyfin" | the Jellyfin container's name (inspected, and restarted if listed in restartable) |
| stages.health | `qbittorrent_container` | string |  | "qbittorrent" | "qbittorrent" | the qBittorrent container's name (inspected) |
| stages.health | `qbittorrent_url` | string |  | "http://127.0.0.1:8080" | "http://127.0.0.1:8080" | qBittorrent's WebUI; 403 counts as up |
| stages.health | `public` | table (URL = status) |  | none | { "https://media.example.com" = 200 } | URL = the status it must answer (redirects not followed); none = no public probes |
| stages.health | `plex_movies_section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library, whose hub row is timed |
| stages.health | `plex_slow` | duration |  | "5s" | "5s" | a hub load slower than this is slow |
| stages.health | `plex_batch` | integer |  | 500 | 500 | library changes since the last optimize that make one due |
| stages.health | `plex_optimize_window` | string | when the stage is enabled | — | "03:00-06:00" | the hours Plex may optimize its database, HH:MM-HH:MM in the config time zone (when nobody watches) |
| stages.health | `volume2` | string |  | none | "/volume2" | the app-data NVMe volume; "" = no volume checks |
| stages.health | `volume2_max_used` | number |  | 75 | 75 | percent used that alerts |
| stages.health | `volume2_allow` | list of strings | when volume2 is set | none | ["apps", "@eaDir"] | the only top-level entries it may hold |
| stages.health | `backup_max_age` | duration |  | "36h" | "36h" | the appbackup stage's newest version; 0 = no check |

### `[stages.jfsync]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.jfsync | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.jfsync | `every` | duration | when the stage is enabled |  | "5m" | how often the stage runs |
| stages.jfsync | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.jfsync | `users` | list of tables | when the stage is enabled | none | [{ name = "you", plex = "your-plex-username", jellyfin = "your-jellyfin-username" }] | one entry per viewer: name, and their plex and jellyfin account names |
| stages.jfsync | `users[].name` | string | in each entry | — |  | projectionist's name for the viewer, used by the watch record |
| stages.jfsync | `users[].plex` | string |  | none |  | Plex account name (PMS /accounts); "" = no Plex account |
| stages.jfsync | `users[].jellyfin` | string |  | none |  | Jellyfin user name; "" = no Jellyfin user |
| stages.jfsync | `apply` | boolean |  | true | true | false: plan only, no app writes (plan_file shows them) |
| stages.jfsync | `plan_file` | string |  | none |  | optional TSV of the run's planned writes |
| stages.jfsync | `movies_section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library |
| stages.jfsync | `tv_section` | integer | when the stage is enabled | — | 2 | the id of the Plex TV library |
| stages.jfsync | `address_every` | duration |  | "24h" | "24h" | how often each app's item listing is refreshed |
| stages.jfsync | `list_timeout` | duration |  | "30m" | "30m" | a whole-section listing takes minutes on a busy NAS |
| stages.jfsync | `plex_backups` | string | when the stage is enabled | — | "/volume1/projectionist/plex-backups" | the folder of Plex database backups: the newest *.db below it seeds the watch history, once |
| stages.jfsync | `webhook_url` | string |  | "http://127.0.0.1:8097/hooks/plex" | "http://127.0.0.1:8097/hooks/plex" | the webhook registered on your plex.tv account; "" = do not register |
| stages.jfsync | `plex_tv` | string |  | "https://plex.tv" | "https://plex.tv" | plex.tv, where the webhook is registered |

### `[stages.letterboxd]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.letterboxd | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.letterboxd | `every` | duration | when the stage is enabled |  | "30m" | how often the stage runs |
| stages.letterboxd | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.letterboxd | `user` | string | when the stage is enabled | — | "your-letterboxd-username" | your Letterboxd username |
| stages.letterboxd | `letterboxd_url` | string |  | "https://letterboxd.com" | "https://letterboxd.com" | Letterboxd's site |
| stages.letterboxd | `boxd_url` | string |  | "https://boxd.it" | "https://boxd.it" | Letterboxd's short-link host |
| stages.letterboxd | `user_agent` | string |  | "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 (projectionist; personal sync of one member's own public diary)" | "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 (projectionist; personal sync of one member's own public diary)" | the User-Agent sent to Letterboxd |
| stages.letterboxd | `max_fetches` | integer |  | 400 | 400 | Letterboxd requests per run; the backfill resumes next run |
| stages.letterboxd | `watchlist_every` | duration |  | "6h" | "6h" | each member's watchlist grid crawl |
| stages.letterboxd | `pages_every` | duration |  | "24h" | "24h" | how often your watched-films grid is read |
| stages.letterboxd | `export_dir` | string |  | none | "/volume1/projectionist/letterboxd" | holds letterboxd-<user>-<date>-utc/ folders; newest wins; "" = none |
| stages.letterboxd | `movies_dir` | string | when the stage is enabled | — | "/volume1/media/Movies" | the folder that holds the films |
| stages.letterboxd | `tv_root` | string | when plex_watchlist is true | none | "/volume1/media/TV" | its shows, as posters reads them: only for the Plex watchlist |
| stages.letterboxd | `missing_file` | string |  | none | "/volume1/media/_watchlist-missing.txt" | your watchlist films not on the NAS, as a text file; "" = none |
| stages.letterboxd | `sink_refresh` | duration |  | "24h" | "24h" | re-read each Plex watchlist this often (also before writing to it) |
| stages.letterboxd | `person` | string | when the stage is enabled | — | "you" | your name in the watch record: the name of your entry in stages.jfsync.users |
| stages.letterboxd | `history` | boolean |  | false | false | also record the export and the feed's backlog, not just entries logged from now on |
| stages.letterboxd | `plex_watchlist` | boolean |  | false | false | each member's Letterboxd watchlist → their own Plex watchlist (visible per their Plex privacy) |
| stages.letterboxd | `keep_hand_added` | boolean |  | false | false | the Plex watchlist loses only this stage's adds: titles put there by hand stay, on the NAS or not |
| stages.letterboxd | `plex_tv` | string |  | "https://clients.plex.tv" | "https://clients.plex.tv" | plex.tv's clients API (each person's watchlist) |
| stages.letterboxd | `discover_url` | string |  | "https://discover.provider.plex.tv" | "https://discover.provider.plex.tv" | Plex Discover's API (watchlist items) |
| stages.letterboxd | `run_budget` | duration |  | "25m" | "25m" | no plex.tv call starts later than this into a run; keep it under "every" |
| stages.letterboxd | `dry_run` | boolean |  | false | false | count events and writes, record and write nothing (nor the missing file) |
| stages.letterboxd | `people` | list of tables |  | none | [{ person = "partner", user = "their-letterboxd-username", name = "Their Name" }] | more members, synced after you on the same fetch budget |
| stages.letterboxd | `people[].person` | string | in each entry | — |  | their name in the watch record: the name of their entry in stages.jfsync.users |
| stages.letterboxd | `people[].user` | string | in each entry | — |  | their Letterboxd username |
| stages.letterboxd | `people[].plex_user` | string |  | none |  | their Plex Home user: its username, or its title for a managed user (no username); "" = none |
| stages.letterboxd | `people[].export_dir` | string |  | none |  | "" = none; may be the owner's: only folders named for User are read |
| stages.letterboxd | `people[].history` | boolean |  | false | false | as the owner's history |
| stages.letterboxd | `people[].keep_hand_added` | boolean |  | false | false | as the owner's keep_hand_added |
| stages.letterboxd | `people[].name` | string |  | none |  | shown on their own tonight shelf; "" = person, capitalized |

### `[stages.lists]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.lists | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.lists | `every` | duration | when the stage is enabled |  | "24h" | how often the stage runs |
| stages.lists | `window` | string |  | none (any time) | "03:00-08:00" | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.lists | `dry_run` | boolean |  | false | false | read and count everything, write nothing |
| stages.lists | `only` | list of strings |  | none |  | shelve only these lists (by name); empty = all |
| stages.lists | `letterboxd_user` | string | when the stage is enabled | — | "your-letterboxd-username" | your Letterboxd username |
| stages.lists | `letterboxd_url` | string |  | "https://letterboxd.com" | "https://letterboxd.com" | Letterboxd's site |
| stages.lists | `shortlink_url` | string |  | "https://boxd.it" | "https://boxd.it" | Letterboxd's short-link host |
| stages.lists | `user_agent` | string |  | "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 projectionist/1.0 (+personal media server)" | "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36 projectionist/1.0 (+personal media server)" | the User-Agent sent to Letterboxd |
| stages.lists | `canon_refresh` | duration |  | "20h" | "20h" | canon list pages are read again after this |
| stages.lists | `own_refresh` | duration |  | "20h" | "20h" | your lists, films page and diary feed are read again after this |
| stages.lists | `export_dir` | string |  | none | "/volume1/projectionist/letterboxd" | holds your unzipped Letterboxd export; "" = none |
| stages.lists | `own_lists` | boolean |  | true | true | also shelve your own Letterboxd lists |
| stages.lists | `skip_own` | list of strings |  | none |  | your lists to leave out (names or slugs, "*" matches any text); default ["*private*"] |
| stages.lists | `movies_dir` | string | when the stage is enabled | — | "/volume1/media/Movies" | the folder that holds the films |
| stages.lists | `missing_file` | string |  | none | "/volume1/media/_lists-missing.txt" | the shelved lists' films not on the NAS; "" = none |
| stages.lists | `plex_section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library |
| stages.lists | `plex_refresh` | duration |  | "24h" | "24h" | the TMDB id → ratingKey map is read at most this often |
| stages.lists | `plex_budget` | duration |  | "30m" | "30m" | the Plex share of a run; what is left waits for the next run |
| stages.lists | `jellyfin_user` | string | when the stage is enabled | — | "your-jellyfin-username" | your Jellyfin user name, whose library gets the shelves |
| stages.lists | `jellyfin_budget` | duration |  | "15m" | "15m" | the Jellyfin share of a run |
| stages.lists | `home` | list of strings |  | none | ["Top 500"] | lists promoted to your Plex Home, in row order; none = no promotion |
| stages.lists | `home_after` | string |  | "movie.recentlyadded" | "movie.recentlyadded" | the Plex hub the first promoted row follows |
| stages.lists | `canon` | list of tables |  | none | [{ name = "Top 500", url = "https://letterboxd.com/official/list/letterboxds-top-500-films/" }] | the Letterboxd lists to shelve, in shelf order; none = only your own |
| stages.lists | `canon[].name` | string | in each entry | — |  | the shelf's title |
| stages.lists | `canon[].url` | string | in each entry | — |  | the Letterboxd list's address |
| stages.lists | `canon[].limit` | integer |  | none |  | keep the first N entries (a Top 250 is the Top 500 with limit 250) |
| stages.lists | `canon[].poster` | string |  | none |  | an image file to use instead of the generated poster |

### `[stages.posters]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.posters | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.posters | `every` | duration | when the stage is enabled |  | "6h" | how often the stage runs |
| stages.posters | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.posters | `movies_root` | string | when the stage is enabled | — | "/volume1/media/Movies" | the folder that holds the films |
| stages.posters | `tv_root` | string | when the stage is enabled | — | "/volume1/media/TV" | the folder that holds the shows |
| stages.posters | `only` | list of strings |  | none |  | title keys ("movie:tmdb-422", "season:tmdb-1400:2", "collection:scary movie") to limit a run to |
| stages.posters | `dry_run` | boolean |  | false | false | titles: candidates and judge only, no poster uploaded |
| stages.posters | `cache_dir` | string |  | "/state/posters" | "/state/posters" | candidate thumbnails and judge inputs (on the state volume) |
| stages.posters | `owner_items` | string | all three of owner_items, owner_posters, owner_backup_db, or none | none | "/volume1/projectionist/plex-export/items.json" | your Plex customizations export's items.json; "" = no export |
| stages.posters | `owner_posters` | string | all three of owner_items, owner_posters, owner_backup_db, or none | none | "/volume1/projectionist/plex-export/posters" | customizations/posters/<kind>/<ratingKey>.jpg |
| stages.posters | `owner_backup_db` | string | all three of owner_items, owner_posters, owner_backup_db, or none | none | "/volume1/projectionist/plex-export/com.plexapp.plugins.library.db" | the Plex DB .backup taken with that export |
| stages.posters | `max_tokens` | integer |  | 1024 | 1024 | reasoning included |
| stages.posters | `budget_usd` | number |  | 12 | 12 | every judge call ever, not per run |
| stages.posters | `fetch_workers` | integer |  | 3 | 3 | candidate downloads at once |
| stages.posters | `apply_workers` | integer |  | 2 | 2 | poster uploads at once |
| stages.posters | `plex_movie_section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library |
| stages.posters | `plex_tv_section` | integer | when the stage is enabled | — | 2 | the id of the Plex TV library |
| stages.posters | `sink_index_every` | duration |  | "24h" | "24h" | how often each app's item list is fetched |
| stages.posters | `sink_alert_after` | duration |  | "72h" | "72h" | an app whose listing fails this long raises an alert |
| stages.posters | `mark` | string |  | "poster-curator 2026-10" | "poster-curator 2026-10" | EXIF ImageDescription on the images the stage chose |
| stages.posters | `tmdb_images` | string |  | "https://image.tmdb.org/t/p" | "https://image.tmdb.org/t/p" | TMDB's image host |
| stages.posters | `collections_owner_file` | string |  | none |  | the collections stage's owner_file: with both files set, collections get posters too |
| stages.posters | `collections_approved_file` | string |  | none |  | the collections stage's approved_file |
| stages.posters | `collection_dry_run` | boolean |  | true | true | collections: candidates and judge only (default) |
| stages.posters | `collection_budget_usd` | number |  | 5 | 5 | every collection judge call ever, apart from budget_usd |
| stages.posters | `director_min_score` | number |  | 4 | 4 | a director's photo needs this judge score or no poster |
| stages.posters | `commons_api` | string |  | "https://commons.wikimedia.org/w/api.php" | "https://commons.wikimedia.org/w/api.php" | Wikimedia Commons' API (directors' photos) |
| stages.posters | `commons_contact` | string |  | none | "you@example.com" | a URL or e-mail for the User-Agent: Wikimedia then allows 20× the rate |
| stages.posters | `taste_references` | list of strings |  | none |  | title keys of posters you chose, shown to the judge as your taste |
| stages.posters | `taste_references_current` | list of strings |  | none |  | titles whose current Plex poster is your taste: fetched once and cached |
| stages.posters | `criterion` | boolean |  | true | true | a film the Criterion Collection released gets its cover art as a candidate |
| stages.posters | `wikidata_sparql` | string |  | "https://query.wikidata.org/sparql" | "https://query.wikidata.org/sparql" | Wikidata's SPARQL endpoint (Criterion releases) |
| stages.posters | `criterion_sitemap` | string |  | "http://sitemap.criterion.com/films.xml" | "http://sitemap.criterion.com/films.xml" | the Criterion Collection's sitemap |
| stages.posters | `wayback` | string |  | "https://web.archive.org" | "https://web.archive.org" | the Internet Archive, for Criterion cover art |
| stages.posters | `archive_dir` | string |  | none | "/volume1/projectionist/poster-archive" | every candidate a title was judged on and its chosen poster, at full size; "" = no archive |
| stages.posters | `mediux` | string |  | "https://mediux.pro" | "https://mediux.pro" | MediUX's site (poster sets, with collections on); "" = off |
| stages.posters | `mediux_assets` | string |  | "https://api.mediux.pro/assets" | "https://api.mediux.pro/assets" | MediUX's image host |

### `[stages.remote4k]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.remote4k | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.remote4k | `every` | duration | when the stage is enabled |  | "15m" | how often the stage runs |
| stages.remote4k | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.remote4k | `playlist` | string | when the stage is enabled | — | "Remote 4K" | the name of the Plex playlist, Jellyfin playlist or Jellyfin collection that lists the films to shrink |
| stages.remote4k | `label` | string | when the stage is enabled | — | "2160p small" | the version name the small copy gets beside the film (e.g. "4K for remote") |
| stages.remote4k | `small_kbps` | integer |  | 20000 | 20000 | a 4K version at or under this already fits the upload |
| stages.remote4k | `work_dir` | string | when the stage is enabled | — | "/volume1/media/.projectionist/remote4k" | a work folder on the same share as the films (the copy is renamed into place) |
| stages.remote4k | `status_file` | string | when the stage is enabled | — | "/volume1/projectionist/remote4k/STATUS.txt" | the text file where the stage says what it is doing |
| stages.remote4k | `list_timeout` | duration |  | "30s" | "30s" | each app's half of the list is given up after this, each request too |

### `[stages.rename]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.rename | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.rename | `every` | duration | when the stage is enabled |  | "15m" | how often the stage runs |
| stages.rename | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.rename | `root` | string | when the stage is enabled | — | "/volume1/media" | the media share: Movies/, TV/ and _archive/ live there |
| stages.rename | `mode` | string |  | "apply" | "apply" | apply (default) \| plan (write the plan to plan_dir, touch nothing) \| undo |
| stages.rename | `plan_dir` | string | when mode is plan | none | "/volume1/projectionist/rename-plan" | plan mode output: ops.tsv, review.tsv, problems.tsv |
| stages.rename | `undo_log` | string | when the stage is enabled | — | "/volume1/media/.projectionist/rename/undo.tsv" | the undo log the stage writes before each move (TSV), e.g. <root>/.projectionist/rename/undo.tsv |
| stages.rename | `undo_units` | list of strings |  | none |  | undo mode: only these units ([] = everything in the log) |
| stages.rename | `redo_undone` | boolean |  | false | false | apply plans files undo put back (default: left until renamed or moved) |
| stages.rename | `ghost_logs` | list of strings |  | none |  | older rename logs: where moved videos used to live; none = no older logs |
| stages.rename | `review_file` | string | when the stage is enabled | — | "/volume1/media/_needs-review.txt" | the text file that lists what the stage left for you to sort out |
| stages.rename | `aligned_log` | string |  | none |  | an older subtitle aligner's CSV log ("retimed ... -> X.srt"); "" = none |
| stages.rename | `sublang_file` | string |  | none |  | optional JSON {path: {lang, sdh}} from a text classifier |
| stages.rename | `notify` | boolean |  | true | true | tell Jellyfin which folders changed (off for a sandbox run) |
| stages.rename | `quiet` | duration |  | "10m" | "10m" | a folder written to more recently is still arriving |

### `[stages.subtitles]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.subtitles | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.subtitles | `every` | duration | when the stage is enabled |  | "6h" | how often the stage runs |
| stages.subtitles | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.subtitles | `roots` | list of strings | when the stage is enabled | — | ["/volume1/media/Movies", "/volume1/media/TV"] | the folders that hold the films and the shows |
| stages.subtitles | `stats_file` | string |  | "/state/sub_stats.tsv" | "/state/sub_stats.tsv" | the per-title subtitle statistics (TSV) |
| stages.subtitles | `output_dir` | string |  | none |  | test runs: sidecars land here under the same relative paths |
| stages.subtitles | `retry_after` | duration |  | "168h" | "168h" | a title with no English subtitle is searched again after this |
| stages.subtitles | `workers` | integer |  | 3 | 3 | titles worked on at once |
| stages.subtitles | `max_titles` | integer |  | none |  | per run; 0 = every due title |
| stages.subtitles | `secrets_dir` | string |  | "/run/secrets" | "/run/secrets" | where the providers' key files are read |
| stages.subtitles | `alass` | string |  | "/usr/local/bin/alass" | "/usr/local/bin/alass" | the alass binary (in the image) |
| stages.subtitles | `providers` | list of strings |  | ["gestdown", "subsource", "subdl", "opensubtitles-stremio", "opensubtitles", "opensubtitles-com"] | ["gestdown", "subsource", "subdl", "opensubtitles-stremio", "opensubtitles", "opensubtitles-com"] | subtitle sources, in order; keyed ones (subsource, subdl, opensubtitles-com) need their key files |
| stages.subtitles | `film_site` | table |  | none | { name = "filmsite", url = "https://subtitles.example.com" } | a film-only subtitle site, { name, url }; its name is what providers lists |
| stages.subtitles | `film_site.name` | string |  | none |  | the provider name (what providers lists and candidates are stored under) |
| stages.subtitles | `film_site.url` | string |  | none |  | the site's address; pages at /movie-imdb/tt<id> |
| stages.subtitles | `legacy_quota` | string |  | none |  | an older downloader's folder whose quota.csv seeds the OpenSubtitles quota once; "" = none |
| stages.subtitles | `align_per_day` | integer |  | 20 | 20 | queued titles aligned in any 24 h; each reads its video once or twice |
| stages.subtitles | `mode` | string |  | "sampled" | "sampled" | sampled (also the sampled fit; the queue aligns only inside align_window) or careful (release match only) |
| stages.subtitles | `align_window` | string | when mode is sampled (the default) | none | "01:00-07:00" | the hours the queue may read whole videos, HH:MM-HH:MM in timezone (when nobody watches) |
| stages.subtitles | `sample_windows` | integer |  | 4 | 4 | stretches the sampled fit reads, spread over the runtime |
| stages.subtitles | `sample_length` | duration |  | "90s" | "90s" | the length of each sampled stretch |
| stages.subtitles | `undo_logs` | list of strings |  | none | ["/volume1/media/.projectionist/rename/undo.tsv"] | rename logs, oldest first: a video's original release name when its .details note has none; none = no logs |
| stages.subtitles | `align_remote_lanes` | integer |  | none |  | titles leased at once to align workers on another machine (projectionist align-worker); 0 = off |
| stages.subtitles | `verify_per_run` | integer |  | 300 | 300 | videos a run whose English sidecars are not yet verified get them fitted to the windows; 0 = off |
| stages.subtitles | `archive_dir` | string | when verify_per_run > 0 (the default) | none | "/volume1/projectionist/subtitle-archive" | where sidecars the verify pass replaces or refuses are kept; must exist |
| stages.subtitles | `plex_db` | string |  | "/volume1/PlexMediaServer/AppData/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db" | "/volume1/PlexMediaServer/AppData/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db" | Plex's library database, read-only: the item a video with a new sidecar belongs to |

### `[stages.tonight]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.tonight | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.tonight | `every` | duration | when the stage is enabled |  | "30m" | how often the stage runs |
| stages.tonight | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.tonight | `mode` | string |  | "shuffle" | "shuffle" | shuffle: the films over the bars, shuffled; recommend: ranked by each person's taste |
| stages.tonight | `picks` | integer |  | 5 | 5 | films picked each day, per person |
| stages.tonight | `pick_after` | string | when the stage is enabled | — | "05:00" | the local time (HH:MM, config time zone) after which a new day's picks are made |
| stages.tonight | `section` | integer | when the stage is enabled | — | 1 | the id of the Plex Movies library |
| stages.tonight | `playlist` | string | when the stage is enabled | — | "Tonight" | each person's own: this Jellyfin playlist, and the Plex shelf "<playlist> · <name>" |
| stages.tonight | `movies_dir` | string | when the stage is enabled | — | "/volume1/media/Movies" | the folder that holds the films |
| stages.tonight | `image_base` | string |  | "https://image.tmdb.org/t/p" | "https://image.tmdb.org/t/p" | TMDB's image host |
| stages.tonight | `jellyfin_user` | string | when the stage is enabled | — | "your-jellyfin-username" | your Jellyfin user name, whose played flags count |
| stages.tonight | `people` | list of tables | when the stage is enabled | none | [{ person = "you", user = "your-letterboxd-username", plex_user = "your-plex-username", name = "Your Name" }] | one entry per person with a shelf (person, user, and plex_user, export_dir, name as needed) |
| stages.tonight | `people[].person` | string | in each entry | — |  | their name in the watch record: the name of their entry in stages.jfsync.users |
| stages.tonight | `people[].user` | string | in each entry | — |  | their Letterboxd username |
| stages.tonight | `people[].plex_user` | string |  | none |  | their Plex Home user: its username, or its title for a managed user (no username); "" = none |
| stages.tonight | `people[].export_dir` | string |  | none |  | "" = none; may be the owner's: only folders named for User are read |
| stages.tonight | `people[].history` | boolean |  | false | false | as the owner's history |
| stages.tonight | `people[].keep_hand_added` | boolean |  | false | false | as the owner's keep_hand_added |
| stages.tonight | `people[].name` | string |  | none |  | shown on their own tonight shelf; "" = person, capitalized |
| stages.tonight | `plex_tv` | string |  | "https://clients.plex.tv" | "https://clients.plex.tv" | plex.tv's clients API |
| stages.tonight | `letterboxd_url` | string |  | "https://letterboxd.com" | "https://letterboxd.com" | Letterboxd's site |
| stages.tonight | `sink_timeout` | duration |  | "3m" | "3m" | per sink per run; what is left over waits for the next run |
| stages.tonight | `plex_map_timeout` | duration |  | "10m" | "10m" | the once-a-day Movies listing; minutes during a scan |
| stages.tonight | `map_every` | duration |  | "24h" | "24h" | how often the TMDB → Plex/Jellyfin item maps are re-read |
| stages.tonight | `min_letterboxd` | number |  | 3.5 | 3.5 | stars: a pick's Letterboxd average is over this |
| stages.tonight | `min_tomatoes` | integer |  | 75 | 75 | %: and its Rotten Tomatoes critics score, where it has one (none: Letterboxd alone decides) |
| stages.tonight | `mdblist_url` | string |  | "https://api.mdblist.com" | "https://api.mdblist.com" | where the ratings come from, 200 films a request |
| stages.tonight | `mdblist_key_file` | string |  | "/run/secrets/mdblist_api_key" | "/run/secrets/mdblist_api_key" | an MDBList API key (free: 1,000 requests a day) |
| stages.tonight | `ratings_every` | duration |  | "720h" | "720h" | a film's ratings are asked for again once this old |

### `[stages.watcher]`

| Section | Key | Type | Required | Default | Example | What it does |
|---|---|---|---|---|---|---|
| stages.watcher | `enabled` | boolean |  | true | true | false turns the stage off; a stage with no table is off |
| stages.watcher | `every` | duration | when the stage is enabled |  | "1m" | how often the stage runs |
| stages.watcher | `window` | string |  | none (any time) |  | HH:MM-HH:MM in timezone, may wrap past midnight: the stage starts only inside it |
| stages.watcher | `roots` | list of strings | when the stage is enabled | — | ["/volume1/media/Movies", "/volume1/media/TV"] | the folders to watch (the films, the shows, the finished downloads) |
| stages.watcher | `trigger` | list of strings |  | ["rename", "details", "subtitles", "posters", "collections", "jfsync"] | ["rename", "details", "subtitles", "posters", "collections", "jfsync"] | stages to trigger for each quiet, changed unit |
| stages.watcher | `quiet` | duration |  | "3m" | "3m" | a changed folder is acted on once it has been quiet this long |
| stages.watcher | `sweep` | duration |  | "15m" | "15m" | length of one run = the safety sweep's cadence |
| stages.watcher | `ignore` | list of strings |  | ["@eaDir", "#recycle", "#snapshot", "_archive", "_assets"] | ["@eaDir", "#recycle", "#snapshot", "_archive", "_assets"] | names neither watched nor counted, at any depth |
| stages.watcher | `partial_suffixes` | list of strings |  | [".part", ".!qB"] | [".part", ".!qB"] | in-progress files: busy, never a change on their own |

## Minimal working example

The smallest useful config: two stages that only read the library and write their own notes and report. It passes
the start-up check (a test proves it), and every other stage stays off.

```toml
owners = ["your-plex-username", "your-jellyfin-username"]

[plex]
preferences = "/volume1/PlexMediaServer/AppData/Plex Media Server/Preferences.xml"

[jellyfin]
key_file = "/run/secrets/jellyfin_key"

[stages.audit]
every = "24h"
root  = "/volume1/media"

[stages.details]
every   = "1h"
library = "/volume1/media"
```
