package details

import "golang.org/x/sys/unix"

// ioIdle sets IOPRIO_CLASS_IDLE (3) for the process (IOPRIO_WHO_PROCESS = 1); x/sys has no wrapper for it.
func ioIdle(pid int) { unix.Syscall(unix.SYS_IOPRIO_SET, 1, uintptr(pid), 3<<13) }
