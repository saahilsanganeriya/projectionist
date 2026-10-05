package subtitles

import "golang.org/x/sys/unix"

// idle drops a process group to nice 19 and the idle I/O class, so Plex and Jellyfin win every contention.
func idle(pgid int) {
	unix.Setpriority(unix.PRIO_PGRP, pgid, 19)
	const whoPgrp, classIdle = 2, 3 // IOPRIO_WHO_PGRP, IOPRIO_CLASS_IDLE
	unix.Syscall(unix.SYS_IOPRIO_SET, whoPgrp, uintptr(pgid), classIdle<<13)
}
