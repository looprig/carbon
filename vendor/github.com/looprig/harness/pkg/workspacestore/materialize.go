package workspacestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
)

// destPerm is the mode Materialize creates a missing destination directory with:
// owner-only, least privilege. The extracted entries carry and restore their own
// archived modes, so this is only the transient container permission.
const destPerm fs.FileMode = 0o700

// Materialize restores the snapshot named ref into dest. It has two paths chosen
// by the state of dest. Truth path — dest missing or an empty directory: fetch
// ref's compressed archive from Blobs and extract it through the trust boundary
// while re-verifying, over the whole compressed stream, that its sha256 equals
// ref; any fetch, extract, or digest mismatch fails closed as *MaterializeError
// with the partial output wiped. Verified-reuse path — dest a non-empty
// directory: a warm volume may already hold the tree, but it is never trusted and
// never wiped; dest is deterministically re-archived and its digest compared to
// ref. A match is a no-op resume (no fetch); a mismatch returns *DestNotEmptyError
// so the caller decides whether to clear and retry. A dest that exists but is not
// a directory is rejected as a wrapped *NotDirError.
func (s *Store) Materialize(ctx context.Context, ref Ref, dest string) error {
	reuse, err := prepareDest(dest)
	if err != nil {
		return &MaterializeError{Ref: ref, Dest: dest, Cause: err}
	}
	if reuse {
		return verifyReuse(ref, dest)
	}
	return s.fetchAndExtract(ctx, ref, dest)
}

// Delete removes ref's snapshot archive from Blobs. It is idempotent: deleting a
// ref whose blob is already absent succeeds, mirroring the storage Blobs
// contract, so a resume that has already discarded its checkpoint is not an error.
func (s *Store) Delete(ctx context.Context, ref Ref) error {
	return s.blobs.Delete(ctx, ref.blobKey())
}

// prepareDest classifies dest for Materialize and readies the truth path. A
// missing dest is created as an empty directory (least-privilege destPerm) and
// reported as not-reuse. An existing non-empty directory selects the
// verified-reuse path (reuse=true). An existing empty directory is not-reuse. A
// dest that exists but is not a directory is rejected with *NotDirError.
func prepareDest(dest string) (bool, error) {
	info, err := os.Stat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return false, os.MkdirAll(dest, destPerm)
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, &NotDirError{Path: dest}
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// fetchAndExtract restores ref's archive into the empty dest on the truth path.
// It tees every fetched byte through sha256 while extractArchive consumes the
// gzip-tar stream, then verifies the whole stream's digest against ref. Any
// failure surfaces as *MaterializeError with the destination left empty: a
// mid-extraction fault is wiped by extractArchive, a post-extraction integrity
// failure is wiped here.
func (s *Store) fetchAndExtract(ctx context.Context, ref Ref, dest string) error {
	rc, err := s.blobs.Get(ctx, ref.blobKey())
	if err != nil {
		return &MaterializeError{Ref: ref, Dest: dest, Cause: err}
	}
	defer func() { _ = rc.Close() }()

	h := sha256.New()
	tee := io.TeeReader(rc, h)
	lim := limits{maxEntries: s.opts.MaxEntries, maxBytes: s.opts.MaxBytes}
	if err := extractArchive(ctx, tee, dest, lim); err != nil {
		return &MaterializeError{Ref: ref, Dest: dest, Cause: err} // extractArchive already wiped dest
	}
	if err := verifyFetchedDigest(tee, h, ref); err != nil {
		wipeDest(dest)
		return &MaterializeError{Ref: ref, Dest: dest, Cause: err}
	}
	return nil
}

// verifyFetchedDigest confirms the fetched archive's sha256 equals ref. It first
// drains any compressed bytes the tar reader left unconsumed: tar.Reader stops at
// the archive's end-of-archive marker and need not read the trailing gzip footer,
// so without this drain the hasher would miss the tail and every verification
// would spuriously fail. A drain fault or a digest mismatch is returned so the
// caller can wipe and fail closed; a mismatch is a typed *IntegrityError.
func verifyFetchedDigest(tee io.Reader, h hash.Hash, ref Ref) error {
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != ref.hex() {
		return &IntegrityError{Ref: ref, Got: got}
	}
	return nil
}

// verifyReuse handles a non-empty dest without ever trusting or wiping it. It
// deterministically re-archives dest into a sha256 hasher — no bytes spooled —
// and compares the digest to ref. A match proves the warm volume holds exactly
// ref's tree, so Materialize is a no-op resume with no fetch. A mismatch is drift:
// *DestNotEmptyError carries the observed digest and leaves the clear-and-retry
// decision to the caller. A re-archive fault (e.g. an unreadable node) surfaces as
// *MaterializeError.
func verifyReuse(ref Ref, dest string) error {
	h := sha256.New()
	if err := writeArchive(h, dest); err != nil {
		return &MaterializeError{Ref: ref, Dest: dest, Cause: err}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got == ref.hex() {
		return nil
	}
	return &DestNotEmptyError{Dest: dest, Want: ref, GotDigest: got}
}

// wipeDest best-effort empties dest's contents (leaving dest itself) after a
// post-extraction integrity failure, where extractArchive saw no error and so did
// not clean up. It reuses the extract-side removeContents through an *os.Root
// sandbox so a symlink planted by the tampered archive is unlinked, never
// traversed. Cleanup is best-effort; the integrity error is the meaningful one.
func wipeDest(dest string) {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return
	}
	removeContents(root)
	_ = root.Close()
}
