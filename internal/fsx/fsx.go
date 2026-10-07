// Package fsx holds the file operations every stage shares: renames that cannot overwrite, copy-on-write clones,
// directory fsync (a rename is durable only once its directories are synced: do it once per unit, not per op), and
// Background, which runs heavy file work at the lowest CPU and I/O priority.
package fsx

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// RenameNoReplace moves old to new atomically and fails with an error matching os.ErrExist if new exists.
func RenameNoReplace(oldPath, newPath string) error {
	return renameNoReplace(oldPath, newPath)
}

// MoveNoReplace moves old to new and fails with an error matching os.ErrExist if new exists: a rename where both share
// a mount, else a Clone that is complete and synced before old is removed (a failure leaves old in place).
func MoveNoReplace(oldPath, newPath string) error {
	if err := RenameNoReplace(oldPath, newPath); !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := Clone(oldPath, newPath); err != nil {
		return err
	}
	return os.Remove(oldPath)
}

// Clone makes dst a copy of src that shares its blocks (btrfs reflink / APFS clonefile) and falls back to a byte
// copy where the filesystem cannot. dst must not exist. A failed byte copy removes the partial dst.
func Clone(src, dst string) error {
	if err := cloneFile(src, dst); err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// SyncDir makes completed renames/creates inside dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
