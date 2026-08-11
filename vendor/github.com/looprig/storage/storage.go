// Package storage defines four neutral storage primitives — Ledger (an
// append-only, CAS-sequenced record log), Leaser (a single-writer epoch lease),
// KV (revision-CAS metadata), and Blobs (content-addressed immutable bytes) —
// plus a typed error taxonomy, ValidateName, and the AppendDefinite ambiguity
// resolver.
//
// Names and keys are canonical by construction (see ValidateName), so no two
// valid names alias one backend location. Every backend must accept ledger
// payloads and KV values up to 1 MiB; larger payloads are the engine's
// responsibility to offload to Blobs.
package storage

import (
	"context"
	"io"
	"strings"
)

// Ledger addresses many ledgers by name. Append commits payload as the record
// immediately after sequence `expected` (CAS on the tip; expected == 0 means the
// ledger must be empty). The committed record's seq is expected+1 by definition,
// so Append returns no sequence. Sequences are 1-based, contiguous, immutable.
//
// Append outcomes are a tri-state:
//   - nil            — committed, definitely.
//   - *ConflictError — something already occupies expected+1. Definite: the record did not land.
//   - *AmbiguousError — the outcome is unknown (lost ack / lost COMMIT response). Only
//     networked backends may return this; fs and memory never do.
//
// Any other error is a definite failure (fail closed, tip unadvanced).
//
// Edge semantics:
//   - Absent == empty. A never-written (or deleted) ledger behaves as empty: Tip returns 0;
//     Read yields an immediately-drained cursor; Append with expected 0 creates it implicitly;
//     Delete of an absent ledger is a no-op success (idempotent).
//   - Reads beyond the tip are empty, not errors. Any from > tip (including tip+1) yields a
//     drained cursor. Cursors are bounded: they observe the tip as of Read and never tail
//     later appends.
//   - Payloads are caller-owned. A backend must not reuse or mutate Record.Payload after Next
//     returns. Zero-length payloads are legal.
//   - Listings are canonical: KV.Keys and Blobs.List return lexicographically ascending,
//     duplicate-free results.
type Ledger interface {
	Append(ctx context.Context, name string, expected uint64, payload []byte) error
	Read(ctx context.Context, name string, from uint64) (Cursor, error)
	Tip(ctx context.Context, name string) (uint64, error)
	Delete(ctx context.Context, name string) error
}

type Record struct {
	Seq     uint64
	Payload []byte
}

type Cursor interface {
	Next(ctx context.Context) (Record, error) // io.EOF when drained
	Close() error
}

// Leaser grants exclusive, epoch-fenced ownership of a name. Acquire fails with
// *LeaseHeldError while a live holder exists; a dead holder's lease is reclaimed
// by the backend's native mechanism (flock released by the OS, KV TTL expiry,
// PG advisory-lock session end). Epochs are strictly increasing across grants
// of the same name.
type Leaser interface {
	Acquire(ctx context.Context, name string) (Lease, error)
}

type Lease interface {
	Epoch() uint64
	Lost() <-chan struct{}             // closed when ownership is lost (expiry, takeover)
	Release(ctx context.Context) error // releasing may cross the network; ctx bounds it
}

// KV holds small CAS'd metadata (the session catalog). Revisions are per-key,
// strictly increasing; Put with expectedRev 0 requires the key to be absent.
type KV interface {
	Get(ctx context.Context, key string) (val []byte, rev uint64, err error)
	Put(ctx context.Context, key string, expectedRev uint64, val []byte) (rev uint64, err error)
	Keys(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// Blobs holds bulk immutable bytes (large-record offload; workspace snapshots).
// Put streams; keys are content-addressed by callers. Existing byte-identical
// content is a success/no-op; existing different content returns
// *BlobConflictError and leaves the original object unchanged. Delete is
// idempotent: deleting an absent key succeeds.
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Composite satisfies Ledger+Leaser+KV+Blobs by embedding one provider per
// primitive. Assembled where dependencies are wired, never inside engines.
type Composite struct {
	Ledger
	Leaser
	KV
	Blobs
}

// IncompleteCompositeError reports that NewComposite was handed one or more nil
// primitives. Missing names them in field order (Ledger, Leaser, KV, Blobs) so
// the assembly site knows exactly which providers were not wired.
type IncompleteCompositeError struct {
	Missing []string
}

func (e *IncompleteCompositeError) Error() string {
	return "storage: incomplete composite: missing [" + strings.Join(e.Missing, " ") + "]"
}

// NewComposite assembles a Composite, rejecting any nil primitive up front so a
// partially-wired backend fails at the composition root rather than at first use.
func NewComposite(l Ledger, le Leaser, kv KV, bl Blobs) (*Composite, error) {
	var missing []string
	if l == nil {
		missing = append(missing, "Ledger")
	}
	if le == nil {
		missing = append(missing, "Leaser")
	}
	if kv == nil {
		missing = append(missing, "KV")
	}
	if bl == nil {
		missing = append(missing, "Blobs")
	}
	if len(missing) > 0 {
		return nil, &IncompleteCompositeError{Missing: missing}
	}
	return &Composite{Ledger: l, Leaser: le, KV: kv, Blobs: bl}, nil
}
