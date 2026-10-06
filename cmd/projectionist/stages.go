package main

import (
	"projectionist/internal/stage"
	"projectionist/internal/stages/appbackup"
	"projectionist/internal/stages/audit"
	"projectionist/internal/stages/collections"
	"projectionist/internal/stages/details"
	"projectionist/internal/stages/gpuguard"
	"projectionist/internal/stages/health"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/stages/letterboxd"
	"projectionist/internal/stages/lists"
	"projectionist/internal/stages/posters"
	"projectionist/internal/stages/remote4k"
	"projectionist/internal/stages/rename"
	"projectionist/internal/stages/subtitles"
	"projectionist/internal/stages/tonight"
	"projectionist/internal/stages/watcher"
)

// stages is every stage this binary can run; a stage runs only if config has an enabled [stages.<name>] table.
// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{
		appbackup.Stage{},
		audit.Stage{},
		collections.Stage{},
		details.Stage{},
		gpuguard.Stage{},
		health.Stage{},
		jfsync.Stage{},
		letterboxd.Stage{},
		lists.Stage{},
		posters.Stage{},
		remote4k.Stage{},
		rename.Stage{},
		subtitles.Stage{},
		tonight.Stage{},
		watcher.Stage{},
	}
}
