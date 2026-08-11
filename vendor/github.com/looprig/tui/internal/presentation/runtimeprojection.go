package presentation

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

type loopRuntimeState struct {
	id              uuid.UUID
	agentName       identity.AgentName
	displayName     string
	description     string
	parentLoopID    uuid.UUID
	parentToolUseID string
	primer          bool
	mode            loop.ModeName
	runtime         event.ModelRuntime
	context         event.ContextMeasurement
	hasContext      bool
	live            bool
}

// runtimeProjection is the event-authoritative current runtime and topology
// view. Available catalog choices deliberately live outside this projection.
//
// The fixed access profile is NOT an event projection: it is a synchronous,
// consumer-supplied constant (SessionPresentation) captured at screen
// construction, so there is no mutable access level or ceiling to fold here.
type runtimeProjection struct {
	loops     map[uuid.UUID]loopRuntimeState
	loopOrder []uuid.UUID
}

func newRuntimeProjection() runtimeProjection {
	return runtimeProjection{loops: make(map[uuid.UUID]loopRuntimeState)}
}

func (p runtimeProjection) loop(id uuid.UUID) (loopRuntimeState, bool) {
	state, ok := p.loops[id]
	return state, ok
}

func (p runtimeProjection) ApplyEvent(ev event.Event) runtimeProjection {
	// ActiveLoopChanged is a session-scoped selection event, not a runtime/topology change:
	// it carries no per-loop runtime this projection folds. It is intentionally ignored — the
	// switch below has no case for it, so it leaves the projection unchanged with no explicit
	// early return.
	loopID := ev.EventHeader().LoopID
	if loopID.IsZero() {
		return p
	}
	state, exists := p.loops[loopID]
	changed := false
	switch value := ev.(type) {
	case event.LoopStarted:
		state = loopRuntimeState{
			id:              loopID,
			agentName:       value.AgentName,
			displayName:     value.DisplayName,
			description:     value.Description,
			parentLoopID:    value.Cause.LoopID,
			parentToolUseID: value.ParentToolUseID,
			primer:          value.Cause.LoopID.IsZero() && value.ParentToolUseID == "",
			mode:            loop.ModeName(value.InitialMode),
			runtime:         value.Runtime,
			live:            true,
		}
		changed = true
	case event.LoopModeChanged:
		state.id = loopID
		state.mode = loop.ModeName(value.Mode)
		state.runtime = value.Runtime
		changed = true
	case event.LoopInferenceChanged:
		state.id = loopID
		state.runtime = value.Runtime
		changed = true
	case event.ContextMeasured:
		state.id = loopID
		state.context = value.Measurement
		state.hasContext = true
		changed = true
	case event.CompactionCommitted:
		state.id = loopID
		state.context = value.PostContext
		state.hasContext = true
		changed = true
	case event.LoopIdle:
		state.id = loopID
		state.live = false
		changed = true
	case event.TurnStarted:
		state.id = loopID
		state.live = true
		changed = true
	}
	if !changed {
		return p
	}

	loops := make(map[uuid.UUID]loopRuntimeState, len(p.loops)+1)
	for id, current := range p.loops {
		loops[id] = current
	}
	loops[loopID] = state
	p.loops = loops
	if !exists {
		p.loopOrder = append(append([]uuid.UUID(nil), p.loopOrder...), loopID)
	}
	return p
}
