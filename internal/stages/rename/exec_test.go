package rename

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingLog is an undo log in memory that counts fsyncs.
type countingLog struct {
	strings.Builder
	syncs int
}

func (c *countingLog) Sync() error { c.syncs++; return nil }

func newExecutor(root string, log undoFile) *executor {
	return newExec(root, log, func() time.Time { return time.Unix(0, 0) })
}

// Catches the executor's refusals: overwriting a file already at a video's target (the unit must stop with
// ErrUnitStopped and move nothing, its subtitles included), deleting a "twin" whose bytes differ, and writing the
// undo log with more than one fsync per unit (the old executor's 18 ops/s came from one per op).
func TestRunUnitRefusals(t *testing.T) {
	root := t.TempDir()
	m := filepath.Join(root, "Movies")
	write(t, filepath.Join(m, "a/A.mkv"), "a")
	write(t, filepath.Join(m, "a/A.srt"), "s")
	write(t, filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.mkv"), "someone else")
	write(t, filepath.Join(m, "b/B.mkv"), "b1")
	write(t, filepath.Join(m, "b/copy/B.mkv"), "b2")
	write(t, filepath.Join(m, "c/C.mkv"), "c")
	write(t, filepath.Join(m, "c/C.srt"), "s")
	log := &countingLog{}
	x := newExecutor(root, log)
	touched := map[string]bool{}

	err := x.runUnit(context.Background(), "movie:tmdb-1", []Op{
		{"movie:tmdb-1", "move", filepath.Join(m, "a/A.srt"), filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.en.srt"), ""},
		{"movie:tmdb-1", "move", filepath.Join(m, "a/A.mkv"), filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.mkv"), ""},
	}, touched)
	if !errors.Is(err, ErrUnitStopped) || !strings.Contains(err.Error(), "target exists") {
		t.Fatalf("overwrite: err = %v, want ErrUnitStopped naming the existing target", err)
	}
	if got, _ := os.ReadFile(filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.mkv")); string(got) != "someone else" || !lexists(filepath.Join(m, "a/A.srt")) {
		t.Fatal("a refused unit moved or overwrote something")
	}

	if err := x.runUnit(context.Background(), "movie:tmdb-2", []Op{
		{"movie:tmdb-2", "delete", filepath.Join(m, "b/copy/B.mkv"), filepath.Join(m, "b/B.mkv"), ""},
	}, touched); err != nil || x.stats["delete_refused"] != 1 || !lexists(filepath.Join(m, "b/copy/B.mkv")) {
		t.Fatalf("different twin: err=%v refused=%d, still there=%v", err, x.stats["delete_refused"], lexists(filepath.Join(m, "b/copy/B.mkv")))
	}

	if err := x.runUnit(context.Background(), "movie:tmdb-3", []Op{
		{"movie:tmdb-3", "move", filepath.Join(m, "c/C.mkv"), filepath.Join(m, "C (2001) {tmdb-3}/C (2001) {tmdb-3}.mkv"), ""},
		{"movie:tmdb-3", "move", filepath.Join(m, "c/C.srt"), filepath.Join(m, "C (2001) {tmdb-3}/C (2001) {tmdb-3}.en.srt"), ""},
	}, touched); err != nil {
		t.Fatal(err)
	}
	if log.syncs != 1 || strings.Count(log.String(), "\tmove\t") != 2 || lexists(filepath.Join(m, "c")) {
		t.Fatalf("syncs=%d (want 1 for the one unit that acted), log:\n%s\nold folder left=%v", log.syncs, log.String(), lexists(filepath.Join(m, "c")))
	}
}

// Catches: a re-run after a unit stopped half-way repeating a move (or failing on it) instead of resuming from the
// disk, and an undo that renames over a file that came back to the original path.
func TestResumeAndUndoNeverOverwrite(t *testing.T) {
	root := t.TempDir()
	m := filepath.Join(root, "Movies")
	src, dst := filepath.Join(m, "d/D.mkv"), filepath.Join(m, "D (2002) {tmdb-4}/D (2002) {tmdb-4}.mkv")
	write(t, dst, "d") // the first run moved the video, then stopped
	log := &countingLog{}
	x := newExecutor(root, log)
	ops := []Op{{"movie:tmdb-4", "move", src, dst, ""}}
	if err := x.runUnit(context.Background(), "movie:tmdb-4", ops, map[string]bool{}); err != nil || x.stats["already_done"] != 1 || log.syncs != 0 {
		t.Fatalf("resume: err=%v already_done=%d syncs=%d", err, x.stats["already_done"], log.syncs)
	}
	write(t, src, "a new download at the old path")
	notUndone, err := x.undoUnits(context.Background(), []undoEntry{{"move", src, dst, "movie:tmdb-4"}}, nil, map[string]bool{})
	if err != nil || len(notUndone) != 1 {
		t.Fatalf("undo: err=%v notUndone=%v", err, notUndone)
	}
	if got, _ := os.ReadFile(src); string(got) != "a new download at the old path" || !lexists(dst) {
		t.Fatal("undo overwrote the file at the original path")
	}
}
