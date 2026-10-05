package ssdmirror

import "golang.org/x/sys/unix"

// lowerPriority gives the calling OS thread idle I/O priority (the NAS disks and the SSD use CFQ, which honours
// it) and nice 19. Both are per thread on Linux; Run locks the copying goroutine to this thread.
func lowerPriority() error {
	const ioprioWhoProcess, ioprioClassIdle = 1, 3 // who=0 with WHO_PROCESS means the calling thread
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<13); errno != 0 {
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
