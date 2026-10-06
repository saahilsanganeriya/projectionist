//go:build !linux

package subtitles

import "golang.org/x/sys/unix"

func idle(pgid int) { unix.Setpriority(unix.PRIO_PGRP, pgid, 19) }

// exitedRead and procRead are 0 off Linux: no /proc/<pid>/io to read.
func exitedRead(int) (int64, int64) { return 0, 0 }

func procRead(int) (int64, int64) { return 0, 0 }
