package workspacestore

import (
	"context"
	"strings"
)

// GC deletes every stored snapshot blob whose Ref is not in `live`, returning the
// Refs it deleted. It is the mark-and-sweep the composition root runs after computing
// the live set (the Refs reachable from any live session's WorkspaceCheckpointed events).
//
// v1 is LIVE-SET-ONLY: it has no age/last-seen safety net, so it MUST NOT run
// concurrently with active snapshotting — a Snapshot writing a not-yet-live Ref (e.g.
// a brand-new checkpoint, or a re-Put of an identical tree) could be deleted mid-flight.
// Age-based safety would need a Stat/last-modified surface on storage.Blobs (future work).
//
// GC is FAIL-SECURE: a key under the workspaces/ prefix that does not parse as a valid v1
// Ref is skipped, never deleted — GC only ever removes blobs it recognizes as its own
// snapshots, so a foreign object sharing the prefix is left untouched rather than treated
// as unreferenced. It checks ctx before listing and before each delete: an already-cancelled
// ctx returns (nil, ctx.Err()) and deletes nothing; a mid-sweep cancellation returns the
// Refs deleted so far and the ctx error. A backend List or Delete failure returns the Refs
// deleted so far wrapped in *GCError.
func (s *Store) GC(ctx context.Context, live map[Ref]struct{}) (deleted []Ref, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keys, err := s.blobs.List(ctx, blobKeyPrefix)
	if err != nil {
		return nil, &GCError{Op: gcOpList, Cause: err}
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		ref, ok := refFromBlobKey(key)
		if !ok {
			continue // fail-secure: an unrecognized key is never deleted
		}
		if _, isLive := live[ref]; isLive {
			continue
		}
		if err := s.blobs.Delete(ctx, ref.blobKey()); err != nil {
			return deleted, &GCError{Op: gcOpDelete, Ref: ref, Cause: err}
		}
		deleted = append(deleted, ref)
	}
	return deleted, nil
}

// refFromBlobKey reconstructs the Ref a snapshot blob key names by stripping
// blobKeyPrefix, prepending refPrefix, and validating the result through ParseRef.
// A key lacking the prefix, or whose digest is not a grammar-valid v1 Ref, yields
// ok == false — the caller treats such a key as foreign and leaves it in place,
// never as an unreferenced snapshot to delete.
func refFromBlobKey(key string) (Ref, bool) {
	digest, ok := strings.CutPrefix(key, blobKeyPrefix)
	if !ok {
		return "", false
	}
	ref, err := ParseRef(refPrefix + digest)
	if err != nil {
		return "", false
	}
	return ref, true
}
