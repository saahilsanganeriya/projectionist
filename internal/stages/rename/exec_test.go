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
// ErrUnitStopped and move nothing, its subtitles included), moving the sidecars of a planned video that has vanished
// since the walk (a stale listing: the unit stops and its subtitle stays), deleting a "twin" whose bytes differ, and
// writing the undo log with more than one fsync per unit (the old executor's 18 ops/s came from one per op).
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
	write(t, filepath.Join(m, "e/E.srt"), "s") // E.mkv was there when the plan was made
	log := &countingLog{}
	x := newExecutor(root, log)

	_, err := x.runUnit(context.Background(), "movie:tmdb-1", []Op{
		{"movie:tmdb-1", "move", filepath.Join(m, "a/A.srt"), filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.en.srt"), ""},
		{"movie:tmdb-1", "move", filepath.Join(m, "a/A.mkv"), filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.mkv"), ""},
	})
	if !errors.Is(err, ErrUnitStopped) || !strings.Contains(err.Error(), "target exists") {
		t.Fatalf("overwrite: err = %v, want ErrUnitStopped naming the existing target", err)
	}
	if got, _ := os.ReadFile(filepath.Join(m, "A (2000) {tmdb-1}/A (2000) {tmdb-1}.mkv")); string(got) != "someone else" || !lexists(filepath.Join(m, "a/A.srt")) {
		t.Fatal("a refused unit moved or overwrote something")
	}

	_, err = x.runUnit(context.Background(), "movie:tmdb-5", []Op{
		{"movie:tmdb-5", "move", filepath.Join(m, "e/E.srt"), filepath.Join(m, "E (2004) {tmdb-5}/E (2004) {tmdb-5}.en.srt"), ""},
		{"movie:tmdb-5", "move", filepath.Join(m, "e/E.mkv"), filepath.Join(m, "E (2004) {tmdb-5}/E (2004) {tmdb-5}.mkv"), ""},
	})
	if !errors.Is(err, ErrUnitStopped) || !strings.Contains(err.Error(), "planned video missing") || !lexists(filepath.Join(m, "e/E.srt")) {
		t.Fatalf("stale listing: err = %v, subtitle left = %v; want ErrUnitStopped naming the missing video, subtitle left",
			err, lexists(filepath.Join(m, "e/E.srt")))
	}

	if _, err := x.runUnit(context.Background(), "movie:tmdb-2", []Op{
		{"movie:tmdb-2", "delete", filepath.Join(m, "b/copy/B.mkv"), filepath.Join(m, "b/B.mkv"), ""},
	}); err != nil || x.stats["delete_refused"] != 1 || !lexists(filepath.Join(m, "b/copy/B.mkv")) {
		t.Fatalf("different twin: err=%v refused=%d, still there=%v", err, x.stats["delete_refused"], lexists(filepath.Join(m, "b/copy/B.mkv")))
	}

	if _, err := x.runUnit(context.Background(), "movie:tmdb-3", []Op{
		{"movie:tmdb-3", "move", filepath.Join(m, "c/C.mkv"), filepath.Join(m, "C (2001) {tmdb-3}/C (2001) {tmdb-3}.mkv"), ""},
		{"movie:tmdb-3", "move", filepath.Join(m, "c/C.srt"), filepath.Join(m, "C (2001) {tmdb-3}/C (2001) {tmdb-3}.en.srt"), ""},
	}); err != nil {
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
	if _, err := x.runUnit(context.Background(), "movie:tmdb-4", ops); err != nil || x.stats["already_done"] != 1 || log.syncs != 0 {
		t.Fatalf("resume: err=%v already_done=%d syncs=%d", err, x.stats["already_done"], log.syncs)
	}
	write(t, src, "a new download at the old path")
	notUndone, _, err := x.undoUnits(context.Background(), []undoEntry{{"move", src, dst, "movie:tmdb-4"}}, nil, func() bool { return false })
	if err != nil || len(notUndone) != 1 {
		t.Fatalf("undo: err=%v notUndone=%v", err, notUndone)
	}
	if got, _ := os.ReadFile(src); string(got) != "a new download at the old path" || !lexists(dst) {
		t.Fatal("undo overwrote the file at the original path")
	}
}

// Catches undo moving files while a guest watches (the gate is asked before each unit, so a viewer arriving mid-undo
// stops the rest), a later run not finishing what a stopped one left, a file moved twice by one unit not going back
// all the way, a move that cannot be undone being reported again on every run, a run with nothing left to undo still
// asking the gate or writing to the log (undo mode stays on, every 15 minutes), and the files undo put back not being
// held from the next apply.
func TestUndoGatedPerUnitAndOnce(t *testing.T) {
	root := t.TempDir()
	m := filepath.Join(root, "Movies")
	a0, a1, a2 := filepath.Join(m, "a/A.mkv"), filepath.Join(m, "a1/A.mkv"), filepath.Join(m, "A (2000) {tmdb-1}/A.mkv")
	b0, b1 := filepath.Join(m, "b/B.mkv"), filepath.Join(m, "B (2001) {tmdb-2}/B.mkv")
	write(t, a2, "a")
	write(t, b1, "b")
	logPath := filepath.Join(root, "undo.tsv")
	at := time.Unix(0, 0)
	write(t, logPath, undoRow(at, "move", a0, a1, "movie:tmdb-1")+undoRow(at, "move", a1, a2, "movie:tmdb-1")+
		undoRow(at, "move", filepath.Join(m, "g/G.mkv"), filepath.Join(m, "G (2003) {tmdb-3}/G.mkv"), "movie:tmdb-3")+ // both gone
		undoRow(at, "move", b0, b1, "movie:tmdb-2"))
	asks := 0
	pass := func(gate func() bool) ([]string, bool) {
		t.Helper()
		rows, _ := readUndo(logPath)
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		notUndone, waited, err := newExecutor(root, f).undoUnits(context.Background(), rows, nil, gate)
		if err != nil {
			t.Fatal(err)
		}
		return notUndone, waited
	}
	if _, waited := pass(func() bool { asks++; return asks > 1 }); !waited || !lexists(b0) || lexists(a0) {
		t.Fatalf("guest arrives after the first unit: waited=%v, B back=%v (want true), A back=%v (want false)", waited, lexists(b0), lexists(a0))
	}
	open := func() bool { asks++; return false }
	if notUndone, waited := pass(open); waited || !lexists(a0) || lexists(a1) || len(notUndone) != 1 {
		t.Fatalf("second run: waited=%v, A back=%v, A left half-way=%v, not undone=%v (want the G row only)", waited, lexists(a0), lexists(a1), notUndone)
	}
	before, _ := os.ReadFile(logPath)
	asked := asks
	if notUndone, _ := pass(open); len(notUndone) > 0 || asks != asked {
		t.Fatalf("nothing left to undo: not undone=%v, gate asked %d more time(s)", notUndone, asks-asked)
	}
	if after, _ := os.ReadFile(logPath); string(after) != string(before) {
		t.Fatalf("nothing left to undo, yet the log grew by %q", strings.TrimPrefix(string(after), string(before)))
	}
	if held, _ := heldPaths(logPath); len(held) != 2 || !held[a0] || !held[b0] {
		t.Fatalf("held = %v, want A and B where undo put them", held)
	}
}

// Catches a file moved while a copy still writes it: a household member's second copy of Matchbox (Finder's Replace)
// truncated the first to 0 bytes at 10:26, the stage's plan from before then moved it at 10:29, and the copy landed
// beside a 0-byte file at the target that blocked every later run. A source changed within the quiet window stops its
// unit before anything is logged or moved; once quiet, it moves.
func TestRunUnitWaitsForACopyStillBeingWritten(t *testing.T) {
	root := t.TempDir()
	m := filepath.Join(root, "Movies")
	src, dst := filepath.Join(m, "M.2026.1080p.mkv"), filepath.Join(m, "M (2026) {tmdb-9}/M (2026) {tmdb-9}.mkv")
	write(t, src, "still arriving")
	log := &countingLog{}
	x := newExecutor(root, log)
	x.quiet, x.now = 10*time.Minute, time.Now
	op := []Op{{"movie:tmdb-9", "move", src, dst, ""}}
	if _, err := x.runUnit(context.Background(), "movie:tmdb-9", op); !errors.Is(err, ErrUnitStopped) ||
		!strings.Contains(err.Error(), "still being written") || !lexists(src) || lexists(dst) || log.Len() != 0 {
		t.Fatalf("a source written just now: err %v, moved %v, logged %q; want ErrUnitStopped naming the write, nothing moved or logged",
			err, lexists(dst), log.String())
	}
	x.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, err := x.runUnit(context.Background(), "movie:tmdb-9", op); err != nil || !lexists(dst) {
		t.Fatalf("once quiet: err %v, moved %v", err, lexists(dst))
	}
}

// Catches an empty file at a video's target (a copy that never finished) blocking the video for good, as Matchbox's did
// from 2026-10-10 until it was moved by hand: the empty file is archived (never deleted) and the video lands; a target
// with anything in it still stops the unit.
func TestRunUnitArchivesAnEmptyTarget(t *testing.T) {
	root := t.TempDir()
	m := filepath.Join(root, "Movies")
	src, dst := filepath.Join(m, "M.2026.1080p.mkv"), filepath.Join(m, "M (2026) {tmdb-9}/M (2026) {tmdb-9}.mkv")
	write(t, src, "the film")
	write(t, dst, "")
	x := newExecutor(root, &countingLog{})
	if _, err := x.runUnit(context.Background(), "movie:tmdb-9", []Op{{"movie:tmdb-9", "move", src, dst, ""}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	archived := filepath.Join(root, "_archive/clutter/Movies/M (2026) {tmdb-9}/M (2026) {tmdb-9}.mkv")
	if st, err := os.Stat(archived); string(got) != "the film" || lexists(src) || err != nil || st.Size() != 0 {
		t.Errorf("target holds %q, source left %v, empty file archived %v; want the film at the target, the empty file in the archive",
			got, lexists(src), err == nil)
	}
}
