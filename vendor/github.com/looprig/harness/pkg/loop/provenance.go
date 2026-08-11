package loop

import "github.com/looprig/core/uuid"

// Provenance identifies the parent turn/step that spawned a loop.
type Provenance struct {
	LoopID uuid.UUID
	TurnID uuid.UUID
	StepID uuid.UUID
}
