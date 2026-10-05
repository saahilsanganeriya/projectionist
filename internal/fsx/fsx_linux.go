package fsx

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) {
		return wrap("rename", oldPath, newPath, err)
	}
	// Filesystem without RENAME_NOREPLACE: link() refuses an existing target, then drop the old name.
	// Regular files only; directories cannot be hard-linked.
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	return os.Remove(oldPath)
}

func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst) // the caller falls back to a byte copy into a fresh dst
	}
	return err
}

func wrap(op, oldPath, newPath string, err error) error {
	if err == nil {
		return nil
	}
	return &os.LinkError{Op: op, Old: oldPath, New: newPath, Err: err}
}
