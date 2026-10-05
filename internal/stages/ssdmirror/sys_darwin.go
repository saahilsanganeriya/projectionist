package ssdmirror

import "golang.org/x/sys/unix"

func lowerPriority() error { return nil } // macOS is only for development and tests

func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
