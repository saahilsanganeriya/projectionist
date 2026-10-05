package rename

import (
	"io/fs"
	"syscall"
	"time"
)

func changeTime(info fs.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	}
	return info.ModTime()
}
