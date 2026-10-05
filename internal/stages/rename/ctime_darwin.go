package rename

import (
	"io/fs"
	"syscall"
	"time"
)

// macOS is only for development and tests.
func changeTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
	}
	return info.ModTime()
}
