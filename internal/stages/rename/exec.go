package rename

// The executor (port of apply_ops.py): one unit (a film or a show) at a time.
//
//	1 decide each op against the disk: done already (src gone, dst there), missing, target taken, twin not identical
//	2 foresee the folders the unit empties; their clutter (.DS_Store, @eaDir) is archived too, never deleted
//	3 write the unit's intents to the undo log and fsync it once
//	4 act: deletes, then videos, then the rest; mkdir (no chmod: it strips the Synology ACL), renameat2 NOREPLACE
//	5 archive the clutter, remove the emptied folders, fsync every touched folder once; append "unit-done"
//
// A unit stops at its first error and stays half-done; the next run re-plans from the disk and finishes it. The
// undo log keeps apply_ops.py's format (ts, op, src, dst, unit) so either tool can read the other's log.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"projectionist/internal/fsx"
)

var clutter = set(".DS_Store", "Thumbs.db", "desktop.ini")

// ErrUnitStopped wraps the reason a unit stopped half-way.
var ErrUnitStopped = errors.New("unit stopped")

// undoFile is the open undo log (an *os.File; tests count its fsyncs).
type undoFile interface {
	WriteString(s string) (int, error)
	Sync() error
}

type executor struct {
	undo  undoFile
	media string   // the share: clutter goes to <media>/_archive/clutter/<its path>
	roots []string // library roots a prune never removes, nor climbs above
	now   func() time.Time
	stats map[string]int
}

func newExec(media string, undo undoFile, now func() time.Time) *executor {
	return &executor{undo: undo, media: media, roots: libraryRoots(media), now: now, stats: map[string]int{}}
}

func (x *executor) count(kind string) { x.stats[kind]++ }

func undoRow(at time.Time, op, src, dst, unit string) string {
	return strings.Join([]string{at.UTC().Format("2006-01-02T15:04:05Z"), op, src, dst, unit}, "\t") + "\n"
}

func isVideo(p string) bool { return videoExt[lowerExt(p)] }

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// runUnit executes one unit's ops; touched collects the folders it changed.
func (x *executor) runUnit(ctx context.Context, unit string, ops []Op, touched map[string]bool) error {
	rank := func(o Op) int {
		switch {
		case o.Op == "delete":
			return 0
		case isVideo(o.Src):
			return 1
		}
		return 2
	}
	ops = append([]Op(nil), ops...)
	sort.SliceStable(ops, func(i, j int) bool { return rank(ops[i]) < rank(ops[j]) })
	var todo []Op
	for _, o := range ops {
		if o.Op != "move" && o.Op != "archive" && o.Op != "delete" {
			continue
		}
		if !lexists(o.Src) {
			if o.Dst != "" && lexists(o.Dst) {
				x.count("already_done")
				continue
			}
			x.count("missing_src")
			if isVideo(o.Src) && o.Op != "delete" {
				return fmt.Errorf("%w: planned video missing (listing stale): %s", ErrUnitStopped, o.Src)
			}
			continue
		}
		if o.Op == "delete" {
			same, err := identical(ctx, o.Src, o.Dst)
			if err != nil {
				return err
			}
			if !same {
				x.count("delete_refused") // archive-not-delete: only a byte-identical copy may go
				continue
			}
		} else if lexists(o.Dst) {
			x.count("target_exists")
			if isVideo(o.Src) {
				return fmt.Errorf("%w: a video's target exists, so its sidecars stay too: %s", ErrUnitStopped, o.Dst)
			}
			continue
		}
		todo = append(todo, o)
	}
	if len(todo) == 0 {
		return nil
	}
	gone, srcDirs := map[string]bool{}, map[string]bool{}
	var landing []string
	for _, o := range todo {
		gone[o.Src], srcDirs[path.Dir(o.Src)] = true, true
		if o.Op != "delete" {
			landing = append(landing, o.Dst)
		}
	}
	emptied := x.emptied(sortedKeys(srcDirs), gone, landing)
	sweepOps := x.clutterOps(emptied)
	if err := x.logIntents(unit, append(todo, sweepOps...)); err != nil {
		return err
	}
	for _, o := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if o.Op == "delete" {
			if err := os.Remove(o.Src); err != nil {
				return fmt.Errorf("%w: %v", ErrUnitStopped, err)
			}
			x.count("deleted")
		} else {
			if err := mkdirs(path.Dir(o.Dst), touched); err != nil {
				return fmt.Errorf("%w: %v", ErrUnitStopped, err)
			}
			if err := fsx.RenameNoReplace(o.Src, o.Dst); err != nil {
				return fmt.Errorf("%w: %v", ErrUnitStopped, err)
			}
			touched[path.Dir(o.Dst)] = true
			x.count(o.Op + "d") // moved / archived
		}
		touched[path.Dir(o.Src)] = true
	}
	if err := x.finishEmptied(sweepOps, emptied, touched); err != nil {
		return err
	}
	_, err := x.undo.WriteString(undoRow(x.now(), "unit-done", "", "", unit))
	return err
}

// logIntents writes ops to the undo log and fsyncs it: the log is durable before anything moves (one fsync per unit).
func (x *executor) logIntents(unit string, ops []Op) error {
	var b strings.Builder
	at := x.now()
	for _, o := range ops {
		b.WriteString(undoRow(at, o.Op, o.Src, o.Dst, unit))
	}
	if _, err := x.undo.WriteString(b.String()); err != nil {
		return fmt.Errorf("undo log: %w", err)
	}
	if err := x.undo.Sync(); err != nil {
		return fmt.Errorf("undo log: %w", err)
	}
	return nil
}

func (x *executor) isRoot(d string) bool {
	for _, r := range x.roots {
		if d == r {
			return true
		}
	}
	return false
}

func (x *executor) underRoot(d string) bool {
	for _, r := range x.roots {
		if strings.HasPrefix(d, r+"/") {
			return true
		}
	}
	return false
}

func isClutter(name string) bool { return clutter[name] || name == "@eaDir" }

// emptied lists, deepest first, the folders at or above dirs that will hold nothing but clutter once the paths in
// gone have left, unless something lands inside them. Library roots are never emptied.
func (x *executor) emptied(dirs []string, gone map[string]bool, landing []string) []string {
	memo := map[string]bool{}
	var empty func(d string) bool
	empty = func(d string) bool {
		if v, seen := memo[d]; seen {
			return v
		}
		memo[d] = false
		if x.isRoot(d) || !x.underRoot(d) {
			return false
		}
		for _, l := range landing {
			if strings.HasPrefix(l, d+"/") {
				return false
			}
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return false
		}
		for _, e := range entries {
			p := d + "/" + e.Name()
			if !isClutter(e.Name()) && !gone[p] && !(e.IsDir() && empty(p)) {
				return false
			}
		}
		memo[d] = true
		return true
	}
	for _, d := range dirs {
		for ; empty(d); d = path.Dir(d) {
		}
	}
	var out []string
	for d, v := range memo {
		if v {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i]) > len(out[j]) || (len(out[i]) == len(out[j]) && out[i] < out[j])
	})
	return out
}

// clutterOps archives the clutter of each emptied folder: archive-not-delete covers .DS_Store and thumbnails too.
func (x *executor) clutterOps(emptied []string) []Op {
	var ops []Op
	for _, d := range emptied {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if isClutter(e.Name()) {
				src := d + "/" + e.Name()
				ops = append(ops, Op{Op: "archive", Src: src, Dst: x.media + "/_archive/clutter/" + strings.TrimPrefix(src, x.media+"/")})
			}
		}
	}
	return ops
}

// finishEmptied moves the clutter out, removes the emptied folders (deepest first) and fsyncs every touched folder.
// A folder that gained an entry meanwhile simply stays.
func (x *executor) finishEmptied(clutterOps []Op, emptied []string, touched map[string]bool) error {
	for _, o := range clutterOps {
		if err := mkdirs(path.Dir(o.Dst), touched); err != nil {
			return err
		}
		if err := fsx.RenameNoReplace(o.Src, o.Dst); err == nil {
			touched[path.Dir(o.Dst)] = true
		}
	}
	for _, d := range emptied {
		if os.Remove(d) == nil { // rmdir: only an empty folder goes; it stays in touched so Jellyfin hears it is gone
			touched[path.Dir(d)] = true
			x.count("pruned")
		}
	}
	for dir := range touched {
		if lexists(dir) {
			if err := fsx.SyncDir(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// mkdirs creates dir and its missing parents. Each new folder takes its parent's owner (chown keeps the Synology ACL
// a plain mkdir inherited; chmod would strip it, so there is none).
func mkdirs(dir string, touched map[string]bool) error {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s is not a folder", dir)
		}
		return nil
	}
	parent := path.Dir(dir)
	if err := mkdirs(parent, touched); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	touched[parent] = true
	if st, err := os.Stat(parent); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			return os.Lchown(dir, int(sys.Uid), int(sys.Gid))
		}
	}
	return nil
}

// identical reports whether two regular files (no symlinks) hold the same bytes.
func identical(ctx context.Context, a, b string) (bool, error) {
	sa, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Lstat(b)
	if err != nil || !sa.Mode().IsRegular() || !sb.Mode().IsRegular() || sa.Size() != sb.Size() || os.SameFile(sa, sb) {
		return false, nil
	}
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ra, rb := bufio.NewReaderSize(fa, 1<<20), bufio.NewReaderSize(fb, 1<<20)
	ba, bb := make([]byte, 1<<20), make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		na, ea := io.ReadFull(ra, ba)
		nb, eb := io.ReadFull(rb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if ea == io.EOF || ea == io.ErrUnexpectedEOF {
			return eb == io.EOF || eb == io.ErrUnexpectedEOF, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}

// rmdirUp removes folder and its parents while they are empty (undo's leftovers), never a library root nor above one.
func (x *executor) rmdirUp(folder string) {
	for !x.isRoot(folder) && x.underRoot(folder) && os.Remove(folder) == nil {
		x.count("pruned")
		folder = path.Dir(folder)
	}
}

type undoEntry struct{ op, src, dst, unit string }

func readUndo(file string) ([]undoEntry, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []undoEntry
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 5 {
			rows = append(rows, undoEntry{f[1], f[2], f[3], f[4]})
		}
	}
	return rows, nil
}

// undoUnits replays moves and archives backwards (dst → src), all units or only the wanted ones. Deletes removed
// byte-identical copies and are not restored. A move whose dst is gone or whose src is taken again is reported.
func (x *executor) undoUnits(ctx context.Context, rows []undoEntry, wanted map[string]bool, touched map[string]bool) (notUndone []string, err error) {
	var units []string
	seen := map[string]bool{}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if len(wanted) > 0 && !wanted[r.unit] {
			continue
		}
		if !seen[r.unit] {
			seen[r.unit] = true
			units = append(units, r.unit)
		}
		if r.op != "move" && r.op != "archive" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return notUndone, err
		}
		if dst, src := lexists(r.dst), lexists(r.src); !dst || src {
			if dst == src { // both there (src taken again) or both gone; src-only is an intent that never ran
				notUndone = append(notUndone, r.src+" <- "+r.dst)
			}
			continue
		}
		if err := mkdirs(path.Dir(r.src), touched); err != nil {
			return notUndone, err
		}
		if err := fsx.RenameNoReplace(r.dst, r.src); err != nil {
			return notUndone, err
		}
		touched[path.Dir(r.src)], touched[path.Dir(r.dst)] = true, true
		x.count("undone")
		x.rmdirUp(path.Dir(r.dst))
	}
	for dir := range touched {
		if lexists(dir) {
			if err := fsx.SyncDir(dir); err != nil {
				return notUndone, err
			}
		}
	}
	sort.Strings(units)
	var b strings.Builder
	for _, u := range units { // undone units are no longer done
		b.WriteString(undoRow(x.now(), "unit-undone", "", "", u))
	}
	if _, err := x.undo.WriteString(b.String()); err != nil {
		return notUndone, err
	}
	return notUndone, x.undo.Sync()
}
