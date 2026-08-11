package presentation

import "github.com/looprig/tui/internal/model"

type (
	displayID            = model.DisplayID
	compactionProjection = model.CompactionProjection
	collapseState        = model.CollapseState
)

func newCollapseState() collapseState { return model.NewCollapseState() }
