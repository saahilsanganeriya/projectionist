package main

import (
	"projectionist/internal/stage"
	"projectionist/internal/stages/details"
	"projectionist/internal/stages/gpuguard"
	"projectionist/internal/stages/remote4k"
	"projectionist/internal/stages/ssdmirror"
	"projectionist/internal/stages/subtitles"
	"projectionist/internal/stages/watcher"
)

// stages is every stage this binary can run; a stage runs only if config has an enabled [stages.<name>] table.
// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{
		details.Stage{},
		gpuguard.Stage{},
		remote4k.Stage{},
		ssdmirror.Stage{},
		subtitles.Stage{},
		watcher.Stage{},
	}
}
