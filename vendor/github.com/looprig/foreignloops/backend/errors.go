package backend

import (
	"errors"
	"fmt"

	"github.com/looprig/foreignloops/driver"
)

// ConfigError reports invalid backend composition or runtime wiring.
type ConfigError struct{ Field, Reason string }

func (e *ConfigError) Error() string {
	return "foreignloop: config: " + e.Field + ": " + e.Reason
}

// ForeignSessionBusyError reports that another process holds the liveness lock
// for the same foreign session and workspace.
type ForeignSessionBusyError struct {
	SID, Cwd string
	PID      int
}

func (e *ForeignSessionBusyError) Error() string {
	return fmt.Sprintf("foreignloop: session %s busy (pid %d holds %s lock)", e.SID, e.PID, e.Cwd)
}

// LockError reports an I/O failure while acquiring a foreign-session lock.
type LockError struct {
	Op    string
	Path  string
	Cause error
}

func (e *LockError) Error() string {
	return "foreignloop: lock " + e.Op + " " + e.Path + ": " + e.Cause.Error()
}

func (e *LockError) Unwrap() error { return e.Cause }

// ForeignResultError reports a result-level failure sent by the foreign agent.
type ForeignResultError struct{ Detail string }

func (e *ForeignResultError) Error() string {
	return "foreignloop: foreign result error: " + e.Detail
}

// modelFacingResultError is a result-level failure whose detail was already
// projected and bounded by the driver for display to a model. Its dedicated
// marker prevents ordinary provider output from becoming model-facing. The
// concrete type stays private; callers classify it through ModelFacingError.
type modelFacingResultError struct{ Detail string }

func (e *modelFacingResultError) Error() string {
	return "foreignloop: model-facing result error: " + e.Detail
}

// ModelFacingError returns the already-safe detail intended for model display.
func (e *modelFacingResultError) ModelFacingError() string { return e.Detail }

func resultError(input driver.Event) error {
	if input.Kind == driver.KindModelFacingError {
		return &modelFacingResultError{Detail: input.ErrText}
	}
	return &ForeignResultError{Detail: input.ErrText}
}

func joinTurnErrors(terminal, closeErr error) error {
	if _, ok := terminal.(*modelFacingResultError); ok {
		// A model-facing projection is the only detail allowed to cross this
		// boundary. Do not join it with an arbitrary stream-close diagnostic.
		return terminal
	}
	return errors.Join(terminal, closeErr)
}

// ForeignProtocolError reports a stream that ended without a result terminal.
type ForeignProtocolError struct{ Reason string }

func (e *ForeignProtocolError) Error() string {
	return "foreignloop: foreign protocol: " + e.Reason
}

// ForeignPublicationError reports a checked lifecycle event that the session
// publisher rejected. The actor must stop rather than continue with a local
// state transition that is absent from the durable event stream.
type ForeignPublicationError struct {
	Event string
	Cause error
}

func (e *ForeignPublicationError) Error() string {
	if e == nil {
		return "foreignloop: lifecycle publication failed"
	}
	if e.Cause == nil {
		return "foreignloop: lifecycle publication failed: " + e.Event
	}
	return "foreignloop: lifecycle publication failed: " + e.Event + ": " + e.Cause.Error()
}

func (e *ForeignPublicationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SnapshotErrorReason classifies why a consistent backend snapshot was not
// available.
type SnapshotErrorReason string

const (
	SnapshotLoopExited  SnapshotErrorReason = "loop_exited"
	SnapshotContextDone SnapshotErrorReason = "context_done"
)

// SnapshotError reports that the backend could not return a consistent view of
// its committed state.
type SnapshotError struct {
	Reason SnapshotErrorReason
	Cause  error
}

func (e *SnapshotError) Error() string {
	switch e.Reason {
	case SnapshotLoopExited:
		return "foreignloop: snapshot failed: loop exited"
	case SnapshotContextDone:
		if e.Cause != nil {
			return "foreignloop: snapshot failed: context done: " + e.Cause.Error()
		}
		return "foreignloop: snapshot failed: context done"
	default:
		return "foreignloop: snapshot failed"
	}
}

func (e *SnapshotError) Unwrap() error { return e.Cause }
