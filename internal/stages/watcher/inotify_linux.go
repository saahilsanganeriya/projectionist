package watcher

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// No IN_MODIFY: a copy's growth is seen by the busy check every `quiet`, and per-write events would flood the queue.
const watchMask = unix.IN_CREATE | unix.IN_CLOSE_WRITE | unix.IN_ATTRIB | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_DELETE | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

type inotify struct {
	fd     int
	file   *os.File // the same fd, non-blocking, so Read waits in the runtime poller and Close wakes it
	events chan []event
	done   chan struct{}
	once   sync.Once

	mu    sync.Mutex
	paths map[int]string // wd → directory
	wds   map[string]int
}

func openInotify() (source, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify_init1: %w", err) // EMFILE: fs.inotify.max_user_instances reached
	}
	in := &inotify{
		fd: fd, file: os.NewFile(uintptr(fd), "inotify"),
		events: make(chan []event, 16), done: make(chan struct{}),
		paths: map[int]string{}, wds: map[string]int{},
	}
	go in.read()
	return in, nil
}

func (in *inotify) Add(dir string) error {
	wd, err := unix.InotifyAddWatch(in.fd, dir, watchMask)
	if errors.Is(err, unix.ENOSPC) {
		return errWatchLimit
	}
	if err != nil {
		return &os.PathError{Op: "inotify_add_watch", Path: dir, Err: err}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if old, ok := in.paths[int(wd)]; ok && old != dir {
		delete(in.wds, old) // the same folder under a new name: a rename whose events were lost
	}
	in.paths[int(wd)], in.wds[dir] = dir, int(wd)
	return nil
}

func (in *inotify) Events() <-chan []event { return in.events }

func (in *inotify) Len() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.paths)
}

func (in *inotify) Close() error {
	var err error
	in.once.Do(func() {
		close(in.done)
		err = in.file.Close()
	})
	return err
}

func (in *inotify) read() {
	defer close(in.events)
	buf := make([]byte, 64<<10)
	for {
		n, err := in.file.Read(buf)
		if err != nil {
			return // closed
		}
		select {
		case in.events <- in.parse(buf[:n]):
		case <-in.done:
			return
		}
	}
}

func (in *inotify) parse(buf []byte) []event {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []event
	for off := 0; off+unix.SizeofInotifyEvent <= len(buf); {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		nameAt := off + unix.SizeofInotifyEvent
		off = nameAt + int(raw.Len)
		if raw.Mask&unix.IN_Q_OVERFLOW != 0 {
			out = append(out, event{Overflow: true})
			continue
		}
		wd := int(raw.Wd)
		dir, ok := in.paths[wd]
		if !ok {
			continue // a watch already dropped
		}
		if raw.Mask&unix.IN_IGNORED != 0 { // the folder is gone, or its watch was removed
			delete(in.paths, wd)
			if in.wds[dir] == wd {
				delete(in.wds, dir)
			}
			continue
		}
		path := dir
		if raw.Len > 0 {
			path = dir + "/" + strings.TrimRight(string(buf[nameAt:off]), "\x00")
		}
		isDir := raw.Mask&unix.IN_ISDIR != 0
		if isDir && raw.Mask&unix.IN_MOVED_FROM != 0 {
			in.forget(path) // its watches would keep reporting under a name that no longer exists
		}
		out = append(out, event{Path: path, Dir: isDir, New: raw.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0})
	}
	return out
}

// forget drops the watches on dir and every folder under it; the caller holds mu.
func (in *inotify) forget(dir string) {
	for d, wd := range in.wds {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			unix.InotifyRmWatch(in.fd, uint32(wd))
			delete(in.wds, d)
			delete(in.paths, wd)
		}
	}
}
