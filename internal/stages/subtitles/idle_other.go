//go:build !linux

package subtitles

import "golang.org/x/sys/unix"

func idle(pgid int) { unix.Setpriority(unix.PRIO_PGRP, pgid, 19) }
