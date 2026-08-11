package sessionstore

import (
	"context"
	"errors"
	"io"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
)

// GCLeaseNotHeldError reports that GC was refused because the session's single-writer
// lease is not held (released, or overtaken by a higher epoch). GC deletes blobs, so
// it must run only as the single writer; running unguarded could reap a blob a live
// owner is still offloading (its pointer append is mid-flight). It fails closed with
// this typed error and deletes nothing. It carries the session and the (stale) epoch
// the refused lease held, and unwraps to a *journal.LeaseLostError for errors.As —
// mirroring pkg/journal's ObjectGC.
type GCLeaseNotHeldError struct {
	SessionID uuid.UUID
	Epoch     uint64
}

func (e *GCLeaseNotHeldError) Error() string {
	return "sessionstore: GC refused for session " + e.SessionID.String() +
		": lease at epoch " + strconv.FormatUint(e.Epoch, 10) + " not held"
}

func (e *GCLeaseNotHeldError) Unwrap() error {
	return &journal.LeaseLostError{SessionID: e.SessionID, Epoch: e.Epoch}
}

// GCScanError reports a failure to scan the session's ledger for the set of blob keys
// referenced by a live pointer: a ledger read/cursor failure, or an undecodable
// envelope or blob pointer. GC fails closed — without a COMPLETE live set it cannot
// safely decide which blobs are orphans, so it deletes nothing rather than risk
// reaping a still-referenced blob. It carries the ledger name and unwraps to the
// underlying cause.
type GCScanError struct {
	Name  string
	Cause error
}

func (e *GCScanError) Error() string {
	return "sessionstore: GC scan of ledger " + strconv.Quote(e.Name) + ": " + e.Cause.Error()
}
func (e *GCScanError) Unwrap() error { return e.Cause }

// GCListError reports a failure to list the session's blob prefix. GC fails closed:
// without the blob inventory it cannot decide what to reap, so it deletes nothing. It
// carries the prefix and unwraps to the underlying cause.
type GCListError struct {
	Prefix string
	Cause  error
}

func (e *GCListError) Error() string {
	return "sessionstore: GC list of blobs " + strconv.Quote(e.Prefix) + ": " + e.Cause.Error()
}
func (e *GCListError) Unwrap() error { return e.Cause }

// GCDeleteError reports a failure to delete one orphaned blob. GC surfaces it rather
// than silently swallowing the failure, so a caller learns the session's blobs could
// not be fully reclaimed. It carries the blob key and unwraps to the underlying cause.
type GCDeleteError struct {
	Key   string
	Cause error
}

func (e *GCDeleteError) Error() string {
	return "sessionstore: GC delete of blob " + strconv.Quote(e.Key) + ": " + e.Cause.Error()
}
func (e *GCDeleteError) Unwrap() error { return e.Cause }

// GCResult summarizes one GC pass. It mirrors pkg/journal's GCResult shape, minus its
// WithinGrace term: storage's Blobs.List exposes no per-blob timestamp, so there is
// no grace window over the storage contract — GC's safety rests entirely on the
// single-writer lease/idle serialization the caller provides (see ObjectGC). On a
// fully successful pass Scanned == Referenced + Deleted.
type GCResult struct {
	// Scanned is the number of blobs listed under the session's blob prefix.
	Scanned int
	// Referenced is the number of listed blobs still referenced by an in-ledger
	// pointer (kept).
	Referenced int
	// Deleted is the number of orphaned blobs reaped this pass; it always equals
	// len(DeletedKeys).
	Deleted int
	// DeletedKeys enumerates the reaped blob keys in lexicographic order (Blobs.List
	// returns sorted keys and the sweep preserves that order). It lets a caller log
	// exactly what was reclaimed without re-deriving it.
	DeletedKeys []string
}

// WorkspaceJournalScanError reports that a retained session journal could not be scanned
// completely for workspace references. Manual workspace GC must fail closed on this error:
// an incomplete live set is unsafe to collect against.
type WorkspaceJournalScanError struct {
	SessionID uuid.UUID
	Cause     error
}

func (e *WorkspaceJournalScanError) Error() string {
	return "sessionstore: scan workspace refs for session " + e.SessionID.String() + ": " + e.Cause.Error()
}

func (e *WorkspaceJournalScanError) Unwrap() error { return e.Cause }

// WorkspaceLiveRefs computes the complete workspace-ref set from the retained
// session IDs supplied by the operator. It scans every journal and retains history from
// both checkpoint and restore transitions. This only discovers refs; collection remains
// an explicit operator action serialized against all snapshot writers.
func (s *Store) WorkspaceLiveRefs(ctx context.Context, retainedSessionIDs []uuid.UUID) (map[workspacestore.Ref]struct{}, error) {
	live := make(map[workspacestore.Ref]struct{})
	for _, id := range retainedSessionIDs {
		err := s.scanWorkspaceEvents(ctx, id, func(_ uint64, ev event.Event) (bool, error) {
			var raw string
			switch e := ev.(type) {
			case event.WorkspaceCheckpointed:
				raw = e.Ref
			case event.WorkspaceRestored:
				raw = e.Ref
			default:
				return true, nil
			}
			ref, err := workspacestore.ParseRef(raw)
			if err != nil {
				return false, err
			}
			live[ref] = struct{}{}
			return true, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return live, nil
}

// WorkspaceCheckpointBySeq finds the checkpoint whose durable journal sequence is seq.
func (s *Store) WorkspaceCheckpointBySeq(ctx context.Context, id uuid.UUID, seq uint64) (CheckpointSummary, bool, error) {
	var found CheckpointSummary
	err := s.scanWorkspaceEvents(ctx, id, func(journalSeq uint64, ev event.Event) (bool, error) {
		if journalSeq != seq {
			return true, nil
		}
		checkpoint, ok := ev.(event.WorkspaceCheckpointed)
		if ok {
			found = checkpointSummary(checkpoint, journalSeq)
		}
		return false, nil
	})
	return found, !found.EventID.IsZero(), err
}

// WorkspaceCheckpointByTurn finds the latest turn-triggered checkpoint caused by turnID.
func (s *Store) WorkspaceCheckpointByTurn(ctx context.Context, id, turnID uuid.UUID) (CheckpointSummary, bool, error) {
	var found CheckpointSummary
	err := s.scanWorkspaceEvents(ctx, id, func(seq uint64, ev event.Event) (bool, error) {
		checkpoint, ok := ev.(event.WorkspaceCheckpointed)
		if ok && checkpoint.Trigger == event.SnapshotTriggerTurnDone && checkpoint.Cause.Coordinates.TurnID == turnID {
			found = checkpointSummary(checkpoint, seq)
		}
		return true, nil
	})
	return found, !found.EventID.IsZero(), err
}

func (s *Store) scanWorkspaceEvents(ctx context.Context, id uuid.UUID, visit func(uint64, event.Event) (bool, error)) error {
	replayer, err := s.OpenEventReplayer(id, ReplayRequest{FromSeq: 0})
	if err != nil {
		return &WorkspaceJournalScanError{SessionID: id, Cause: err}
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		return &WorkspaceJournalScanError{SessionID: id, Cause: err}
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return &WorkspaceJournalScanError{SessionID: id, Cause: err}
		}
		keepGoing, visitErr := visit(seq, ev)
		if visitErr != nil {
			return &WorkspaceJournalScanError{SessionID: id, Cause: visitErr}
		}
		if !keepGoing {
			return nil
		}
	}
}

// ObjectGC reaps orphaned offload blobs from one session's content-addressed blob
// prefix: blobs no in-ledger pointer references. An orphan arises from the writer's
// blob-durable-before-pointer discipline — the offload Put lands BEFORE the blobptr
// append, so a crash in that gap leaves a durable blob with no pointer.
//
// It is lease-guarded: it deletes, so it runs only while holding a valid single-writer
// lease and is therefore the single deleter. That lease guard is also the whole of its
// concurrency safety. GC MUST NOT run concurrently with active appends/offloads to the
// same session: a blob whose pointer append is still in flight would be observed as
// unreferenced by the scan and wrongly reaped. The caller serializes GC with the writer
// — typically running it while holding the session lease (as the single writer) or when
// the session is idle. Unlike pkg/journal's ObjectGC there is no grace window backstop:
// storage's Blobs.List surfaces no ModTime, so an in-flight upload cannot be protected
// by age; the serialization is load-bearing, not advisory.
//
// It is the GC analogue of the sessionstore journal (write) and replayers (read), wired
// at the composition root via Store.OpenObjectGC.
type ObjectGC struct {
	id     uuid.UUID      // the session this GC reaps (for the lease guard + error context)
	lease  journal.Lease  // single-writer ownership token (injected; never acquired or released here)
	ledger storage.Ledger // the append-only record log scanned for live pointers
	blobs  storage.Blobs  // the content-addressed blob store swept for orphans
	name   string         // the bound ledger name (ledgerName(id)); also the blob-key prefix root
}

// OpenObjectGC binds an offload-blob GC to session id under the given single-writer
// lease (DIP: the composition root acquires the lease and passes it in; GC never
// acquires or releases one, and depends only on the narrow journal.Lease view). A nil
// lease fails closed with *NilLeaseError. The ledger and blob store come from the
// validated Store backend, so they are guaranteed non-nil here.
func (s *Store) OpenObjectGC(id uuid.UUID, lease journal.Lease) (*ObjectGC, error) {
	if lease == nil {
		return nil, &NilLeaseError{SessionID: id}
	}
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	return &ObjectGC{
		id:     id,
		lease:  lease,
		ledger: s.backend.Ledger,
		blobs:  s.backend.Blobs,
		name:   name,
	}, nil
}

// GC runs one live-set-sweep pass under the held lease. It (1) refuses unless the lease
// is held — GC deletes, so it must be the single writer; (2) scans the session's ledger
// and builds the LIVE set of blob keys referenced by a blobptr record; (3) lists the
// session's blob prefix and deletes every listed blob NOT in the live set. It returns a
// summary of the pass. Every failure is a typed fail-closed error; on a scan or list
// failure it deletes nothing (an incomplete live set must never drive a delete).
func (g *ObjectGC) GC(ctx context.Context) (GCResult, error) {
	// Lease guard: GC is the single deleter. Refuse if the lease is not held.
	if !g.leaseHeld() {
		return GCResult{}, &GCLeaseNotHeldError{SessionID: g.id, Epoch: g.lease.Epoch()}
	}

	live, err := g.collectLive(ctx)
	if err != nil {
		return GCResult{}, err
	}

	keys, err := g.listBlobs(ctx)
	if err != nil {
		return GCResult{}, err
	}

	return g.sweep(ctx, live, keys)
}

// leaseHeld reports whether the ownership lease is still held: both its validity flag
// and its loss channel must say so. It mirrors the write-side guard; combined with the
// caller's serialization it is GC's entire safety story.
func (g *ObjectGC) leaseHeld() bool {
	if !g.lease.Valid() {
		return false
	}
	select {
	case <-g.lease.Lost():
		return false
	default:
		return true
	}
}

// collectLive scans the whole session ledger from the first record and builds the set
// of blob keys referenced by a blobptr record (decode the envelope; for a blobptr,
// decode its pointer and record ptr.Key). A non-blobptr record references no blob and
// is skipped. It fails closed with a *GCScanError on ANY read or decode failure: an
// incomplete live set could classify a still-referenced blob as an orphan and reap it.
func (g *ObjectGC) collectLive(ctx context.Context) (map[string]struct{}, error) {
	cur, err := g.ledger.Read(ctx, g.name, 1)
	if err != nil {
		return nil, &GCScanError{Name: g.name, Cause: err}
	}
	defer cur.Close()

	live := make(map[string]struct{})
	for {
		rec, err := cur.Next(ctx)
		if errors.Is(err, io.EOF) {
			return live, nil
		}
		if err != nil {
			return nil, &GCScanError{Name: g.name, Cause: err}
		}
		env, err := decodeEnvelope(rec.Payload)
		if err != nil {
			return nil, &GCScanError{Name: g.name, Cause: err}
		}
		if kind(env.Kind) != kindBlobPtr {
			continue
		}
		ptr, err := decodeBlobPointer(env.Body)
		if err != nil {
			return nil, &GCScanError{Name: g.name, Cause: err}
		}
		live[ptr.Key] = struct{}{}
	}
}

// listBlobs enumerates the session's content-addressed blob prefix
// ("sessions/<id>/blobs/"). storage's Blobs.List treats an empty prefix as zero
// blobs (an empty result, never an error), so an empty session needs no special case;
// any other failure fails closed as a *GCListError.
func (g *ObjectGC) listBlobs(ctx context.Context) ([]string, error) {
	prefix := g.name + blobsInfix
	keys, err := g.blobs.List(ctx, prefix)
	if err != nil {
		return nil, &GCListError{Prefix: prefix, Cause: err}
	}
	return keys, nil
}

// sweep deletes each listed blob that is NOT in the live set, re-checking the lease
// before each delete so a loss mid-pass stops further deletes at once (fail secure). A
// delete failure fails closed as a *GCDeleteError. Because Blobs.List returns keys in
// lexicographic order, DeletedKeys is likewise ordered.
func (g *ObjectGC) sweep(ctx context.Context, live map[string]struct{}, keys []string) (GCResult, error) {
	res := GCResult{Scanned: len(keys)}
	for _, k := range keys {
		if _, ok := live[k]; ok {
			res.Referenced++
			continue
		}
		// Re-guard the lease before each delete: a loss mid-pass must stop deleting at
		// once rather than finish reaping as a no-longer-single writer.
		if !g.leaseHeld() {
			return res, &GCLeaseNotHeldError{SessionID: g.id, Epoch: g.lease.Epoch()}
		}
		if err := g.blobs.Delete(ctx, k); err != nil {
			return res, &GCDeleteError{Key: k, Cause: err}
		}
		res.Deleted++
		res.DeletedKeys = append(res.DeletedKeys, k)
	}
	return res, nil
}
