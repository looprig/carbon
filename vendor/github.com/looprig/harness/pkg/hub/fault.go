package hub

import (
	"context"
	"fmt"

	"github.com/looprig/harness/pkg/event"
)

// SessionPersistenceFault is the typed error the hub raises when a REQUIRED durable
// append of an Enduring event fails (the durable tap's append-before-apply step). It
// carries the offending Event (so an operator log names what could not be persisted)
// and the underlying Cause from the journal (a *journal.AppendError,
// *FenceViolationError, …) so a reporter can errors.As the root failure. It is the
// fail-secure signal: the live event whose append failed is NOT delivered, and the
// session that owns the hub stops accepting new work.
type SessionPersistenceFault struct {
	// Event is the Enduring event whose required durable append failed — either the
	// triggering event the loop published or a hub-synthesized session event
	// (SessionActive/SessionIdle/SessionStopped). Never nil on a real fault.
	Event event.Event
	// Cause is the underlying journal failure (typed). It may be nil only in the
	// degenerate construction a test exercises; a real fault always chains the
	// append error.
	Cause error
}

// SessionAbortedError reports a publish rejected after construction teardown
// sealed the unpublished session's durable event tap.
type SessionAbortedError struct{ Cause error }

func (e *SessionAbortedError) Error() string {
	if e.Cause == nil {
		return "hub: session aborted"
	}
	return "hub: session aborted: " + e.Cause.Error()
}

func (e *SessionAbortedError) Unwrap() error { return e.Cause }

func (e *SessionPersistenceFault) Error() string {
	// Name the dynamic event type so a failed SessionStopped append is distinguishable
	// from a SessionIdle one in an operator log; never log the event payload itself.
	evType := fmt.Sprintf("%T", e.Event)
	if e.Cause == nil {
		return "hub: session persistence fault on " + evType
	}
	return "hub: session persistence fault on " + evType + ": " + e.Cause.Error()
}

func (e *SessionPersistenceFault) Unwrap() error { return e.Cause }

// FatalPublication marks a required-journal failure for dependency-inverted
// producers. Callers can fail closed without importing hub's concrete type.
func (*SessionPersistenceFault) FatalPublication() bool { return true }

// FaultReporter is the hub's escalation seam for a required-durable-append failure.
// The hub depends only on this narrow interface (Dependency Inversion): it never sees
// the Session's closing latch or its WaitIdle registry, and the Session (which
// implements it) never sees the hub's append path. The implementation must be
// fail-secure — on a fault the owning session stops accepting new Submit/NewLoop and
// wakes any WaitIdle waiter with the fault — and must not block the hub's publish
// path (the hub calls it inline, outside the hub lock).
//
// ctx is the publish context: the reporter may use it to bound any work it does, but
// must not depend on it staying live (a fault may arrive on a cancelled publish).
type FaultReporter interface {
	ReportFault(ctx context.Context, fault *SessionPersistenceFault)
}

// nopFaultReporter is the default reporter wired into a hub built without an injected
// one (existing constructor callers, headless/no-persistence mode). It drops the
// fault — appropriate only when there is no durable append that can fail (the nop
// appender never errors), so this reporter is never actually invoked in that mode. It
// keeps the hub's FaultReporter field non-nil so the publish path needs no nil check.
type nopFaultReporter struct{}

func (nopFaultReporter) ReportFault(context.Context, *SessionPersistenceFault) {}
