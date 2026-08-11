// Package memstore is the in-memory reference backend for storage's four
// primitives. It is the conformance oracle other backends are checked against:
// correct, allocation-simple, and dependency-free. Because the four primitives
// have colliding method names (Ledger/KV/Blobs all declare Delete; KV and Blobs
// declare Get/Put with different signatures), no single Go type can satisfy all
// four — so memstore is built as four separate unexported backing types that are
// wired into a *storage.Composite at the composition root.
package memstore

import (
	"context"
	"io"
	"sync"

	"github.com/looprig/storage"
)

// New assembles the in-memory reference backend: a *storage.Composite wiring
// one fresh backing store per primitive (ledger, leaser, KV, blobs). All four
// providers are non-nil, so the underlying NewComposite can never report an
// incomplete composite — a non-nil error here is impossible and is treated as an
// unrecoverable programmer error (panic) rather than propagated.
func New() *storage.Composite {
	c, err := storage.NewComposite(newLedgerStore(), newLeaserStore(), newKVStore(), newBlobStore())
	if err != nil {
		panic(err) // unreachable: every primitive above is non-nil
	}
	return c
}

// ledgerStore is the in-memory Ledger backing type: many named ledgers, each an
// ordered slice of immutable records, guarded by a single RWMutex. Records are
// 1-based and contiguous by construction (Append only ever extends the tip).
//
// As an in-process oracle it performs no blocking I/O and does NOT honor ctx
// cancellation or deadlines; each method's ctx parameter exists solely to
// satisfy the storage.Ledger contract.
type ledgerStore struct {
	mu      sync.RWMutex
	ledgers map[string][][]byte
}

// newLedgerStore returns an empty ledgerStore ready for use.
func newLedgerStore() *ledgerStore {
	return &ledgerStore{ledgers: make(map[string][][]byte)}
}

// Compile-time proof that *ledgerStore honors the Ledger contract.
var _ storage.Ledger = (*ledgerStore)(nil)

// Append commits payload as the record immediately after sequence expected
// (CAS on the tip). expected must equal the current record count, so expected==0
// requires the ledger empty or absent. On mismatch it returns a *ConflictError
// and leaves state untouched; a memory backend never returns *AmbiguousError.
// The stored record is a fresh copy of payload (copy-in), so later caller
// mutation cannot reach committed data. Zero-length payloads are legal.
func (s *ledgerStore) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if err := storage.ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	records := s.ledgers[name]
	if uint64(len(records)) != expected {
		return &storage.ConflictError{Name: name, Expected: expected}
	}

	stored := make([]byte, len(payload))
	copy(stored, payload)
	s.ledgers[name] = append(records, stored)
	return nil
}

// Read returns a bounded cursor over records with sequence >= from. The cursor
// observes the tip as of this call and never tails later appends; from > tip
// (including tip+1) and an absent ledger both yield an immediately-drained
// cursor. from < 1 is clamped to the first record. Each Next hands back a fresh
// payload copy (copy-out).
func (s *ledgerStore) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := s.ledgers[name]
	tip := uint64(len(records))

	var start uint64 // 0-based index of the first record to yield
	if from > 1 {
		start = from - 1
	}
	if start >= tip {
		return &ledgerCursor{}, nil // drained
	}

	// Boundedness comes from freezing the view's length here at Read time: under
	// the append-only invariant existing records are never rewritten, so a
	// fixed-length window plus Next's copy-out already never observes a later
	// append. The make+copy of the slice headers is defensive only — it fully
	// decouples the cursor from the store's backing array (payload bytes are
	// shared but never mutated in place by the store).
	snapshot := make([][]byte, tip-start)
	copy(snapshot, records[start:tip])
	return &ledgerCursor{records: snapshot, base: start + 1}, nil
}

// Tip returns the current record count for name (0 if absent).
func (s *ledgerStore) Tip(ctx context.Context, name string) (uint64, error) {
	if err := storage.ValidateName(name); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return uint64(len(s.ledgers[name])), nil
}

// Delete removes the ledger; it is idempotent, so deleting an absent ledger
// succeeds. After Delete the ledger is absent == empty (Tip 0, Read drained).
func (s *ledgerStore) Delete(ctx context.Context, name string) error {
	if err := storage.ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ledgers, name)
	return nil
}

// ledgerCursor is a bounded, single-pass reader over a snapshot of ledger
// records taken at Read time. It holds no lock and observes no later append.
type ledgerCursor struct {
	records [][]byte // snapshot; index 0 has sequence base
	base    uint64   // 1-based sequence of records[0]
	pos     int      // index of the next record to yield
}

// Next yields the next record with a freshly copied payload (copy-out), or
// io.EOF once the snapshot is drained.
func (c *ledgerCursor) Next(ctx context.Context) (storage.Record, error) {
	if c.pos >= len(c.records) {
		return storage.Record{}, io.EOF
	}
	src := c.records[c.pos]
	payload := make([]byte, len(src))
	copy(payload, src)
	// #nosec G115 -- c.pos only increments from 0 and is bounded by len(c.records) (checked above), never negative
	rec := storage.Record{Seq: c.base + uint64(c.pos), Payload: payload}
	c.pos++
	return rec, nil
}

// Close releases the cursor. The in-memory cursor holds nothing, so it is a nil
// success; the method exists to honor the Cursor contract.
func (c *ledgerCursor) Close() error {
	return nil
}
