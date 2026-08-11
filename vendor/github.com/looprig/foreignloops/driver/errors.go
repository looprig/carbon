package driver

import (
	"errors"
	"fmt"
)

// ErrSteerAdmissionCapacity identifies a bounded, pre-admission rejection.
// It means no ACP steering write was attempted and the caller may safely
// choose its normal queued fallback.
var ErrSteerAdmissionCapacity = errors.New("foreignloop: steering admission capacity exhausted")

// SteerAdmissionError reports that the fixed steering-observation reservation
// lane was full before the request entered the actor mailbox. Its text is
// intentionally bounded and does not include provider or request details.
type SteerAdmissionError struct{}

func (*SteerAdmissionError) Error() string { return ErrSteerAdmissionCapacity.Error() }
func (*SteerAdmissionError) Unwrap() error { return ErrSteerAdmissionCapacity }

// SpawnError reports that a foreign agent could not be started.
type SpawnError struct{ Cause error }

func (e *SpawnError) Error() string { return "foreignloop: spawn: " + e.Cause.Error() }
func (e *SpawnError) Unwrap() error { return e.Cause }

// ExitError reports a non-successful foreign-agent process exit.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("foreignloop: agent exited %d", e.Code) }

// DecodeError reports that a foreign-agent stream could not be decoded.
type DecodeError struct{ Cause error }

func (e *DecodeError) Error() string { return "foreignloop: decode: " + e.Cause.Error() }
func (e *DecodeError) Unwrap() error { return e.Cause }

// HistoryError reports that authoritative history could not be read or decoded.
type HistoryError struct{ Cause error }

func (e *HistoryError) Error() string {
	return "foreignloop: authoritative history: " + e.Cause.Error()
}

func (e *HistoryError) Unwrap() error { return e.Cause }
