package event

import (
	"time"

	"github.com/looprig/core/uuid"
)

// Clock and IDGen are injected so tests are deterministic (mirrors session's
// injected idGenerator seam): the Factory mints from these rather than calling
// time.Now/uuid.New directly, so a test can pin both. IDGen returns an error so a
// crypto/rand failure propagates (matching session's idGenerator
// func() (uuid.UUID, error)) rather than being swallowed.
type Clock func() time.Time
type IDGen func() (uuid.UUID, error)

// Factory mints fresh event Headers, stamping each with a new EventID and the
// current CreatedAt at creation time. It is the single creation seam every
// Enduring event flows through so the journal sees a stable idempotency key and
// creation timestamp.
type Factory struct {
	newID IDGen
	now   Clock
}

// NewFactory wires the id generator and clock the Factory mints from.
func NewFactory(newID IDGen, now Clock) *Factory { return &Factory{newID: newID, now: now} }

// Stamp returns a COPY of h with a fresh EventID and CreatedAt, preserving the
// caller's existing Coordinates and Cause (the producer set those before
// stamping). A crypto/rand failure from newID is propagated, never swallowed, and
// no partial Header escapes — the returned Header carries a zero EventID on error.
func (f *Factory) Stamp(h Header) (Header, error) {
	id, err := f.newID()
	if err != nil {
		return Header{}, err
	}
	h.EventID = id
	h.CreatedAt = f.now()
	return h, nil
}

// StampWorkflowActivity stamps a fully specified WorkflowActivity with the
// caller's stable source activity ID. Unlike Stamp, it never calls newID: retry
// logic must be able to submit the same journal identity again. An explicit
// CreatedAt is preserved so a deterministic producer can make the complete
// journal payload byte-identical across retries; a zero CreatedAt uses the
// factory clock for ordinary callers. The body is validated after stamping so
// the returned value is safe to send through the normal Hub/journal path.
func (f *Factory) StampWorkflowActivity(ev WorkflowActivity, deterministicID uuid.UUID) (WorkflowActivity, error) {
	if deterministicID.IsZero() {
		return WorkflowActivity{}, &InvalidEventError{Event: "WorkflowActivity", Field: FieldEventID, Rule: RuleRequired}
	}
	if !ev.EventID.IsZero() && ev.EventID != deterministicID {
		return WorkflowActivity{}, &InvalidEventError{Event: "WorkflowActivity", Field: FieldEventID, Rule: RuleInvalid}
	}
	ev.EventID = deterministicID
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = f.now()
	}
	if err := ValidateEvent(ev); err != nil {
		return WorkflowActivity{}, err
	}
	return ev, nil
}

// StampCompactWaiterResolved stamps CreatedAt while preserving and verifying a
// resolved waiter's deterministic EventID. It cannot inject an arbitrary ID.
func (f *Factory) StampCompactWaiterResolved(ev CompactWaiterResolved) (Header, error) {
	return f.stampCompactWaiterReply(ev.Header, ev.AttemptID, true, "CompactWaiterResolved")
}

// StampCompactWaiterRejected stamps CreatedAt while preserving and verifying a
// rejected waiter's deterministic EventID. It cannot inject an arbitrary ID.
func (f *Factory) StampCompactWaiterRejected(ev CompactWaiterRejected) (Header, error) {
	return f.stampCompactWaiterReply(ev.Header, ev.AttemptID, false, "CompactWaiterRejected")
}

func (f *Factory) stampCompactWaiterReply(h Header, attempt CompactAttemptID, resolved bool, name EventName) (Header, error) {
	if attempt.IsZero() {
		return Header{}, &InvalidEventError{Event: name, Field: FieldAttemptID, Rule: RuleInvalid}
	}
	if h.Cause.CommandID.IsZero() {
		return Header{}, &InvalidEventError{Event: name, Field: FieldCommandID, Rule: RuleInvalid}
	}
	if h.EventID.IsZero() {
		return Header{}, &InvalidEventError{Event: name, Field: FieldEventID, Rule: RuleRequired}
	}
	want := CompactWaiterReplyID(attempt, h.Cause.CommandID, resolved)
	if h.EventID != want {
		return Header{}, &InvalidEventError{Event: name, Field: FieldEventID, Rule: RuleInvalid}
	}
	h.CreatedAt = f.now()
	return h, nil
}

// NewHeader mints a fresh EventID + CreatedAt onto an empty Header. Callers fill
// Coordinates/Cause. It is Stamp of the zero Header.
func (f *Factory) NewHeader() (Header, error) { return f.Stamp(Header{}) }
