// Package subtitles keeps an English text subtitle beside every movie and episode (port of scripts/sub_stats.py and
// scripts/subfetch2.py). It is a reconciler: each run walks the library and, for each video still lacking an English
// sidecar, does only the missing step.
//
//	embedded English text track       -> extracted with ffmpeg
//	else candidates from the providers (OpenSubtitles legacy REST; OpenSubtitles.com, Subdl with keys), best first:
//	  English PGS/VobSub track exists  -> cue starts fitted to its caption starts (align/retime, ported exactly)
//	  else                             -> alass aligns the candidate to the speech in the audio
//	accepted                           -> <video>.en.srt, else .eng.srt: created O_EXCL, never overwritten
//
// Titles without English audio go first, then those with an English image track. Three titles at a time, at idle CPU
// and I/O priority, each waiting while anyone but the owner is watching. Every attempt is an event; a title that got
// no subtitle is tried again only after retry_after. Each run also rewrites the sub_stats TSV (the rename planner's
// input). Jellyfin hears of each new sidecar; Plex watches the disk.
package subtitles

import (
	"bytes"
	"context"
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

	"projectionist/internal/fsx"
	"projectionist/internal/library"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

const (
	name       = "subtitles"
	userAgent  = "projectionist v0.1"
	maxTries   = 4    // candidates downloaded per title
	alassAgree = 0.75 // alass: share of cues that must sit within 0.5 s of the median shift (6 of 8 chunks in align)
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
}

func defaults() Options {
	return Options{
		Roots:      []string{"/volume1/media/Movies", "/volume1/media/TV"},
		StatsFile:  "/state/sub_stats.tsv",
		RetryAfter: 7 * 24 * time.Hour,
		Workers:    3,
		SecretsDir: "/run/secrets",
		Alass:      "/usr/local/bin/alass",
	}
}

// Stage's fields are test hooks; the zero value talks to the real services.
type Stage struct {
	openSubtitles, openSubtitlesCom, subdlAPI, subdlDL, tmdbAPI string
	sleep                                                       func(context.Context, time.Duration) error
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
);`

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true,
	".m2ts": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".webm": true}

type title struct {
	root, path string
	kind, id   string // TMDB kind ("movie" | "tv") and id; kind "imdb" when the folder names the IMDb id
	show       string // set for an episode
	season     int
	episode    int
	info       probe.Info
}

type run struct {
	env       *stage.Env
	opt       Options
	providers []Provider
	tmdb      *tmdb

	mu      sync.Mutex
	rep     *stage.Report
	written []string
	alerted map[string]bool
}

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if _, err := env.Store.DB().ExecContext(ctx, schema); err != nil {
		return err
	}
	r := s.newRun(env, opt, rep)
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
	r.tellJellyfin(ctx)
	rep.Summary = fmt.Sprintf("%d sidecar(s) written; %d title(s) lacked one, %d were due", len(r.written), len(titles), len(due))
	return ctx.Err()
}

func (s Stage) newRun(env *stage.Env, opt Options, rep *stage.Report) *run {
	sleep, hc := s.sleep, &http.Client{Timeout: 60 * time.Second}
	// The legacy API's redirects lead to host "_"; a redirect is an answer, never a hop to follow.
	noRedirect := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if sleep == nil {
		sleep = realSleep
	}
	newPacer := func() *pacer { return &pacer{gap: time.Second, now: env.Now, sleep: sleep} }
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	r := &run{env: env, opt: opt, rep: rep, alerted: map[string]bool{},
		tmdb: &tmdb{base: or(s.tmdbAPI, "https://api.themoviedb.org/3"), tokenFile: filepath.Join(opt.SecretsDir, "tmdb_read_token"),
			hc: &http.Client{Timeout: 20 * time.Second}, cache: map[string]int{}}}
	r.providers = []Provider{&legacy{base: or(s.openSubtitles, "https://rest.opensubtitles.org/search/"), userAgent: userAgent, hc: noRedirect, pace: newPacer()}}
	if key := filepath.Join(opt.SecretsDir, "opensubtitles_api_key"); fileExists(key) {
		r.providers = append(r.providers, &dotCom{base: or(s.openSubtitlesCom, "https://api.opensubtitles.com/api/v1"), keyFile: key,
			userAgent: userAgent, hc: hc, pace: newPacer()})
	}
	if key := filepath.Join(opt.SecretsDir, "subdl_api_key"); fileExists(key) {
		r.providers = append(r.providers, &subdl{api: or(s.subdlAPI, "https://api.subdl.com/api/v1/subtitles"),
			dl: or(s.subdlDL, "https://dl.subdl.com"), keyFile: key, hc: hc, pace: newPacer()})
	}
	return r
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

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
	last := map[string]time.Time{}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT path, last_try FROM subtitles_titles`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, at string
		if err := rows.Scan(&p, &at); err != nil {
			rows.Close()
			return nil, err
		}
		last[p], _ = time.Parse(store.TimeFormat, at)
	}
	rows.Close()
	var due []*title
	for _, t := range titles {
		if at, ok := last[t.path]; ok && r.env.Now().Before(at.Add(r.opt.RetryAfter)) {
			continue
		}
		info, err := r.env.Probe.Probe(ctx, t.path)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r.env.Log.Warn("probe failed", "event", "probe_error", "item", t.path, "err", err)
			r.count("probe_failed")
			continue
		}
		t.info = info
		due = append(due, t)
	}
	key := func(t *title) (bool, bool) { return hasEnglishAudio(t.info), len(imageTracks(t.info)) == 0 }
	sort.SliceStable(due, func(i, j int) bool { // non-English audio first, then titles with an image reference
		ai, ri := key(due[i])
		aj, rj := key(due[j])
		if ai != aj {
			return !ai
		}
		return !ri && rj
	})
	if r.opt.MaxTitles > 0 && len(due) > r.opt.MaxTitles {
		due = due[:r.opt.MaxTitles]
	}
	return due, nil
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

// textTrack is the embedded English text track worth extracting: not forced, not commentary, full rather than SDH
// when both exist.
func textTrack(info probe.Info) *probe.Subtitle {
	var best *probe.Subtitle
	for i, s := range info.Subtitles {
		if !s.Text || !english(s.Language) || s.Forced || strings.Contains(strings.ToLower(s.Title), "comment") {
			continue
		}
		if best == nil || best.HearingImpaired && !s.HearingImpaired {
			best = &info.Subtitles[i]
		}
	}
	return best
}

// ---- one title ----

// outcome is what happened to a title (or one attempt). Deferred titles are not recorded, so the next run tries again.
type outcome struct {
	result string
	detail string
	fields map[string]any
}

func (r *run) process(ctx context.Context, t *title) {
	if err := r.env.Viewers.WaitIdle(ctx); err != nil {
		return
	}
	o := r.fetch(ctx, t)
	if ctx.Err() != nil {
		return // shutting down mid-title: nothing was written, the next run starts it again
	}
	r.count(o.result)
	if o.result == "deferred" {
		r.env.Log.Info("title deferred", "event", "subtitle_deferred", "item", t.path, "detail", o.detail)
		return
	}
	r.event(ctx, t.path, o)
	if _, err := r.env.Store.DB().ExecContext(ctx, `INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(path) DO UPDATE SET outcome=excluded.outcome, detail=excluded.detail, tries=tries+1, last_try=excluded.last_try`,
		t.path, o.result, o.detail, r.env.Now().UTC().Format(store.TimeFormat)); err != nil {
		r.env.Log.Error("title record failed", "event", "store_error", "item", t.path, "err", err)
	}
}

func (r *run) fetch(ctx context.Context, t *title) outcome {
	duration := t.info.Duration
	if tr := textTrack(t.info); tr != nil {
		data, err := r.cmd(ctx, r.env.FFmpeg.FFmpegPath(), "-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path,
			"-map", fmt.Sprintf("0:%d", tr.Index), "-c:s", "srt", "-f", "srt", "-")
		if err == nil && sane(cues(string(data)), duration) {
			return r.write(t, data, "extracted", map[string]any{"stream": tr.Index, "codec": tr.Codec})
		}
		r.env.Log.Info("embedded track not usable", "event", "extract_rejected", "item", t.path, "stream", tr.Index, "err", err)
	}

	q, err := r.query(ctx, t)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	// A provider that cannot be searched only raises its alert: the others' candidates still decide, and the title is
	// recorded, so an unreachable provider does not make every run download the same failed candidates again. A
	// download refused for quota defers the title instead (nothing was tried; the quota resets daily).
	var cands []Candidate
	for _, p := range r.providers {
		found, err := p.Search(ctx, q)
		switch {
		case errors.Is(err, errParked):
			r.alertParked(p, err)
		case err != nil:
			return outcome{result: "error", detail: p.Name() + " search: " + err.Error()}
		}
		cands = append(cands, found...)
	}
	incomplete := false
	deferOr := func(o outcome) outcome {
		if incomplete {
			return outcome{result: "deferred", detail: "a download quota is spent for this run"}
		}
		return o
	}
	cands = order(cands, releaseName(t.path))
	if len(cands) == 0 {
		return deferOr(outcome{result: "no_candidates"})
	}

	ref, err := r.reference(ctx, t)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	defer ref.cleanup()
	if ref.label == "" {
		return outcome{result: "no_reference", detail: ref.why}
	}
	for _, c := range cands[:min(len(cands), maxTries)] {
		attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "reference": ref.label}
		txt, err := c.provider.Download(ctx, c)
		if errors.Is(err, errParked) {
			incomplete = true
			r.alertParked(c.provider, err)
			continue
		}
		if err != nil {
			r.event(ctx, t.path, outcome{"download_failed", err.Error(), attempt})
			continue
		}
		txt = stripAds(txt)
		attempt["cues"] = len(cues(txt))
		out, result, ok := r.fit(ctx, ref, txt, attempt)
		if !ok && c.Source == "opensubtitles-hash" && c.ours {
			// made for this very file and labelled with its title; the timing cannot tell (a laugh track fools speech)
			out, result, ok = txt, "kept", true
			attempt["reference"] = "hash-trusted"
		}
		if !ok {
			r.event(ctx, t.path, outcome{result: "rejected_misaligned", fields: attempt})
			continue
		}
		spans := cues(out)
		if !sane(spans, duration) {
			r.event(ctx, t.path, outcome{result: "rejected_length", fields: attempt})
			continue
		}
		if ref.starts != nil {
			attempt["ref_median_ms"] = math.Round(medianOffsetMS(spans, ref.starts))
		}
		return r.write(t, []byte(out), result, attempt)
	}
	return deferOr(outcome{result: "no_aligned_candidate", detail: fmt.Sprintf("%d tried", min(len(cands), maxTries))})
}

// query gathers what the providers search by: the file hash, and the IMDb id from the folder or from TMDB.
func (r *run) query(ctx context.Context, t *title) (Query, error) {
	hash, size, err := openSubtitlesHash(t.path)
	if err != nil {
		return Query{}, err
	}
	q := Query{Size: size, Hash: hash, Name: t.show, Show: t.show != "", Season: t.season, Episode: t.episode}
	if t.kind == "movie" || t.kind == "tv" {
		q.TMDB = t.id
	}
	switch t.kind {
	case "imdb":
		q.IMDb, _ = strconv.Atoi(strings.TrimPrefix(t.id, "tt"))
	case "movie", "tv":
		if q.IMDb, err = r.tmdb.imdb(ctx, t.kind, t.id); err != nil {
			r.env.Log.Warn("tmdb lookup failed", "event", "tmdb_error", "item", t.path, "err", err) // hash and name still search
		}
	}
	return q, nil
}

// reference is the timing a candidate must line up with: an English image track's caption starts, else alass's
// speech segments of the default audio track.
type reference struct {
	label  string // "pgs" | "alass" | "" (none)
	why    string
	bits   bitset
	starts []float64
	dir    string // alass work files
	speech string
}

func (ref reference) cleanup() {
	if ref.dir != "" {
		os.RemoveAll(ref.dir)
	}
}

func (r *run) reference(ctx context.Context, t *title) (reference, error) {
	for _, s := range imageTracks(t.info) {
		out, err := r.cmd(ctx, r.env.FFmpeg.FFmpegPath(), "-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path,
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
	if len(t.info.Audio) == 0 {
		return reference{why: "no usable image track and no audio"}, nil
	}
	if !fileExists(r.opt.Alass) {
		r.alertOnce("alass", "alass not found at %s: titles without an English image track get no subtitle", r.opt.Alass)
		return reference{why: "alass missing"}, nil
	}
	dir, err := os.MkdirTemp("", "subtitles-")
	if err != nil {
		return reference{}, err
	}
	ref := reference{label: "alass", dir: dir, speech: filepath.Join(dir, "speech.srt")}
	audio := t.info.Audio[0]
	for _, a := range t.info.Audio {
		if a.Default {
			audio = a
			break
		}
	}
	// "_" as the subtitle makes alass write the speech segments it found as an SRT: decoded once, reused per candidate.
	if _, err := r.cmd(ctx, r.opt.Alass, t.path, "_", ref.speech, "--index", strconv.Itoa(audio.Index)); err != nil { // the stream index
		if ctx.Err() != nil {
			ref.cleanup()
			return reference{}, ctx.Err()
		}
		ref.label, ref.why = "", "alass could not read the audio: "+err.Error()
		r.alertOnce("alass-failed", "alass failed (titles without an English image track get no subtitle): %v", err)
	}
	return ref, nil
}

var ratioRe = regexp.MustCompile(`ratio is ([0-9.]+)(?:/([0-9.]+))?`)

// fit lines a candidate up with the reference; it returns the text to write and kept|retimed, or ok=false.
func (r *run) fit(ctx context.Context, ref reference, txt string, attempt map[string]any) (string, string, bool) {
	spans := cues(txt)
	if len(spans) < minCues {
		return "", "", false
	}
	if ref.label == "pgs" {
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
	in, out := filepath.Join(ref.dir, "candidate.srt"), filepath.Join(ref.dir, "aligned.srt")
	os.Remove(out)
	if err := os.WriteFile(in, []byte(txt), 0o644); err != nil {
		return "", "", false
	}
	stdout, err := r.cmd(ctx, r.opt.Alass, ref.speech, in, out)
	if err != nil {
		return "", "", false
	}
	aligned, err := os.ReadFile(out)
	if err != nil {
		return "", "", false
	}
	ratio := 1.0
	if m := ratioRe.FindStringSubmatch(string(stdout)); m != nil && m[2] != "" {
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[2], 64)
		ratio = a / b
	}
	moved := cues(string(aligned))
	offset, agree, ok := shifts(spans, moved, ratio)
	attempt["offset_ms"], attempt["ratio"], attempt["agree"] = offset, ratio, math.Round(agree*100)/100
	if !ok || agree < alassAgree {
		return "", "", false
	}
	if slices.Equal(spans, moved) {
		return txt, "kept", true
	}
	return string(aligned), "retimed", true
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
func (r *run) write(t *title, data []byte, result string, fields map[string]any) outcome {
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
	r.written = append(r.written, dest)
	r.mu.Unlock()
	r.env.Log.Info("subtitle written", "event", "subtitle_written", "item", t.path, "result", result, "sidecar", filepath.Base(dest))
	return outcome{result: result, detail: filepath.Base(dest), fields: fields}
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

// cmd runs a helper at idle CPU and I/O priority in its own process group (so alass's ffmpeg is idle too and dies
// with it) and returns its stdout.
func (r *run) cmd(ctx context.Context, bin string, args ...string) ([]byte, error) {
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
	if err := c.Wait(); err != nil {
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

// alertParked raises an alert when a provider kept failing; a spent download quota is routine and only logged.
func (r *run) alertParked(p Provider, err error) {
	if errors.Is(err, errQuota) {
		r.env.Log.Info("provider quota spent", "event", "provider_parked", "provider", p.Name())
		return
	}
	r.alertOnce(p.Name(), "%s unavailable for the rest of the run: %v", p.Name(), err)
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

// tellJellyfin announces this run's sidecars plus any a failed earlier push left queued; Plex watches the disk.
func (r *run) tellJellyfin(ctx context.Context) {
	if r.opt.OutputDir != "" {
		return
	}
	var pending []string
	if _, err := r.env.Store.GetJSON(ctx, name, "jellyfin_pending", &pending); err != nil {
		r.env.Log.Warn("jellyfin queue unreadable", "event", "store_error", "err", err)
	}
	pending = append(pending, r.written...)
	if len(pending) == 0 {
		return
	}
	if err := r.env.Jellyfin.MediaUpdated(ctx, pending...); err != nil {
		r.env.Log.Warn("jellyfin not told; queued for the next run", "event", "jellyfin_error", "files", len(pending), "err", err)
	} else {
		pending = nil
	}
	if err := r.env.Store.PutJSON(ctx, name, "jellyfin_pending", pending); err != nil {
		r.env.Log.Warn("jellyfin queue not saved", "event", "store_error", "err", err)
	}
}
