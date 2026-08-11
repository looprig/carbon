package workspacestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
)

// Snapshot archives the tree rooted at root into a spooled temp file while teeing
// every byte through sha256, derives the content-addressed Ref from that digest,
// and uploads the archive to Blobs under the Ref's blob key — but only if that key
// is still absent, so an unchanged tree re-snapshots into a no-op upload. Spooling
// to disk means the working set never has to fit in memory, and because the digest
// is complete before any byte is sent, the key is final before the upload begins.
// Every failure mode — an unusable root, a walk/archive error, or a Blobs error —
// surfaces as *SnapshotError with the underlying cause reachable via errors.As.
func (s *Store) Snapshot(ctx context.Context, root string) (Ref, error) {
	if err := validateRoot(root); err != nil {
		return "", &SnapshotError{Root: root, Cause: err}
	}

	tmp, err := os.CreateTemp(s.opts.SpoolDir, "ws-snap-*")
	if err != nil {
		return "", &SnapshotError{Root: root, Cause: err}
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	ref, err := archiveAndHash(tmp, root)
	if err != nil {
		return "", &SnapshotError{Root: root, Cause: err}
	}

	if err := s.uploadIfAbsent(ctx, ref, tmp); err != nil {
		return "", &SnapshotError{Root: root, Cause: err}
	}
	return ref, nil
}

// validateRoot confirms root is an existing directory usable as an archive root.
// A nonexistent or otherwise unstat-able path yields the underlying os error; a
// path that exists but is not a directory yields *NotDirError. os.Stat follows a
// final symlink, so a symlink to a directory is accepted as one.
func validateRoot(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &NotDirError{Path: root}
	}
	return nil
}

// archiveAndHash streams the deterministic archive of the tree at root into tmp
// while teeing every byte through sha256, then derives and grammar-checks the
// content-addressed Ref from the resulting digest. On success tmp holds the full
// archive positioned at end; rewinding it before upload is the caller's job. The
// Ref is routed through ParseRef so a malformed digest could never escape as a
// Ref, though a 64-char lowercase hex digest always satisfies the grammar.
func archiveAndHash(tmp io.Writer, root string) (Ref, error) {
	hasher := sha256.New()
	if err := writeArchive(io.MultiWriter(tmp, hasher), root); err != nil {
		return "", err
	}
	return ParseRef(refPrefix + hex.EncodeToString(hasher.Sum(nil)))
}

// uploadIfAbsent uploads the spooled archive to Blobs under ref's blob key unless
// that key already holds content — the content-addressed dedup that turns an
// unchanged re-snapshot into zero uploads. Before uploading it rewinds tmp to the
// start so Put streams the whole archive from disk, never buffering it in memory.
// Both Blobs calls are bounded by ctx.
func (s *Store) uploadIfAbsent(ctx context.Context, ref Ref, tmp io.ReadSeeker) error {
	present, err := s.blobPresent(ctx, ref)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return s.blobs.Put(ctx, ref.blobKey(), tmp)
}

// blobPresent reports whether ref's exact blob key already exists in Blobs. It
// lists with the full key as the prefix (a full key is trivially its own prefix)
// and tests for an exact-string member, so a longer key that merely shares this
// prefix is never mistaken for a hit.
func (s *Store) blobPresent(ctx context.Context, ref Ref) (bool, error) {
	key := ref.blobKey()
	keys, err := s.blobs.List(ctx, key)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if k == key {
			return true, nil
		}
	}
	return false, nil
}

// NotDirError reports that a path required to be a directory is not one — a
// snapshot root that resolved to a regular file, a socket, or another
// non-directory node. Path is the offending path and is safe to log.
type NotDirError struct {
	Path string
}

func (e *NotDirError) Error() string {
	return "workspacestore: not a directory: " + strconv.Quote(e.Path)
}
