package memstore

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/looprig/storage"
)

// kvEntry is a single stored KV value with its current per-key revision. The
// value slice is a private copy the store owns; it is never handed out or
// accepted by reference (Put copies in, Get copies out).
type kvEntry struct {
	val []byte
	rev uint64
}

// kvStore is the in-memory KV backing type: revision-CAS'd metadata guarded by a
// single RWMutex. Revisions are per-key, strictly increasing, and start at 1 on
// create; they do not persist across Delete (a re-created key restarts at 1).
//
// As an in-process oracle it performs no blocking I/O and does NOT honor ctx
// cancellation; each method's ctx parameter exists solely to satisfy the
// storage.KV contract.
type kvStore struct {
	mu      sync.RWMutex
	entries map[string]kvEntry
}

// newKVStore returns an empty kvStore ready for use.
func newKVStore() *kvStore {
	return &kvStore{entries: make(map[string]kvEntry)}
}

// Compile-time proof that *kvStore honors the KV contract.
var _ storage.KV = (*kvStore)(nil)

// Get returns a copy of the value at key plus its current revision. It validates
// the key first (*InvalidNameError); an absent key yields a *KeyNotFoundError.
// The returned slice is a fresh copy (copy-out), so caller mutation cannot reach
// stored data.
func (s *kvStore) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	if err := storage.ValidateName(key); err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[key]
	if !ok {
		return nil, 0, &storage.KeyNotFoundError{Key: key}
	}
	out := make([]byte, len(e.val))
	copy(out, e.val)
	return out, e.rev, nil
}

// Put performs a revision compare-and-swap. It validates the key first
// (*InvalidNameError). expectedRev must equal the key's current revision — 0 for
// an absent key, so expectedRev 0 is create-only. On mismatch it returns a
// *ConflictError{Name: key, Expected: expectedRev} and leaves state untouched.
// On success it stores a copy of val (copy-in), bumps the revision by one, and
// returns the new revision.
func (s *kvStore) Put(ctx context.Context, key string, expectedRev uint64, val []byte) (uint64, error) {
	if err := storage.ValidateName(key); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.entries[key].rev // 0 when the key is absent
	if expectedRev != cur {
		return 0, &storage.ConflictError{Name: key, Expected: expectedRev}
	}

	stored := make([]byte, len(val))
	copy(stored, val)
	newRev := cur + 1
	s.entries[key] = kvEntry{val: stored, rev: newRev}
	return newRev, nil
}

// Keys returns the keys whose string has prefix, lexicographically ascending and
// duplicate-free (map keys are unique by construction). An empty prefix returns
// all keys. Per the contract, Keys does NOT validate prefix — a partial-segment
// prefix like "sessions/" is a substring filter, not a valid name.
func (s *kvStore) Keys(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []string
	for k := range s.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes key; it validates the key first (*InvalidNameError) and is
// idempotent, so deleting an absent key succeeds. After Delete the key is absent
// (Get reports *KeyNotFoundError) and its revision counter is forgotten.
func (s *kvStore) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}
