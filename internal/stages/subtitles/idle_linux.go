package subtitles

import (
	"bytes"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// idle drops a process group to nice 19 and the lowest best-effort I/O priority, so Plex and Jellyfin win every
// contention while the work still progresses (the idle I/O class waits behind every other reader).
func idle(pgid int) {
	unix.Setpriority(unix.PRIO_PGRP, pgid, 19)
	const whoPgrp, classBestEffort, lowest = 2, 2, 7 // IOPRIO_WHO_PGRP, IOPRIO_CLASS_BE, level 7 of 0-7
	unix.Syscall(unix.SYS_IOPRIO_SET, whoPgrp, uintptr(pgid), classBestEffort<<13|lowest)
}

// exitedRead waits for pid to exit without reaping it and returns the bytes it read (rchar of /proc/<pid>/io, which
// includes the children it reaped: an upper bound on what it read from the video, pipes from its children included)
// and, of those, the bytes the disk served (read_bytes: what the page cache did not hold).
func exitedRead(pid int) (rchar, readBytes int64) {
	var info unix.Siginfo
	if unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil) != nil {
		return 0, 0
	}
	return procRead(pid)
}

// procRead is rchar and read_bytes from /proc/<pid>/io, 0 when it cannot be read.
func procRead(pid int) (rchar, readBytes int64) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0, 0
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("rchar: ")); ok {
			rchar, _ = strconv.ParseInt(string(v), 10, 64)
		} else if v, ok := bytes.CutPrefix(line, []byte("read_bytes: ")); ok {
			readBytes, _ = strconv.ParseInt(string(v), 10, 64)
		}
	}
	return rchar, readBytes
}
