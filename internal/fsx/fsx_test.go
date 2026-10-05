package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Catches: a rename or clone that silently overwrites an existing file (the library rule: never overwrite).
func TestNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	os.WriteFile(a, []byte("A"), 0o644)
	os.WriteFile(b, []byte("B"), 0o644)
	if err := RenameNoReplace(a, b); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto existing: err = %v, want ErrExists", err)
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
	if err := Clone(c, b); !errors.Is(err, ErrExists) {
		t.Fatalf("clone onto existing: err = %v, want ErrExists", err)
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
