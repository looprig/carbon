package hub

import (
	"errors"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// SessionPhase is the coarse quiescence phase of the whole session. SessionIdle is
// the zero value, so a freshly built session is idle until its first turn.
type SessionPhase uint8

const (
	SessionIdle    SessionPhase = iota // quiescent — user may type again; zero value
	SessionActive                      // >=1 loop busy, or a hand-back in flight
	SessionStopped                     // after Shutdown (distinct from Idle)
)

// ErrSessionStopped is returned by WaitIdle when the session has stopped (either
// already at entry or stopped while a caller was waiting). It is a leaf sentinel
// with no additional context fields.
var ErrSessionStopped = errors.New("hub: session stopped")

// activityKind distinguishes outstanding work so a busy loop, a pending hand-back,
// and a blocking hustle with the same uuid coexist as distinct set entries.
type activityKind uint8

const (
	kindLoop   activityKind = iota // {loop, LoopID}: a busy loop
	kindWake                       // {wake, subagentLoopID}: a pending hand-back
	kindHustle                     // {hustle, RunID}: a blocking one-shot invocation
)

// activityKey is the set key for one outstanding unit of work. Keying on {kind, id}
// lets {loop, X} and {wake, X} coexist.
type activityKey struct {
	kind activityKind
	id   uuid.UUID
}

// sessionState is the quiescence model: a set of outstanding work plus the phase.
// active is empty iff the session is quiescent. All mutations run under the hub
// lock (there is no session goroutine); sessionState itself carries no lock.
type sessionState struct {
	active map[activityKey]struct{}
	phase  SessionPhase
}

// newSessionState builds an idle, empty sessionState with its set initialized.
func newSessionState() sessionState {
	return sessionState{active: make(map[activityKey]struct{})}
}

// add inserts a key (idempotent: re-adding an existing key does not double-count).
func (s *sessionState) add(k activityKey) {
	if s.active == nil {
		s.active = make(map[activityKey]struct{})
	}
	s.active[k] = struct{}{}
}

// remove deletes a key (underflow-safe: deleting an absent key is a no-op).
func (s *sessionState) remove(k activityKey) { delete(s.active, k) }

// applyActivity runs the shared edge-derivation for every active-mutating
// operation (event-derived in PublishEvent, and the session methods expectTurn/
// cancelExpectTurn). It applies mutate, then derives the at-most-one phase edge:
//
//   - SessionStopped is terminal: return nil WITHOUT running mutate.
//   - Active -> Idle (non-empty -> empty): set phase=SessionIdle, return a SessionIdle
//     event.
//   - Idle -> Active (empty -> non-empty): set phase=SessionActive, return a
//     SessionActive event.
//   - otherwise (no emptiness edge): return nil.
//
// Derived events carry Header{SessionID} and zero loop/turn/step ids — they are the
// RAW (unstamped) edge events; the caller mints their EventID+CreatedAt via the
// Factory before durably appending them. Waking WaitIdle waiters on the Active->Idle
// edge is the CALLER's job, sequenced AFTER the derived SessionIdle is durably
// appended (fail-secure: a failed append must not falsely wake a waiter to "idle").
// The caller detects that edge by the returned event's concrete type (SessionIdle).
// sessionState itself owns no waiter registry (SRP).
func (s *sessionState) applyActivity(sessionID uuid.UUID, mutate func()) event.Event {
	if s.phase == SessionStopped {
		return nil // stopped is terminal; never mutate
	}
	wasEmpty := len(s.active) == 0
	mutate()
	isEmpty := len(s.active) == 0

	switch {
	case !wasEmpty && isEmpty: // Active -> Idle
		s.phase = SessionIdle
		return event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}}
	case wasEmpty && !isEmpty: // Idle -> Active
		s.phase = SessionActive
		return event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}}
	default:
		return nil // no edge crossed
	}
}
