package main

import (
	"projectionist/internal/stage"
	"projectionist/internal/stages/gpuguard"
	"projectionist/internal/stages/health"
	"projectionist/internal/stages/remote4k"
)

// stages is every stage this binary can run; a stage runs only if config has an enabled [stages.<name>] table.
// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{
		gpuguard.Stage{},
		health.Stage{},
		remote4k.Stage{},
	}
}
