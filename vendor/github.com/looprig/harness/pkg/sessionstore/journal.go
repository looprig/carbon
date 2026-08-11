package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
)

// appendTimeout bounds a single Append's ledger round-trip (offload upload plus
// the AppendDefinite) independent of the caller's context. The writer holds its
// mutex across the whole operation, so an unbounded backend call would wedge every
// queued Append behind it; this per-append deadline fails one stuck call fast and
// keeps the serialized writer live. The value matches the journal's historical
// per-append publish deadline, carried over to the storage-backed writer.
const appendTimeout = 5 * time.Second

// hydrateTimeout bounds OpenJournal's full-ledger walk to hydrate the idempotency
// index, independent of the caller's context — a wedged backend must not hang Open
// forever. It is more generous than appendTimeout because it may need to read and
// (for every offloaded record) fetch a whole session's history, not one record.
const hydrateTimeout = 30 * time.Second

// blobsInfix is the name segment separating a session's ledger prefix from its
// content-addressed offload blobs: a blob lands at "sessions/<uuid>/blobs/<sha>".
const blobsInfix = "/blobs/"

var errOpeningAppendMiddleware = errors.New("sessionstore: opening append middleware must delegate exactly once")

// NilLeaseError reports that a Store constructor (OpenJournal or OpenObjectGC) was
// handed a nil lease. The lease is a required dependency (DIP): the composition root
// acquires it via AcquireLease and passes it in. The constructor fails closed with
// this typed error rather than deferring a nil dereference to first use (stamping the
// epoch into the opening fence, or the GC lease guard).
type NilLeaseError struct {
	SessionID uuid.UUID
}

func (e *NilLeaseError) Error() string {
	return "sessionstore: session " + e.SessionID.String() + ": nil lease"
}

// sessionJournal is the concrete single-writer serializer over a storage ledger:
// it frames each JournalRecord as a versioned envelope, offloads an over-threshold
// frame to Blobs before appending a small pointer in its place, and commits under
// CAS fencing on the tracked tip. It is the sessionstore port of the NATS
// journal's WRITE semantics onto storage — storage.AppendDefinite owns the
// ambiguous-ack / conflict resolution the old journal did by hand.
type sessionJournal struct {
	id        uuid.UUID      // the session this journal owns (for fence + error context)
	lease     journal.Lease  // single-writer ownership token (injected; never acquired here)
	ledger    storage.Ledger // the append-only record log this journal is the sole writer of
	blobs     storage.Blobs  // content-addressed offload store for over-threshold frames
	name      string         // the bound ledger name (ledgerName(id))
	threshold int            // frame size (bytes) above which a record is offloaded

	// mu serializes Append and guards ready + trackedTip. The serializer is
	// single-writer by contract; the mutex makes that safe even if a caller fans
	// Append across goroutines.
	mu sync.Mutex
	// ready is set true only after the opening fence has committed (the journal has
	// taken ownership of the tip). Append refuses with a typed JournalNotReadyError
	// until then so no record ever precedes the ownership fence.
	ready bool
	// trackedTip is the ledger sequence the next append must fence on: the last
	// sequence this writer committed (the tip observed at Open, then each append's
	// new seq). A stale writer whose trackedTip is behind the real tip is rejected
	// by storage's CAS on append.
	trackedTip uint64

	// idx tracks every idempotency id already durable in this session's log —
	// hydrated from the full ledger AFTER the opening fence has claimed ownership
	// (see OpenJournalWithOpeningAppend and hydrateJournalIndexes) — so a
	// redelivered Append/AppendIdempotent can be detected and deduplicated instead
	// of writing a second frame. It is guarded by mu exactly like ready/trackedTip:
	// only appendChecked reads or updates it once the journal is open, and Open
	// itself holds mu across both the fence commit and the hydration that follows
	// it, so no external caller can observe or mutate it before hydration finishes.
	idx *journal.IdempotencyIndex
	// deliveryTransitions indexes the logical request state for phased delegate
	// commands. It is guarded by mu together with idx and is updated only after
	// the corresponding physical frame commits.
	deliveryTransitions map[uuid.UUID]deliveryTransition
}

type deliveryTransition struct {
	fingerprint journal.Fingerprint
	intentSeq   uint64
	fallbackSeq uint64
}

// Compile-time proofs that *sessionJournal honors both the plain journal.SessionJournal
// contract and its optional idempotent extension.
var (
	_ journal.SessionJournal    = (*sessionJournal)(nil)
	_ journal.IdempotentJournal = (*sessionJournal)(nil)
)

// OpenJournal binds a single-writer journal to session id's ledger and takes
// ownership of the tip by writing the opening fence — a fence-kind envelope
// carrying the lease epoch — as an append fenced on the ledger's current tip. That
// fence advances the tip, so any stale prior writer's next CAS append conflicts;
// only once it commits is the journal ready to accept Appends. The lease is a
// required dependency (DIP): a nil lease fails closed with *NilLeaseError.
func (s *Store) OpenJournal(ctx context.Context, id uuid.UUID, lease journal.Lease) (journal.SessionJournal, error) {
	return s.OpenJournalWithOpeningAppend(ctx, id, lease, nil)
}

// OpenJournalWithOpeningAppend is OpenJournal with middleware around the
// ownership fence append. The middleware sees the fence while it is still part
// of journal construction; the journal is returned only after that append
// commits and ready is set. Later appends are not decorated by this seam.
func (s *Store) OpenJournalWithOpeningAppend(
	ctx context.Context,
	id uuid.UUID,
	lease journal.Lease,
	middleware journal.AppendMiddleware,
) (journal.SessionJournal, error) {
	if lease == nil {
		return nil, &NilLeaseError{SessionID: id}
	}
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	// Bound the tip read on the same per-append budget so a wedged backend cannot
	// block Open indefinitely (every I/O call carries a deadline).
	tipCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	defer cancel()
	tip, err := s.backend.Ledger.Tip(tipCtx, name)
	if err != nil {
		return nil, err
	}

	j := &sessionJournal{
		id:                  id,
		lease:               lease,
		ledger:              s.backend.Ledger,
		blobs:               s.backend.Blobs,
		name:                name,
		threshold:           s.opts.OffloadThreshold,
		trackedTip:          tip,
		idx:                 journal.NewIdempotencyIndex(),
		deliveryTransitions: make(map[uuid.UUID]deliveryTransition),
	}

	// Take ownership FIRST, immediately after the tip read: on the middleware-free
	// path there is no intervening I/O at all — the same tight tip-then-CAS
	// coupling every later Append already has via trackedTip (writeLocked never
	// re-reads the tip; it CASes on whatever is already tracked in memory). A
	// caller-supplied AppendMiddleware (as both of internal/sessionruntime's own
	// callers — Lifecycle.NewSession and restoreTopologySession — install via
	// journal.HookMiddleware whenever a hook handles OperationJournalAppend) still
	// runs between the tip read and the CAS below and could itself do I/O — that
	// gap is real but bounded by whatever the hook does, not by a full-ledger
	// walk, and it existed in this exact position in the pre-fix code too.
	// Claiming the fence this early, BEFORE the idempotency-index hydration below,
	// closes the window in which a still-live predecessor writer (a crash-path
	// teardown append, or a parked goroutine unblocked by context cancellation —
	// see handBackRequest) could land a write on this ledger and advance the tip
	// out from under a tip value cached before a slow walk. The first append is
	// the opening fence, stamping the lease
	// epoch and fenced on the current tip. A stale prior owner (or higher-epoch
	// successor) that advanced the ledger causes this CAS to conflict and Open to
	// fail closed. Only once it commits is the journal ready.
	fence := journal.NewFenceRecord(id, journal.LeaseFence{Epoch: lease.Epoch()})
	j.mu.Lock()
	defer j.mu.Unlock()
	delegations := 0
	var fenceSeq uint64
	var fenceErr error
	appendOpening := journal.AppendFunc(func(appendCtx context.Context, _ journal.JournalRecord) (uint64, error) {
		delegations++
		if delegations != 1 {
			return 0, errOpeningAppendMiddleware
		}
		// The middleware may derive context but cannot substitute the ownership
		// record: construction always commits this exact fence THROUGH THE RAW
		// writeLocked path — never through appendChecked's idempotency gate — so
		// even a repeated lease epoch (whose id would otherwise look like a prior
		// duplicate) still physically advances the tip and fences out a stale
		// writer.
		fenceSeq, fenceErr = j.writeLocked(appendCtx, fence)
		return fenceSeq, fenceErr
	})
	if middleware != nil {
		appendOpening = middleware(appendOpening)
	}
	if appendOpening == nil {
		return nil, errOpeningAppendMiddleware
	}
	// The middleware's returned pair is deliberately ignored. Open derives its
	// result only from the guarded real append, so a decorator cannot suppress
	// an error or fabricate a successful fence sequence.
	_, _ = appendOpening(ctx, fence)
	if delegations != 1 {
		return nil, errOpeningAppendMiddleware
	}
	if fenceErr != nil {
		return nil, fenceErr
	}

	// Now that ownership is claimed, hydrate the idempotency index from whatever
	// is durable — including the fence just committed above, and anything else
	// that lands on the ledger before this walk observes it, since a fresh
	// full-ledger read has no dependency on the pre-fence tip snapshot. Deferring
	// this SLOW walk until after the fence commits is what closes the race: no
	// predecessor writer can land another append once the fence has fenced it out
	// (its next CAS conflicts against the tip this fence already advanced), so
	// hydration can safely take as long as it needs without widening the window
	// the opening fence itself is exposed to. j.ready stays false and j is not
	// yet returned to any caller, so nothing can call Append/AppendIdempotent
	// through this instance and race the walk. A ledger that had nothing durable
	// before this fence (tip 0, the common fresh-session case) has nothing more to
	// hydrate beyond what the fence's own hygiene Observe below already records,
	// so the walk is skipped entirely rather than performed for no reason.
	if tip > 0 {
		hydrateCtx, hydrateCancel := context.WithTimeout(ctx, hydrateTimeout)
		idx, transitions, hydrateErr := hydrateJournalIndexes(hydrateCtx, s.backend.Ledger, s.backend.Blobs, name)
		hydrateCancel()
		if hydrateErr != nil {
			// Accepted trade-off of hydrating after the fence: the fence above has
			// already durably committed (the tip is advanced) even though Open now
			// fails closed and returns this journal to no caller. That leaves an
			// orphaned fence stamped with this lease's epoch sitting in the ledger —
			// impossible in the pre-fix ordering, where nothing could fail once the
			// fence's writeLocked succeeded. It is not a correctness problem: the
			// caller's failure path releases the lease as usual, and the next Open
			// simply reads a fresh tip past this stray fence and claims its own —
			// the same self-healing the epoch/fencing design already relies on for
			// any crash between a fence commit and full ownership. Surfacing it here
			// so a future reader chasing an orphaned fence in production logs has a
			// documented, expected cause rather than a mystery.
			return nil, hydrateErr
		}
		j.idx = idx
		j.deliveryTransitions = transitions
	}
	// Keep the index authoritative for the fence itself too. This is hygiene, not
	// load-bearing: the fence always commits through the raw writeLocked path above
	// regardless of what the index holds. It is also not redundant with the
	// hydration walk above, which — for a fresh session (tip 0 before this fence)
	// — is skipped and so never observes the fence on its own. A MarshalLeaseFence
	// failure here is unreachable in practice (a LeaseFence is one uint64) and is
	// simply not observed rather than failing a fence that has already durably
	// committed.
	if body, marshalErr := journal.MarshalLeaseFence(fence.Fence()); marshalErr == nil {
		j.idx.Observe(fence.IdempotencyID(), fenceSeq, journal.NewFingerprint(string(kindFence), body))
	}
	j.ready = true
	return j, nil
}

func hydrateJournalIndexes(ctx context.Context, ledger storage.Ledger, blobs storage.Blobs, name string) (*journal.IdempotencyIndex, map[uuid.UUID]deliveryTransition, error) {
	idx := journal.NewIdempotencyIndex()
	transitions := make(map[uuid.UUID]deliveryTransition)
	cur, err := ledger.Read(ctx, name, 1)
	if err != nil {
		return nil, nil, &ReplayReadError{Name: name, Cause: err}
	}
	base := &baseCursor{name: name, blobs: blobs, cur: cur}
	defer func() { _ = base.close() }()
	for {
		r, nextErr := base.next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return idx, transitions, nil
		}
		if nextErr != nil {
			return nil, nil, nextErr
		}
		idx.Observe(r.id, r.seq, journal.NewFingerprint(string(r.kind), r.body))
		if err := observeHydratedDeliveryTransition(transitions, r); err != nil {
			return nil, nil, err
		}
	}
}

// observeHydratedDeliveryTransition rebuilds the logical request index from
// one durable command frame. A fallback is accepted only after its intent has
// appeared earlier in ledger order with the same phase-normalized payload.
func observeHydratedDeliveryTransition(transitions map[uuid.UUID]deliveryTransition, r resolved) error {
	if r.kind != kindCommand {
		return nil
	}
	decoded, err := command.UnmarshalCommand(r.body)
	if err != nil {
		return &ReplayDecodeError{Seq: r.seq, Cause: err}
	}
	input, ok := decoded.(command.UserInput)
	if !ok || !input.DelegateDeliveryPhase.Valid() {
		return nil
	}
	record := journal.NewCommandRecord(uuid.UUID{}, uuid.UUID{}, input)
	if record.IdempotencyID() != r.id {
		return &journal.DeliveryTransitionError{
			CommandID: input.CommandID,
			Phase:     input.DelegateDeliveryPhase,
			Reason:    "physical id does not match command phase",
		}
	}
	fingerprint, err := record.NormalizedDeliveryFingerprint()
	if err != nil {
		return err
	}
	logicalID := input.CommandID
	prior, exists := transitions[logicalID]
	switch input.DelegateDeliveryPhase {
	case command.DelegateDeliveryPhaseIntent:
		if exists {
			if prior.fingerprint != fingerprint {
				return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "intent payload changed"}
			}
			if prior.intentSeq != 0 {
				return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "duplicate intent frame"}
			}
		}
		transitions[logicalID] = deliveryTransition{fingerprint: fingerprint, intentSeq: r.seq}
	case command.DelegateDeliveryPhaseFallbackQueued:
		if !exists || prior.intentSeq == 0 {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "fallback precedes intent"}
		}
		if prior.fingerprint != fingerprint {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "fallback payload differs from intent"}
		}
		if prior.fallbackSeq != 0 {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "duplicate fallback frame"}
		}
		prior.fallbackSeq = r.seq
		transitions[logicalID] = prior
	}
	return nil
}

// Append serializes rec behind mu, refuses if the journal is not ready or its lease
// is lost, then deduplicates by idempotency id, and — for a genuinely new record —
// frames, offloads-if-large, and commits rec under CAS on the tracked tip. The whole
// operation holds mu so the guard, dedup check, offload, append, tip advance, and
// index update are one atomic step; the append carries its own per-append deadline so
// one stuck call cannot wedge the queued writers. On success it returns the assigned
// (or, for a deduplicated retry, the ORIGINAL) ledger sequence. Append and
// AppendIdempotent share the same core (appendChecked); Append simply discards the
// Appended flag for callers that only need the sequence/error — see AppendIdempotent
// (journal.IdempotentJournal) for callers that need to distinguish a fresh append from
// a deduplicated retry.
func (b *sessionJournal) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	result, err := b.appendChecked(ctx, rec)
	return result.Sequence, err
}

// AppendIdempotent is Append's richer counterpart (journal.IdempotentJournal): see
// Append's doc for the shared mechanics. It exists so a caller that must react
// differently to a fresh append versus a deduplicated retry (e.g. skip a live
// broadcast for a duplicate) can observe that distinction via AppendResult.Appended.
func (b *sessionJournal) AppendIdempotent(ctx context.Context, rec journal.JournalRecord) (journal.AppendResult, error) {
	return b.appendChecked(ctx, rec)
}

// appendChecked is the shared serialized core behind Append and AppendIdempotent. It
// guards readiness/lease exactly as the plain Append always has, then — under the
// SAME lock — fingerprints rec's persisted (kind, body) via encodeRecordBody (the
// same codec path writeLocked/frame already use to encode for the wire; it never
// encodes a record's transient routing, e.g. CommandRecord's session/loop dispatch
// target, so that routing can never enter the fingerprint) and consults the hydrated
// IdempotencyIndex:
//   - an id never seen before is written as a new record (writeLocked) and then
//     observed into the index;
//   - an id seen before with an IDENTICAL fingerprint is deduplicated: no second
//     frame is written, and the ORIGINAL sequence is returned with Appended=false;
//   - an id seen before with a DIFFERENT fingerprint fails closed with a typed
//     *journal.IdempotencyCollisionError.
func (b *sessionJournal) appendChecked(ctx context.Context, rec journal.JournalRecord) (journal.AppendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.ready {
		return journal.AppendResult{}, &journal.JournalNotReadyError{SessionID: b.id}
	}
	if !b.leaseHeld() {
		return journal.AppendResult{}, &journal.JournalLeaseLostError{SessionID: b.id, Epoch: b.lease.Epoch()}
	}
	var commandRecord journal.CommandRecord
	var hasPhasedCommand bool
	if candidate, ok := rec.(journal.CommandRecord); ok && candidate.DeliveryPhase() != "" {
		if err := journal.ValidateCommandRecordRoute(candidate); err != nil {
			return journal.AppendResult{}, err
		}
		commandRecord = candidate
		hasPhasedCommand = true
	}

	k, body, err := b.encodeRecordBody(rec)
	if err != nil {
		return journal.AppendResult{}, err
	}
	id := rec.IdempotencyID()
	fp := journal.NewFingerprint(string(k), body)
	if seq, duplicate, checkErr := b.idx.Check(id, fp); checkErr != nil {
		return journal.AppendResult{}, checkErr
	} else if duplicate {
		return journal.AppendResult{Sequence: seq, Appended: false}, nil
	}

	var transitionRecord *journal.CommandRecord
	var pendingTransition deliveryTransition
	if hasPhasedCommand && commandRecord.DeliveryPhase().Valid() {
		pending, err := b.prepareDeliveryTransition(commandRecord)
		if err != nil {
			return journal.AppendResult{}, err
		}
		transitionRecord = &commandRecord
		pendingTransition = pending
	}

	seq, err := b.writeLocked(ctx, rec)
	if err != nil {
		return journal.AppendResult{}, err
	}
	b.idx.Observe(id, seq, fp)
	if transitionRecord != nil {
		if b.deliveryTransitions == nil {
			b.deliveryTransitions = make(map[uuid.UUID]deliveryTransition)
		}
		if transitionRecord.DeliveryPhase() == command.DelegateDeliveryPhaseIntent {
			pendingTransition.intentSeq = seq
		} else {
			pendingTransition.fallbackSeq = seq
		}
		b.deliveryTransitions[transitionRecord.LogicalCommandID()] = pendingTransition
	}
	return journal.AppendResult{Sequence: seq, Appended: true}, nil
}

func (b *sessionJournal) prepareDeliveryTransition(record journal.CommandRecord) (deliveryTransition, error) {
	fingerprint, err := record.NormalizedDeliveryFingerprint()
	if err != nil {
		return deliveryTransition{}, err
	}
	logicalID := record.LogicalCommandID()
	prior, exists := b.deliveryTransitions[logicalID]
	switch record.DeliveryPhase() {
	case command.DelegateDeliveryPhaseIntent:
		if exists {
			if prior.fingerprint != fingerprint {
				return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "intent payload changed"}
			}
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "intent already durable"}
		}
		return deliveryTransition{fingerprint: fingerprint}, nil
	case command.DelegateDeliveryPhaseFallbackQueued:
		if !exists || prior.intentSeq == 0 {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback precedes intent"}
		}
		if prior.fingerprint != fingerprint {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback payload differs from intent"}
		}
		if prior.fallbackSeq != 0 {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback already durable"}
		}
		return prior, nil
	default:
		return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "unsupported delivery phase"}
	}
}

// leaseHeld reports whether the ownership lease is still held: both its validity
// flag and its loss channel must say so. It is the fast-path ownership guard; the
// ledger's CAS fence is the hard backstop that catches a loss this guard races.
func (b *sessionJournal) leaseHeld() bool {
	if !b.lease.Valid() {
		return false
	}
	select {
	case <-b.lease.Lost():
		return false
	default:
		return true
	}
}

// writeLocked is the serialized write core shared by Append (after its ready/lease
// guard) and OpenJournal (the opening fence, which is what SETS ready). The caller
// MUST hold mu. It derives a per-append child context, frames rec (offloading an
// over-threshold frame to Blobs first), commits the resulting bytes under CAS on
// the tracked tip via storage.AppendDefinite, and on success advances the tip and
// returns the new sequence. On any failure the tip is left unadvanced (fail closed).
func (b *sessionJournal) writeLocked(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	childCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	defer cancel()

	recordBytes, err := b.frame(childCtx, rec)
	if err != nil {
		return 0, err
	}
	if err := storage.AppendDefinite(childCtx, b.ledger, b.name, b.trackedTip, recordBytes); err != nil {
		return 0, b.mapAppendErr(rec, err)
	}
	b.trackedTip++
	return b.trackedTip, nil
}

// frame encodes rec into the bytes to append: its codec body wrapped in a versioned
// envelope, and — if that envelope exceeds the offload threshold — replaced by a
// small blobptr envelope whose real bytes were first written to Blobs
// (blob-durable-before-pointer). It runs under mu (writeLocked holds it), so the
// upload is serialized with the append that references it.
func (b *sessionJournal) frame(ctx context.Context, rec journal.JournalRecord) ([]byte, error) {
	k, body, err := b.encodeRecordBody(rec)
	if err != nil {
		return nil, err
	}
	env, err := encodeEnvelope(envelope{V: envelopeVersion, Kind: string(k), ID: rec.IdempotencyID(), Body: body})
	if err != nil {
		return nil, err
	}
	if len(env) <= b.threshold {
		return env, nil
	}
	return b.offload(ctx, rec, env)
}

// offload writes an over-threshold frame's full bytes to Blobs under their
// content-addressed key BEFORE returning the small blobptr envelope that stands in
// for it in the ledger — so a pointer can never reference a blob that is not yet
// durable. On any Blobs failure it fails closed with a typed *journal.RecordTooLargeError
// rather than inlining an oversized record (which would breach the 1 MiB ledger floor).
func (b *sessionJournal) offload(ctx context.Context, rec journal.JournalRecord, env []byte) ([]byte, error) {
	sum := sha256.Sum256(env)
	shahex := hex.EncodeToString(sum[:])
	// Reuse the already-validated ledger name as the blob-key prefix so the key can
	// never diverge from the session's canonical name derivation.
	key := b.name + blobsInfix + shahex

	// Blob durable first: no dangling pointer.
	if err := b.blobs.Put(ctx, key, bytes.NewReader(env)); err != nil {
		return nil, &journal.RecordTooLargeError{
			Subject: b.name,
			MsgID:   rec.IdempotencyID(),
			Length:  len(env),
			Cause:   err,
		}
	}

	ptr, err := encodeBlobPointer(blobPointer{Key: key, Size: int64(len(env)), SHA256: shahex})
	if err != nil {
		return nil, err
	}
	return encodeEnvelope(envelope{V: envelopeVersion, Kind: string(kindBlobPtr), ID: rec.IdempotencyID(), Body: ptr})
}

// mapAppendErr translates a storage append failure into the journal's error
// vocabulary so callers keep classifying at the journal level: a definite CAS
// conflict (a stale writer fenced out) becomes *journal.AppendError; a still-
// ambiguous outcome becomes *journal.AmbiguousAckError; any other error is
// surfaced unchanged (fail closed). The tip is already left unadvanced by the caller.
func (b *sessionJournal) mapAppendErr(rec journal.JournalRecord, err error) error {
	var conflict *storage.ConflictError
	if errors.As(err, &conflict) {
		return &journal.AppendError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	var ambiguous *storage.AmbiguousError
	if errors.As(err, &ambiguous) {
		return &journal.AmbiguousAckError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	// AppendDefinite's verifyAppend could not read the contested tip to resolve a
	// conflict, so the append outcome is genuinely UNKNOWN — the same unresolved,
	// decide-fail-or-retry case as a lingering ambiguous ack. Map it to the journal's
	// AmbiguousAckError (carrying the verify error) rather than leaking storage's
	// type through the facade.
	var verify *storage.AppendVerifyError
	if errors.As(err, &verify) {
		return &journal.AmbiguousAckError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	return err
}

// encodeRecordBody encodes a record's payload via the codec for its concrete kind
// and names the envelope kind that carries it: an event via event.MarshalEvent, a
// command via command.MarshalCommand, a fence via journal.MarshalLeaseFence. The
// switch is over the sealed JournalRecord sum, so the default arm is unreachable
// for an in-package record; it fails closed with a typed *journal.RecordKindError
// rather than panicking if a foreign type ever satisfies the marker. It additionally
// returns the envelope kind (JournalRecord exposes no Kind(); the concrete type is the
// source of truth). Error context uses the ledger name (b.name), the record's
// backend-neutral destination.
func (b *sessionJournal) encodeRecordBody(rec journal.JournalRecord) (kind, []byte, error) {
	switch r := rec.(type) {
	case journal.EventRecord:
		if _, private := r.Event().(event.GatePrepared); private {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: errors.New("GatePrepared is private; append journal.GatePreparedRecord")}
		}
		body, err := event.MarshalEvent(r.Event())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindEvent, body, nil
	case journal.CommandRecord:
		body, err := command.MarshalCommand(r.Command())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindCommand, body, nil
	case journal.FenceRecord:
		body, err := journal.MarshalLeaseFence(r.Fence())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindFence, body, nil
	case journal.GatePreparedRecord:
		body, err := journal.MarshalGatePreparedRecord(r)
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindGatePrepared, body, nil
	default:
		return "", nil, &journal.RecordKindError{Subject: b.name}
	}
}
