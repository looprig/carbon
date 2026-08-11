// This file defines what a caller may observe about a binding's auth posture.
//
// Status is the counterpart to TokenSet: same subject, opposite contract.
// TokenSet holds the credential and refuses to be printed; Status holds no
// credential and is built to be printed — logged, rendered in a UI, and shipped
// to telemetry as-is. Keeping them as two types is what lets the second be
// freely copied into observability paths that the first must never enter.
//
// Nothing here may ever carry token material. The fields are exactly the
// non-secret facts: what state auth is in, when it lapses, what it is good for,
// and a bounded classification of why it failed.

package auth

import (
	"slices"
	"time"
)

// State is a binding's auth posture. The zero value is not a valid state.
type State uint8

// The auth states. Values are contiguous starting at 1; the zero value is
// reserved as "no state".
//
// The set is deliberately small and mirrors the Class taxonomy: every state a
// caller can act on differently, and no state it cannot. In particular there is
// no "authenticating" state here — a binding being mid-flow is a lifecycle
// fact, owned by the connection's own state machine (see internal/lifecycle's
// StateAuthenticating), not a property of the credentials.
const (
	// StateAnonymous is a binding with no auth configured. It is not a
	// failure: plenty of servers need no credentials.
	StateAnonymous State = iota + 1
	// StateRequired is a binding that needs credentials it does not have.
	// This is the state that warrants an interactive login.
	StateRequired
	// StateAuthenticated is a binding holding a usable token.
	StateAuthenticated
	// StateExpired is a binding whose token has lapsed. It is separate from
	// StateRequired because it may be recoverable without the user: a refresh
	// token, if there is one, resolves it silently.
	StateExpired
	// StateDenied is a binding whose authorization was refused. It is
	// separate from StateFailed because retrying will not help — the answer
	// was "no", not "something broke".
	StateDenied
	// StateFailed is a binding whose auth broke: discovery, registration, or
	// a refresh that errored rather than being refused.
	StateFailed
	stateSentinel // must remain last; used by tests for exhaustiveness
)

// String returns a stable lowercase snake_case identifier for the state.
// Undeclared values return "unknown".
func (s State) String() string {
	switch s {
	case StateAnonymous:
		return "anonymous"
	case StateRequired:
		return "required"
	case StateAuthenticated:
		return "authenticated"
	case StateExpired:
		return "expired"
	case StateDenied:
		return "denied"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Status is a snapshot of a binding's auth posture. It is a value: callers may
// hold, copy, log, and mutate it freely.
//
// Build one with NewStatus, which bounds Failure and detaches Scopes. The
// fields are exported because this type exists to be read — by a UI, a log
// line, a metric — and hiding them behind accessors would buy nothing: there is
// no secret here to protect.
type Status struct {
	// State is the auth posture.
	State State
	// Expiry is when the current token lapses. Zero when there is no token or
	// the server stated no expiry.
	Expiry time.Time
	// Scopes are the scopes currently granted, if any.
	Scopes []string
	// Failure is a bounded, normalized, secret-free classification of what
	// went wrong. Empty unless State is a failure state.
	Failure string
}

// NewStatus builds a Status, bounding failure to MaxMessageBytes, normalizing
// its control characters, and cloning scopes so the Status cannot alias the
// caller's slice.
//
// failure must not contain secret material: it is rendered verbatim wherever
// the Status goes, which is by design everywhere.
func NewStatus(state State, expiry time.Time, scopes []string, failure string) Status {
	return Status{
		State:   state,
		Expiry:  expiry,
		Scopes:  slices.Clone(scopes),
		Failure: boundMessage(failure),
	}
}

// StatusOf derives the observable posture of a token set as of now. It is the
// single place the credential model and the observable model meet, so that
// "what does this token mean?" has one answer instead of one per caller.
//
// It reports only what a TokenSet can prove: a set with no access token is
// StateRequired, a lapsed one is StateExpired, and a usable one is
// StateAuthenticated. StateAnonymous, StateDenied, and StateFailed are not
// derivable from a token — they are facts about configuration or about what a
// server said — so the flow that learns them builds its Status directly.
func StatusOf(set TokenSet, now time.Time) Status {
	state := StateAuthenticated
	switch {
	case !set.Valid():
		state = StateRequired
	case set.Expired(now):
		state = StateExpired
	}
	// set.scopes is cloned once, by NewStatus. Going through set.Scopes()
	// would clone it a second time and throw the first copy away.
	return NewStatus(state, set.Expiry(), set.scopes, "")
}
