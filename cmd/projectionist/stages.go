package main

import (
	"projectionist/internal/stage"
	"projectionist/internal/stages/audit"
	"projectionist/internal/stages/collections"
	"projectionist/internal/stages/details"
	"projectionist/internal/stages/gpuguard"
	"projectionist/internal/stages/health"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/stages/posters"
	"projectionist/internal/stages/remote4k"
	"projectionist/internal/stages/rename"
	"projectionist/internal/stages/ssdmirror"
	"projectionist/internal/stages/subtitles"
	"projectionist/internal/stages/watcher"
)

// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{
		audit.Stage{},
		collections.Stage{},
		details.Stage{},
		gpuguard.Stage{},
		health.Stage{},
		jfsync.Stage{},
		posters.Stage{},
		remote4k.Stage{},
		rename.Stage{},
		ssdmirror.Stage{},
		subtitles.Stage{},
		watcher.Stage{},
	}
}
