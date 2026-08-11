// Package lifecycle provides the per-binding client lifecycle state machine:
// the legal states, the legal transitions between them, a thread-safe current
// state, and change notification. It owns nothing else — transports, catalogs
// and session wiring live elsewhere. Illegal transitions report
// *TransitionError; callers wrap it into their own error taxonomy (this
// package deliberately does not import pkg/client or internal/protocol).
//
// The modelled lifecycle is:
//
//	configured
//	    |
//	    v
//	starting -> authenticating -> discovering -> ready
//	    |             |              |
//	    +-------------+--------------+-> failed
//	                                      |
//	                                      v
//	                                  reconnecting
//
//	ready -> degraded -> reconnecting -> ready
//	  |
//	  v
//	closing -> closed
package lifecycle

import (
	"fmt"
	"slices"
	"sync"
)

// State is a binding's lifecycle state. The zero value is not a valid state.
type State uint8

// The declared lifecycle states. stateSentinel must remain the final entry:
// tests derive the declared range from it, so a state appended before it is
// automatically covered.
const (
	// StateConfigured is a binding that has been validated but not started.
	StateConfigured State = iota + 1
	// StateStarting is a binding whose transport is being established.
	StateStarting
	// StateAuthenticating is a binding performing authentication. Startup may
	// skip this state entirely for servers that require no auth.
	StateAuthenticating
	// StateDiscovering is a binding fetching its catalog from the server.
	StateDiscovering
	// StateReady is a binding serving calls normally.
	StateReady
	// StateDegraded is a binding still serving calls but with reduced
	// capability or a known fault.
	StateDegraded
	// StateReconnecting is a binding re-establishing its transport, which
	// re-runs authentication and discovery as needed.
	StateReconnecting
	// StateFailed is a binding that is not serving calls; it may be retried
	// via StateReconnecting or shut down.
	StateFailed
	// StateClosing is a binding shutting down. Shutdown always wins: it is
	// reachable from every non-terminal state.
	StateClosing
	// StateClosed is the terminal, absorbing state.
	StateClosed

	stateSentinel // must remain last; used by tests for exhaustiveness
)

// stateNames maps each declared state to its stable lowercase identifier.
// These identifiers are part of this package's contract: they surface in
// errors and telemetry, so they must not change.
var stateNames = [stateSentinel]string{
	StateConfigured:     "configured",
	StateStarting:       "starting",
	StateAuthenticating: "authenticating",
	StateDiscovering:    "discovering",
	StateReady:          "ready",
	StateDegraded:       "degraded",
	StateReconnecting:   "reconnecting",
	StateFailed:         "failed",
	StateClosing:        "closing",
	StateClosed:         "closed",
}

// unknownState renders any State outside the declared range, including the
// zero value and the sentinel.
const unknownState = "unknown"

// String returns the state's stable lowercase identifier, or "unknown" for any
// value outside the declared range.
func (s State) String() string {
	if !s.valid() {
		return unknownState
	}
	return stateNames[s]
}

// Terminal reports whether s admits no further transitions. Only StateClosed
// is terminal.
func (s State) Terminal() bool {
	return s == StateClosed
}

// valid reports whether s is a declared state.
func (s State) valid() bool {
	return s >= StateConfigured && s < stateSentinel
}

// stateSet is a bitset of States, one bit per state value.
type stateSet uint16

// stateSetBits is stateSet's width. The assertion below fails the build if a
// newly declared state would not fit, rather than silently truncating the set.
const stateSetBits = 16

const _ uint = stateSetBits - uint(stateSentinel)

// setOf builds a stateSet from declared states. It is only ever called with
// constants from the block above, all of which are in range.
func setOf(states ...State) stateSet {
	var set stateSet
	for _, s := range states {
		set |= 1 << s
	}
	return set
}

// has reports whether s is a member. Callers must have range-checked s.
func (set stateSet) has(s State) bool {
	return set&(1<<s) != 0
}

// transitions is the legal-transition table: transitions[from] holds every
// state reachable from from in one step. No state lists itself, so
// self-transitions are illegal. Absent/empty entries have no outgoing edge.
//
// The rationale for the less obvious edges: startup may skip authentication
// for no-auth servers and go straight to discovering; reconnecting re-runs
// authentication and discovery; a failed binding is retried via reconnecting;
// closing is reachable from every non-terminal state because shutdown always
// wins; closed is absorbing.
var transitions = [stateSentinel]stateSet{
	StateConfigured:     setOf(StateStarting, StateClosing),
	StateStarting:       setOf(StateAuthenticating, StateDiscovering, StateReady, StateFailed, StateClosing),
	StateAuthenticating: setOf(StateDiscovering, StateReady, StateFailed, StateClosing),
	StateDiscovering:    setOf(StateReady, StateFailed, StateClosing),
	StateReady:          setOf(StateDegraded, StateReconnecting, StateFailed, StateClosing),
	StateDegraded:       setOf(StateReady, StateReconnecting, StateFailed, StateClosing),
	StateReconnecting:   setOf(StateAuthenticating, StateDiscovering, StateReady, StateDegraded, StateFailed, StateClosing),
	StateFailed:         setOf(StateReconnecting, StateClosing),
	StateClosing:        setOf(StateClosed),
	StateClosed:         0, // terminal: absorbing
}

// CanTransition reports whether the transition from -> to is legal. It is
// fail-closed: a state outside the declared range — including the zero value —
// is never a legal source or destination, and no state may transition to
// itself.
func CanTransition(from, to State) bool {
	if !from.valid() || !to.valid() {
		return false
	}
	return transitions[from].has(to)
}

// TransitionError reports an attempted transition the state machine forbids.
type TransitionError struct {
	From, To State
}

// Error renders "illegal lifecycle transition <from> -> <to>".
func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal lifecycle transition %s -> %s", e.From, e.To)
}

// Machine is a binding's current lifecycle state, guarded for concurrent use,
// with change notification for registered watchers. The zero value is not
// usable; call NewMachine.
//
// A Machine has no notion of who owns a transition: any goroutine may move it,
// and every To is judged against the state at that moment. Because shutdown is
// reachable from every non-terminal state, a multi-step sequence such as
// startup can be overtaken at any step by a concurrent close. Callers must
// therefore treat a *TransitionError as a legitimate race outcome and unwind;
// see To.
type Machine struct {
	mu       sync.Mutex
	current  State
	watchers []*watcher
	pending  []change // transitions committed but not yet notified
	draining bool     // a goroutine is delivering pending notifications
}

// watcher is one registered callback. It is tracked by pointer so cancel can
// identify it regardless of its position in the slice.
type watcher struct {
	fn func(from, to State)
}

// change is one committed transition awaiting notification.
type change struct {
	from, to State
}

// NewMachine returns a Machine in StateConfigured.
func NewMachine() *Machine {
	return &Machine{current: StateConfigured}
}

// State returns the current state. It is safe to call from a Watch callback.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// WatcherCount reports how many watchers are currently registered.
//
// It exists so that a caller which registers a watcher can prove it
// deregistered it. Nothing else can: a leaked watcher spawns no goroutine (see
// Watch — delivery runs on the committing goroutine), and once a machine is in
// a terminal state no transition can be made to reveal one. What a leak costs
// is the callback and everything it captured, held for the machine's lifetime,
// which is invisible until it matters.
func (m *Machine) WatcherCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watchers)
}

// To transitions the machine to next, returning *TransitionError if the
// transition is illegal from the current state — including a self-transition
// or a state outside the declared range — in which case the current state is
// unchanged.
//
// The legality check is against the CURRENT state, not against an expected
// source state: To is not a compare-and-swap. A caller cannot assert that the
// state it last observed still held, because another goroutine may have moved
// the machine in between. Callers needing that guarantee must serialize their
// own transitions.
//
// Consequently a *TransitionError from a step of an otherwise-correct sequence
// (say discovering -> ready during startup) does not imply a bug in the
// caller: it usually means someone else legally moved the machine first, and
// since shutdown is reachable from every non-terminal state, that someone is
// most often a concurrent close. The contract for such a caller is to treat
// the error as "my sequence has been overtaken", abandon the remaining steps,
// and unwind — not to retry, force the state, or panic.
//
// On success the new state is committed before any watcher is notified, and
// notifications are delivered without holding the machine's lock, so a
// callback may call back into the machine.
//
// Notification is serialized: transitions are delivered to watchers in the
// order they were committed, never concurrently. The committing goroutine
// normally performs the delivery itself and returns once its own notification
// has been delivered. If another goroutine is already delivering, the
// committing goroutine enqueues its change and returns immediately, leaving
// delivery to that goroutine — no change is ever dropped. This is also what
// makes a To call from inside a callback safe: it commits and returns, and the
// callback's own delivery loop notifies afterwards.
//
// If a watcher panics, the panic is re-raised to the caller of To once every
// watcher and pending change has been delivered, so the machine is left
// consistent and usable. When several watchers panic on one drain, only the
// first value propagates.
func (m *Machine) To(next State) error {
	m.mu.Lock()

	from := m.current
	if !CanTransition(from, next) {
		m.mu.Unlock()
		return &TransitionError{From: from, To: next}
	}
	m.current = next
	m.pending = append(m.pending, change{from: from, to: next})

	if m.draining {
		// Another goroutine owns delivery and will pick this change up.
		m.mu.Unlock()
		return nil
	}
	m.draining = true
	if p := m.drain(); p != nil {
		panic(p.value)
	}
	return nil
}

// drain delivers every pending change to the watchers, then hands ownership of
// delivery back. The caller must hold m.mu with m.draining already set; drain
// returns with m.mu released and m.draining cleared, whatever the watchers do.
//
// A panicking watcher must not corrupt the machine, so each callback is
// isolated: a panic is recovered, the remaining watchers and pending changes
// are still delivered, and the first recovered value is returned for the
// caller to re-raise. Abandoning the loop instead would clear neither
// m.draining (wedging every later To into the early return above, silently
// notifying nobody) nor m.pending (dropping changes other goroutines committed
// and already returned nil for).
func (m *Machine) drain() *watcherPanic {
	defer func() {
		m.draining = false
		m.mu.Unlock()
	}()

	var first *watcherPanic
	for len(m.pending) > 0 {
		c := m.pending[0]
		m.pending = slices.Delete(m.pending, 0, 1)
		// Snapshot under the lock so a concurrent Watch/cancel cannot race
		// with delivery, then release it: callbacks must never run with the
		// machine locked.
		snapshot := slices.Clone(m.watchers)
		m.mu.Unlock()
		for _, w := range snapshot {
			if p := notify(w, c); p != nil && first == nil {
				first = p
			}
		}
		m.mu.Lock()
	}
	return first
}

// watcherPanic carries a value recovered from a panicking watcher so it can be
// re-raised once the machine is consistent again. recover is typed any by the
// language; the value is never inspected or passed on, only re-panicked, so it
// is not narrowed to a domain type.
type watcherPanic struct {
	value any
}

// notify invokes w for change c, converting a panic into a returned value so
// one bad watcher cannot abort delivery to the others.
func notify(w *watcher, c change) (p *watcherPanic) {
	defer func() {
		if r := recover(); r != nil {
			p = &watcherPanic{value: r}
		}
	}()
	w.fn(c.from, c.to)
	return nil
}

// Watch registers fn to be called on every successful transition, and returns
// a cancel func that deregisters it.
//
// Callbacks are invoked in registration order, after the new state has been
// committed, and never while the machine's lock is held — fn may safely call
// State (or To; see that method) on the machine. fn runs on the goroutine
// performing delivery and blocks further notification, so it must not block.
// A panic in fn is isolated: the other watchers still receive the change, and
// the value is re-raised to the caller of To (see that method).
//
// Registration is NOT a snapshot of the current state. Watchers are resolved
// when a change's delivery BEGINS, not when it was committed, so a watcher
// registered after a transition committed but before its delivery began still
// receives it — possible whenever registration races an in-flight drain. A
// caller that seeds its own view from State and then calls Watch may therefore
// be told about a change it has already accounted for, and must tolerate the
// repeat rather than assume every delivery is news. (Registering during a
// change's delivery — from another watcher's callback — is the other side of
// the same rule: that watcher misses the in-flight change and starts at the
// next one.)
//
// cancel is idempotent and safe to call concurrently. Once it returns, fn will
// not be invoked for any notification whose delivery starts afterwards; a
// notification already in flight may still complete.
//
// A nil fn is a programmer error and panics.
func (m *Machine) Watch(fn func(from, to State)) (cancel func()) {
	if fn == nil {
		panic("lifecycle.Machine.Watch: fn must not be nil")
	}
	w := &watcher{fn: fn}

	m.mu.Lock()
	m.watchers = append(m.watchers, w)
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if i := slices.Index(m.watchers, w); i >= 0 {
				m.watchers = slices.Delete(m.watchers, i, i+1)
			}
		})
	}
}
