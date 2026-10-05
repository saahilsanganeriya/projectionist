// Package subtitles keeps an English text subtitle beside every movie and episode (port of scripts/sub_stats.py and
// scripts/subfetch2.py, made cheap-first). It is a reconciler: each run walks the library and, for each video still
// lacking an English sidecar, does only the missing step, and reads the video only where nothing cheaper will do.
//
//	embedded English text track       -> skipped: the players show it, so nothing is fetched and nothing read
//	else candidates from the providers (Gestdown, SITEC, Subdl with a key; OpenSubtitles legacy and .com last), best
//	first, skipping any already rejected for this file:
//	  release-matched                  -> written WITHOUT reading the video once cheapCheck passes: an OpenSubtitles
//	                                      file-hash match, or a subtitle naming the file's original release group
//	  quick mode (careful is the default), also:
//	    any plausible candidate        -> written WITHOUT reading the video once cheapCheck passes, release matches
//	                                      first, then by rank: labelled with this title and naming no other episode
//	                                      or edition; a special only when its original name carries its S00Exx
//	    an English VobSub sidecar      -> the check is a fit to <video>.en.idx's caption starts (its text only), not
//	                                      cheapCheck; the subtitle is written retimed onto them
//	  otherwise                        -> the alignment queue
//	the queue, while nobody but the owner watches, at most align_per_day titles in any 24 h (quick mode: only inside
//	align_window, the night, and a run outside it asks the scheduler for one at its opening), best first, each candidate
//	downloaded only once the one before it failed:
//	  English PGS/VobSub track         -> cue starts fitted to its caption starts (that stream alone demuxed); one
//	                                      that does not fit is rescued if alass's alignment to the speech does
//	  else                             -> alass aligns the candidate to the speech in the audio
//	accepted                           -> <video>.en.srt, else .eng.srt: created O_EXCL, never overwritten
//
// OpenSubtitles is searched only once no other provider's release-matched subtitle passed, and only its hash match is
// used, unless no other provider has any candidate (then the queue may try its others); while it is out it holds back
// only a title no other provider has a candidate for. In careful mode nothing is written unverified: a queued title
// waits until it is aligned. Quick mode writes a plausible candidate whose timing nothing checked beyond its length and
// stated frame rate, and that file is final: never overwritten, and its video no longer lacks a subtitle, so no later
// run looks at it again. The duration, frame rate and tracks come from the probe cache, else the .details note (ffprobe
// runs only for a video that has neither); the original release name from that note, else the rename logs. Three titles
// at a time look for a release match; the queue aligns one at a time, and an alignment counts against align_per_day
// only once it is about to read the video. While anyone but the owner is watching no title starts, and every running
// ffmpeg/alass is stopped. Every attempt is an event, each verdict with the bytes read from the video; a title that got
// no subtitle is tried again after retry_after, then after 2, 4 and 8 times it, and a queued title is searched again
// for a release match on the same schedule. A title is recorded only once TMDB and every provider it needs answered
// (and alass ran, where it is needed); else it is deferred, not recorded, and an alert names the cause (a spent
// download quota is routine and only logged). Each run also refreshes sub_stats.py's TSV over the whole library.
// Jellyfin hears of each new sidecar through a queue kept in the store; Plex watches the disk.
package subtitles

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fsx"
	"projectionist/internal/library"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const (
	name       = "subtitles"
	userAgent  = "projectionist v0.1"
	maxTries   = 4    // candidates downloaded per title, on each route
	alassAgree = 0.75 // alass: share of cues that must sit within 0.5 s of the median shift (6 of 8 chunks in align)
	rescueMax  = 300  // ms: a rescued subtitle's median distance to the captions (the gate's bar)
	// A release match cheapCheck refused waits this long in http_cache for its alignment, so it is downloaded once.
	keptFor = 90 * 24 * time.Hour
	keptAs  = "subtitles:"
)

type Options struct {
	Roots      []string      `toml:"roots"`
	StatsFile  string        `toml:"stats_file"`
	OutputDir  string        `toml:"output_dir"` // test runs: sidecars land here under the same relative paths
	RetryAfter time.Duration `toml:"retry_after"`
	Workers    int           `toml:"workers"`
	MaxTitles  int           `toml:"max_titles"` // per run; 0 = every due title
	SecretsDir string        `toml:"secrets_dir"`
	Alass      string        `toml:"alass"`
	Providers  []string      `toml:"providers"` // opensubtitles-com and subdl also need opensubtitles_api_key, subdl_api_key in secrets_dir
	// subfetch2.py's OUT folder: its quota.csv and downloads.csv seed the OpenSubtitles quota once, at cutover.
	LegacyQuota string `toml:"legacy_quota"`
	AlignPerDay int    `toml:"align_per_day"` // queued titles aligned in any 24 h; each reads its video once or twice
	// careful (the default): only a release match is written unaligned, and the queue aligns at any hour. quick: a
	// downloaded subtitle that passes the cheap check is written even when it was not made for this release (its
	// timing unverified, for good), and the queue aligns only inside align_window (HH:MM-HH:MM, config timezone).
	Mode        string `toml:"mode"`
	AlignWindow string `toml:"align_window"`
	// Rename logs, oldest first: the original release name of a video whose .details note does not give one.
	UndoLogs []string `toml:"undo_logs"`
}

func defaults() Options {
	return Options{
		Roots:       []string{"/volume1/media/Movies", "/volume1/media/TV"},
		StatsFile:   "/state/sub_stats.tsv",
		RetryAfter:  7 * 24 * time.Hour,
		Workers:     3,
		SecretsDir:  "/run/secrets",
		Alass:       "/usr/local/bin/alass",
		Providers:   []string{"gestdown", "sitec", "subdl", "opensubtitles", "opensubtitles-com"},
		LegacyQuota: "/volume1/PlexMediaServer/subtitle-downloads",
		AlignPerDay: 20,
		Mode:        "careful",
		AlignWindow: "01:00-07:00",
		UndoLogs: []string{"/volume1/PlexMediaServer/rename/undo.tsv", "/volume1/PlexMediaServer/rename/undo_auto.tsv",
			"/volume1/media/.projectionist/rename/legacy-undo.tsv", "/volume1/media/.projectionist/rename/legacy-undo_auto.tsv",
			"/volume1/media/.projectionist/rename/undo.tsv"},
	}
}

// Stage's fields are test hooks; the zero value talks to the real services.
type Stage struct {
	openSubtitles, openSubtitlesCom, subdlAPI, subdlDL, gestdownAPI, sitecSite string
	sleep                                                                     func(context.Context, time.Duration) error
}

func (Stage) Name() string { return name }

const schema = `
CREATE TABLE IF NOT EXISTS subtitles_titles (
  path     TEXT PRIMARY KEY,
  outcome  TEXT NOT NULL,
  detail   TEXT NOT NULL,
  tries    INTEGER NOT NULL,
  last_try TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS subtitles_stats (
  path     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  row      TEXT NOT NULL
);
-- titles waiting for an alignment: worked by priority (0 no English audio, 1 an English image track, 2 the rest),
-- then oldest first
CREATE TABLE IF NOT EXISTS subtitles_queue (
  path      TEXT PRIMARY KEY,
  priority  INTEGER NOT NULL,
  queued_at TEXT NOT NULL
);`

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true,
	".m2ts": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".webm": true}

type title struct {
	root, path string
	kind, id   string // TMDB kind ("movie" | "tv") and id; kind "imdb" when the folder names the IMDb id
	show       string // set for an episode
	season     int
	episode    int
	key        string // this very file (size:mtime): what its rejections are recorded against

	// What describe learned without opening the video. info's stream indexes are valid only once probed.
	described, probed, ffprobed             bool // ffprobed: ffprobe read the video this run (its bytes are not in read)
	info                                    probe.Info
	durLo, durHi, fps                       float64
	englishAudio, englishImage, englishText bool
	release                                 string // the original release names, space-joined (rank's input)
	origin                                  origin
	read                                    int64      // bytes read from the video by the stage and its helpers
	idx                                     *reference // quick mode: the caption starts of its English VobSub sidecar
}

type run struct {
	env       *stage.Env
	opt       Options
	providers []Provider
	undo      func() map[string]string // video path -> its first path in the rename logs, read on first need

	mu        sync.Mutex
	rep       *stage.Report
	written   int
	alerted   map[string]bool
	alassDown bool                   // alass failed on a title this run: the titles behind it wait for the next run
	window    config.Window          // quick mode: when the queue may align
	pools     map[string][]Candidate // titles queued this run: their candidates, so the queue need not search again

	jf     sync.Mutex // the Jellyfin queue
	pushed time.Time
}

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Mode != "quick" && opt.Mode != "careful" {
		return fmt.Errorf("subtitles: mode %q: want quick or careful", opt.Mode)
	}
	window, err := config.ParseWindow(opt.AlignWindow)
	if err != nil {
		return fmt.Errorf("subtitles: align_window: %w", err)
	}
	if _, err := env.Store.DB().ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := env.Store.CachePrune(ctx, keptAs, env.Now().Add(-keptFor)); err != nil {
		return err
	}
	r, err := s.newRun(ctx, env, opt, rep)
	if err != nil {
		return err
	}
	r.window = window
	titles, err := r.scan(ctx)
	if err != nil {
		return err
	}
	due, err := r.plan(ctx, titles)
	if err != nil {
		return err
	}
	queue := make(chan *title)
	var wg sync.WaitGroup
	for range max(1, opt.Workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range queue {
				r.process(ctx, t)
			}
		}()
	}
feed:
	for _, t := range due {
		select {
		case queue <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	err = r.workQueue(ctx, titles)
	r.tellJellyfin(ctx, "", true)
	r.summarise(ctx, len(titles), len(due))
	// Quick mode outside its window: the cadence alone may reach the window late or not at all. Never inside it,
	// where the next opening is now: a capped queue would be run again at once, over and over.
	if rep.Counts["queue_waiting"] > 0 && !r.alignNow() {
		rep.Wake = window.Next(env.Now(), env.Loc)
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (s Stage) newRun(ctx context.Context, env *stage.Env, opt Options, rep *stage.Report) (*run, error) {
	sleep, hc := s.sleep, &http.Client{Timeout: 60 * time.Second}
	// The legacy API's redirects lead to host "_"; a redirect is an answer, never a hop to follow.
	noRedirect := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if sleep == nil {
		sleep = realSleep
	}
	newPacer := func(gap time.Duration) *pacer { return &pacer{gap: gap, now: env.Now, sleep: sleep} }
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	r := &run{env: env, opt: opt, rep: rep, alerted: map[string]bool{}, pools: map[string][]Candidate{}}
	r.undo = sync.OnceValue(func() map[string]string {
		orig, _, err := library.Originals(opt.UndoLogs)
		if err != nil {
			env.Log.Warn("rename logs unreadable: original names only from the notes", "event", "undo_log_error", "err", err)
		}
		return orig
	})
	key := func(name string) string { return filepath.Join(opt.SecretsDir, name) }
	for _, p := range opt.Providers {
		switch p {
		case "opensubtitles":
			l := &legacy{base: or(s.openSubtitles, "https://rest.opensubtitles.org/search/"), userAgent: userAgent, hc: noRedirect,
				pace: newPacer(time.Second), dl: newPacer(dlGap)}
			l.dl.quota429 = true
			var st quotaState
			found, err := env.Store.GetJSON(ctx, name, "opensubtitles_quota", &st)
			if err == nil && !found && opt.LegacyQuota != "" {
				st, err = legacyQuota(opt.LegacyQuota, env.Now())
			}
			if err != nil {
				return nil, err
			}
			l.restore(st)
			l.keep = func(st quotaState) {
				if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, "opensubtitles_quota", st); err != nil {
					env.Log.Error("quota state not saved", "event", "store_error", "err", err)
				}
			}
			if !found { // the import happens once: from now on the store holds the quota
				l.keep(st)
			}
			r.providers = append(r.providers, l)
		case "opensubtitles-com":
			if k := key("opensubtitles_api_key"); fileExists(k) {
				r.providers = append(r.providers, &dotCom{base: or(s.openSubtitlesCom, "https://api.opensubtitles.com/api/v1"), keyFile: k,
					userAgent: userAgent, hc: hc, pace: newPacer(time.Second)})
			}
		case "subdl":
			if k := key("subdl_api_key"); fileExists(k) {
				r.providers = append(r.providers, &subdl{api: or(s.subdlAPI, "https://api.subdl.com/api/v1/subtitles"),
					dl: or(s.subdlDL, "https://dl.subdl.com"), keyFile: k, hc: hc, pace: newPacer(time.Second)})
			}
		case "gestdown": // free community services: half the rate
			r.providers = append(r.providers, &gestdown{base: or(s.gestdownAPI, "https://api.gestdown.info"), hc: hc,
				pace: newPacer(2 * time.Second), shows: map[string]string{}})
		case "sitec":
			r.providers = append(r.providers, &sitec{base: or(s.sitecSite, "https://subtitles.example.com"), hc: hc, pace: newPacer(2 * time.Second)})
		default:
			return nil, fmt.Errorf("subtitles: unknown provider %q", p)
		}
	}
	return r, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// legacyQuota is subfetch2.py v8's own record of the IP's quota: the latest resume_after in quota.csv and the last
// 24 h of downloads.csv, so a cutover neither restarts the day's budget nor downloads into a deny. Missing files are
// no record.
func legacyQuota(dir string, now time.Time) (quotaState, error) {
	read := func(file, column string) ([]time.Time, error) {
		f, err := os.Open(filepath.Join(dir, file))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		rows, err := csv.NewReader(f).ReadAll()
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		col := slices.Index(rows[0], column)
		var out []time.Time
		for _, row := range rows[1:] {
			if t, err := time.Parse(time.RFC3339, row[col]); col >= 0 && err == nil {
				out = append(out, t)
			}
		}
		return out, nil
	}
	var st quotaState
	resume, err := read("quota.csv", "resume_after")
	if err != nil {
		return st, err
	}
	for _, t := range resume {
		if t.After(st.ResumeAfter) {
			st.ResumeAfter = t
		}
	}
	downloads, err := read("downloads.csv", "utc")
	for _, t := range downloads {
		if t.After(now.Add(-24 * time.Hour)) {
			st.Downloads = append(st.Downloads, t.UTC())
		}
	}
	return st, err
}

// ---- the walk: stats for every subtitle file, and the videos still lacking an English sidecar ----

func (r *run) scan(ctx context.Context) ([]*title, error) {
	db := r.env.Store.DB()
	cache := map[string]statsCache{}
	rows, err := db.QueryContext(ctx, `SELECT path, size, mtime_ns, row FROM subtitles_stats`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		var c statsCache
		if err := rows.Scan(&p, &c.size, &c.mtime, &c.row); err != nil {
			rows.Close()
			return nil, err
		}
		cache[p] = c
	}
	rows.Close()

	var tsv strings.Builder
	tsv.WriteString(statsHeader)
	fresh := map[string]statsCache{}
	seen := map[string]bool{}
	var titles []*title
	for _, root := range r.opt.Roots {
		pyWalk(root, func(dir string, files []string) {
			if ctx.Err() != nil {
				return
			}
			names := files
			if r.opt.OutputDir != "" { // a test run's own sidecars count too, so a rerun does not fetch again
				if es, err := os.ReadDir(r.mirror(root, dir)); err == nil {
					names = append([]string(nil), files...)
					for _, e := range es {
						names = append(names, e.Name())
					}
				}
			}
			for _, f := range files {
				p := dir + "/" + f
				if subtitleFile(f) {
					st, err := os.Stat(p)
					if err != nil {
						continue
					}
					seen[p] = true
					c, ok := cache[p]
					if !ok || c.size != st.Size() || c.mtime != st.ModTime().UnixNano() {
						raw, err := os.ReadFile(p)
						if err != nil {
							r.env.Log.Warn("subtitle unreadable", "event", "stats_error", "path", p, "err", err)
							continue
						}
						c = statsCache{st.Size(), st.ModTime().UnixNano(), statsRow(pyExt(f), raw)}
						fresh[p] = c
					}
					tsv.WriteString(p + "\t" + c.row + "\n")
				}
				if t := parseTitle(root, p); t != nil && !hasEnglishSidecar(strings.TrimSuffix(f, filepath.Ext(f)), names) {
					titles = append(titles, t)
				}
			}
		})
	}
	if err := r.saveStats(ctx, cache, fresh, seen, tsv.String()); err != nil {
		return nil, err
	}
	return titles, nil
}

// statsCache is one cached sub_stats row, valid while the file keeps its size and mtime.
type statsCache struct {
	size, mtime int64
	row         string
}

func (r *run) saveStats(ctx context.Context, cache, fresh map[string]statsCache, seen map[string]bool, tsv string) error {
	tx, err := r.env.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for p, c := range fresh {
		if _, err := tx.ExecContext(ctx, `INSERT INTO subtitles_stats(path, size, mtime_ns, row) VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, row=excluded.row`, p, c.size, c.mtime, c.row); err != nil {
			return err
		}
	}
	for p := range cache {
		if !seen[p] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM subtitles_stats WHERE path=?`, p); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if old, err := os.ReadFile(r.opt.StatsFile); err == nil && string(old) == tsv {
		return nil
	}
	tmp := r.opt.StatsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(tsv), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.opt.StatsFile) // our own report file, not a library file: replacing it is the point
}

func parseTitle(root, p string) *title {
	if !videoExt[strings.ToLower(filepath.Ext(p))] {
		return nil
	}
	if m, ok := library.ParseMovie(p); ok {
		kind := "movie"
		if m.ID.Source == "imdb" {
			kind = "imdb"
		}
		return &title{root: root, path: p, kind: kind, id: m.ID.Value}
	}
	if e, ok := library.ParseEpisode(p); ok {
		kind := ""
		if e.ID.Source == "tmdb" {
			kind = "tv"
		}
		return &title{root: root, path: p, kind: kind, id: e.ID.Value, show: e.Show, season: e.Season, episode: e.Episodes[0]}
	}
	return nil
}

// hasEnglishSidecar is subfetch2.py's test, <stem>.[...].(en|eng|english)[...].(srt|ass|ssa|vtt), except that a
// forced track (foreign dialogue only) does not count as an English subtitle.
func hasEnglishSidecar(stem string, names []string) bool {
	prefix := strings.ToLower(stem) + "."
	for _, n := range names {
		n = strings.ToLower(n)
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		parts := strings.Split(n[len(prefix):], ".")
		last := len(parts) - 1
		switch parts[last] {
		case "srt", "ass", "ssa", "vtt":
		default:
			continue
		}
		english, forced := false, false
		for _, p := range parts[:last] {
			english = english || p == "en" || p == "eng" || p == "english"
			forced = forced || p == "forced"
		}
		if english && !forced {
			return true
		}
	}
	return false
}

// mirror is where a test run writes what belongs in dir.
func (r *run) mirror(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		rel = filepath.Base(dir)
	}
	return filepath.Join(r.opt.OutputDir, filepath.Base(root), rel)
}

// ---- planning ----

func english(lang string) bool { return lang == "en" || lang == "eng" }

func (r *run) plan(ctx context.Context, titles []*title) ([]*title, error) {
	next := map[string]time.Time{}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT path, last_try, tries FROM subtitles_titles`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, at string
		var tries int
		if err := rows.Scan(&p, &at, &tries); err != nil {
			rows.Close()
			return nil, err
		}
		last, _ := time.Parse(store.TimeFormat, at)
		next[p] = last.Add(r.opt.RetryAfter << min(tries-1, 3)) // each miss doubles the wait, up to 8x retry_after
	}
	rows.Close()
	var due []*title
	for _, t := range titles {
		if at, ok := next[t.path]; ok && r.env.Now().Before(at) {
			continue
		}
		if !r.describe(ctx, t) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if t.englishText { // the players show it: nothing to fetch and nothing to read
			r.count("embedded_text")
			continue
		}
		due = append(due, t)
	}
	sort.SliceStable(due, func(i, j int) bool { // non-English audio first, then titles with an image reference
		if due[i].englishAudio != due[j].englishAudio {
			return !due[i].englishAudio
		}
		return due[i].englishImage && !due[j].englishImage
	})
	if r.opt.MaxTitles > 0 && len(due) > r.opt.MaxTitles {
		due = due[:r.opt.MaxTitles]
	}
	return due, nil
}

// describe learns what the cheap route needs without opening the video: the duration, frame rate and tracks from the
// probe cache, else from the .details note, else (a video neither describes) from ffprobe; the original release names
// from the note, else the rename logs. False when the video could not be described (logged and counted).
func (r *run) describe(ctx context.Context, t *title) bool {
	if t.described {
		return true
	}
	n, hasNote := readNote(t.path)
	names := n.names
	if orig, ok := r.undo()[t.path]; len(names) == 0 && ok {
		names = []string{filepath.Base(orig), filepath.Base(filepath.Dir(orig))}
	}
	t.release, t.origin = filepath.Base(t.path), originOf(names)
	if len(names) > 0 {
		t.release = strings.Join(names, " ")
	}
	info, cached, err := r.env.Probe.Lookup(ctx, t.path)
	switch {
	case err != nil:
	case cached:
		t.useInfo(info)
	case hasNote && n.durHi > 0:
		t.durLo, t.durHi, t.fps = n.durLo, n.durHi, n.fps
		for _, a := range n.audio {
			lang, _, _ := strings.Cut(a, " ")
			t.englishAudio = t.englishAudio || english(lang)
		}
		for _, s := range n.subs {
			t.englishImage = t.englishImage || noteTrack(s, "hdmv_pgs_subtitle", "dvd_subtitle")
			t.englishText = t.englishText || noteTrack(s, "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text")
		}
	default:
		err = r.probe(ctx, t)
	}
	if err != nil {
		r.env.Log.Warn("probe failed", "event", "probe_error", "item", t.path, "err", err)
		r.count("probe_failed")
		return false
	}
	t.described = true
	return true
}

// probe reads the video's streams (from the cache, else with ffprobe): the queue needs their indexes.
func (r *run) probe(ctx context.Context, t *title) error {
	if t.probed {
		return nil
	}
	if _, cached, err := r.env.Probe.Lookup(ctx, t.path); err == nil && !cached {
		t.ffprobed = true
	}
	info, err := r.env.Probe.Probe(ctx, t.path)
	if err != nil {
		return err
	}
	t.useInfo(info)
	return nil
}

func (t *title) useInfo(info probe.Info) {
	t.info, t.probed = info, true
	t.durLo, t.durHi = info.Duration, info.Duration
	if info.Video != nil {
		t.fps = info.Video.FPS
	}
	t.englishAudio, t.englishImage, t.englishText = hasEnglishAudio(info), len(imageTracks(info)) > 0, hasEnglishText(info)
}

func hasEnglishAudio(info probe.Info) bool {
	for _, a := range info.Audio {
		if english(a.Language) {
			return true
		}
	}
	return false
}

func imageTracks(info probe.Info) []probe.Subtitle {
	var out []probe.Subtitle
	for _, s := range info.Subtitles {
		if (s.Codec == "hdmv_pgs_subtitle" || s.Codec == "dvd_subtitle") && english(s.Language) && !s.Forced {
			out = append(out, s)
		}
	}
	return out
}

// hasEnglishText tells whether the video carries an English text track the players show: not forced, not commentary.
func hasEnglishText(info probe.Info) bool {
	for _, s := range info.Subtitles {
		if s.Text && english(s.Language) && !s.Forced && !strings.Contains(strings.ToLower(s.Title), "comment") {
			return true
		}
	}
	return false
}

// ---- one title ----

// outcome is what happened to a title (or one attempt). Deferred titles are not recorded, so the next run tries again.
type outcome struct {
	result string
	detail string
	fields map[string]any
	wrote  bool
	pool   []Candidate // "queued": the candidates its alignment will try, best first
}

// process looks for a release match for a due title; one it does not find sends the title to the queue.
func (r *run) process(ctx context.Context, t *title) {
	if err := r.env.Viewers.WaitIdle(ctx); err != nil {
		return
	}
	o := r.fetch(ctx, t)
	if o.result == "queued" {
		r.mu.Lock()
		r.pools[t.path] = o.pool
		r.mu.Unlock()
	}
	r.record(ctx, t, o)
}

// record counts an outcome, adds it to the history and the title's row, and keeps the queue: a queued title is added
// (keeping its place when it was already there), any other verdict takes it out.
func (r *run) record(ctx context.Context, t *title, o outcome) {
	if ctx.Err() != nil && !o.wrote {
		return // shutting down mid-title: nothing was written, the next run starts it again
	}
	ctx = context.WithoutCancel(ctx) // a sidecar written as the run stops is still recorded
	r.count(o.result)
	switch o.result {
	case "deferred":
		r.env.Log.Info("title deferred", "event", "subtitle_deferred", "item", t.path, "detail", o.detail)
		return
	case "release_matched":
		r.count("written_release_matched")
	case "best_match":
		r.count("written_best_match")
	case "idx_kept", "idx_retimed":
		r.count("written_idx")
	case "kept", "retimed", "rescued":
		r.count("written_aligned")
	}
	if o.fields == nil {
		o.fields = map[string]any{}
	}
	if o.wrote {
		r.count(fmt.Sprint("provider_hits_", o.fields["provider"]))
	}
	o.fields["video_bytes"], o.fields["ffprobe"] = t.read, t.ffprobed
	r.event(ctx, t.path, o)
	db := r.env.Store.DB()
	now := r.env.Now().UTC().Format(store.TimeFormat)
	if _, err := db.ExecContext(ctx, `INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(path) DO UPDATE SET outcome=excluded.outcome, detail=excluded.detail, tries=tries+1, last_try=excluded.last_try`,
		t.path, o.result, o.detail, now); err != nil {
		r.env.Log.Error("title record failed", "event", "store_error", "item", t.path, "err", err)
	}
	var err error
	if o.result == "queued" {
		priority := 2
		switch {
		case !t.englishAudio:
			priority = 0
		case t.englishImage:
			priority = 1
		}
		_, err = db.ExecContext(ctx, `INSERT INTO subtitles_queue(path, priority, queued_at) VALUES (?, ?, ?) ON CONFLICT(path) DO NOTHING`,
			t.path, priority, now)
	} else {
		_, err = db.ExecContext(ctx, `DELETE FROM subtitles_queue WHERE path=?`, t.path)
	}
	if err != nil {
		r.env.Log.Error("queue write failed", "event", "store_error", "item", t.path, "err", err)
	}
}

// fetch is the cheap route: the providers' candidates, any release-matched one written once cheapCheck passes, and
// otherwise the candidates an alignment may try ("queued"). The video is read only for OpenSubtitles' file hash
// (128 KiB), and only when OpenSubtitles is about to be searched.
func (r *run) fetch(ctx context.Context, t *title) outcome {
	q, err := r.query(t)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	if t.kind == "movie" || t.kind == "tv" {
		// OpenSubtitles finds nearly every subtitle by the IMDb id, SITEC only by it, Gestdown by the TheTVDB id: without
		// TMDB the title waits for it, rather than being searched without them and recorded as having none.
		id, _ := strconv.Atoi(t.id)
		d, err := r.env.TMDB.Details(ctx, tmdb.Kind(t.kind), id)
		switch {
		case errors.Is(err, tmdb.ErrNotFound): // TMDB has no such title: searched without its ids
		case err != nil:
			if ctx.Err() == nil {
				r.alertOnce("tmdb", "TMDB lookups failing, titles wait for them: %v", err)
			}
			return outcome{result: "deferred", detail: err.Error()}
		default:
			q.IMDb, _ = strconv.Atoi(strings.TrimPrefix(d.ExternalIDs.IMDb, "tt"))
			q.TVDB = d.ExternalIDs.TVDB
		}
	}
	rejected, err := r.rejected(ctx, t.path, t.key)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	if r.quick() {
		t.idx = vobsubReference(t.path, t.durHi)
	}
	// A title is recorded only once every provider has answered its search and let its candidates be tried; while one
	// is out (an outage, the day's quota, a refusal earlier in the run) the title waits for the next run, which skips
	// the candidates already rejected for this file. A metered provider that is out holds back only a title no cheaper
	// provider has a candidate for: beside theirs it would add no more than a hash match, and neither route may wait on
	// its quota. A title a provider refuses (a status no retry changes, or an answer that is not one) is recorded with
	// the refusal once every other provider answered, so a refusal of that title alone cannot hold up every run.
	out, meteredOut, refusal, found, tried := false, false, "", 0, 0
	var pool []Candidate
	missing := func(p Provider) { // p could not search or serve this title
		out, meteredOut = out || !metered(p), meteredOut || metered(p)
	}
	for _, last := range []bool{false, true} { // the metered providers only when nothing cheaper was written
		var cands []Candidate
		for _, p := range r.providers {
			if metered(p) != last {
				continue
			}
			err := parked(p) // a parked provider costs no request and no hash read
			if err == nil && last && q.Hash == "" {
				if q.Hash, _, err = openSubtitlesHash(t.path); err != nil {
					return outcome{result: "error", detail: err.Error()}
				}
				t.read += 2 * min(q.Size, 65536) // the first and the last 64 KiB
			}
			var got []Candidate
			if err == nil {
				got, err = p.Search(ctx, q)
			}
			switch {
			case err == nil:
				cands = append(cands, got...)
				continue
			case ctx.Err() != nil:
				return outcome{result: "deferred"}
			case errors.Is(err, errParked):
				missing(p)
			default:
				p.pacer().park(err)
				refusal = p.Name() + " search: " + err.Error()
			}
			r.alertParked(p, t.path, err)
		}
		cands = order(cands, t.release)
		if r.quick() { // a subtitle made for this release before any other, however popular
			sort.SliceStable(cands, func(i, j int) bool { return t.releaseMatched(cands[i]) && !t.releaseMatched(cands[j]) })
		}
		found += len(cands)
		cheaper := len(pool) // a metered provider's candidates other than its hash match join only an empty pool
		for _, c := range cands {
			if rejected[c.key()] == "aligned" {
				continue
			}
			matched := t.releaseMatched(c)
			try := matched
			if r.quick() { // OpenSubtitles' downloads (180 a day, 8 min apart) go only to its hash match, as in careful mode
				try = (matched || !last && t.plausible(c)) && t.specialSure()
			}
			if rejected[c.key()] == "" && tried < maxTries && try {
				tried++
				attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "id": c.key(), "file": t.key}
				txt, err := r.download(ctx, c)
				switch {
				case errors.Is(err, errParked):
					missing(c.provider)
					r.alertParked(c.provider, t.path, err)
					continue
				case ctx.Err() != nil:
					return outcome{result: "deferred"}
				case err != nil: // that file (a dead link, a season pack): the next candidate, as subfetch2.py does
					r.event(ctx, t.path, outcome{result: "download_failed", detail: err.Error(), fields: attempt})
					continue
				}
				out, result, why := r.check(ctx, t, c, txt, matched, attempt)
				if why == "" {
					return r.write(ctx, t, []byte(out), result, attempt)
				}
				if err := r.env.Store.CachePut(ctx, keptAs+c.key(), []byte(txt), r.env.Now()); err != nil {
					r.env.Log.Warn("refused candidate not kept: its alignment downloads it again", "event", "store_error", "item", t.path, "err", err)
				}
				r.event(ctx, t.path, outcome{result: "rejected_cheap", detail: why, fields: attempt})
			}
			if !last || cheaper == 0 || c.HashMatch && c.ours {
				pool = append(pool, c)
			}
		}
	}
	switch {
	case out || meteredOut && len(pool) == 0: // a provider that was out might have had what this title needs
		return outcome{result: "deferred", detail: "a provider was out"}
	case refusal != "":
		return outcome{result: "error", detail: refusal}
	case found == 0:
		return outcome{result: "no_candidates"}
	case len(pool) == 0: // no download, no video read
		return outcome{result: "no_aligned_candidate", detail: "every candidate was rejected for this file before"}
	}
	var best []string // why it was not release-matched: the group sought, and what the best candidates name
	for _, c := range pool[:min(len(pool), 5)] {
		best = append(best, c.provider.Name()+": "+strings.TrimSpace(c.Release+" "+c.FileName))
	}
	return outcome{result: "queued", detail: fmt.Sprintf("%d candidate(s)", len(pool)), pool: pool,
		fields: map[string]any{"group": t.origin.group, "candidates": best}}
}

// check is the test a downloaded candidate passes to be written without reading the video: in quick mode, a fit to the
// title's English VobSub captions where it has them (retimed onto them); else cheapCheck. It returns the text to write
// and the result, or why the candidate fails.
func (r *run) check(ctx context.Context, t *title, c Candidate, txt string, matched bool, attempt map[string]any) (string, string, string) {
	if t.idx != nil {
		out, result, ok := r.fit(ctx, t.idx, txt, attempt)
		if !ok || !sane(cues(out), t.durHi) {
			return "", "", "does not fit the English VobSub captions (.en.idx)"
		}
		attempt["reference"], attempt["ref_median_ms"] = "idx", math.Round(medianOffsetMS(cues(out), t.idx.starts))
		return out, "idx_" + result, ""
	}
	if why := cheapCheck(cues(txt), t.durLo, t.durHi, t.fps, subFPS(c)); why != "" {
		return "", "", why
	}
	if !matched {
		attempt["reference"] = "cheap_check"
		return txt, "best_match", ""
	}
	attempt["reference"] = "release"
	return txt, "release_matched", ""
}

func (r *run) quick() bool { return r.opt.Mode == "quick" }

// download is c's text with OpenSubtitles' ads stripped: from the store when the cheap route downloaded it before (a
// release match cheapCheck refused), else from its provider.
func (r *run) download(ctx context.Context, c Candidate) (string, error) {
	if body, ok, err := r.env.Store.CacheGet(ctx, keptAs+c.key(), r.env.Now().Add(-keptFor)); err == nil && ok {
		return string(body), nil
	}
	txt, err := c.provider.Download(ctx, c)
	if err != nil {
		return "", err
	}
	return stripAds(txt), nil
}

// rejected are the candidates (provider:id) rejected for this very file before: "aligned" when an alignment or the
// length check rejected one (never tried again), "cheap" when only cheapCheck did (an alignment may still try it).
func (r *run) rejected(ctx context.Context, path, key string) (map[string]string, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT json_extract(fields, '$.id'), status FROM events WHERE stage=? AND item=?
		AND status IN ('rejected_misaligned', 'rejected_length', 'rejected_cheap') AND json_extract(fields, '$.file')=?`, name, path, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		if status != "rejected_cheap" {
			out[id] = "aligned"
		} else if out[id] == "" {
			out[id] = "cheap"
		}
	}
	return out, rows.Err()
}

// query gathers what the providers search by from the title's names; fetch adds the ids from TMDB, and the file hash
// only for OpenSubtitles. It also sets the title's file key.
func (r *run) query(t *title) (Query, error) {
	st, err := os.Stat(t.path)
	if err != nil {
		return Query{}, err
	}
	t.key = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	q := Query{Size: st.Size(), Name: t.show, Show: t.show != "", Season: t.season, Episode: t.episode}
	switch t.kind {
	case "imdb":
		q.IMDb, _ = strconv.Atoi(strings.TrimPrefix(t.id, "tt"))
	case "movie", "tv":
		q.TMDB = t.id
	}
	return q, nil
}

// ---- the alignment queue ----

// workQueue aligns queued titles, best first, while nobody but the owner watches and align_per_day allows (in quick
// mode, also only inside align_window); a title keeps its place until its alignment ends in a sidecar or a verdict.
// A queued title first gets another look for a release match (a subtitle may have been added since), unless this run
// queued it.
func (r *run) workQueue(ctx context.Context, lacking []*title) error {
	if ctx.Err() != nil {
		return nil
	}
	byPath := map[string]*title{}
	for _, t := range lacking {
		byPath[t.path] = t
	}
	db := r.env.Store.DB()
	rows, err := db.QueryContext(ctx, `SELECT path FROM subtitles_queue ORDER BY priority, queued_at, path`)
	if err != nil {
		return err
	}
	var queued []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		queued = append(queued, p)
	}
	rows.Close()
	for _, p := range queued {
		t := byPath[p]
		if t == nil { // it has an English sidecar now, or it moved (its new path queues anew)
			if _, err := db.ExecContext(ctx, `DELETE FROM subtitles_queue WHERE path=?`, p); err != nil {
				return err
			}
			continue
		}
		starts, err := r.alignments(ctx)
		if err != nil || len(starts) >= r.opt.AlignPerDay || !r.alignNow() {
			return err
		}
		// a guest may watch past the window's end: the next title's check above then ends the queue's work
		if err := r.env.Viewers.WaitIdle(ctx); err != nil || !r.alignNow() || !r.describe(ctx, t) {
			continue
		}
		if why := r.alassOut(); why != "" && !t.englishImage { // it would need alass: it waits, with no search or cap spent
			r.record(ctx, t, outcome{result: "deferred", detail: why})
			continue
		}
		r.mu.Lock()
		pool, fresh := r.pools[p]
		r.mu.Unlock()
		o := outcome{result: "queued"}
		if !fresh {
			o = r.fetch(ctx, t)
			pool = o.pool
		}
		if o.result == "queued" {
			o = r.align(ctx, t, pool)
		}
		r.record(ctx, t, o)
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

// alignNow: careful mode aligns at any hour, quick mode only inside align_window in the config timezone.
func (r *run) alignNow() bool { return !r.quick() || r.window.Contains(r.env.Now(), r.env.Loc) }

// alignments are the start times of the alignments in the last 24 h, kept in the store so no restart exceeds the cap.
func (r *run) alignments(ctx context.Context) ([]time.Time, error) {
	var starts []time.Time
	if _, err := r.env.Store.GetJSON(ctx, name, "alignments", &starts); err != nil {
		return nil, err
	}
	day := r.env.Now().Add(-24 * time.Hour)
	return slices.DeleteFunc(starts, func(s time.Time) bool { return !s.After(day) }), nil
}

// align is the queue's work on one title: its candidates downloaded one at a time, best first, each only once the one
// before it failed (an OpenSubtitles download is quota); the reference read from the video once the first arrives (a
// refused download costs no read); each fitted to it, and one the captions reject given to alass (the rescue).
func (r *run) align(ctx context.Context, t *title, pool []Candidate) outcome {
	var ref *reference
	defer func() {
		if ref != nil {
			ref.cleanup()
		}
	}()
	incomplete := false
	for _, c := range pool[:min(len(pool), maxTries)] {
		attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "id": c.key(), "file": t.key}
		txt, err := r.download(ctx, c)
		switch {
		case errors.Is(err, errParked):
			incomplete = true
			r.alertParked(c.provider, t.path, err)
			continue
		case ctx.Err() != nil:
			return outcome{result: "deferred"}
		case err != nil:
			r.event(ctx, t.path, outcome{result: "download_failed", detail: err.Error(), fields: attempt})
			continue
		}
		if ref == nil {
			if err := r.probe(ctx, t); err != nil {
				return outcome{result: "error", detail: err.Error()}
			}
			got, err := r.reference(ctx, t)
			if err != nil {
				return outcome{result: "error", detail: err.Error()}
			}
			ref = &got
			if ref.label == "" {
				if ref.deferred {
					return outcome{result: "deferred", detail: ref.why}
				}
				return outcome{result: "no_reference", detail: ref.why}
			}
		}
		attempt["reference"], attempt["cues"] = ref.label, len(cues(txt))
		out, result, ok := r.fit(ctx, ref, txt, attempt)
		if !ok && ref.label == "pgs" {
			out, result, ok = r.rescue(ctx, t, ref, txt, attempt)
		}
		if ctx.Err() != nil {
			return outcome{result: "deferred"}
		}
		if !ok {
			r.event(ctx, t.path, outcome{result: "rejected_misaligned", fields: attempt})
			continue
		}
		spans := cues(out)
		if !sane(spans, t.info.Duration) {
			r.event(ctx, t.path, outcome{result: "rejected_length", fields: attempt})
			continue
		}
		if ref.starts != nil {
			attempt["ref_median_ms"] = math.Round(medianOffsetMS(spans, ref.starts))
		}
		return r.write(ctx, t, []byte(out), result, attempt)
	}
	if incomplete {
		return outcome{result: "deferred", detail: "a provider was out"}
	}
	return outcome{result: "no_aligned_candidate", detail: fmt.Sprintf("%d tried", min(len(pool), maxTries))}
}

// summarise is the run's report: what was written by which route, what waits in the queue, and the cap used.
func (r *run) summarise(ctx context.Context, lacking, due int) {
	ctx = context.WithoutCancel(ctx)
	starts, _ := r.alignments(ctx)
	var waiting int
	r.env.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM subtitles_queue`).Scan(&waiting)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count("cap_used", len(starts))
	r.rep.Count("queue_waiting", waiting)
	c := r.rep.Counts
	var hits []string
	for k, n := range c {
		if p, ok := strings.CutPrefix(k, "provider_hits_"); ok {
			hits = append(hits, fmt.Sprintf("%s=%d", p, n))
		}
	}
	slices.Sort(hits)
	r.rep.Summary = fmt.Sprintf("%s mode: %d sidecar(s) written: %d release-matched, %d best match after the cheap check, "+
		"%d fitted to a VobSub sidecar, %d aligned; %d queued, %d waiting in the queue; alignments %d of %d in 24 h; "+
		"provider hits %s; %d title(s) lacked a sidecar, %d of them carry an English text track, %d were due",
		r.opt.Mode, r.written, c["written_release_matched"], c["written_best_match"], c["written_idx"], c["written_aligned"],
		c["queued"], waiting, len(starts), r.opt.AlignPerDay, cmp.Or(strings.Join(hits, " "), "none"), lacking, c["embedded_text"], due)
}

// reference is the timing a candidate must line up with: an English image track's caption starts (from the video, or
// in quick mode from a VobSub sidecar's text), else alass's speech segments of the default audio track.
type reference struct {
	label    string // "pgs" | "idx" | "alass" | "" (none)
	why      string
	deferred bool // none because alass is out, not because of the title
	bits     bitset
	starts   []float64
	dir      string // alass work files
	speech   string // alass's speech segments as an SRT, once heard
	talk     bitset // the same segments as bins, for the cross-check
	deaf     bool   // hearing failed for a rescue: no more rescues for this title
}

func (ref *reference) cleanup() {
	if ref.dir != "" {
		os.RemoveAll(ref.dir)
	}
}

func (r *run) reference(ctx context.Context, t *title) (reference, error) {
	images := imageTracks(t.info)
	hearable := func() (reference, bool) {
		if len(t.info.Audio) == 0 {
			return reference{why: "no usable image track and no audio"}, false
		}
		if why := r.alassOut(); why != "" {
			return reference{why: why, deferred: true}, false
		}
		return reference{}, true
	}
	if len(images) == 0 { // alass is the only reference: a title it cannot serve costs no read and no cap
		if ref, ok := hearable(); !ok {
			return ref, nil
		}
	}
	// The video is read from here on: one of the day's align_per_day, kept in the store before the read.
	starts, err := r.alignments(ctx)
	if err == nil {
		err = r.env.Store.PutJSON(ctx, name, "alignments", append(starts, r.env.Now()))
	}
	if err != nil {
		return reference{why: err.Error(), deferred: true}, nil
	}
	for _, s := range images {
		out, err := r.cmd(ctx, t, r.env.FFmpeg.FFmpegPath(), "-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path,
			"-map", fmt.Sprintf("0:%d", s.Index), "-c", "copy", "-f", "framecrc", "-")
		if err != nil {
			if ctx.Err() != nil {
				return reference{}, ctx.Err()
			}
			continue
		}
		if starts, bits, ok := pgsReference(string(out), t.info.Duration); ok {
			return reference{label: "pgs", bits: bits, starts: starts}, nil
		}
	}
	if ref, ok := hearable(); !ok {
		return ref, nil
	}
	ref := reference{label: "alass"}
	if err := r.hear(ctx, t, &ref); err != nil {
		ref.cleanup()
		if ctx.Err() != nil {
			return reference{}, ctx.Err()
		}
		// This title's audio or alass itself: the title is recorded, and the titles behind it wait for the next run
		// rather than each fail the same way and lose a week.
		r.mu.Lock()
		r.alassDown = true
		r.mu.Unlock()
		r.alertOnce("alass-failed", "alass failed (titles without an English image track wait for the next run): %v", err)
		return reference{why: "alass could not read the audio: " + err.Error()}, nil
	}
	return ref, nil
}

// alassOut is why alass cannot run now ("" when it can): missing (alerted once), or it failed on a title this run.
func (r *run) alassOut() string {
	r.mu.Lock()
	down := r.alassDown
	r.mu.Unlock()
	switch {
	case !fileExists(r.opt.Alass):
		r.alertOnce("alass", "alass not found at %s: titles without an English image track wait for it", r.opt.Alass)
		return "alass missing"
	case down:
		return "alass failed on another title this run"
	}
	return ""
}

// hear has alass find the speech in the default audio track, decoded once per title and reused per candidate: "_"
// as the subtitle makes alass write the speech segments it found as an SRT.
func (r *run) hear(ctx context.Context, t *title, ref *reference) error {
	dir, err := os.MkdirTemp("", "subtitles-")
	if err != nil {
		return err
	}
	ref.dir, ref.speech = dir, filepath.Join(dir, "speech.srt")
	audio := t.info.Audio[0]
	for _, a := range t.info.Audio {
		if a.Default {
			audio = a
			break
		}
	}
	if _, err := r.cmd(ctx, t, r.opt.Alass, t.path, "_", ref.speech, "--index", strconv.Itoa(audio.Index)); err != nil { // the stream index
		return err
	}
	raw, err := os.ReadFile(ref.speech)
	if err != nil {
		return err
	}
	ref.talk = spanBits(cues(string(raw)), 1)
	return nil
}

var ratioRe = regexp.MustCompile(`ratio is ([0-9.]+)(?:/([0-9.]+))?`)

// realign has alass align txt to the heard speech; it returns the aligned text and the frame-rate ratio alass chose.
func (r *run) realign(ctx context.Context, ref *reference, txt string) (string, float64, error) {
	in, out := filepath.Join(ref.dir, "candidate.srt"), filepath.Join(ref.dir, "aligned.srt")
	os.Remove(out)
	if err := os.WriteFile(in, []byte(txt), 0o644); err != nil {
		return "", 0, err
	}
	stdout, err := r.cmd(ctx, nil, r.opt.Alass, ref.speech, in, out)
	if err != nil {
		return "", 0, err
	}
	aligned, err := os.ReadFile(out)
	if err != nil {
		return "", 0, err
	}
	ratio := 1.0
	if m := ratioRe.FindStringSubmatch(string(stdout)); m != nil && m[2] != "" {
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[2], 64)
		ratio = a / b
	}
	return string(aligned), ratio, nil
}

// fit lines a candidate up with the reference; it returns the text to write and kept|retimed, or ok=false.
func (r *run) fit(ctx context.Context, ref *reference, txt string, attempt map[string]any) (string, string, bool) {
	spans := cues(txt)
	if len(spans) < minCues {
		return "", "", false
	}
	if ref.label != "alass" {
		starts := make([]span, len(spans)) // cue starts against caption starts: a cue stays up after its line ends
		for i, s := range spans {
			starts[i] = span{s.start, s.start}
		}
		f, ok := align(starts, ref.bits)
		if !ok {
			return "", "", false
		}
		attempt["offset_ms"], attempt["ratio"] = math.Round(f.Offset*1000), f.Ratio
		attempt["z"], attempt["agree"] = math.Round(f.Z*10)/10, f.Agree
		if f.Ratio == 1.0 && f.Offset == 0.0 {
			return txt, "kept", true
		}
		return retime(txt, f.Ratio, f.Offset), "retimed", true
	}
	aligned, ratio, err := r.realign(ctx, ref, txt)
	if err != nil {
		return "", "", false
	}
	moved := cues(aligned)
	offset, agree, ok := shifts(spans, moved, ratio)
	attempt["offset_ms"], attempt["ratio"], attempt["agree"] = offset, ratio, math.Round(agree*100)/100
	if !ok || agree < alassAgree {
		return "", "", false
	}
	// Cross-check with subfetch2.py's own speech test: each cue's first second must sit on the speech alass heard, in
	// sync by the Python's own test (6 of 8 chunks within 0.5 s, no shift or stretch left). It rejects a wrong
	// subtitle that alass shifted whole, and an alignment left off by a drift (21 Grams: 0.6 s off its captions).
	first := make([]span, len(moved))
	for i, c := range moved {
		first[i] = span{c.start, math.Min(c.end, c.start+1)}
	}
	check, ok := align(first, ref.talk)
	attempt["z"], attempt["chunks"] = math.Round(check.Z*10)/10, check.Agree
	if !ok || check.Ratio != 1 || check.Offset != 0 {
		return "", "", false
	}
	if slices.Equal(spans, moved) {
		return txt, "kept", true
	}
	return aligned, "retimed", true
}

// rescue gives a candidate the captions rejected (a cut, a recap or a drift the fit cannot follow) to alass, aligned
// to the speech, and keeps alass's output when that fits the captions and its cues sit within rescueMax of them: the
// fit's in-sync test allows 0.5 s per chunk, and alass's pieces can each land that far off (The Good, the Bad and the
// Ugly: +0.5 s, then -0.46 s, a median 461 ms from its captions). Without alass it does nothing.
func (r *run) rescue(ctx context.Context, t *title, ref *reference, txt string, attempt map[string]any) (string, string, bool) {
	r.mu.Lock()
	down := r.alassDown
	r.mu.Unlock()
	if ref.deaf || down || len(t.info.Audio) == 0 || !fileExists(r.opt.Alass) {
		return "", "", false
	}
	if ref.speech == "" {
		if err := r.hear(ctx, t, ref); err != nil {
			ref.deaf = true
			r.env.Log.Info("no rescue: alass could not hear the title", "event", "rescue_skipped", "item", t.path, "err", err)
			return "", "", false
		}
	}
	aligned, _, err := r.realign(ctx, ref, txt)
	if err != nil {
		return "", "", false
	}
	attempt["rescue"] = "alass"
	out, _, ok := r.fit(ctx, &reference{label: "pgs", bits: ref.bits}, aligned, attempt)
	if !ok {
		return "", "", false
	}
	if ms := math.Round(medianOffsetMS(cues(out), ref.starts)); ms > rescueMax {
		attempt["rescue_median_ms"] = ms
		return "", "", false
	}
	return out, "rescued", true
}

// shifts compares alass's output with its input cue by cue: the median shift (ms) and the share of cues within 0.5 s
// of it. alass splits a file where it finds cuts, so a wrong subtitle scatters into many shifts.
func shifts(before, after []span, ratio float64) (median float64, agree float64, ok bool) {
	if len(before) == 0 || len(before) != len(after) {
		return 0, 0, false
	}
	d := make([]float64, len(before))
	for i := range before {
		d[i] = math.Round(after[i].start*1000) - math.Trunc(math.Round(before[i].start*1000)*ratio)
	}
	median = pyMedian(d)
	n := 0
	for _, x := range d {
		if math.Abs(x-median) <= 500 {
			n++
		}
	}
	return median, float64(n) / float64(len(d)), true
}

// write creates the sidecar and records the decision; a test run writes under output_dir instead.
func (r *run) write(ctx context.Context, t *title, data []byte, result string, fields map[string]any) outcome {
	dir := filepath.Dir(t.path)
	if r.opt.OutputDir != "" {
		dir = r.mirror(t.root, dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return outcome{result: "error", detail: err.Error()}
		}
	}
	base := filepath.Join(dir, strings.TrimSuffix(filepath.Base(t.path), filepath.Ext(t.path)))
	dest, err := writeSidecar(base, data)
	switch {
	case err != nil:
		return outcome{result: "error", detail: err.Error(), fields: fields}
	case dest == "":
		return outcome{result: "not_written", detail: ".en.srt and .eng.srt both exist", fields: fields}
	}
	fields["sidecar"] = filepath.Base(dest)
	r.mu.Lock()
	r.written++
	r.mu.Unlock()
	r.env.Log.Info("subtitle written", "event", "subtitle_written", "item", t.path, "result", result, "sidecar", filepath.Base(dest))
	r.tellJellyfin(ctx, dest, false)
	return outcome{result: result, detail: filepath.Base(dest), fields: fields, wrote: true}
}

// writeSidecar creates base.en.srt, else base.eng.srt, never replacing a file; it returns "" when both exist, and the
// path once the file is synced and re-reads identical.
func writeSidecar(base string, data []byte) (string, error) {
	for _, dest := range []string{base + ".en.srt", base + ".eng.srt"} {
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // no chmod: it would strip the Synology ACL
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dest) // ours, half written
			return "", err
		}
		if back, err := os.ReadFile(dest); err != nil || !bytes.Equal(back, data) {
			return "", fmt.Errorf("%s re-read differs from what was written", dest)
		}
		return dest, fsx.SyncDir(filepath.Dir(dest))
	}
	return "", nil
}

// ---- side effects shared by the workers ----

// cmd runs a helper at idle CPU and the lowest best-effort I/O priority in its own process group (so alass's ffmpeg
// runs so too and dies with it) and returns its stdout; the bytes it and its children read (rchar, Linux) are added to
// t's when t is the video it reads. The group is stopped while anyone but the owner is watching (an unknown count
// counts as watching), checked every viewer-gate poll, and continued once they stop.
func (r *run) cmd(ctx context.Context, t *title, bin string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = append(os.Environ(), "ALASS_FFMPEG_PATH="+r.env.FFmpeg.FFmpegPath(), "ALASS_FFPROBE_PATH="+r.env.FFmpeg.FFprobePath())
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Start(); err != nil {
		return nil, err
	}
	idle(c.Process.Pid)
	done := make(chan error, 1)
	go func() {
		read := exitedRead(c.Process.Pid) // before Wait reaps it, while its /proc/<pid>/io still exists
		if t != nil {
			t.read += read
		}
		done <- c.Wait()
	}()
	tick := time.NewTicker(r.env.Viewers.Poll)
	defer tick.Stop()
	var err error
	for paused, running := false, true; running; {
		select {
		case err = <-done:
			running = false
		case <-tick.C:
			others, cerr := r.env.Viewers.Count(ctx)
			if busy := cerr != nil || others > 0; busy != paused {
				sig := syscall.SIGCONT
				if busy {
					sig = syscall.SIGSTOP
				}
				syscall.Kill(-c.Process.Pid, sig)
				paused = busy
				r.env.Log.Info("helper paused for viewers", "event", "subtitle_pause", "bin", filepath.Base(bin), "paused", paused, "others", others, "err", cerr)
			}
		}
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, msg)
	}
	return stdout.Bytes(), nil
}

func (r *run) count(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count(kind, 1)
}

func (r *run) alertOnce(key, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alerted[key] {
		r.alerted[key] = true
		r.rep.Alert(format, args...)
	}
}

// alertParked raises an alert, once a run, when a provider kept failing or refused; a spent download quota is
// routine and only logged, once.
func (r *run) alertParked(p Provider, item string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alerted[p.Name()] {
		return
	}
	r.alerted[p.Name()] = true
	if errors.Is(err, errQuota) {
		r.env.Log.Info("provider quota spent", "event", "provider_parked", "provider", p.Name(), "err", err)
		return
	}
	r.rep.Alert("%s unavailable, titles that need it wait (at %s): %v", p.Name(), item, err)
}

// event records one attempt or decision in the history (the old log2.csv row).
func (r *run) event(ctx context.Context, path string, o outcome) {
	fields := map[string]any{}
	for k, v := range o.fields {
		fields[k] = v
	}
	if o.detail != "" {
		fields["detail"] = o.detail
	}
	if err := r.env.Store.AddEvent(ctx, r.env.Now(), name, path, o.result, fields); err != nil {
		r.env.Log.Error("history write failed", "event", "store_error", "item", path, "err", err)
	}
	r.env.Log.Info("subtitle "+o.result, "event", "subtitle_attempt", "item", path, "result", o.result, "detail", o.detail)
}

const pushEvery = 15 * time.Minute // a backlog run lasts days: Jellyfin hears of its sidecars at least this often

// tellJellyfin queues a new sidecar (path set) in the store at once, so a run stopped early loses none, and pushes the
// queue when forced (the end of a run) or when the last push is pushEvery old. Plex watches the disk.
func (r *run) tellJellyfin(ctx context.Context, path string, force bool) {
	if r.opt.OutputDir != "" {
		return
	}
	r.jf.Lock()
	defer r.jf.Unlock()
	keep := context.WithoutCancel(ctx) // a sidecar written as the run stops still joins the queue
	var pending []string
	if _, err := r.env.Store.GetJSON(keep, name, "jellyfin_pending", &pending); err != nil {
		r.env.Log.Error("jellyfin queue unreadable", "event", "store_error", "err", err)
	}
	if path != "" {
		pending = append(pending, path)
	}
	if len(pending) == 0 {
		return
	}
	if ctx.Err() == nil && (force || r.env.Now().Sub(r.pushed) >= pushEvery) {
		r.pushed = r.env.Now()
		if err := r.env.Jellyfin.MediaUpdated(ctx, pending...); err != nil {
			r.env.Log.Warn("jellyfin not told; kept queued", "event", "jellyfin_error", "files", len(pending), "err", err)
		} else {
			pending = nil
		}
	}
	if err := r.env.Store.PutJSON(keep, name, "jellyfin_pending", pending); err != nil {
		r.env.Log.Error("jellyfin queue not saved", "event", "store_error", "err", err)
	}
}
