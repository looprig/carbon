package sessionstore

import (
	"context"
	"errors"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
)

// sessionLease adapts a storage.Lease to journal.Lease. storage's lease is
// name-scoped and exposes only Epoch/Lost/Release; the journal contract additionally
// requires SessionID and a boolean Valid. This adapter carries the session id the
// lease was acquired for and derives Valid from the wrapped Lost channel, delegating
// everything else. It never controls the lease lifecycle beyond passing Release
// through — the holder owns that.
type sessionLease struct {
	inner     storage.Lease
	sessionID uuid.UUID
}

// Compile-time proof that *sessionLease honors the journal.Lease contract.
var _ journal.Lease = (*sessionLease)(nil)

// Epoch returns the wrapped lease's fencing epoch.
func (l *sessionLease) Epoch() uint64 { return l.inner.Epoch() }

// Lost returns the wrapped lease's loss channel, closed when ownership ends.
func (l *sessionLease) Lost() <-chan struct{} { return l.inner.Lost() }

// SessionID returns the session this lease was acquired for. storage's lease has no
// session identity of its own, so the adapter supplies it.
func (l *sessionLease) SessionID() uuid.UUID { return l.sessionID }

// Release relinquishes the wrapped lease. ctx bounds the round-trip; the call is
// idempotent to the extent the backend's lease is.
func (l *sessionLease) Release(ctx context.Context) error { return l.inner.Release(ctx) }

// Valid reports whether the lease is still held. It is a non-blocking read of the
// wrapped Lost channel: still valid unless the channel has closed. A journal must
// refuse to append once this is false.
func (l *sessionLease) Valid() bool {
	select {
	case <-l.inner.Lost():
		return false
	default:
		return true
	}
}

// AcquireLease acquires single-writer ownership of a session's stream and returns it
// as a journal.Lease. It derives (and validates) the session's ledger name, acquires
// the storage lease over that name, and wraps the result so the journal sees a
// journal.Lease. A storage *LeaseHeldError — the expected "someone else owns this
// session" outcome — is translated to the journal's own *LeaseHeldError, keyed by
// session id and the live holder's epoch, so callers classify it at the journal level
// without depending on storage's error vocabulary. Any other backend error is
// surfaced unchanged (fail closed).
func (s *Store) AcquireLease(ctx context.Context, id uuid.UUID) (journal.Lease, error) {
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	inner, err := s.backend.Leaser.Acquire(ctx, name)
	if err != nil {
		var held *storage.LeaseHeldError
		if errors.As(err, &held) {
			return nil, &journal.LeaseHeldError{SessionID: id, Epoch: held.HolderEpoch}
		}
		return nil, err
	}
	return &sessionLease{inner: inner, sessionID: id}, nil
}
