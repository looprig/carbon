package memstore

import (
	"context"
	"sync"

	"github.com/looprig/storage"
)

// leaserStore is the in-memory Leaser backing type: it grants exclusive,
// epoch-fenced ownership of names. Each name has an independent, strictly
// increasing epoch counter that advances on every grant, and at most one live
// (un-released) holder at a time — a second Acquire is refused with a
// *LeaseHeldError while a holder is live.
//
// As an in-process oracle it has no TTL, no takeover, and no cross-process
// reclaim: a lease's Lost() channel closes only on Release, and each method's
// ctx parameter exists solely to satisfy the storage.Leaser contract.
type leaserStore struct {
	mu      sync.Mutex
	holders map[string]*memLease // live holder per name, absent when free
	epochs  map[string]uint64    // highest epoch granted per name, monotonic
}

// newLeaserStore returns an empty leaserStore ready for use.
func newLeaserStore() *leaserStore {
	return &leaserStore{
		holders: make(map[string]*memLease),
		epochs:  make(map[string]uint64),
	}
}

// Compile-time proof that *leaserStore honors the Leaser contract.
var _ storage.Leaser = (*leaserStore)(nil)

// Acquire grants exclusive ownership of name. It validates the name first
// (*InvalidNameError), then refuses with a *LeaseHeldError naming the current
// holder's epoch if a live holder exists. Otherwise it advances the name's
// epoch counter (strictly increasing across grants of the same name; independent
// per name) and returns a fresh lease. Epochs start at 1.
func (s *leaserStore) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if h := s.holders[name]; h != nil {
		return nil, &storage.LeaseHeldError{Name: name, HolderEpoch: h.epoch}
	}

	epoch := s.epochs[name] + 1
	s.epochs[name] = epoch
	lease := &memLease{
		store: s,
		name:  name,
		epoch: epoch,
		lost:  make(chan struct{}),
	}
	s.holders[name] = lease
	return lease, nil
}

// memLease is a single grant of a leaserStore name. Its epoch is fixed at
// construction; its lost channel closes exactly once, when the grant ends via
// Release. All mutable state (the released flag, the lost channel's closure, and
// the store's holder slot) is guarded by the parent store's mutex, so there is a
// single lock and no lock-ordering hazard.
type memLease struct {
	store *leaserStore
	name  string
	epoch uint64

	lost     chan struct{} // closed on Release; reference fixed at construction
	released bool          // guarded by store.mu
}

// Epoch returns the fixed, strictly-increasing epoch stamped on this grant.
func (l *memLease) Epoch() uint64 {
	return l.epoch
}

// Lost returns a channel closed when ownership ends. In memstore that is Release
// only — there is no TTL or takeover — so the channel closes exactly once.
func (l *memLease) Lost() <-chan struct{} {
	return l.lost
}

// Release ends the grant: it closes Lost() and frees the name for re-acquisition
// (the next Acquire advances the epoch). It is idempotent — a second Release is a
// no-op returning nil. The ctx exists only to satisfy the Lease contract.
func (l *memLease) Release(ctx context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()

	if l.released {
		return nil // double-Release is a no-op
	}
	l.released = true
	close(l.lost)

	// Free the name only if we still occupy the holder slot. memstore has no
	// takeover, so this is always us — the guard is defensive.
	if l.store.holders[l.name] == l {
		delete(l.store.holders, l.name)
	}
	return nil
}
