package details

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// lowestIO is best-effort class (2) level 7, the lowest priority that still gets disk time. The idle class (3)
// starves under sustained load: on the NAS, with other jobs keeping the disks busy, a 256-read hole probe crawled
// at one 4 KiB read per 1-2 s. IOPRIO_WHO_PROCESS = 1; x/sys has no wrapper for ioprio_set.
const lowestIO = 2<<13 | 7

func ioIdle(pid int) { unix.Syscall(unix.SYS_IOPRIO_SET, 1, uintptr(pid), lowestIO) }

// idleIO runs fn with the calling thread at the lowest I/O priority, for reads the Go process does itself;
// who = 0 is the calling thread, and fn's file reads are blocking syscalls on it.
func idleIO(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	old, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, 0, 0)
	if errno != 0 {
		old = 0
	}
	unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, lowestIO)
	defer unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, old)
	return fn()
}
