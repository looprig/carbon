package model

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// compactionAttempt identifies one loop-owned compaction attempt. Terminal attempts remain
// tombstoned for the session so a replayed terminal suppresses an earlier buffered start.
type compactionAttempt struct {
	loopID    uuid.UUID
	attemptID event.CompactAttemptID
}

// compactionProjection is the pure, per-session view of compaction activity. Values are
// immutable: ApplyEvent clones only the map it changes, so live delivery and restore folding
// can share the same reducer without retaining aliases to prior states.
type CompactionProjection struct {
	active    map[uuid.UUID]event.CompactAttemptID
	terminals map[compactionAttempt]struct{}
}

// ApplyEvent folds compaction lifecycle events. Starts activate their loop unless the same
// attempt is terminal; terminals tombstone their exact pair and clear only a matching start.
func (p CompactionProjection) ApplyEvent(ev event.Event) CompactionProjection {
	switch value := ev.(type) {
	case event.CompactionStarted:
		return p.applyStarted(value.EventHeader().LoopID, value.AttemptID)
	case event.CompactionCommitted:
		return p.applyTerminal(value.EventHeader().LoopID, value.AttemptID)
	case event.CompactionRejected:
		return p.applyTerminal(value.EventHeader().LoopID, value.AttemptID)
	default:
		return p
	}
}

// IsActive reports whether loopID currently has a non-terminal compaction attempt.
func (p CompactionProjection) IsActive(loopID uuid.UUID) bool {
	_, ok := p.active[loopID]
	return ok
}

func (p CompactionProjection) applyStarted(loopID uuid.UUID, attemptID event.CompactAttemptID) CompactionProjection {
	if p.IsTerminal(loopID, attemptID) || p.active[loopID] == attemptID {
		return p
	}
	next := p
	next.active = cloneActiveCompactions(p.active)
	next.active[loopID] = attemptID
	return next
}

func (p CompactionProjection) applyTerminal(loopID uuid.UUID, attemptID event.CompactAttemptID) CompactionProjection {
	key := compactionAttempt{loopID: loopID, attemptID: attemptID}
	_, alreadyTerminal := p.terminals[key]
	activeMatches := p.active[loopID] == attemptID
	if alreadyTerminal && !activeMatches {
		return p
	}

	next := p
	if !alreadyTerminal {
		next.terminals = cloneTerminalCompactions(p.terminals)
		next.terminals[key] = struct{}{}
	}
	if activeMatches {
		next.active = cloneActiveCompactions(p.active)
		delete(next.active, loopID)
	}
	return next
}

// IsTerminal reports whether the attempt has already reached a durable terminal event.
func (p CompactionProjection) IsTerminal(loopID uuid.UUID, attemptID event.CompactAttemptID) bool {
	_, ok := p.terminals[compactionAttempt{loopID: loopID, attemptID: attemptID}]
	return ok
}

func cloneActiveCompactions(in map[uuid.UUID]event.CompactAttemptID) map[uuid.UUID]event.CompactAttemptID {
	out := make(map[uuid.UUID]event.CompactAttemptID, len(in)+1)
	for loopID, attemptID := range in {
		out[loopID] = attemptID
	}
	return out
}

func cloneTerminalCompactions(in map[compactionAttempt]struct{}) map[compactionAttempt]struct{} {
	out := make(map[compactionAttempt]struct{}, len(in)+1)
	for attempt := range in {
		out[attempt] = struct{}{}
	}
	return out
}
