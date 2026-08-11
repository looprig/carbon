package fsstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/looprig/storage"
)

// This file implements storage.Blobs over the local filesystem: one file per key,
// laid out as <root>/blobs/<key> where the key's '/'-separated segments become
// directory components. A blob file holds the raw bytes verbatim — no header — so
// Get streams the object directly.
//
// # Content-addressed immutability
//
// A key's bytes are immutable once written. Put reads the reader fully, then under
// a per-key mutex inspects the target: if it is absent the bytes are staged in a
// sibling temp file, fsynced, and atomically renamed into place (ancestor dirs
// fsynced); if it already holds byte-identical content the Put is a no-op success
// (the file is NOT rewritten); if it holds different content Put fails with a
// *BlobConflictError and leaves the original object untouched. The per-key mutex
// makes that check-then-write atomic in-process, so two racing Puts of the same key
// resolve to one write plus a no-op-or-conflict rather than a lost update.
//
// Get opens a fresh fd per call and returns the *os.File as an independent
// io.ReadCloser, so concurrent readers never share state; it takes no lock (an
// absent key is reported, a present one opened — the atomic-rename write path means
// a reader never observes a partial object).
//
// ctx is accepted to satisfy the storage.Blobs contract but, like the memstore
// reference backend, local filesystem syscalls are not interrupted mid-call.

// BlobRootError reports that newBlobStore was given an unusable root (for example,
// an empty path).
type BlobRootError struct {
	Root   string
	Reason string
}

func (e *BlobRootError) Error() string {
	return "fsstore: invalid blob root " + strconv.Quote(e.Root) + ": " + e.Reason
}

// BlobPathError reports that a blob key mapped to a filesystem location outside the
// store's blobs directory. It is defense in depth: ValidateName already forbids the
// '..' and empty segments that could escape, so a valid key never triggers it — a
// triggered BlobPathError means an unvalidated key reached path mapping.
type BlobPathError struct {
	Key  string
	Path string
}

func (e *BlobPathError) Error() string {
	return "fsstore: blob key " + strconv.Quote(e.Key) + " maps outside the store root: " + strconv.Quote(e.Path)
}

// BlobIOError wraps an underlying filesystem failure (mkdir, open, read, write,
// fsync, rename, remove, walk) with the operation and path for diagnosis. Cause is
// the os/syscall error and is exposed via Unwrap.
type BlobIOError struct {
	Op    string
	Path  string
	Cause error
}

func (e *BlobIOError) Error() string {
	return "fsstore: blob " + e.Op + " " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying filesystem error.
func (e *BlobIOError) Unwrap() error { return e.Cause }

// blobStore is the filesystem Blobs backend: content-addressed immutable byte
// objects rooted at <root>/blobs, one file per key. It tracks a per-key mutex for
// every key it has touched so a Put's existence-check + write stays atomic
// in-process.
type blobStore struct {
	root  string // cleaned store root; <root>/blobs holds the blob files
	blobs string // cleaned <root>/blobs
	mu    sync.Mutex
	keys  map[string]*sync.Mutex
}

// Compile-time proof that *blobStore honors the Blobs contract.
var _ storage.Blobs = (*blobStore)(nil)

// newBlobStore creates <root> and <root>/blobs at 0700 and returns a Blobs backend
// rooted there. It rejects an empty root and wraps any mkdir failure in a typed
// error.
func newBlobStore(root string) (*blobStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &BlobRootError{Root: root, Reason: "empty root"}
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, &BlobIOError{Op: "mkdir", Path: clean, Cause: err}
	}
	blobs := filepath.Join(clean, "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return nil, &BlobIOError{Op: "mkdir", Path: blobs, Cause: err}
	}
	return &blobStore{root: clean, blobs: blobs, keys: make(map[string]*sync.Mutex)}, nil
}

// pathFor maps a storage key to its file path under blobs and verifies the result
// stays within blobs (containment / defense in depth). The key's '/'-separated
// segments become path components. Containment is LEXICAL: it assumes an
// exclusively-owned root created at 0700 and does NOT resolve symlinks. Owning the
// store root is the deployment's responsibility (identical to the ledger backend).
func (s *blobStore) pathFor(key string) (string, error) {
	full := filepath.Clean(filepath.Join(s.blobs, filepath.FromSlash(key)))
	// The trailing separator in the prefix rejects both an escaping path and the
	// blobs root itself (an empty key), and defends against a sibling directory whose
	// name merely starts with "blobs".
	if !strings.HasPrefix(full, s.blobs+string(os.PathSeparator)) {
		return "", &BlobPathError{Key: key, Path: full}
	}
	return full, nil
}

// lockFor returns the per-key mutex guarding key's check-then-write, creating the
// registry entry on first use. It is used by the mutating paths (Put, Delete);
// Get and List rely on atomic rename instead.
func (s *blobStore) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.keys[key]
	if m == nil {
		m = &sync.Mutex{}
		s.keys[key] = m
	}
	return m
}

// Put reads r to completion and stores the bytes at key. It validates the key first
// (*InvalidNameError) and propagates a reader error from io.ReadAll verbatim. Under
// the per-key mutex: an absent key is written via a fsynced temp file atomically
// renamed into place (ancestor dirs fsynced); a key already holding byte-identical
// content is a no-op success; a key holding different content yields a
// *BlobConflictError with the original object unchanged.
func (s *blobStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	path, perr := s.pathFor(key)
	if perr != nil {
		return perr
	}
	data, rerr := io.ReadAll(r)
	if rerr != nil {
		return rerr // propagate the caller's reader failure verbatim (memstore precedent)
	}

	m := s.lockFor(key)
	m.Lock()
	defer m.Unlock()

	existing, err := os.ReadFile(path) // #nosec G304 -- path is validated and contained under blobs
	switch {
	case err == nil:
		if bytes.Equal(existing, data) {
			return nil // byte-identical: content-addressed no-op, file left as-is
		}
		return &storage.BlobConflictError{Key: key} // different: original unchanged
	case errors.Is(err, fs.ErrNotExist):
		// Absent: fall through to the write path.
	default:
		return &BlobIOError{Op: "read", Path: path, Cause: err}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &BlobIOError{Op: "mkdir", Path: dir, Cause: err}
	}
	if err := writeFileAtomic(dir, path, data, blobIOErr); err != nil {
		return err
	}
	// A new blob may have created intermediate directories (e.g. blobs/snaps), so
	// fsync the ancestor chain up to blobs to make the new dirents crash-durable.
	if err := fsyncDirChain(dir, s.blobs, blobIOErr); err != nil {
		return err
	}
	return nil
}

// Get opens key and returns its bytes as an independent io.ReadCloser. It validates
// the key first (*InvalidNameError); an absent key yields a *BlobNotFoundError. Each
// Get opens a FRESH fd, so concurrent readers never share a cursor and closing one
// does not affect another. Get takes no lock: the atomic-rename write path means an
// opened file is always a whole committed object.
func (s *blobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storage.ValidateName(key); err != nil {
		return nil, err
	}
	path, perr := s.pathFor(key)
	if perr != nil {
		return nil, perr
	}
	f, err := os.Open(path) // #nosec G304 -- path is validated and contained under blobs
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &storage.BlobNotFoundError{Key: key}
	}
	if err != nil {
		return nil, &BlobIOError{Op: "open", Path: path, Cause: err}
	}
	return f, nil
}

// Delete removes key's file under the per-key mutex; it validates the key first
// (*InvalidNameError) and is idempotent, so deleting an absent key succeeds. After
// Delete the key is free: a fresh Put of any content stores it with no lingering
// conflict against the removed bytes.
func (s *blobStore) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	path, perr := s.pathFor(key)
	if perr != nil {
		return perr
	}

	m := s.lockFor(key)
	m.Lock()
	defer m.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &BlobIOError{Op: "remove", Path: path, Cause: err}
	}
	return nil
}

// List returns the keys whose string has prefix, lexicographically ascending and
// duplicate-free, by walking the blobs tree and mapping each committed file's path
// back to a key (in-progress temp files are skipped). An empty prefix returns all
// keys. Per the contract, List does NOT validate prefix — a partial-segment prefix
// is a substring filter, not a valid name. List takes no lock.
func (s *blobStore) List(ctx context.Context, prefix string) ([]string, error) {
	out, err := walkKeys(s.blobs, prefix)
	if err != nil {
		return nil, &BlobIOError{Op: "walk", Path: s.blobs, Cause: err}
	}
	return out, nil
}

// blobIOErr adapts *BlobIOError to the constructor writeFileAtomic expects, so the
// shared atomic-write helper surfaces Blobs-typed IO errors.
func blobIOErr(op, path string, cause error) error {
	return &BlobIOError{Op: op, Path: path, Cause: cause}
}
