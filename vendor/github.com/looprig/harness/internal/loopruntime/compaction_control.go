package loopruntime

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

const compactionControlWaiterCapacity = 64

const compactionPriorityCommandCapacity = 8

// CompactionCoordinationErrorKind identifies infrastructure failures that prevent
// the control actor from creating or durably resolving a compaction obligation.
type CompactionCoordinationErrorKind string

const (
	CompactionCoordinationAttemptID CompactionCoordinationErrorKind = "attempt_id"
	CompactionCoordinationOutcome   CompactionCoordinationErrorKind = "outcome"
	CompactionCoordinationBasis     CompactionCoordinationErrorKind = "basis"
)

// CompactionCoordinationError is the typed internal hand-off used by later
// controller/finalizer tasks. Infrastructure failures are never converted into a
// false CompactionRejected or CompactWaiterRejected event.
type CompactionCoordinationError struct {
	Kind  CompactionCoordinationErrorKind
	Cause error
}

func (e *CompactionCoordinationError) Error() string {
	var message string
	switch e.Kind {
	case CompactionCoordinationAttemptID:
		message = "loopruntime: compaction attempt id generation failed"
	case CompactionCoordinationOutcome:
		message = "loopruntime: compaction outcome publication failed"
	case CompactionCoordinationBasis:
		message = "loopruntime: compaction attempted basis mismatch"
	default:
		message = "loopruntime: compaction coordination failed"
	}
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *CompactionCoordinationError) Unwrap() error { return e.Cause }

type compactionAdmissionKind uint8

const (
	compactionAdmissionUnspecified compactionAdmissionKind = iota
	compactionAdmissionOpened
	compactionAdmissionJoined
	compactionAdmissionDuplicate
	compactionAdmissionLaneFull
)

type compactionAdmission struct {
	Kind      compactionAdmissionKind
	AttemptID event.CompactAttemptID
}

type compactionBoundaryKind uint8

const (
	compactionBoundaryUnspecified compactionBoundaryKind = iota
	compactionBoundaryStep
	compactionBoundaryTurn
)

type compactionDispositionKind uint8

const (
	compactionDispositionNone compactionDispositionKind = iota
	compactionDispositionStart
	compactionDispositionReject
)

type compactionDisposition struct {
	Kind         compactionDispositionKind
	Attempt      *compactionAttempt
	RejectReason event.CompactRejectReason
	hookScope    *compactionHookScope
	input        *loop.CompactionInput
	preRejected  *contextCompactionAwaitResult
}

// compactionDispositionSink accepts ownership of a start/reject decision made at
// a safe actor boundary. Implementations must return promptly; the concrete
// hustle adapter and actor-owned finalizer are wired by later tasks.
type compactionDispositionSink interface {
	CoordinateCompaction(context.Context, compactionDisposition) error
}

type compactionCandidateSink interface {
	CoordinateCompactionCandidate(context.Context, compactionDisposition, compactionExecutionCandidate) error
}

// compactionFailure is the narrow typed result seam that later public waiters can
// bind to. It never claims a durable rejection was written.
type compactionFailure struct {
	WaiterCommandIDs []uuid.UUID
	Err              error
}

type compactionFailureSink interface {
	ReportCompactionFailure(context.Context, compactionFailure)
}

type fatalPublicationError interface {
	FatalPublication() bool
}

func isFatalPublication(err error) bool {
	var fatal fatalPublicationError
	return errors.As(err, &fatal) && fatal.FatalPublication()
}

type actorCommandHandler func(command.Command) bool

// arbitrateCompactionBoundary applies the bounded snapshot of priority controls
// already queued at a safe boundary before compaction is consumed. The production
// lane is capped at compactionPriorityCommandCapacity, so this preserves command
// order without an unbounded drain or touching the ordinary FIFO lane.
func arbitrateCompactionBoundary(priorityCommands <-chan command.Command, handle actorCommandHandler, dispatch func()) bool {
	snapshotSize := len(priorityCommands)
	if snapshotSize > compactionPriorityCommandCapacity {
		snapshotSize = compactionPriorityCommandCapacity
	}
	handled := 0
	if snapshotSize == 0 {
		select {
		case cmd, ok := <-priorityCommands:
			if !ok || handle(cmd) {
				return true
			}
			handled = 1
		default:
			dispatch()
			return false
		}
		snapshotSize = handled + len(priorityCommands)
		if snapshotSize > compactionPriorityCommandCapacity {
			snapshotSize = compactionPriorityCommandCapacity
		}
	}
	for i := handled; i < snapshotSize; i++ {
		cmd, ok := <-priorityCommands
		if !ok || handle(cmd) {
			return true
		}
	}
	dispatch()
	return false
}

type compactionPhase uint8

const (
	compactionPhasePending compactionPhase = iota
	compactionPhaseInProgress
)

type compactionWaiter struct {
	commandID uuid.UUID
	createdAt time.Time
}

// compactionAttempt is the actor-owned shared obligation. WaiterCommandIDs is
// always a defensive, canonical projection: CreatedAt ascending, then UUID bytes.
type compactionAttempt struct {
	AttemptID        event.CompactAttemptID
	WaiterCommandIDs []uuid.UUID
	Reason           event.CompactionReason
	Basis            event.ContextBasis
	StartedAt        time.Time
	Cause            identity.Cause
}

type pendingCompaction struct {
	attemptID event.CompactAttemptID
	waiters   []compactionWaiter
	reason    event.CompactionReason
	basis     event.ContextBasis
	startedAt time.Time
	phase     compactionPhase
	cause     identity.Cause
}

// compactionControl owns one coalescing slot and a bounded waiter slice. It is
// mutated only by the loop actor, so it needs no locks and cannot grow without
// bound. User-input queue state is intentionally absent from this type.
type compactionControl struct {
	waiterCapacity int
	pending        *pendingCompaction
	interrupting   bool
	shuttingDown   bool
}

func newCompactionControl(waiterCapacity int) *compactionControl {
	if waiterCapacity <= 0 {
		waiterCapacity = compactionControlWaiterCapacity
	}
	return &compactionControl{waiterCapacity: waiterCapacity}
}

// blocksInput reports whether this loop has an unresolved compaction obligation.
// Pending and in-progress attempts both freeze the existing actor-owned input
// inbox: no queued message may start or fold until a durable compaction terminal
// clears the slot. Compaction is orthogonal to loopStatus, which continues to
// describe whether a turn goroutine exists.
func (c *compactionControl) blocksInput() bool {
	return c != nil && c.pending != nil
}

func (c *compactionControl) admit(request command.Compact, idGen idGenerator) (compactionAdmission, error) {
	if err := command.ValidateCommand(request); err != nil {
		return compactionAdmission{}, err
	}
	if c.pending != nil {
		return c.join(request), nil
	}
	id, err := idGen()
	if err != nil || id.IsZero() {
		return compactionAdmission{}, &CompactionCoordinationError{Kind: CompactionCoordinationAttemptID, Cause: err}
	}
	attemptID := event.CompactAttemptID(id)
	c.pending = &pendingCompaction{
		attemptID: attemptID,
		waiters:   []compactionWaiter{waiterFromCompact(request)},
		reason:    compactionReason(request.Agency),
		phase:     compactionPhasePending,
		cause: identity.Cause{
			CommandID:   request.CommandID,
			Coordinates: request.Cause.Coordinates,
			Agency:      request.Agency,
		},
	}
	return compactionAdmission{Kind: compactionAdmissionOpened, AttemptID: attemptID}, nil
}

func (c *compactionControl) join(request command.Compact) compactionAdmission {
	commandID := request.CommandHeader().CommandID
	for _, waiter := range c.pending.waiters {
		if waiter.commandID == commandID {
			return compactionAdmission{Kind: compactionAdmissionDuplicate, AttemptID: c.pending.attemptID}
		}
	}
	if len(c.pending.waiters) >= c.waiterCapacity {
		return compactionAdmission{Kind: compactionAdmissionLaneFull, AttemptID: c.pending.attemptID}
	}
	c.pending.waiters = append(c.pending.waiters, waiterFromCompact(request))
	sort.Slice(c.pending.waiters, func(i, j int) bool {
		left, right := c.pending.waiters[i], c.pending.waiters[j]
		if !left.createdAt.Equal(right.createdAt) {
			return left.createdAt.Before(right.createdAt)
		}
		return bytes.Compare(left.commandID[:], right.commandID[:]) < 0
	})
	return compactionAdmission{Kind: compactionAdmissionJoined, AttemptID: c.pending.attemptID}
}

func (c *compactionControl) freezeBasis(attemptID event.CompactAttemptID, basis event.ContextBasis) error {
	if c.pending == nil || c.pending.attemptID != attemptID || basis.Revision == 0 || basis.ThroughEventID.IsZero() {
		return &CompactionCoordinationError{Kind: CompactionCoordinationBasis}
	}
	if c.pending.basis != (event.ContextBasis{}) && c.pending.basis != basis {
		return &CompactionCoordinationError{Kind: CompactionCoordinationBasis}
	}
	c.pending.basis = basis
	return nil
}

func (c *compactionControl) markStarted(attemptID event.CompactAttemptID, startedAt time.Time) *compactionAttempt {
	if c.pending == nil || c.pending.attemptID != attemptID || c.pending.phase != compactionPhaseInProgress || startedAt.IsZero() {
		return nil
	}
	c.pending.startedAt = startedAt
	return c.pendingAttempt()
}

func (c *compactionControl) interrupt() {
	if c.pending != nil {
		c.interrupting = true
	}
}

func (c *compactionControl) shutdown() { c.shuttingDown = true }

func (c *compactionControl) cancellationRejectReason() event.CompactRejectReason {
	if c.shuttingDown {
		return event.CompactRejectShuttingDown
	}
	if c.interrupting {
		return event.CompactRejectInterrupted
	}
	return event.CompactRejectUnspecified
}

func (c *compactionControl) pendingAtBoundary() bool {
	return c.pending != nil && c.pending.phase == compactionPhasePending
}

// atBoundary is the only transition that consumes a pending request. Shutdown
// and interrupt dispositions are checked first, so a control signal observed by
// the actor always outranks starting compaction at the next safe boundary.
func (c *compactionControl) atBoundary(boundary compactionBoundaryKind) compactionDisposition {
	if c.pending == nil || (boundary != compactionBoundaryStep && boundary != compactionBoundaryTurn) {
		return compactionDisposition{}
	}
	if c.pending.phase == compactionPhaseInProgress {
		return compactionDisposition{}
	}
	if c.shuttingDown {
		return c.reject(event.CompactRejectShuttingDown)
	}
	if c.interrupting {
		return c.reject(event.CompactRejectInterrupted)
	}
	c.pending.phase = compactionPhaseInProgress
	return compactionDisposition{Kind: compactionDispositionStart, Attempt: c.pendingAttempt()}
}

func (c *compactionControl) reject(reason event.CompactRejectReason) compactionDisposition {
	attempt := c.pendingAttempt()
	c.pending = nil
	c.interrupting = false
	return compactionDisposition{Kind: compactionDispositionReject, Attempt: attempt, RejectReason: reason}
}

// abort releases an attempt whose execution sink declined ownership. The actor
// retains no in-progress tombstone: every waiter is returned through the typed
// infrastructure-failure seam, and a later command may open a fresh attempt.
func (c *compactionControl) abort(attemptID event.CompactAttemptID) *compactionAttempt {
	if c.pending == nil || c.pending.attemptID != attemptID {
		return nil
	}
	attempt := c.pendingAttempt()
	c.pending = nil
	c.interrupting = false
	return attempt
}

func (c *compactionControl) complete(attemptID event.CompactAttemptID) *compactionAttempt {
	return c.abort(attemptID)
}

func (c *compactionControl) pendingAttempt() *compactionAttempt {
	if c.pending == nil {
		return nil
	}
	waiters := make([]uuid.UUID, len(c.pending.waiters))
	for i, waiter := range c.pending.waiters {
		waiters[i] = waiter.commandID
	}
	return &compactionAttempt{
		AttemptID: c.pending.attemptID, WaiterCommandIDs: waiters, Reason: c.pending.reason,
		Basis: c.pending.basis, StartedAt: c.pending.startedAt, Cause: c.pending.cause,
	}
}

func waiterFromCompact(request command.Compact) compactionWaiter {
	header := request.CommandHeader()
	return compactionWaiter{commandID: header.CommandID, createdAt: header.CreatedAt}
}

func compactionReason(agency identity.Agency) event.CompactionReason {
	if agency == identity.AgencyUser {
		return event.CompactionReasonManual
	}
	return event.CompactionReasonAutomatic
}
