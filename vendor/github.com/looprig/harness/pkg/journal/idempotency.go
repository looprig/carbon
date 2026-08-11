package journal

import (
	"context"
	"crypto/sha256"
	"strconv"
)

// AppendResult reports the outcome of an append issued through an IdempotentJournal:
// the durable sequence the record occupies, and whether this call durably persisted a
// NEW frame (Appended=true) or deduplicated an identical retry of an already-durable
// record (Appended=false; Sequence is then the ORIGINAL append's sequence, not a new
// one).
type AppendResult struct {
	Sequence uint64
	Appended bool
}

// IdempotentJournal is the OPTIONAL extension a SessionJournal implementation may
// satisfy to deduplicate a redelivered append by idempotency id. It embeds
// SessionJournal so an idempotent implementation is usable anywhere a plain
// SessionJournal is expected — the existing narrow Append seam is never weakened or
// replaced. A caller that additionally wants to know whether ITS OWN call produced a
// new durable frame or deduplicated a retry (e.g. to skip a live broadcast for a
// duplicate) type-asserts for IdempotentJournal and calls AppendIdempotent instead of
// Append.
type IdempotentJournal interface {
	SessionJournal
	// AppendIdempotent behaves exactly like Append — same fencing, same errors —
	// except a record whose IdempotencyID() already names a durable record with an
	// IDENTICAL persisted kind+payload is detected and reported via
	// AppendResult.Appended=false (carrying the ORIGINAL sequence) rather than
	// durably appended a second time. A record whose id names a durable record with
	// a DIFFERENT persisted kind or payload fails closed with a typed
	// *IdempotencyCollisionError.
	AppendIdempotent(ctx context.Context, rec JournalRecord) (AppendResult, error)
}

// IdempotencyCollisionError reports that a record's idempotency id already names a
// durable record in the log with a DIFFERENT persisted kind or payload — a genuine id
// collision (a bug or a forged retry), never a legitimate duplicate retry (which is
// always byte-identical to what is already durable). The append fails closed rather
// than silently accepting a differently-shaped record under a reused id.
type IdempotencyCollisionError struct {
	ID  string
	Seq uint64 // the ledger sequence the ORIGINAL (colliding) record already occupies
}

func (e *IdempotencyCollisionError) Error() string {
	return "journal: idempotency collision for id " + strconv.Quote(e.ID) +
		": already durable at seq " + strconv.FormatUint(e.Seq, 10) + " with a different kind/payload"
}

// Fingerprint identifies a record's persisted kind and payload bytes — exactly what a
// backend durably writes — independent of any transient in-memory routing a record
// wrapper additionally carries (CommandRecord's session/loop dispatch target is never
// persisted, so it never enters a Fingerprint: a backend derives Fingerprint from the
// SAME (kind, codec-marshaled body) pair it writes to the log, never from the record's
// Go value). Two records fingerprint equal if and only if a backend would durably
// persist byte-identical frames for them.
type Fingerprint struct {
	kind string
	sum  [sha256.Size]byte
}

// NewFingerprint derives the Fingerprint of a record's persisted envelope kind (the
// backend-neutral kind name, e.g. "event"/"command"/"fence") and its codec-marshaled
// payload bytes, exactly as a backend encodes them before persisting.
func NewFingerprint(kind string, body []byte) Fingerprint {
	return Fingerprint{kind: kind, sum: sha256.Sum256(body)}
}

// IdempotencyIndex tracks, for every idempotency id already durable in a session's
// log, the ledger sequence it occupies and the Fingerprint of what was persisted under
// it. A backend hydrates one from its full durable ledger (see the sessionstore
// package) before accepting new appends, then consults and updates it — via Check and
// Observe — under the same lock that already serializes its writes. It is NOT safe for
// concurrent use on its own; the caller's append-serializing lock is its only
// synchronization.
type IdempotencyIndex struct {
	entries map[string]indexEntry
}

// indexEntry is one IdempotencyIndex slot: the sequence an id's record occupies and
// the Fingerprint of what was persisted under it.
type indexEntry struct {
	seq         uint64
	fingerprint Fingerprint
}

// NewIdempotencyIndex returns an empty index ready for hydration.
func NewIdempotencyIndex() *IdempotencyIndex {
	return &IdempotencyIndex{entries: make(map[string]indexEntry)}
}

// Observe records that id occupies seq with fingerprint fp, overwriting any prior
// entry for id. A backend calls it once per record while hydrating from history, and
// once more immediately after each new durable append lands.
func (idx *IdempotencyIndex) Observe(id string, seq uint64, fp Fingerprint) {
	idx.entries[id] = indexEntry{seq: seq, fingerprint: fp}
}

// Check consults the index for id against the CANDIDATE fingerprint fp of a record
// about to be appended:
//   - id has never been observed: (0, false, nil) — the caller should proceed to
//     durably append the record as new.
//   - id was observed with an IDENTICAL fingerprint: (seq, true, nil) — the caller
//     should report AppendResult{Sequence: seq, Appended: false} WITHOUT appending.
//   - id was observed with a DIFFERENT fingerprint: (0, false, *IdempotencyCollisionError)
//     — the caller must fail the append closed.
func (idx *IdempotencyIndex) Check(id string, fp Fingerprint) (seq uint64, duplicate bool, err error) {
	existing, ok := idx.entries[id]
	if !ok {
		return 0, false, nil
	}
	if existing.fingerprint == fp {
		return existing.seq, true, nil
	}
	return 0, false, &IdempotencyCollisionError{ID: id, Seq: existing.seq}
}
