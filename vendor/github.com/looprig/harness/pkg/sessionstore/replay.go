package sessionstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
)

// ReplayRequest positions a sessionstore replay. It carries an exported inclusive
// start sequence because journal.ReplayRequest hides its start behind a
// package-private journal.StartPos that an out-of-package replayer cannot read: the
// storage replayer's positioning must therefore flow through this request, set when
// the replayer is opened. Subject/loop narrowing is not part of storage replay — a
// session is one ledger, walked whole and filtered by envelope kind — so this
// request needs only the start position.
type ReplayRequest struct {
	// FromSeq is the inclusive ledger sequence to begin at. Storekit sequences are
	// 1-based and Ledger.Read(from) yields the record at Seq==from first; 0 (and 1)
	// both begin at the first record.
	FromSeq uint64
}

// BlobIntegrityError reports an offloaded record whose fetched blob bytes do not
// hash to the sha256 its ledger pointer named: sha256(bytes) != pointer.SHA256, so
// the blob has been corrupted or substituted. It fails secure — replay surfaces it
// rather than decoding tampered bytes — and carries the record's ledger sequence,
// the blob key, and both the expected (pointer) and actual hashes.
type BlobIntegrityError struct {
	Seq  uint64
	Key  string
	Want string // the sha256 the pointer named (expected)
	Got  string // sha256 of the bytes actually fetched
}

func (e *BlobIntegrityError) Error() string {
	return "sessionstore: offloaded record at seq " + strconv.FormatUint(e.Seq, 10) +
		" (blob " + strconv.Quote(e.Key) + ") is corrupt: fetched bytes hash " + strconv.Quote(e.Got) +
		", pointer names " + strconv.Quote(e.Want)
}

// BlobPointerIDMismatchError reports an offloaded record whose OUTER blobptr
// envelope's idempotency id does not match the id embedded in the RESOLVED inner
// envelope. The writer always stamps the exact same id on both halves of an offload
// (see sessionJournal.offload/frame — both the inline pre-offload envelope and its
// blobptr stand-in carry rec.IdempotencyID()), so a mismatch means the pointer and the
// blob it names have drifted apart. Replay fails closed rather than trusting either id
// blindly — this is the id-integrity counterpart to BlobIntegrityError's content hash
// check, and matters because a durable idempotency index is keyed by this id.
type BlobPointerIDMismatchError struct {
	Seq     uint64
	Key     string
	OuterID string
	InnerID string
}

func (e *BlobPointerIDMismatchError) Error() string {
	return "sessionstore: offloaded record at seq " + strconv.FormatUint(e.Seq, 10) +
		" (blob " + strconv.Quote(e.Key) + ") id mismatch: pointer names " + strconv.Quote(e.OuterID) +
		", resolved envelope carries " + strconv.Quote(e.InnerID)
}

// BlobUnavailableError reports that an offloaded record's backing blob could not be
// fetched: a dangling pointer (the blob is absent — Cause is a
// *storage.BlobNotFoundError) or any other Blobs.Get / read failure. It fails
// closed — replay surfaces it rather than yielding a zero-valued record — so a
// missing blob can never be mistaken for a drained backlog. It carries the record's
// ledger sequence and the blob key, and unwraps to the underlying cause.
type BlobUnavailableError struct {
	Seq   uint64
	Key   string
	Cause error
}

func (e *BlobUnavailableError) Error() string {
	return "sessionstore: offloaded record at seq " + strconv.FormatUint(e.Seq, 10) +
		" references blob " + strconv.Quote(e.Key) + " that could not be read: " + e.Cause.Error()
}
func (e *BlobUnavailableError) Unwrap() error { return e.Cause }

// ReplayDecodeError reports a failure to decode a replayed ledger record into its
// typed form: an undecodable envelope, an undecodable blob pointer, an unexpected
// (post-resolution) envelope kind, or a codec unmarshal failure on the record's
// body. It fails secure — replay surfaces it rather than skipping or zero-valuing
// the record — and carries the offending record's ledger sequence and the
// underlying cause (a *EnvelopeError, an event/command codec error, etc.).
type ReplayDecodeError struct {
	Seq   uint64
	Cause error
}

func (e *ReplayDecodeError) Error() string {
	return "sessionstore: replay decode at seq " + strconv.FormatUint(e.Seq, 10) + ": " + e.Cause.Error()
}
func (e *ReplayDecodeError) Unwrap() error { return e.Cause }

// ReplayReadError reports a failure to read the next record from the ledger cursor
// (a backend Ledger.Read or Cursor.Next failure). It fails closed: replay surfaces
// it rather than guessing the backlog is drained. It carries the ledger name and
// unwraps to the underlying cause.
type ReplayReadError struct {
	Name  string
	Cause error
}

func (e *ReplayReadError) Error() string {
	return "sessionstore: replay read on ledger " + strconv.Quote(e.Name) + ": " + e.Cause.Error()
}
func (e *ReplayReadError) Unwrap() error { return e.Cause }

// OpenEventReplayer returns a read-side replayer over session id's ledger that
// surfaces the session's events only — commands and internal fences are filtered
// out, matching pkg/journal's subject-filtered EventReplayer (which binds a consumer
// to the event subjects alone). Positioning comes from req.FromSeq (inclusive). The
// returned value satisfies the unchanged journal.EventReplayer interface; its Open
// binds the ledger cursor. Construction does no I/O — the ctx-bounded read happens in
// Open — so it takes no context. A zero id yields a concrete (empty) session ledger,
// not a wildcard, so it is allowed and simply replays as empty.
func (s *Store) OpenEventReplayer(id uuid.UUID, req ReplayRequest) (journal.EventReplayer, error) {
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	return &eventReplayer{
		ledger:     s.backend.Ledger,
		blobs:      s.backend.Blobs,
		name:       name,
		fromSeq:    req.FromSeq,
		publicOnly: true,
	}, nil
}

// OpenInternalEventReplayer returns the privileged event stream used by restore
// and catalog repair. Product-facing readers use OpenEventReplayer instead.
func (s *Store) OpenInternalEventReplayer(id uuid.UUID, req ReplayRequest) (journal.EventReplayer, error) {
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	return &eventReplayer{ledger: s.backend.Ledger, blobs: s.backend.Blobs, name: name, fromSeq: req.FromSeq}, nil
}

// OpenInternalRecordReplayer returns the privileged full read side used by restore
// and storage maintenance. It surfaces EVERY record — public and internal events,
// commands, and fences — in ledger-sequence order. Product-facing readers must use
// OpenEventReplayer, which filters non-public event visibility. Positioning comes
// from req.FromSeq (inclusive). The returned value satisfies journal.RecordReplayer's
// full-stream contract; its Open binds the ledger cursor. Construction does no I/O,
// so it takes no context.
func (s *Store) OpenInternalRecordReplayer(id uuid.UUID, req ReplayRequest) (journal.RecordReplayer, error) {
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	return &recordReplayer{
		id:      id,
		ledger:  s.backend.Ledger,
		blobs:   s.backend.Blobs,
		name:    name,
		fromSeq: req.FromSeq,
	}, nil
}

// eventReplayer is the concrete journal.EventReplayer over one session's storage
// ledger. It holds no per-replay state: every Open builds an independent ledger
// cursor, so concurrent replays do not interfere.
type eventReplayer struct {
	ledger     storage.Ledger
	blobs      storage.Blobs
	name       string
	fromSeq    uint64
	publicOnly bool
}

var _ journal.EventReplayer = (*eventReplayer)(nil)

// Open binds a ledger cursor at the replayer's inclusive start sequence and returns
// an EventCursor over it. Follow:true fails closed with a typed
// *journal.FollowUnsupportedError (live tailing is not implemented, matching
// pkg/journal) rather than silently behaving as a cold cursor.
//
// req.LoopID, when non-zero, narrows the replay to that loop exactly as pkg/journal's
// subject-filtered EventReplayer does: it keeps the session-scoped events (which the
// NATS filter captures via the session-event subject) PLUS that loop's events, and
// drops every OTHER loop's events. This is load-bearing for restore's foldLoop,
// which must not fold one loop's events into the requested root loop's thread. A zero
// LoopID replays all loops' events (unnarrowed). The session and start are bound at
// OpenEventReplayer time; positioning is not re-read from req (journal.StartPos is
// package-private), but LoopID and Follow are exported and honored here.
func (r *eventReplayer) Open(ctx context.Context, req journal.ReplayRequest) (journal.EventCursor, error) {
	if req.Follow {
		return nil, &journal.FollowUnsupportedError{Stream: r.name}
	}
	cur, err := r.ledger.Read(ctx, r.name, r.fromSeq)
	if err != nil {
		return nil, &ReplayReadError{Name: r.name, Cause: err}
	}
	return &eventCursor{loopID: req.LoopID, publicOnly: r.publicOnly, base: baseCursor{name: r.name, blobs: r.blobs, cur: cur}}, nil
}

// recordReplayer is the concrete journal.RecordReplayer over one session's storage
// ledger. Unlike eventReplayer it carries the session id: a command or fence record
// does not embed its routing session in the envelope, so the replayer stamps the
// bound session id onto the reconstructed CommandRecord/FenceRecord.
type recordReplayer struct {
	id      uuid.UUID
	ledger  storage.Ledger
	blobs   storage.Blobs
	name    string
	fromSeq uint64
}

var _ journal.RecordReplayer = (*recordReplayer)(nil)

// Open binds a ledger cursor at the replayer's inclusive start sequence and returns
// a RecordCursor over it. Follow:true fails closed exactly as eventReplayer.Open.
func (r *recordReplayer) Open(ctx context.Context, req journal.ReplayRequest) (journal.RecordCursor, error) {
	if req.Follow {
		return nil, &journal.FollowUnsupportedError{Stream: r.name}
	}
	cur, err := r.ledger.Read(ctx, r.name, r.fromSeq)
	if err != nil {
		return nil, &ReplayReadError{Name: r.name, Cause: err}
	}
	return &recordCursor{id: r.id, base: baseCursor{name: r.name, blobs: r.blobs, cur: cur}}, nil
}

// resolved is one fully-resolved ledger record: its real (post-blobptr-resolution)
// envelope kind, its authoritative body bytes, its ledger sequence, and its
// idempotency id preserved from the envelope (for an offloaded record, the outer
// blobptr's id, verified equal to the resolved inner envelope's id — see
// baseCursor.resolveBlob). id is an internal-replay detail: the typed cursors
// (EventCursor/RecordCursor) re-derive a decoded record's IdempotencyID() from its
// payload rather than trusting this field, but it is the join key the idempotency
// index hydration (pkg/sessionstore/journal.go) relies on to key each ledger entry
// without a full typed decode.
type resolved struct {
	kind kind
	body []byte
	seq  uint64
	id   string
}

// baseCursor is the shared read + resolve machinery both cursors wrap. It walks one
// storage ledger cursor, decodes each envelope, and resolves a blobptr transparently
// (fetch + sha256-verify + decode the offloaded envelope) so the caller only ever
// sees a real record kind. A cursor is a single-reader handle — concurrent next calls
// are not supported.
//
// mu guards baseCursor's OWN fields (cur, closed): it makes Close idempotent and
// guarantees a next after Close observes closed and returns io.EOF rather than racing
// the field, with no Go-level data race on those fields. It does NOT serialize the
// UNDERLYING storage cursor's Next/Close — next reads cur under mu but releases it
// before calling cur.Next(ctx), so a Close concurrent with an in-flight next may reach
// the backend cursor while its Next is running. That is sound for the drained-snapshot
// backends here (memstore's Close is a no-op); a future networked backend whose Close
// tears down a live subscription must provide its own Next/Close safety and must not
// rely on serialization this layer does not give.
type baseCursor struct {
	name  string
	blobs storage.Blobs

	mu     sync.Mutex
	cur    storage.Cursor
	closed bool
}

// next reads and resolves the next ledger record. It returns io.EOF when the cursor
// is drained. A blobptr record is resolved transparently to its original kind+body;
// every failure is a typed fail-secure error that does NOT advance past the record.
func (b *baseCursor) next(ctx context.Context) (resolved, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return resolved{}, io.EOF
	}
	cur := b.cur
	b.mu.Unlock()

	rec, err := cur.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return resolved{}, io.EOF
		}
		return resolved{}, &ReplayReadError{Name: b.name, Cause: err}
	}

	env, err := decodeEnvelope(rec.Payload)
	if err != nil {
		return resolved{}, &ReplayDecodeError{Seq: rec.Seq, Cause: err}
	}
	if kind(env.Kind) == kindBlobPtr {
		return b.resolveBlob(ctx, env, rec.Seq)
	}
	return resolved{kind: kind(env.Kind), body: env.Body, seq: rec.Seq, id: env.ID}, nil
}

// resolveBlob rehydrates an offloaded record: decode its pointer, fetch the named
// blob, verify the fetched bytes hash to the pointer's sha256 (fail secure on a
// mismatch), then decode the fetched bytes as the ORIGINAL envelope and return its
// real kind+body. A missing/unreadable blob → *BlobUnavailableError; a hash mismatch
// → *BlobIntegrityError; an undecodable pointer or offloaded envelope →
// *ReplayDecodeError. The read is bounded to the pointer's declared Size (+1 to
// detect an over-long blob) so a substituted oversized blob cannot exhaust memory
// before the hash check runs.
func (b *baseCursor) resolveBlob(ctx context.Context, env envelope, seq uint64) (resolved, error) {
	ptr, err := decodeBlobPointer(env.Body)
	if err != nil {
		return resolved{}, &ReplayDecodeError{Seq: seq, Cause: err}
	}

	rc, err := b.blobs.Get(ctx, ptr.Key)
	if err != nil {
		return resolved{}, &BlobUnavailableError{Seq: seq, Key: ptr.Key, Cause: err}
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, ptr.Size+1))
	if err != nil {
		return resolved{}, &BlobUnavailableError{Seq: seq, Key: ptr.Key, Cause: err}
	}

	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	// A wrong length is a corruption too; the hash comparison catches it (the pointer
	// hashes exactly Size bytes), so a single BlobIntegrityError covers both.
	if int64(len(raw)) != ptr.Size || got != ptr.SHA256 {
		return resolved{}, &BlobIntegrityError{Seq: seq, Key: ptr.Key, Want: ptr.SHA256, Got: got}
	}

	inner, err := decodeEnvelope(raw)
	if err != nil {
		return resolved{}, &ReplayDecodeError{Seq: seq, Cause: err}
	}
	// The offloaded bytes are the ORIGINAL non-blobptr envelope; a nested blobptr is
	// malformed (the writer never nests offloads). Fail secure rather than loop.
	if kind(inner.Kind) == kindBlobPtr {
		return resolved{}, &ReplayDecodeError{Seq: seq, Cause: &EnvelopeError{Reason: "nested blobptr offload"}}
	}
	// The writer always stamps the SAME idempotency id on both the outer blobptr
	// (env, passed in by the caller) and the inner offloaded envelope it points at.
	// A mismatch means they have drifted apart — fail closed rather than trust
	// either half blindly (the id becomes a durable idempotency-index key).
	if inner.ID != env.ID {
		return resolved{}, &BlobPointerIDMismatchError{Seq: seq, Key: ptr.Key, OuterID: env.ID, InnerID: inner.ID}
	}
	return resolved{kind: kind(inner.Kind), body: inner.Body, seq: seq, id: inner.ID}, nil
}

// close tears down the underlying ledger cursor. It is idempotent: a second call is
// a no-op.
func (b *baseCursor) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.cur == nil {
		return nil
	}
	if err := b.cur.Close(); err != nil {
		return &ReplayReadError{Name: b.name, Cause: err}
	}
	return nil
}

// eventCursor is the concrete journal.EventCursor: it yields the session's events
// only, skipping command and fence records — the same records pkg/journal's
// subject-filtered EventReplayer never delivers — and, when loopID is set, skipping
// every other loop's events too.
type eventCursor struct {
	// loopID, when non-zero, narrows delivery to session-scoped events + this loop's
	// events; a zero value delivers all loops' events. It mirrors the loop-narrowed
	// subject filter of pkg/journal's EventReplayer (session-event + this loop's event
	// subject).
	loopID     uuid.UUID
	publicOnly bool
	base       baseCursor
}

var _ journal.EventCursor = (*eventCursor)(nil)

// Next returns the next event and its ledger sequence, skipping any command or fence
// record and — when loopID is set — any other loop's event, or io.EOF once the ledger
// is drained. A decode/blob failure fails secure as a typed error.
func (c *eventCursor) Next(ctx context.Context) (event.Event, uint64, error) {
	for {
		r, err := c.base.next(ctx)
		if err != nil {
			return nil, 0, err
		}
		switch r.kind {
		case kindEvent:
			ev, err := event.UnmarshalEvent(r.body)
			if err != nil {
				return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: err}
			}
			if !c.deliver(ev) {
				continue // loop-narrowed: another loop's event, dropped like the NATS filter
			}
			return ev, r.seq, nil
		case kindCommand, kindFence, kindGatePrepared:
			continue // events only — commands, fences, and private gate-prepared records are filtered out
		default:
			return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: &EnvelopeError{Reason: "unexpected kind " + strconv.Quote(string(r.kind))}}
		}
	}
}

// deliver reports whether ev passes this cursor's loop filter. An unnarrowed cursor
// (zero loopID) delivers every event. A narrowed cursor delivers session-scoped
// events and events of its own loop, and drops every other loop's events — routing on
// the event's Scope()/LoopID, the same routing the journal's event records carry.
func (c *eventCursor) deliver(ev event.Event) bool {
	if c.publicOnly && ev.Visibility() != event.Public {
		return false
	}
	if c.loopID.IsZero() {
		return true
	}
	if ev.Scope() == event.ScopeSession {
		return true
	}
	return ev.EventHeader().LoopID == c.loopID
}

// Close tears down the cursor. Idempotent.
func (c *eventCursor) Close() error { return c.base.close() }

// recordCursor is the concrete journal.RecordCursor: it yields every record —
// events, commands, and fences — as the matching journal.JournalRecord variant. It
// stamps the bound session id onto reconstructed command and fence records (whose
// routing session the envelope does not carry).
type recordCursor struct {
	id   uuid.UUID
	base baseCursor
}

var _ journal.RecordCursor = (*recordCursor)(nil)

// Next returns the next record and its ledger sequence, or io.EOF once the ledger is
// drained. It dispatches on the resolved envelope kind into the matching
// JournalRecord variant. A decode/blob failure fails secure as a typed error.
//
// A CommandRecord is reconstructed with the bound session id and a ZERO dispatch
// loop id: the envelope frames only {kind, id, payload} and does not persist a
// command's routing loop id (the NATS journal recovered it from the record's
// subject; there is no subject in a storage ledger). The sole consumer
// (transcript/journalsource) uses only the wrapped command, so the dropped loop id
// is immaterial there.
func (c *recordCursor) Next(ctx context.Context) (journal.JournalRecord, uint64, error) {
	r, err := c.base.next(ctx)
	if err != nil {
		return nil, 0, err
	}
	switch r.kind {
	case kindEvent:
		ev, err := event.UnmarshalEvent(r.body)
		if err != nil {
			return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: err}
		}
		return journal.NewEventRecord(ev), r.seq, nil
	case kindCommand:
		cmd, err := command.UnmarshalCommand(r.body)
		if err != nil {
			return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: err}
		}
		return journal.NewCommandRecord(c.id, uuid.UUID{}, cmd), r.seq, nil
	case kindFence:
		fence, err := journal.UnmarshalLeaseFence(r.body)
		if err != nil {
			return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: err}
		}
		return journal.NewFenceRecord(c.id, fence), r.seq, nil
	case kindGatePrepared:
		rec, err := journal.UnmarshalGatePreparedRecord(r.body)
		if err != nil {
			return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: err}
		}
		return rec, r.seq, nil
	default:
		return nil, 0, &ReplayDecodeError{Seq: r.seq, Cause: &EnvelopeError{Reason: "unexpected kind " + strconv.Quote(string(r.kind))}}
	}
}

// Close tears down the cursor. Idempotent.
func (c *recordCursor) Close() error { return c.base.close() }
