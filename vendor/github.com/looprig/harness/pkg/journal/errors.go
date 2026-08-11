package journal

import (
	"strconv"

	"github.com/looprig/core/uuid"
)

// This file holds the journal's backend-neutral error vocabulary: the typed failures a
// SessionJournal implementation returns and callers errors.As against. They carry only
// backend-agnostic context (the record's identity, the fenced sequence, the session),
// never any backend transport detail.

// MarshalRecordError wraps a failure to encode a record's payload before it is
// persisted. It names the record's routing/destination identifier so a caller can
// correlate the failure without re-inspecting the payload, and unwraps to the
// underlying codec error (an *event.EphemeralNotPersistableError,
// *command.UnknownCommandTypeError, a *FenceEncodeError, …) for errors.As inspection.
type MarshalRecordError struct {
	Subject string
	Cause   error
}

func (e *MarshalRecordError) Error() string {
	return "journal: marshal record for " + strconv.Quote(e.Subject) + ": " + e.Cause.Error()
}
func (e *MarshalRecordError) Unwrap() error { return e.Cause }

// RecordKindError reports a JournalRecord whose concrete type is outside the sealed sum
// the serializer encodes. It is unreachable for an in-package record (the sum is sealed
// by the unexported marker); it exists so a serializer's default arm fails closed with
// a typed error rather than panicking.
type RecordKindError struct {
	Subject string
}

func (e *RecordKindError) Error() string {
	return "journal: unknown record kind for " + strconv.Quote(e.Subject)
}

// AppendError wraps a definite failure to persist a record to the session log. It
// carries the record's routing/destination identifier, its idempotency id, and the
// expected sequence the append was fenced under, and unwraps to the underlying backend
// error (a context deadline, a fence rejection, a transport error). The fence stays
// unadvanced when this is returned, so the next Append re-fences on the same tip.
type AppendError struct {
	Subject  string
	MsgID    string
	Expected uint64
	Cause    error
}

func (e *AppendError) Error() string {
	return "journal: append to " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", expected-seq " + strconv.FormatUint(e.Expected, 10) + "): " + e.Cause.Error()
}
func (e *AppendError) Unwrap() error { return e.Cause }

// AmbiguousAckError reports an append whose outcome the backend could not resolve: the
// persist call was lost or timed out and a bounded retry stayed ambiguous, so the
// serializer cannot tell whether the record landed. The fence stays unadvanced, so the
// next Append re-fences on the same tip; the caller decides whether to fail the session
// or retry later. It carries the record's routing/destination identifier, its
// idempotency id, the expected sequence the append fenced on, and the underlying cause.
type AmbiguousAckError struct {
	Subject  string
	MsgID    string
	Expected uint64
	Cause    error
}

func (e *AmbiguousAckError) Error() string {
	return "journal: ambiguous ack (unresolved) for " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", expected-seq " + strconv.FormatUint(e.Expected, 10) + "): " + e.Cause.Error()
}
func (e *AmbiguousAckError) Unwrap() error { return e.Cause }

// RecordTooLargeError reports a record whose marshaled payload exceeded the inline
// threshold but could NOT be offloaded to the backend's content-addressed blob store
// (the store was unavailable or the upload failed). The journal fails closed with this
// typed error rather than silently persisting an over-threshold record. It carries the
// record's routing/destination identifier, its idempotency id, the payload length, and
// the underlying offload cause.
type RecordTooLargeError struct {
	Subject string
	MsgID   string
	Length  int
	Cause   error
}

func (e *RecordTooLargeError) Error() string {
	return "journal: record too large to inline and offload failed for " + strconv.Quote(e.Subject) +
		" (msg-id " + strconv.Quote(e.MsgID) +
		", len " + strconv.Itoa(e.Length) + "): " + e.Cause.Error()
}
func (e *RecordTooLargeError) Unwrap() error { return e.Cause }

// JournalNotReadyError reports an Append attempted before the journal's opening
// LeaseFence was acknowledged. The journal writes the LeaseFence as its first append and
// only marks itself ready once it lands; an Append before that fails closed with this
// typed error rather than racing the fence. It carries the session so a caller can
// correlate the failure.
type JournalNotReadyError struct {
	SessionID uuid.UUID
}

func (e *JournalNotReadyError) Error() string {
	return "journal: session " + e.SessionID.String() + " not ready (opening LeaseFence not yet acknowledged)"
}

// JournalLeaseLostError reports an Append refused because the journal's ownership lease
// was lost — released by the holder or overtaken by a higher epoch. Once the lease is
// gone the journal fails every append fast and never re-fetches or advances its
// expected sequence: a new owner (higher epoch) has written, or will write, its own
// LeaseFence, so this stale journal's fence would reject the append anyway. Failing here
// is the fast-path guard; the backend fence is the hard backstop. It carries the session
// and the lost lease's epoch and unwraps to a *LeaseLostError for errors.As.
type JournalLeaseLostError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *JournalLeaseLostError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" append refused: lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " lost"
}

func (e *JournalLeaseLostError) Unwrap() error {
	return &LeaseLostError{SessionID: e.SessionID, Epoch: e.Epoch}
}
