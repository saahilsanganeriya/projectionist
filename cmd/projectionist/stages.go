package main

import (
	"projectionist/internal/stage"
	"projectionist/internal/stages/collections"
	"projectionist/internal/stages/gpuguard"
	"projectionist/internal/stages/remote4k"
	"projectionist/internal/stages/ssdmirror"
	"projectionist/internal/stages/watcher"
)

// stages is every stage this binary can run; a stage runs only if config has an enabled [stages.<name>] table.
// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{
		collections.Stage{},
		gpuguard.Stage{},
		remote4k.Stage{},
		ssdmirror.Stage{},
		watcher.Stage{},
	}
}
