package details

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// ioIdle sets IOPRIO_CLASS_IDLE (3) for a process (IOPRIO_WHO_PROCESS = 1); x/sys has no wrapper for it.
func ioIdle(pid int) { unix.Syscall(unix.SYS_IOPRIO_SET, 1, uintptr(pid), 3<<13) }

// idleIO runs fn with the calling thread in the idle I/O class, for reads the Go process does itself; who = 0
// is the calling thread, and fn's file reads are blocking syscalls on it.
func idleIO(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	old, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, 0, 0)
	if errno != 0 {
		old = 0
	}
	unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, 3<<13)
	defer unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, old)
	return fn()
}
