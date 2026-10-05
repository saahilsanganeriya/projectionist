package ssdmirror

import "golang.org/x/sys/unix"

// setPriority sets the calling OS thread to nice 19 and the idle I/O class, or else the lowest best-effort level
// (what nice 19 alone gives). The NAS disks and the SSD use CFQ, which honours both; on Linux both are per thread,
// and Run locks the run's goroutine to its thread.
func setPriority(idle bool) error {
	const whoProcess, classBestEffort, classIdle = 1, 2, 3 // who=0 with WHO_PROCESS is the calling thread
	prio := classBestEffort<<13 | 7
	if idle {
		prio = classIdle << 13
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, 0, uintptr(prio)); errno != 0 {
		return errno
	}
	return unix.Setpriority(unix.PRIO_PROCESS, unix.Gettid(), 19)
}

// freeBytes is what Python's shutil.disk_usage(path).free reports: f_bavail × f_frsize.
func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	size := st.Frsize
	if size == 0 {
		size = st.Bsize
	}
	return int64(st.Bavail) * size, nil
}

const noatime = unix.O_NOATIME
