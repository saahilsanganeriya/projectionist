// Package watcher is the real-time trigger: it follows the library and finished-download folders with inotify and,
// once a changed folder has been quiet for a while, asks the scheduler to run the stages that act on new files.
//
//	inotify event ─┐
//	               ├─► unit pending ─► quiet for `quiet`? ─► content differs from last trigger? ─► Trigger(each stage)
//	sweep (walk) ──┘        ▲  busy: names/sizes/mtimes still moving
//	                        └──────────────────────────┘
//
// A unit is a root's direct child: one movie folder, one whole TV show, one torrent folder or loose file. A unit is
// QUIET when the names, sizes and mtimes under it have not changed for `quiet`, so a long SMB or rsync copy keeps it
// busy. In-progress files (dot files, *.part, *.!qB) count as busy but never start a unit; @eaDir, dot directories
// (.details) and the configured names are neither watched nor counted. A quiet unit triggers only when its content
// signature (in-progress files and subtitle files left out) differs from the one recorded at its last trigger, so each
// real change triggers once: not .DS_Store, not .details, not a subtitle, not the sweep re-finding what inotify already
// handled.
//
// Each run is one sweep period: walk every root (watch each folder; queue units that are new, gone, or hold a folder
// modified since the last sweep), follow events until `sweep` has passed, then return, so /health sees alerts every
// period; with every <= sweep the scheduler starts the next period at once. The first sweep of a root records its
// units without triggering. A kernel queue overflow (lost events) sweeps at once. Running out of inotify watches
// raises an alert and leaves the unwatched folders to the sweep. Known gap: a file rewritten in place, with no
// folder change, is seen only by inotify.
//
// Stages are triggered by name only: each is a reconciler that finds its own work, so a stage's own writes (a rename)
// cause at most one more no-op round; a subtitle the subtitles stage writes causes none.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

const (
	name    = "watcher"
	missing = "missing" // signature of a unit that no longer exists
	// DSM's kernel stamps files from its coarse tick clock, so a sweep looks back this far past its cutoff;
	// anything re-found that way is filtered by the content signature.
	slack = 2 * time.Second
)

var errWatchLimit = errors.New("inotify watch limit reached")

var subtitleExt = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sub": true, ".idx": true, ".smi": true}

type Options struct {
	Roots           []string      `toml:"roots" required:"the folders to watch (the films, the shows, the finished downloads)"`
	Trigger         []string      `toml:"trigger"` // stages to trigger for each quiet, changed unit
	Quiet           time.Duration `toml:"quiet"`
	Sweep           time.Duration `toml:"sweep"`            // length of one run = the safety sweep's cadence
	Ignore          []string      `toml:"ignore"`           // names neither watched nor counted, at any depth
	PartialSuffixes []string      `toml:"partial_suffixes"` // in-progress files: busy, never a change on their own
}

func defaults() Options {
	return Options{
		Trigger:         []string{"rename", "details", "subtitles", "posters", "collections", "jfsync"},
		Quiet:           3 * time.Minute,
		Sweep:           15 * time.Minute,
		Ignore:          []string{"@eaDir", "#recycle", "#snapshot", "_archive", "_assets"},
		PartialSuffixes: []string{".part", ".!qB"},
	}
}

func (Stage) Check(decode func(any) error) []config.Problem {
	opt := defaults()
	ps, ok := config.CheckStage(name, decode, &opt)
	if ok && opt.Quiet <= 0 {
		ps = append(ps, config.Problem{Key: "stages.watcher.quiet", Want: "must be longer than 0"})
	}
	if ok && opt.Sweep <= 0 {
		ps = append(ps, config.Problem{Key: "stages.watcher.sweep", Want: "must be longer than 0"})
	}
	return ps
}

// source is a set of directory watches; inotify on Linux, a fake in tests.
type source interface {
	Add(dir string) error // errWatchLimit when the kernel's per-user watch limit is reached
	Events() <-chan []event
	Len() int // watches held
	Close() error
}

type event struct {
	Path     string
	Dir      bool // the entry is a directory
	New      bool // created or moved in: a new directory needs watches of its own
	Overflow bool // the kernel dropped events
}

type Stage struct {
	open func() (source, error) // nil: the platform's inotify
}

func (Stage) Name() string { return name }

type pending struct {
	Busy    string    `json:"busy"`
	QuietAt time.Time `json:"quiet_at"`
	Source  string    `json:"source"` // what found it: inotify or sweep
	dirty   bool      // changed since Busy was taken
}

// state is kept between runs; the per-unit signatures live in the watcher_units table.
type state struct {
	LastSweep map[string]time.Time `json:"last_sweep"` // per root: start of its last complete walk
	Pending   map[string]*pending  `json:"pending"`
}

type watcher struct {
	env   *stage.Env
	rep   *stage.Report
	opt   Options
	src   source
	full  bool              // the watch limit was hit this run
	known map[string]string // unit → content signature at its last trigger
	st    state
}

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if len(opt.Roots) == 0 || opt.Quiet <= 0 || opt.Sweep <= 0 {
		return errors.New("watcher: roots, quiet and sweep must be set")
	}
	for i, r := range opt.Roots {
		opt.Roots[i] = filepath.Clean(r)
	}
	open := s.open
	if open == nil {
		open = openInotify
	}
	src, err := open()
	if err != nil {
		return err
	}
	defer src.Close()
	w := &watcher{env: env, rep: rep, opt: opt, src: src}
	if err := w.load(ctx); err != nil {
		return err
	}
	start := env.Now()
	err = w.sweep(ctx, start)
	if err == nil {
		err = w.follow(ctx, start.Add(opt.Sweep))
	}
	if err != nil && ctx.Err() == nil {
		return err
	}
	rep.Summary = fmt.Sprintf("%d watches; %d units quiet; %d pending", src.Len(), rep.Counts["units_quiet"], len(w.st.Pending))
	return env.Store.PutJSON(context.WithoutCancel(ctx), name, "state", w.st)
}

func (w *watcher) load(ctx context.Context) error {
	db := w.env.Store.DB()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS watcher_units (unit TEXT PRIMARY KEY, sig TEXT NOT NULL, at TEXT NOT NULL)`); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT unit, sig FROM watcher_units`)
	if err != nil {
		return err
	}
	defer rows.Close()
	w.known = map[string]string{}
	for rows.Next() {
		var unit, sig string
		if err := rows.Scan(&unit, &sig); err != nil {
			return err
		}
		w.known[unit] = sig
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := w.env.Store.GetJSON(ctx, name, "state", &w.st); err != nil {
		return err
	}
	if w.st.LastSweep == nil {
		w.st.LastSweep = map[string]time.Time{}
	}
	if w.st.Pending == nil {
		w.st.Pending = map[string]*pending{}
	}
	return nil
}

// record stores a unit's content signature (or forgets a unit that is gone).
func (w *watcher) record(ctx context.Context, unit, sig string) error {
	db := w.env.Store.DB()
	if sig == missing {
		delete(w.known, unit)
		_, err := db.ExecContext(ctx, `DELETE FROM watcher_units WHERE unit=?`, unit)
		return err
	}
	w.known[unit] = sig
	_, err := db.ExecContext(ctx, `INSERT INTO watcher_units(unit, sig, at) VALUES (?, ?, ?)
		ON CONFLICT(unit) DO UPDATE SET sig=excluded.sig, at=excluded.at`, unit, sig, w.env.Now().UTC().Format(store.TimeFormat))
	return err
}

// ---- classification and signatures ----

type kind int

const (
	ignored kind = iota
	partial      // in-progress file: keeps a pending unit busy, never starts one
	counted
)

func (o *Options) nameKind(base string, isDir bool) kind {
	switch {
	case slices.Contains(o.Ignore, base), isDir && strings.HasPrefix(base, "."):
		return ignored
	case strings.HasPrefix(base, "."):
		return partial
	case !isDir && slices.ContainsFunc(o.PartialSuffixes, func(s string) bool { return strings.HasSuffix(base, s) }):
		return partial
	}
	return counted
}

// unitOf maps a path to its unit (the root's direct child) and kind; unit is "" outside the roots and for a root.
func (o *Options) unitOf(path string, isDir bool) (string, kind) {
	for _, root := range o.Roots {
		rel, ok := strings.CutPrefix(path, root+"/")
		if !ok || rel == "" {
			continue
		}
		parts := strings.Split(rel, "/")
		for _, dir := range parts[:len(parts)-1] {
			if o.nameKind(dir, true) == ignored {
				return "", ignored
			}
		}
		return root + "/" + parts[0], o.nameKind(parts[len(parts)-1], isDir)
	}
	return "", ignored
}

// sigs fingerprints a unit from the names, sizes and mtimes under it (folders by name only, since a folder's mtime
// moves when @eaDir or .details appears): busy covers in-progress files too, content leaves them out, and subtitle
// files: the subtitles stage writes and retimes them beside known videos all day, and no triggered stage acts on one
// (rename and subtitles find a dropped one on their own schedule; a new Subs folder still counts, by its name).
func (o *Options) sigs(unit string) (busy, content string, err error) {
	b, c := fnv.New64a(), fnv.New64a()
	found := false
	err = filepath.WalkDir(unit, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // gone mid-walk: the next check sees the new state
			}
			return err
		}
		found = true
		k := counted
		if path != unit {
			k = o.nameKind(d.Name(), d.IsDir())
		}
		if k == ignored {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		line := path[len(unit):] + "/\n"
		if !d.IsDir() {
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			line = fmt.Sprintf("%s\x00%d\x00%d\n", path[len(unit):], info.Size(), info.ModTime().UnixNano())
		}
		b.Write([]byte(line))
		if k == counted && (d.IsDir() || !subtitleExt[strings.ToLower(filepath.Ext(d.Name()))]) {
			c.Write([]byte(line))
		}
		return nil
	})
	if !found && err == nil {
		return missing, missing, nil
	}
	return fmt.Sprintf("%016x", b.Sum64()), fmt.Sprintf("%016x", c.Sum64()), err
}

// ---- watches and the sweep ----

func (w *watcher) watch(dir string) {
	if w.full {
		return
	}
	err := w.src.Add(dir)
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist): // gone before we got to it
	case errors.Is(err, errWatchLimit):
		w.full = true
		limit, _ := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
		w.rep.Alert("inotify watch limit reached after %d watches (fs.inotify.max_user_watches=%s, shared by every process of this user): "+
			"folders past it are only seen by the %s sweep; raise it with sysctl fs.inotify.max_user_watches=65536",
			w.src.Len(), strings.TrimSpace(string(limit)), w.opt.Sweep)
	default:
		w.env.Log.Warn("watch failed", "event", "watch_error", "path", dir, "err", err)
	}
}

// watchTree watches dir and every counted folder under it (a folder created or moved in).
func (w *watcher) watchTree(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != dir && w.opt.nameKind(d.Name(), true) == ignored {
			return filepath.SkipDir
		}
		w.watch(path)
		return nil
	})
}

func mtime(d fs.DirEntry) time.Time {
	info, err := d.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// sweep walks every root (watching each counted folder before reading it, so no change slips between the two) and
// queues each unit that is new, gone, or holds a folder modified since the root's last sweep. The first sweep of a
// root records its units instead.
func (w *watcher) sweep(ctx context.Context, start time.Time) error {
	for _, root := range w.opt.Roots {
		newest := map[string]time.Time{} // unit → newest folder mtime in it (a loose file: its own)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return err
				}
				w.env.Log.Warn("walk failed", "event", "walk_error", "path", path, "err", err)
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			unit, k := w.opt.unitOf(path, d.IsDir())
			switch {
			case path == root:
				w.watch(path)
			case k != counted && d.IsDir():
				return filepath.SkipDir
			case k != counted:
			case d.IsDir():
				w.watch(path)
				if mt := mtime(d); mt.After(newest[unit]) || newest[unit].IsZero() {
					newest[unit] = mt
				}
			case unit == path:
				newest[unit] = mtime(d)
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.rep.Alert("watch root %s unreadable, not watched this period: %v", root, err)
			continue
		}
		since, seeded := w.st.LastSweep[root]
		found := 0
		for unit, mt := range newest {
			_, isKnown := w.known[unit]
			switch {
			case !seeded:
				_, content, err := w.opt.sigs(unit)
				if err == nil {
					err = w.record(ctx, unit, content)
				}
				if err != nil {
					return err
				}
			case !isKnown || !mt.Before(since.Add(-slack)):
				if w.touch(unit, "sweep", true) {
					found++
				}
			}
		}
		for unit := range w.known {
			if _, ok := newest[unit]; ok || !strings.HasPrefix(unit, root+"/") {
				continue
			}
			if !seeded {
				if err := w.record(ctx, unit, missing); err != nil {
					return err
				}
			} else if w.touch(unit, "sweep", true) {
				found++
			}
		}
		w.refresh()
		w.st.LastSweep[root] = start
		w.rep.Count("sweep_found", found)
		w.env.Log.Info("swept", "event", "sweep", "root", root, "units", len(newest), "found", found, "seeded", !seeded,
			"watches", w.src.Len(), "duration_ms", w.env.Now().Sub(start).Milliseconds())
	}
	return nil
}

// ---- following events ----

// touch marks unit as changed; create starts tracking it. A sweep re-finding a pending unit leaves it be.
// It reports whether the unit became pending.
func (w *watcher) touch(unit, source string, create bool) bool {
	p, ok := w.st.Pending[unit]
	switch {
	case !ok && !create:
		return false
	case !ok:
		p = &pending{Source: source}
		w.st.Pending[unit] = p
	case source == "sweep":
		return false
	}
	p.dirty = true
	return !ok
}

// refresh restarts the quiet period of every unit changed since its last signature.
func (w *watcher) refresh() {
	now := w.env.Now()
	for unit, p := range w.st.Pending {
		if !p.dirty {
			continue
		}
		busy, _, err := w.opt.sigs(unit)
		if err != nil {
			w.env.Log.Warn("signature failed", "event", "sig_error", "unit", unit, "err", err)
		}
		p.Busy, p.QuietAt, p.dirty = busy, now.Add(w.opt.Quiet), false
	}
}

func (w *watcher) follow(ctx context.Context, until time.Time) error {
	for {
		now := w.env.Now()
		if !now.Before(until) {
			return nil
		}
		wake := until
		for _, p := range w.st.Pending {
			if p.QuietAt.Before(wake) {
				wake = p.QuietAt
			}
		}
		timer := time.NewTimer(wake.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case batch, ok := <-w.src.Events():
			timer.Stop()
			if !ok {
				return errors.New("watcher: inotify stopped delivering events")
			}
			if err := w.handle(ctx, batch); err != nil {
				return err
			}
		case <-timer.C:
			w.settleQuiet(ctx)
		}
	}
}

func (w *watcher) handle(ctx context.Context, batch []event) error {
	overflow := false
	for _, ev := range batch {
		w.rep.Count("events", 1)
		if ev.Overflow {
			overflow = true
			continue
		}
		unit, k := w.opt.unitOf(ev.Path, ev.Dir)
		if k == ignored {
			continue
		}
		if ev.Dir && ev.New {
			w.watchTree(ev.Path)
		}
		w.touch(unit, "inotify", k == counted)
	}
	w.refresh()
	if !overflow {
		return nil
	}
	w.rep.Count("overflows", 1)
	w.env.Log.Warn("inotify queue overflowed; sweeping now", "event", "inotify_overflow")
	return w.sweep(ctx, w.env.Now())
}

// settleQuiet triggers every unit whose quiet period ended with its files unchanged.
func (w *watcher) settleQuiet(ctx context.Context) {
	now := w.env.Now()
	for unit, p := range w.st.Pending {
		if p.QuietAt.After(now) {
			continue
		}
		busy, content, err := w.opt.sigs(unit)
		if err != nil {
			w.env.Log.Warn("signature failed", "event", "sig_error", "unit", unit, "err", err)
		}
		if err != nil || busy != p.Busy {
			p.Busy, p.QuietAt = busy, now.Add(w.opt.Quiet)
			continue
		}
		delete(w.st.Pending, unit)
		if old, ok := w.known[unit]; ok && old == content || !ok && content == missing {
			w.rep.Count("unchanged", 1)
			continue
		}
		var triggered, idle []string
		for _, s := range w.opt.Trigger {
			if w.env.Trigger(s) {
				triggered = append(triggered, s)
			} else {
				idle = append(idle, s)
			}
		}
		vanished := content == missing
		w.rep.Count("units_quiet", 1)
		w.env.Log.Info("unit quiet", "event", "unit_quiet", "unit", unit, "source", p.Source, "vanished", vanished,
			"triggered", strings.Join(triggered, ","), "not_scheduled", strings.Join(idle, ","))
		err = w.env.Store.AddEvent(ctx, now, name, unit, "quiet", map[string]any{"source": p.Source, "vanished": vanished, "triggered": triggered})
		if err == nil {
			err = w.record(ctx, unit, content)
		}
		if err != nil {
			w.env.Log.Error("store failed", "event", "store_error", "unit", unit, "err", err)
		}
	}
}
