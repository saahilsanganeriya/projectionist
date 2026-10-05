// Package details keeps "<dir>/.details/<video file>.txt" true for every video under the library's naming
// scheme: what the file is, read from the file with ffprobe, and where it came from, read from the rename logs.
// Port of rename/write_details_files.py, which replaced write_details.py (Plex's API, which timed out during scans).
//
// A reconciler: each run walks the library, renders every note from cached facts and rewrites only the notes
// whose text changed. New ffprobe runs, peak-bitrate scans and decode checks are bounded per run and cached by
// (path, size, mtime), so a run resumes where the last one stopped; the newest files go first. Heavy work (the
// whole-file packet scan, the hole probe and decode samples) comes after every note is in place, so a 45-minute
// scan never holds back a new file's note, and runs only while nobody but the owner is watching. Every ffprobe and
// ffmpeg it starts runs at nice 19 and the lowest best-effort I/O priority (see background).
//
// Facts other stages can query:
//
//	probe_cache.info   JSON per file, e.g. json_extract(info, '$.video.hdr10plus'), '$.video.dv_profile', '$.chapters'
//	details_files      status/error (unreadable files), peak_kbps/peak_at_s, clean/clean_detail, per (path, size, mtime_ns)
package details

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

const name = "details"

type Options struct {
	Library  string   `toml:"library"`
	Sections []string `toml:"sections"` // folders under Library that hold the scheme
	// Rename logs, oldest first (TSV: time, op, old path, new path, unit). A missing one is skipped, but a run
	// that reads no move row at all fails: every note would lose its original name. [] means "no logs".
	UndoLogs []string `toml:"undo_logs"`
	// Per-run bounds; files already probed or scanned cost nothing.
	MaxProbes       int   `toml:"max_probes"`
	MaxPeakScans    int   `toml:"max_peak_scans"`
	MaxDecodeChecks int   `toml:"max_decode_checks"`
	PeakAboveKbps   int64 `toml:"peak_above_kbps"` // only files whose average bitrate is above this get a peak scan
	// An unreadable verdict is retried after this long, in case ffprobe failed for a passing reason.
	RetryUnreadable time.Duration `toml:"retry_unreadable"`
	// NotesRoot, when set, writes notes into a mirror of the library under it instead of beside the videos
	// (dry runs and gates). PathsFile, when set, limits the run to the videos it lists, one path per line.
	NotesRoot string `toml:"notes_root"`
	PathsFile string `toml:"paths_file"`
}

func defaults() Options {
	return Options{
		Library:  "/volume1/media",
		Sections: []string{"Movies", "TV"},
		// The Python renamers' logs, the copies the rename stage keeps of them, then its own log.
		UndoLogs: []string{"/volume1/PlexMediaServer/rename/undo.tsv", "/volume1/PlexMediaServer/rename/undo_auto.tsv",
			"/volume1/media/.projectionist/rename/legacy-undo.tsv", "/volume1/media/.projectionist/rename/legacy-undo_auto.tsv",
			"/volume1/media/.projectionist/rename/undo.tsv"},
		MaxProbes:       400,
		MaxPeakScans:    2,
		MaxDecodeChecks: 60,
		PeakAboveKbps:   20000,
		RetryUnreadable: 24 * time.Hour,
	}
}

const schema = `CREATE TABLE IF NOT EXISTS details_files (
  path         TEXT PRIMARY KEY,
  size         INTEGER NOT NULL,
  mtime_ns     INTEGER NOT NULL,
  status       TEXT NOT NULL,            -- ok | unreadable
  error        TEXT NOT NULL DEFAULT '', -- why it is unreadable
  peak_kbps    INTEGER,                  -- highest 10 s bitrate over all streams; NULL = not scanned, 0 = scan failed
  peak_at_s    INTEGER,                  -- where that window starts
  clean        INTEGER,                  -- 1 = start, middle and end decode cleanly; 0 = not; NULL = not checked
  clean_detail TEXT NOT NULL DEFAULT '',
  checked_at   TEXT NOT NULL,            -- when status was last decided (UTC)
  note_sha     TEXT NOT NULL DEFAULT '', -- the note as last written or confirmed, and its size/mtime then:
  note_size    INTEGER NOT NULL DEFAULT 0, -- a run only stats a note whose text is unchanged (a read costs
  note_mtime   INTEGER NOT NULL DEFAULT 0  -- ~100x a stat on the busy NAS)
)`

// fileRow is one details_files row. A row whose size or mtime no longer match the file is forgotten.
type fileRow struct {
	Size, MTime int64
	Status      string
	Error       string
	PeakKbps    sql.NullInt64
	PeakAt      sql.NullInt64
	Clean       sql.NullBool
	CleanDetail string
	CheckedAt   time.Time
	NoteSHA     string
	NoteSize    int64
	NoteMTime   int64
}

type video struct {
	path        string
	identity    string // "tmdb-949", from the scheme folder the file lives under
	size, mtime int64
}

var (
	schemeFolder = regexp.MustCompile(`^(.+) \((\d{4})\) \{((?:tmdb|tvdb|imdb)-[^}]+)\}$`)
	videoExt     = map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".m4v": true, ".mov": true, ".ts": true, ".wmv": true, ".mpg": true, ".webm": true}
	subtitleExt  = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sub": true, ".idx": true}
)

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	// A missing ffprobe would otherwise mark every video unreadable.
	if _, err := os.Stat(env.Probe.FFprobe); err != nil {
		return fmt.Errorf("ffprobe: %w", err)
	}
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	orig, moves, err := originals(opt.UndoLogs)
	if err != nil {
		return err
	}
	if len(opt.UndoLogs) > 0 && moves == 0 {
		return fmt.Errorf("no move row in any rename log (%s): every note would lose its original name", strings.Join(opt.UndoLogs, ", "))
	}
	videos, subtitles, err := walk(opt.Library, opt.Sections)
	if err != nil {
		return err
	}
	full := opt.PathsFile == ""
	if !full {
		if videos, err = only(videos, opt.PathsFile); err != nil {
			return err
		}
	}
	sort.SliceStable(videos, func(i, j int) bool { return videos[i].mtime > videos[j].mtime }) // new arrivals first
	rows, err := loadRows(ctx, db)
	if err != nil {
		return err
	}

	r := &run{env: env, opt: opt, rep: rep, db: db, orig: orig, subtitles: subtitles,
		probes: opt.MaxProbes, peaks: opt.MaxPeakScans, decodes: opt.MaxDecodeChecks, heavyOK: true}
	seen := map[string]bool{}
	for _, v := range videos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seen[v.path] = true
		if err := r.one(ctx, v, rows[v.path]); err != nil {
			return err
		}
	}
	for _, p := range r.queue {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		before := *p.row
		if err := r.heavyFacts(ctx, p.v, p.info, p.row); err != nil {
			return err
		}
		if *p.row != before {
			if err := r.note(p.v, p.info, p.row); err != nil {
				return err
			}
			if err := r.save(ctx, p.v.path, p.row); err != nil {
				return err
			}
		}
	}
	if full {
		for path := range rows {
			if !seen[path] {
				if _, err := db.ExecContext(ctx, `DELETE FROM details_files WHERE path=?`, path); err != nil {
					return err
				}
			}
		}
	}
	for _, bad := range []struct {
		what  string
		paths []string
	}{{"newly unreadable", r.newUnreadable}, {"newly failing the decode check", r.newUnclean}} {
		if len(bad.paths) > 0 {
			rep.Alert("%d video(s) %s (details_files): %s", len(bad.paths), bad.what, strings.Join(first(bad.paths, 5), "; "))
		}
	}
	c := rep.Counts
	rep.Summary = fmt.Sprintf("%d videos: %d notes written, %d probed (%d deferred), %d unreadable, %d peak scans, %d decode checks (%d not clean), %d heavy deferred",
		len(videos), c["written"], c["probed"], c["probe_deferred"], c["unreadable"], c["peak_scanned"], c["decode_checked"], c["not_clean"], c["heavy_deferred"])
	return nil
}

type run struct {
	env       *stage.Env
	opt       Options
	rep       *stage.Report
	db        *sql.DB
	orig      map[string]string
	subtitles map[string][]string // dir → the subtitle files in it, from the walk

	probes, peaks, decodes int  // what is left of this run's bounds
	heavyOK                bool // false once a guest was seen: heavy work waits for a later run
	queue                  []pending
	newUnreadable          []string
	newUnclean             []string
}

// pending is a video whose heavy facts are missing, queued behind the notes pass.
type pending struct {
	v    video
	info probe.Info
	row  *fileRow
}

func (r *run) needsPeak(info probe.Info, row *fileRow) bool {
	return info.BitRate > r.opt.PeakAboveKbps*1000 && !row.PeakKbps.Valid
}

func (r *run) one(ctx context.Context, v video, row *fileRow) error {
	if row == nil || row.Size != v.size || row.MTime != v.mtime {
		row = &fileRow{Size: v.size, MTime: v.mtime} // new or changed file: earlier verdicts (and note state) no longer apply
	}
	before := *row
	info, ok, err := r.env.Probe.Lookup(ctx, v.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // moved away since the walk
	}
	if err != nil {
		return err
	}
	if !ok {
		retry := row.Status == "unreadable" && r.env.Now().Sub(row.CheckedAt) < r.opt.RetryUnreadable
		switch {
		case retry:
			r.rep.Count("unreadable", 1)
			return nil
		case r.probes <= 0:
			r.rep.Count("probe_deferred", 1)
			return nil
		}
		r.probes--
		r.rep.Count("probed", 1)
		background(func() { info, err = r.env.Probe.Probe(ctx, v.path) })
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err == nil && info.Video == nil {
		err = errors.New("no video stream")
	}
	if err != nil {
		if row.Status != "unreadable" {
			r.newUnreadable = append(r.newUnreadable, v.path)
			r.env.Log.Warn("video unreadable", "event", "video_unreadable", "item", v.path, "err", err)
		}
		row.Status, row.Error, row.CheckedAt = "unreadable", err.Error(), r.env.Now()
		r.rep.Count("unreadable", 1)
		return r.save(ctx, v.path, row)
	}
	if row.Status != "ok" {
		row.Status, row.Error, row.CheckedAt = "ok", "", r.env.Now()
	}
	if err := r.note(v, info, row); err != nil {
		return err
	}
	if (r.needsPeak(info, row) || !row.Clean.Valid) && len(r.queue) < r.opt.MaxPeakScans+r.opt.MaxDecodeChecks {
		r.queue = append(r.queue, pending{v, info, row})
	}
	if *row != before {
		return r.save(ctx, v.path, row)
	}
	return nil
}

// note brings the note in line with the file. When the text is what the row says was last written and the note
// still has that size and mtime, a stat suffices; otherwise the note is read, compared and rewritten if needed.
func (r *run) note(v video, info probe.Info, row *fileRow) error {
	text := render(v, info, r.original(v.path), r.sidecarsOf(v.path), row)
	sum := sha256.Sum256([]byte(text))
	sha := hex.EncodeToString(sum[:])
	path := notePath(r.opt, v.path)
	st, err := os.Stat(path)
	if err == nil && sha == row.NoteSHA && st.Size() == row.NoteSize && st.ModTime().UnixNano() == row.NoteMTime {
		return nil
	}
	written, err := writeNote(path, text)
	if err != nil {
		return err
	}
	if written {
		r.rep.Count("written", 1)
		r.env.Log.Debug("note written", "event", "note_written", "item", v.path)
	}
	if st, err = os.Stat(path); err != nil {
		return err
	}
	row.NoteSHA, row.NoteSize, row.NoteMTime = sha, st.Size(), st.ModTime().UnixNano()
	return nil
}

// heavyFacts fills in the peak bitrate (big files only) and the decode check, within this run's bounds.
func (r *run) heavyFacts(ctx context.Context, v video, info probe.Info, row *fileRow) error {
	if r.needsPeak(info, row) && r.peaks > 0 {
		if !r.heavyOK {
			r.rep.Count("heavy_deferred", 1)
		} else {
			r.peaks--
			var kbps int64
			var at int
			done, err := r.heavy(ctx, func(ctx context.Context) (err error) {
				kbps, at, err = peakBitrate(ctx, r.env.FFmpeg.FFprobePath(), v.path)
				return err
			})
			switch {
			case !done && err == nil:
				r.heavyOK = false
				r.rep.Count("heavy_deferred", 1)
			case err != nil && ctx.Err() != nil:
				return ctx.Err()
			case err != nil:
				r.env.Log.Warn("peak scan failed", "event", "peak_failed", "item", v.path, "err", err)
				row.PeakKbps, row.PeakAt = sql.NullInt64{Valid: true}, sql.NullInt64{} // 0 = failed; retried when the file changes
			default:
				row.PeakKbps, row.PeakAt = sql.NullInt64{Int64: kbps, Valid: true}, sql.NullInt64{Int64: int64(at), Valid: true}
				r.rep.Count("peak_scanned", 1)
				r.env.Log.Info("peak scanned", "event", "peak_scanned", "item", v.path, "peak_kbps", kbps, "at_s", at)
			}
		}
	}
	if !row.Clean.Valid && r.decodes > 0 {
		if !r.heavyOK {
			r.rep.Count("heavy_deferred", 1)
			return nil
		}
		r.decodes--
		var clean bool
		var detail string
		done, err := r.heavy(ctx, func(ctx context.Context) (err error) {
			clean, detail, err = decodeCheck(ctx, r.env.FFmpeg, v.path, info.Video, info.Duration)
			return err
		})
		switch {
		case !done && err == nil:
			r.heavyOK = false
			r.rep.Count("heavy_deferred", 1)
		case err != nil && gone(v.path):
			return nil // moved away since the walk (a missing ffmpeg is an ErrNotExist too, and must fail)
		case err != nil:
			return err // the decoder could not be run at all, or the stage is stopping
		default:
			row.Clean, row.CleanDetail = sql.NullBool{Bool: clean, Valid: true}, detail
			r.rep.Count("decode_checked", 1)
			if !clean {
				r.newUnclean = append(r.newUnclean, v.path)
				r.rep.Count("not_clean", 1)
				r.env.Log.Warn("decode check failed", "event", "decode_unclean", "item", v.path, "detail", detail)
			}
		}
	}
	return nil
}

// heavy runs fn only while nobody but the owner is watching, and cancels it when a guest starts. done is false
// (with a nil error) when the work must wait for a later run; an unknown viewer count counts as a guest.
func (r *run) heavy(ctx context.Context, fn func(context.Context) error) (done bool, err error) {
	if n, err := r.env.Viewers.Count(ctx); err != nil || n > 0 {
		r.env.Log.Info("heavy work deferred", "event", "heavy_deferred", "others", n, "err", err)
		return false, nil
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(r.env.Viewers.Poll)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				if n, err := r.env.Viewers.Count(hctx); hctx.Err() == nil && (err != nil || n > 0) {
					r.env.Log.Info("heavy work interrupted", "event", "heavy_interrupted", "others", n, "err", err)
					cancel()
					return
				}
			}
		}
	}()
	background(func() { err = fn(hctx) })
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case hctx.Err() != nil:
		return false, nil
	}
	return true, err
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func (r *run) original(path string) string {
	if o, ok := r.orig[path]; ok {
		return o
	}
	return path
}

// sidecarsOf lists the subtitle files beside a video that share its stem, as the suffix after "<stem>."
// ("en.srt", "en.sdh.srt"), sorted.
func (r *run) sidecarsOf(path string) []string {
	prefix := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + "."
	var out []string
	for _, n := range r.subtitles[filepath.Dir(path)] {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n[len(prefix):])
		}
	}
	sort.Strings(out)
	return out
}

func (r *run) save(ctx context.Context, path string, row *fileRow) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO details_files(path, size, mtime_ns, status, error, peak_kbps, peak_at_s, clean, clean_detail, checked_at,
		  note_sha, note_size, note_mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, status=excluded.status, error=excluded.error,
		  peak_kbps=excluded.peak_kbps, peak_at_s=excluded.peak_at_s, clean=excluded.clean, clean_detail=excluded.clean_detail, checked_at=excluded.checked_at,
		  note_sha=excluded.note_sha, note_size=excluded.note_size, note_mtime=excluded.note_mtime`,
		path, row.Size, row.MTime, row.Status, row.Error, row.PeakKbps, row.PeakAt, row.Clean, row.CleanDetail, row.CheckedAt.UTC().Format(time.RFC3339Nano),
		row.NoteSHA, row.NoteSize, row.NoteMTime)
	return err
}

func loadRows(ctx context.Context, db *sql.DB) (map[string]*fileRow, error) {
	q, err := db.QueryContext(ctx, `SELECT path, size, mtime_ns, status, error, peak_kbps, peak_at_s, clean, clean_detail, checked_at,
		note_sha, note_size, note_mtime FROM details_files`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	rows := map[string]*fileRow{}
	for q.Next() {
		var path, checked string
		row := &fileRow{}
		if err := q.Scan(&path, &row.Size, &row.MTime, &row.Status, &row.Error, &row.PeakKbps, &row.PeakAt, &row.Clean, &row.CleanDetail, &checked,
			&row.NoteSHA, &row.NoteSize, &row.NoteMTime); err != nil {
			return nil, err
		}
		if row.CheckedAt, err = time.Parse(time.RFC3339Nano, checked); err != nil {
			return nil, err
		}
		rows[path] = row
	}
	return rows, q.Err()
}

// walk lists every video below a scheme folder ("Title (Year) {tmdb-N}") of each section: the main files, season
// folders and extras alike, and the subtitle files by folder. Dot and @ entries (.details, @eaDir) are skipped; a
// file that vanishes mid-walk is too.
func walk(library string, sections []string) ([]video, map[string][]string, error) {
	var out []video
	subtitles := map[string][]string{}
	for _, sec := range sections {
		tops, err := os.ReadDir(filepath.Join(library, sec))
		if err != nil {
			return nil, nil, err
		}
		for _, top := range tops {
			m := schemeFolder.FindStringSubmatch(top.Name())
			if m == nil || !top.IsDir() {
				continue
			}
			root := filepath.Join(library, sec, top.Name())
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if n := d.Name(); p != root && (n[0] == '.' || n[0] == '@') {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				ext := strings.ToLower(filepath.Ext(p))
				if !d.IsDir() && subtitleExt[ext] {
					subtitles[filepath.Dir(p)] = append(subtitles[filepath.Dir(p)], d.Name())
				}
				if d.IsDir() || !videoExt[ext] {
					return nil
				}
				fi, err := d.Info()
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				out = append(out, video{path: p, identity: m[3], size: fi.Size(), mtime: fi.ModTime().UnixNano()})
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return out, subtitles, nil
}

func only(videos []video, pathsFile string) ([]video, error) {
	raw, err := os.ReadFile(pathsFile)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		want[l] = true
	}
	var out []video
	for _, v := range videos {
		if want[v.path] {
			out = append(out, v)
		}
	}
	return out, nil
}

// originals maps each moved path to the first name it had: logs are read in order, so a file moved twice keeps
// the name from before its first move. A row read before is skipped: the rename stage's legacy-undo*.tsv are
// copies of the Python logs, and replaying a move breaks a chain once its path was reused (P→Q, then R→P: a second
// P→Q would give Q the original name R). A missing log is skipped (one appears on first use); moves counts the
// distinct move rows.
func originals(logs []string) (orig map[string]string, moves int, err error) {
	orig = map[string]string{}
	seen := map[string]bool{}
	for _, path := range logs {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			col := strings.Split(line, "\t")
			if len(col) != 5 || col[1] != "move" || seen[line] {
				continue
			}
			seen[line] = true
			moves++
			if first, ok := orig[col[2]]; ok {
				orig[col[3]] = first
			} else {
				orig[col[3]] = col[2]
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	return orig, moves, nil
}

func notePath(opt Options, videoPath string) string {
	dir := filepath.Dir(videoPath)
	if opt.NotesRoot != "" {
		if rel, err := filepath.Rel(opt.Library, dir); err == nil {
			dir = filepath.Join(opt.NotesRoot, rel)
		}
	}
	return filepath.Join(dir, ".details", filepath.Base(videoPath)+".txt")
}

// writeNote replaces the note only when its text changed: a temp file in .details, then one rename. The folder
// is made with a plain mkdir, which inherits the share's Synology ACL (a chmod would strip it).
func writeNote(path, text string) (written bool, err error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == text {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	os.Remove(tmp) // left by a run that died mid-write
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
	}
	return s
}
