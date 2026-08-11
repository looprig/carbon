package journal

import (
	"context"
	"strconv"

	"github.com/looprig/core/uuid"
)

// Lease is the single-writer ownership token for one session's durable log. A
// SessionJournal depends on it (DIP): the composition root acquires a Lease from a
// backend and passes it in; the journal stamps the lease's Epoch into its first
// LeaseFence and refuses to append once the lease is lost. The holder (the composition
// root), not the journal, calls Release — the journal only reads Epoch and the
// validity/loss signals (see ownershipToken, the narrower view it depends on).
type Lease interface {
	ownershipToken
	// SessionID is the session this lease grants single-writer ownership of.
	SessionID() uuid.UUID
	// Release relinquishes the lease: stops any heartbeat, marks it no longer held
	// (firing Lost), and best-effort clears the entry so a successor can re-acquire
	// without waiting out the TTL. Idempotent.
	Release(ctx context.Context) error
}

// ownershipToken is the narrow view the journal depends on (interface segregation):
// the fencing epoch plus the validity/loss signals it needs to gate appends. It does
// NOT expose Release — the journal never controls the lease lifecycle.
type ownershipToken interface {
	// Epoch is the monotonically increasing fencing epoch this lease holds. A higher
	// epoch always out-ranks a lower one; the journal stamps it into its LeaseFence.
	Epoch() uint64
	// Valid reports whether the lease is still held (not released, not lost to a
	// higher-epoch takeover). A journal must refuse to append once this is false.
	Valid() bool
	// Lost returns a channel closed when the lease is lost — released by the holder,
	// or overtaken by a higher epoch detected on a heartbeat renewal. It never carries
	// a value; select on it to react to loss.
	Lost() <-chan struct{}
}

// LeaseHeldError reports that acquiring a lease lost the single-holder race: the
// session's lease is currently held by a live (unexpired) holder, or a concurrent
// acquirer won the race. It carries the session and the epoch currently fenced so a
// caller can log who holds it. It is the expected, non-fatal "someone else owns this
// session" outcome — the loser must not write to the log.
type LeaseHeldError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *LeaseHeldError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" lease held at epoch " + strconv.FormatUint(e.Epoch, 10)
}

// LeaseLostError reports an operation attempted on a lease that is no longer held: it
// was released, or a higher-epoch holder took over (detected on a heartbeat renewal).
// It carries the session and the lease's epoch. The journal returns it (wrapped in a
// JournalLeaseLostError) when an Append is attempted after the lease is lost.
type LeaseLostError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *LeaseLostError) Error() string {
	return "journal: session " + e.SessionID.String() +
		" lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " lost"
}
