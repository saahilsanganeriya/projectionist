package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Catches: a rename or clone that silently overwrites an existing file (the library rule: never overwrite).
func TestNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("B"), 0o644)
	if err := RenameNoReplace(a, b); !errors.Is(err, os.ErrExist) {
		t.Fatalf("rename onto existing: err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(b); string(got) != "B" {
		t.Fatal("target was overwritten")
	}
	if err := RenameNoReplace(a, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("source still exists after rename")
	}
	if err := Clone(c, b); !errors.Is(err, os.ErrExist) {
		t.Fatalf("clone onto existing: err = %v, want os.ErrExist", err)
	}
	d := filepath.Join(dir, "d")
	if err := Clone(c, d); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(d); string(got) != "A" {
		t.Fatalf("clone content = %q", got)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
}

// Catches a move across mounts (the library to an archive on another bind mount, which a rename refuses with EXDEV)
// that fails, loses the file, leaves it in both places, or overwrites a file already at the target. Runs where a
// second filesystem is at hand (/dev/shm in `make test-linux`'s container).
func TestMoveNoReplaceAcrossMounts(t *testing.T) {
	dir := t.TempDir()
	var here, there syscall.Stat_t
	syscall.Stat(dir, &here)
	if syscall.Stat("/dev/shm", &there) != nil || here.Dev == there.Dev {
		t.Skip("no second filesystem at /dev/shm")
	}
	other, err := os.MkdirTemp("/dev/shm", "fsx-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(other) })
	src, dst := filepath.Join(dir, "a.srt"), filepath.Join(other, "a.srt")
	os.WriteFile(src, []byte("A"), 0o644)
	if err := MoveNoReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "A" || fileThere(src) {
		t.Fatalf("after the move: target %q, source still there %v; want A, gone", got, fileThere(src))
	}
	os.WriteFile(src, []byte("B"), 0o644)
	if err := MoveNoReplace(src, dst); !errors.Is(err, os.ErrExist) {
		t.Fatalf("move onto an existing file: err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "A" || !fileThere(src) {
		t.Errorf("after the refused move: target %q, source there %v; want A untouched, the source kept", got, fileThere(src))
	}
}

func fileThere(p string) bool { _, err := os.Stat(p); return err == nil }
