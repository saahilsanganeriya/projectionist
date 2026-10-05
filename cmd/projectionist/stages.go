package main

import "projectionist/internal/stage"

// stages is every stage this binary can run; a stage runs only if config has an enabled [stages.<name>] table.
// One line per stage, kept sorted, so parallel branches merge cleanly.
func stages() []stage.Stage {
	return []stage.Stage{}
}
