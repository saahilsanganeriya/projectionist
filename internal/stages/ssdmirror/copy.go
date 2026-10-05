package ssdmirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"projectionist/internal/fsx"
	"projectionist/internal/stage"
)

const (
	chunk        = 4 << 20  // bulk read/write size
	sampleWindow = 64 << 10 // head and tail compared to prove a rename
	resumeWindow = 1 << 20  // head and tail of a partial copy compared before it is resumed
	statusEvery  = 30 * time.Second
	maxFailures  = 3 // consecutive failed actions before the run gives up (an unplugged SSD fails every one)
)

// progress is stored as JSON and rendered as PROGRESS.txt.
type progress struct {
	State   string    `json:"state"`
	Films   int       `json:"films"`
	First   string    `json:"first,omitempty"`
	Last    string    `json:"last,omitempty"`
	Bytes   int64     `json:"bytes"`  // to copy this run
	Copied  int64     `json:"copied"` // written this run
	Current string    `json:"current,omitempty"`
	Paused  bool      `json:"paused,omitempty"`
	PastCut int       `json:"past_cut,omitempty"` // films on the SSD after the A→Z cut: kept, no longer updated
	Started time.Time `json:"started"`
	Orphans []string  `json:"orphans,omitempty"` // SSD files whose library twin is gone and that match no archived copy
}

type mirror struct {
	ctx        context.Context
	env        *stage.Env
	opt        Options
	rep        *stage.Report
	pace       pacer
	stamp      string             // this run's folder under ssd_archive
	archive    map[int64][]string // the library's archived files by size, built on first use
	lastGate   time.Time
	lastStatus time.Time
	prog       progress
}

func (m *mirror) execute(acts []action) error {
	failures, film, filmFiles, filmBytes := 0, "", 0, int64(0)
	flush := func() {
		if film != "" && filmFiles > 0 {
			m.event(film, "copied", map[string]any{"files": filmFiles, "bytes": filmBytes})
		}
		filmFiles, filmBytes = 0, 0
	}
	for _, a := range acts {
		if a.film != film && (a.kind == "copy" || a.kind == "update") {
			flush()
			film, m.prog.Current = a.film, a.film
			m.status()
		}
		before := m.prog.Copied
		err := m.apply(a)
		switch {
		case err == nil:
			failures = 0
			if a.kind == "copy" || a.kind == "update" {
				filmFiles, filmBytes = filmFiles+1, filmBytes+m.prog.Copied-before
			}
			continue
		case m.ctx.Err() != nil:
			return m.ctx.Err()
		case errors.Is(err, syscall.ENOSPC):
			m.rep.Alert("SSD full while writing %s; the next run resumes when space frees: %v", a.dst, err)
			return nil
		case errors.Is(err, fs.ErrNotExist) && strings.HasPrefix(a.src, m.opt.Source+string(filepath.Separator)):
			m.rep.Count("vanished", 1) // moved in the library mid-run; the next run follows it
			continue
		}
		m.rep.Count("failed", 1)
		m.rep.Alert("%s %s: %v", a.kind, a.src, err)
		m.env.Log.Error("action failed", "event", "action_error", "kind", a.kind, "path", a.src, "err", err)
		if failures++; failures >= maxFailures {
			return fmt.Errorf("giving up after %d consecutive failures: %w", failures, err)
		}
	}
	flush()
	return nil
}

func (m *mirror) apply(a action) error {
	if err := m.gate(); err != nil {
		return err
	}
	switch a.kind {
	case "copy", "update":
		return m.copyFile(a)
	case "rename":
		return m.rename(a)
	case "orphan":
		return m.orphan(a)
	default: // drop-temp: our own partial copy of a file no longer planned
		if err := os.Remove(a.src); err != nil {
			return err
		}
		m.pruneEmpty(filepath.Dir(a.src))
		return nil
	}
}

// copyFile makes a.dst a complete copy of a.src (bytes and mtime) without ever overwriting: data goes to a temp
// beside dst, resumed when a verified partial is already there, then renamed into place.
func (m *mirror) copyFile(a action) error {
	tmp := filepath.Join(filepath.Dir(a.dst), "."+filepath.Base(a.dst)+tempSuffix)
	if fi, err := os.Lstat(a.dst); err == nil {
		if done, err := m.settle(a, fi.Size(), tmp); done || err != nil {
			return err
		}
	}
	if err := m.ensureDir(filepath.Dir(a.dst)); err != nil {
		return err
	}
	off := int64(0)
	if fi, err := os.Lstat(tmp); err == nil && fi.Size() <= a.size {
		if ok, err := sampled(tmp, a.src, fi.Size(), resumeWindow); err != nil {
			return err
		} else if ok {
			off = fi.Size()
			m.prog.Bytes -= off
		}
	}
	in, err := openRead(a.src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if err := m.fill(in, out, off); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, a.mtime, a.mtime); err != nil {
		return err
	}
	if err := renameNoClobber(tmp, a.dst); err != nil {
		return err
	}
	if off > 0 {
		m.rep.Count("resumed", 1)
	}
	m.rep.Count("copied", 1)
	m.env.Log.Info("file copied", "event", "file_copied", "item", a.film, "path", a.dst, "bytes", a.size, "resumed_from", off)
	return nil
}

// fill writes in[off:] to out[off:] (dropping anything past off), paced and gated, and syncs it.
func (m *mirror) fill(in, out *os.File, off int64) error {
	if err := out.Truncate(off); err != nil {
		return err
	}
	if _, err := in.Seek(off, io.SeekStart); err != nil {
		return err
	}
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, chunk)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return err
			}
			m.prog.Copied += int64(n)
			if err := m.throttle(n); err != nil {
				return err
			}
			if m.env.Now().Sub(m.lastStatus) >= statusEvery {
				m.status()
			}
		}
		if rerr == io.EOF {
			return out.Sync()
		}
		if rerr != nil {
			return rerr
		}
	}
}

// settle handles a file already at dst that diff found not current. Same bytes: only its mtime is fixed (done).
// A verified prefix of the source (a copy cut short; fill.py wrote straight to the final name): it becomes the
// temp and is resumed. Anything else is disposed of, never overwritten.
func (m *mirror) settle(a action, have int64, tmp string) (done bool, err error) {
	switch {
	case have == a.size:
		same, err := m.equalFiles(a.dst, a.src)
		if err != nil {
			return false, err
		}
		if same {
			m.prog.Bytes -= a.size
			return true, os.Chtimes(a.dst, a.mtime, a.mtime)
		}
	case have < a.size:
		prefix, err := sampled(a.dst, a.src, have, resumeWindow)
		if err != nil {
			return false, err
		}
		if prefix {
			if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
			return false, renameNoClobber(a.dst, tmp)
		}
	}
	return false, m.dispose(a.film, a.dst, have)
}

// dispose takes a stale SSD file out of the way: deleted when byte-identical to an archived library file (the
// bytes are kept there), otherwise moved under ssd_archive.
func (m *mirror) dispose(film, path string, size int64) error {
	twin, err := m.archivedTwin(path, size)
	if err != nil {
		return err
	}
	if twin != "" {
		if err := os.Remove(path); err != nil {
			return err
		}
		m.rep.Count("deleted", 1)
		m.event(film, "stale_deleted", map[string]any{"path": path, "archived": twin})
		return nil
	}
	rel, err := filepath.Rel(m.opt.Target, path)
	if err != nil {
		return err
	}
	dest := filepath.Join(m.opt.SSDArchive, m.stamp, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := renameNoClobber(path, dest); err != nil {
		return err
	}
	m.rep.Count("archived", 1)
	m.event(film, "stale_archived", map[string]any{"path": path, "to": dest})
	return nil
}

// orphan handles an SSD file whose library twin is gone: deleted on proof that the library archived the same
// bytes, otherwise kept and reported.
func (m *mirror) orphan(a action) error {
	twin, err := m.archivedTwin(a.src, a.size)
	if err != nil {
		return err
	}
	rel, _ := filepath.Rel(m.opt.Target, a.src)
	if twin == "" {
		m.prog.Orphans = append(m.prog.Orphans, rel)
		m.rep.Count("orphan", 1)
		return nil
	}
	if err := os.Remove(a.src); err != nil {
		return err
	}
	m.pruneEmpty(filepath.Dir(a.src))
	m.rep.Count("deleted", 1)
	m.event(topName(rel), "orphan_deleted", map[string]any{"path": a.src, "archived": twin})
	return nil
}

func (m *mirror) rename(a action) error {
	if err := m.ensureDir(filepath.Dir(a.dst)); err != nil {
		return err
	}
	if fold(a.src) == fold(a.dst) { // case only: exFAT treats both names as one, so go through a third
		if err := renameNoClobber(a.src, a.src+caseSuffix); err != nil {
			return err
		}
		a.src += caseSuffix
	}
	if err := renameNoClobber(a.src, a.dst); err != nil {
		return err
	}
	m.pruneEmpty(filepath.Dir(a.src))
	m.rep.Count("renamed", 1)
	m.event(a.film, "renamed", map[string]any{"from": a.src, "to": a.dst})
	return nil
}

// archivedTwin returns a file under library_archive with the same bytes as path, or "".
func (m *mirror) archivedTwin(path string, size int64) (string, error) {
	if m.archive == nil {
		m.archive = map[int64][]string{}
		err := filepath.WalkDir(m.opt.LibraryArchive, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && strings.Contains(d.Name(), "@eaDir") {
				return filepath.SkipDir
			}
			if info, err := d.Info(); err == nil && d.Type().IsRegular() {
				m.archive[info.Size()] = append(m.archive[info.Size()], p)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	for _, cand := range m.archive[size] {
		if same, err := m.equalFiles(path, cand); err != nil || same {
			return cand, err
		}
	}
	return "", nil
}

// ensureDir creates dir and its missing parents below Target, never Target itself: an absent Target means the SSD
// is not mounted, and a copy made anyway would fill the system disk.
func (m *mirror) ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil || dir == m.opt.Target || !strings.HasPrefix(dir, m.opt.Target) {
		return err
	}
	if err := m.ensureDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// pruneEmpty removes dir and its parents below Target while they are empty.
func (m *mirror) pruneEmpty(dir string) {
	for dir != m.opt.Target && strings.HasPrefix(dir, m.opt.Target) && os.Remove(dir) == nil {
		dir = filepath.Dir(dir)
	}
}

// gate holds heavy work while anyone but the owner is watching; it asks the servers at most once per Poll.
func (m *mirror) gate() error {
	if !m.lastGate.IsZero() && m.env.Now().Sub(m.lastGate) < m.env.Viewers.Poll {
		return nil
	}
	if n, err := m.env.Viewers.Count(m.ctx); err != nil || n > 0 { // unknown counts as busy
		m.prog.Paused = true
		m.status()
		if err := m.env.Viewers.WaitIdle(m.ctx); err != nil {
			return err
		}
		m.prog.Paused = false
		m.pace.reset() // no catch-up burst after a pause
	}
	m.lastGate = m.env.Now()
	return nil
}

func (m *mirror) throttle(n int) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if err := m.pace.take(m.ctx, n); err != nil {
		return err
	}
	return m.gate()
}

func (m *mirror) event(item, status string, fields map[string]any) {
	if err := m.env.Store.AddEvent(context.WithoutCancel(m.ctx), m.env.Now(), name, item, status, fields); err != nil {
		m.env.Log.Error("history write failed", "event", "store_error", "item", item, "err", err)
	}
	m.env.Log.Info(status, append([]any{"event", status, "item", item}, flatten(fields)...)...)
}

func flatten(fields map[string]any) []any {
	out := make([]any, 0, 2*len(fields))
	for k, v := range fields {
		out = append(out, k, v)
	}
	return out
}

// status rewrites PROGRESS.txt (fill.py's layout) and stores the same progress.
func (m *mirror) status() {
	p, now := m.prog, m.env.Now()
	m.lastStatus = now
	rate := float64(p.Copied) / max(1, now.Sub(p.Started).Seconds())
	eta := time.Duration(0)
	if rate > 0 {
		eta = time.Duration(float64(p.Bytes-p.Copied)/rate) * time.Second
	}
	loc := m.env.Loc
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SSD mirror: %s   updated %s\n", strings.ToUpper(p.State), now.In(loc).Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "movies:  %d planned A->Z (%s ... %s)\n", p.Films, p.First, p.Last)
	fmt.Fprintf(&b, "copied:  %.0f of %.0f GB new data (%.1f%%)\n", float64(p.Copied)/1e9, float64(p.Bytes)/1e9, 100*float64(p.Copied)/max(1, float64(p.Bytes)))
	fmt.Fprintf(&b, "speed:   %.0f MB/s   ETA %dh %02dm\nnow:     %s\n", rate/1e6, int(eta.Hours()), int(eta.Minutes())%60, p.Current)
	if p.Paused {
		b.WriteString("paused:  someone other than the owner is watching\n")
	}
	if p.PastCut > 0 {
		fmt.Fprintf(&b, "past cut: %d film(s) on the SSD after the last one that fits are kept but no longer updated\n", p.PastCut)
	}
	if len(p.Orphans) > 0 {
		fmt.Fprintf(&b, "kept:    %d SSD file(s) whose library twin is gone and that match no archived copy:\n", len(p.Orphans))
		for _, o := range p.Orphans {
			b.WriteString("  " + o + "\n")
		}
	}
	if err := writeAtomic(m.opt.StatusFile, b.String()); err != nil {
		m.env.Log.Warn("status write failed", "event", "status_error", "err", err)
	}
	if err := m.env.Store.PutJSON(context.WithoutCancel(m.ctx), name, "progress", p); err != nil {
		m.env.Log.Error("store write failed", "event", "store_error", "err", err)
	}
}

// sampled reports whether two files match over their first and last window bytes of the first n (all n bytes
// when n ≤ 2·window): proof enough that two equal-size files are one file renamed, or that a partial copy is a
// prefix of its source (a cut-short copy loses its tail, which the tail sample catches).
func sampled(a, b string, n, window int64) (bool, error) {
	w := min(n, window)
	if same, err := equalRange(a, b, 0, w); err != nil || !same {
		return same, err
	}
	return equalRange(a, b, n-w, w)
}

func equalRange(a, b string, off, n int64) (bool, error) {
	read := func(p string) ([]byte, error) {
		f, err := openRead(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, n)
		_, err = f.ReadAt(buf, off)
		return buf, err
	}
	x, err := read(a)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	y, err := read(b)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return bytes.Equal(x, y), err
}

// equalFiles compares two whole files, paced and gated like a copy (it reads as much).
func (m *mirror) equalFiles(a, b string) (bool, error) {
	fa, err := openRead(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := openRead(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		for _, e := range []error{ea, eb} {
			if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
				return false, e
			}
		}
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if na < chunk {
			return true, nil
		}
		if err := m.throttle(na); err != nil {
			return false, err
		}
	}
}

// openRead opens without updating the access time: on btrfs an atime write is a metadata copy-on-write, on the SSD a
// directory-entry write. O_NOATIME needs file ownership or CAP_FOWNER, hence the fallback.
func openRead(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|noatime, 0)
	if errors.Is(err, syscall.EPERM) {
		return os.Open(p)
	}
	return f, err
}

// pacer holds bulk reads to rate bytes/s on the injected clock.
type pacer struct {
	rate  float64 // 0: no cap
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	since time.Time
	sent  float64
}

func (p *pacer) take(ctx context.Context, n int) error {
	if p.rate <= 0 {
		return nil
	}
	now := p.now()
	if p.since.IsZero() {
		p.since = now
	}
	p.sent += float64(n)
	ahead := p.since.Add(time.Duration(p.sent / p.rate * float64(time.Second))).Sub(now)
	if ahead <= 0 {
		if ahead < -time.Second {
			p.since, p.sent = now, 0 // a slow stretch earns no burst afterwards
		}
		return nil
	}
	return p.sleep(ctx, ahead)
}

func (p *pacer) reset() { p.since, p.sent = time.Time{}, 0 }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// exclusiveRename is fsx.RenameNoReplace; a variable so a test can play the SSD's exFAT, which on DSM's kernel 4.4
// is not expected to support RENAME_NOREPLACE, nor the hard link fsx falls back to.
var exclusiveRename = fsx.RenameNoReplace

// renameNoClobber never replaces newPath. Where the filesystem cannot refuse atomically it checks first, which is
// race-free here: this stage is the SSD's only writer, and the scheduler never runs two copies of it.
func renameNoClobber(oldPath, newPath string) error {
	err := exclusiveRename(oldPath, newPath)
	if err == nil || errors.Is(err, fsx.ErrExists) {
		return err
	}
	if _, err := os.Lstat(newPath); err == nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: fsx.ErrExists}
	}
	return os.Rename(oldPath, newPath)
}
