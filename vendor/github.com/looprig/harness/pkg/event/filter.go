package event

import "github.com/looprig/core/uuid"

// EventFilter is a subscriber's declared interest: which loop producers it wants
// events from, separated by class. It is declared interest (deterministic,
// evaluated at fan-out before the bounded send), distinct from backpressure drop.
// An agent's token firehose excluded by Ephemeral never even enters that
// subscriber's egress buffer.
type EventFilter struct {
	Ephemeral LoopScope // TokenDelta + tool lifecycle (ToolCallStarted/Completed) delivery
	Enduring  LoopScope // loop-produced StepDone, gates, terminals
}

// LoopScope selects which loops a class of events is delivered from. All is a
// short-circuit "every loop"; otherwise only loop ids present in Loops match.
type LoopScope struct {
	All   bool                   // deliver from every loop
	Loops map[uuid.UUID]struct{} // when !All, only these loop ids
}

// Matches reports whether loopID is in scope. All short-circuits to true; with a
// nil or empty Loops set and All false, nothing matches.
func (s LoopScope) Matches(loopID uuid.UUID) bool {
	if s.All {
		return true
	}
	_, ok := s.Loops[loopID]
	return ok
}

// ShouldDeliver reports whether ev passes filter for one subscriber. Session-scoped
// events (including the durable WorkflowActivity timeline) always deliver,
// bypassing LoopScope. Loop-scoped events are matched by the class-appropriate
// LoopScope against the producing loop's Header.LoopID. It is evaluated at fan-out,
// before the bounded send, so a filtered-out firehose never enters the egress
// buffer.
func ShouldDeliver(filter EventFilter, ev Event) bool {
	if ev == nil || ev.Visibility() != Public {
		return false
	}
	if ev.Scope() == ScopeSession {
		return true
	}
	scope := filter.Enduring
	if ev.Class() == Ephemeral {
		scope = filter.Ephemeral
	}
	return scope.Matches(ev.EventHeader().LoopID)
}
