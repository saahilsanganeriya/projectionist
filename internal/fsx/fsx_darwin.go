package fsx

import (
	"os"

	"golang.org/x/sys/unix"
)

// macOS is only for development and tests.
func renameNoReplace(oldPath, newPath string) error {
	if err := unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}

func cloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return &os.LinkError{Op: "clonefile", Old: src, New: dst, Err: err}
	}
	return nil
}
