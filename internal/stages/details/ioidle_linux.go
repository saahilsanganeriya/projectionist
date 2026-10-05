package details

import (
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// lowestIO is best-effort class (2) level 7, the lowest priority that still gets disk time. The idle class (3)
// starves under sustained load: on the NAS, with other jobs keeping the disks busy, a 256-read hole probe crawled
// at one 4 KiB read per 1-2 s. IOPRIO_WHO_PROCESS = 1; x/sys has no wrapper for ioprio_set.
const lowestIO = 2<<13 | 7

// background runs fn on an OS thread of its own at nice 19 and the lowest best-effort I/O priority (the NAS disks
// use CFQ, which honours it), so Plex and Jellyfin win any contention. Linux keeps both per thread, the children
// fn starts (ffprobe, ffmpeg) inherit them, and so do fn's own reads. The thread is discarded afterwards: it ends
// locked, because raising a nice value again needs CAP_SYS_NICE, which the container drops. The Go runtime never
// clones a new thread from a locked one, so the priority cannot leak to other stages.
func background(fn func()) {
	panicked := make(chan any, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with this goroutine
		defer func() { panicked <- recover() }()
		syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19) // who = 0: the calling thread
		unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, lowestIO)
		fn()
	}()
	if p := <-panicked; p != nil {
		panic(p) // to the scheduler's recover, as if fn had run on the caller's goroutine
	}
}
