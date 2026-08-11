package process

// State is one of the externally visible supervised-process lifecycle
// states (spec "State machine").
type State string

// The closed set of externally visible states.
const (
	StateStarting      State = "starting"
	StateRunning       State = "running"
	StateExited        State = "exited"
	StateFailed        State = "failed"
	StateTimedOut      State = "timed_out"
	StateInterrupted   State = "interrupted"
	StateTerminated    State = "terminated"
	StateKilled        State = "killed"
	StateLostOnRestore State = "lost_on_restore"
)

// allStates is the closed state domain backing Valid.
var allStates = map[State]bool{
	StateStarting:      true,
	StateRunning:       true,
	StateExited:        true,
	StateFailed:        true,
	StateTimedOut:      true,
	StateInterrupted:   true,
	StateTerminated:    true,
	StateKilled:        true,
	StateLostOnRestore: true,
}

// Valid reports whether s belongs to the closed state domain.
func (s State) Valid() bool { return allStates[s] }

// terminalStates are the states that accept no further transitions (spec
// "State machine": "Terminal states are immutable").
var terminalStates = map[State]bool{
	StateExited:        true,
	StateFailed:        true,
	StateTimedOut:      true,
	StateInterrupted:   true,
	StateTerminated:    true,
	StateKilled:        true,
	StateLostOnRestore: true,
}

// Terminal reports whether s is a terminal state that accepts no further
// transitions.
func (s State) Terminal() bool { return terminalStates[s] }

// transitions encodes exactly the approved edges from the spec's "State
// machine" section:
//
//	starting -> running
//	starting -> failed
//	starting -> terminated | killed
//	running  -> exited | failed | timed_out | terminated | killed
//	running  -> interrupted (only when interrupt causes exit)
//	starting | running -> lost_on_restore (restore reconciliation)
//
// Terminal states are deliberately absent as map keys, so a lookup against
// one returns the zero (nil) adjacency set and canTransition reports false
// for every destination, including a self-transition.
var transitions = map[State]map[State]bool{
	StateStarting: {
		StateRunning:       true,
		StateFailed:        true,
		StateTerminated:    true,
		StateKilled:        true,
		StateLostOnRestore: true,
	},
	StateRunning: {
		StateExited:        true,
		StateFailed:        true,
		StateTimedOut:      true,
		StateTerminated:    true,
		StateKilled:        true,
		StateInterrupted:   true,
		StateLostOnRestore: true,
	},
}

// canTransition reports whether moving from `from` to `to` is an approved
// edge in the supervision state machine.
func canTransition(from, to State) bool {
	return transitions[from][to]
}

// TransitionError reports an attempted state transition that is not an
// approved edge in the supervision state machine (including any transition
// out of a terminal state, or into an unrecognized state).
type TransitionError struct {
	From State
	To   State
}

func (e *TransitionError) Error() string {
	return "process: invalid state transition " + string(e.From) + " -> " + string(e.To)
}

// transition validates from -> to against the approved state machine and
// returns to on success. On an unapproved edge it returns from unchanged and
// a *TransitionError, so a caller that ignores the error cannot accidentally
// observe a moved state. It is the unexported transition-validation seam
// this task's domain requires; a process record built in a later task calls
// it from within this same package.
func transition(from, to State) (State, error) {
	if !canTransition(from, to) {
		return from, &TransitionError{From: from, To: to}
	}
	return to, nil
}
