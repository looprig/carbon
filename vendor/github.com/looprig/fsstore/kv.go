package fsstore

import (
	"context"
	"encoding/binary"
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

// This file implements storage.KV over the local filesystem: one file per key,
// laid out as <root>/kv/<key> where the key's '/'-separated segments become
// directory components. Each file is a small fixed rev header (the uint64 revision,
// little-endian) followed by the raw value bytes:
//
//	[rev uint64 LE][value ...]
//
// # Revision model
//
// The persisted rev header is the sole source of revision truth: it is read from
// disk on every Get and every Put's CAS check, so revisions survive a process
// restart with no in-memory cache to reconcile. Revisions are per-key, strictly
// increasing, and start at 1 on create; a Delete removes the file, so a re-created
// key restarts at 1.
//
// # Locking model
//
// A per-key in-process mutex (a registry keyed by key, mirroring the ledger's
// name->file registry) serializes each key's read-check-write so a Put's CAS is
// atomic within this process. There is NO cross-process advisory lock: the write
// is made durable and atomic by staging the new content in a sibling temp file,
// fsyncing it, and os.Rename-ing it over the target (rename is atomic on POSIX), so
// a concurrent reader always observes either the whole old file or the whole new
// one — never a partial write. Reads (Get, Keys) therefore take no lock: atomic
// rename is their consistency guarantee.
//
// ctx is accepted to satisfy the storage.KV contract but, like the memstore
// reference backend, local filesystem syscalls are not interrupted mid-call.

// revHeaderSize is the fixed on-disk width, in bytes, of a KV file's revision
// header: a single little-endian uint64.
const revHeaderSize = 8

// KVRootError reports that newKVStore was given an unusable root (for example, an
// empty path).
type KVRootError struct {
	Root   string
	Reason string
}

func (e *KVRootError) Error() string {
	return "fsstore: invalid kv root " + strconv.Quote(e.Root) + ": " + e.Reason
}

// KVPathError reports that a KV key mapped to a filesystem location outside the
// store's kv directory. It is defense in depth: ValidateName already forbids the
// '..' and empty segments that could escape, so a valid key never triggers it — a
// triggered KVPathError means an unvalidated key reached path mapping.
type KVPathError struct {
	Key  string
	Path string
}

func (e *KVPathError) Error() string {
	return "fsstore: kv key " + strconv.Quote(e.Key) + " maps outside the store root: " + strconv.Quote(e.Path)
}

// KVIOError wraps an underlying filesystem failure (mkdir, open, read, write,
// fsync, rename, remove, walk) with the operation and path for diagnosis. Cause is
// the os/syscall error and is exposed via Unwrap.
type KVIOError struct {
	Op    string
	Path  string
	Cause error
}

func (e *KVIOError) Error() string {
	return "fsstore: kv " + e.Op + " " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying filesystem error.
func (e *KVIOError) Unwrap() error { return e.Cause }

// errShortRevHeader is the leaf cause recorded in a KVCorruptError when a key file
// is present but too short to hold the fixed revision header.
var errShortRevHeader = errors.New("fsstore: kv file shorter than the rev header")

// KVCorruptError reports that a key's file could not be trusted: it is present but
// too short to hold the revision header. Both Get and Put fail closed rather than
// treat the file as absent (rev 0), which would let a torn file silently reset the
// revision counter. Cause is errShortRevHeader.
type KVCorruptError struct {
	Path  string
	Cause error
}

func (e *KVCorruptError) Error() string {
	return "fsstore: kv file " + strconv.Quote(e.Path) + " is corrupt"
}

// Unwrap exposes the underlying cause (errShortRevHeader).
func (e *KVCorruptError) Unwrap() error { return e.Cause }

// kvStore is the filesystem KV backend: revision-CAS'd metadata rooted at
// <root>/kv, one file per key. It tracks a per-key mutex for every key it has
// touched so read-check-write stays atomic in-process; the on-disk rev header is
// the authoritative revision.
type kvStore struct {
	root string // cleaned store root; <root>/kv holds the key files
	kv   string // cleaned <root>/kv
	mu   sync.Mutex
	keys map[string]*sync.Mutex
}

// Compile-time proof that *kvStore honors the KV contract.
var _ storage.KV = (*kvStore)(nil)

// newKVStore creates <root> and <root>/kv at 0700 and returns a KV backend rooted
// there. It rejects an empty root and wraps any mkdir failure in a typed error.
func newKVStore(root string) (*kvStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &KVRootError{Root: root, Reason: "empty root"}
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, &KVIOError{Op: "mkdir", Path: clean, Cause: err}
	}
	kv := filepath.Join(clean, "kv")
	if err := os.MkdirAll(kv, 0o700); err != nil {
		return nil, &KVIOError{Op: "mkdir", Path: kv, Cause: err}
	}
	return &kvStore{root: clean, kv: kv, keys: make(map[string]*sync.Mutex)}, nil
}

// pathFor maps a storage key to its file path under kv and verifies the result
// stays within kv (containment / defense in depth). The key's '/'-separated
// segments become path components. Containment is LEXICAL: it assumes an
// exclusively-owned root created at 0700 and does NOT resolve symlinks. Owning the
// store root is the deployment's responsibility (identical to the ledger backend).
func (s *kvStore) pathFor(key string) (string, error) {
	full := filepath.Clean(filepath.Join(s.kv, filepath.FromSlash(key)))
	// The trailing separator in the prefix rejects both an escaping path and the kv
	// root itself (an empty key), and defends against a sibling directory whose name
	// merely starts with "kv".
	if !strings.HasPrefix(full, s.kv+string(os.PathSeparator)) {
		return "", &KVPathError{Key: key, Path: full}
	}
	return full, nil
}

// lockFor returns the per-key mutex guarding key's read-check-write, creating the
// registry entry on first use. It is only used by the mutating paths (Put, Delete);
// reads rely on atomic rename instead.
func (s *kvStore) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.keys[key]
	if m == nil {
		m = &sync.Mutex{}
		s.keys[key] = m
	}
	return m
}

// Get returns a copy of the value at key plus its current revision. It validates
// the key first (*InvalidNameError); an absent key yields a *KeyNotFoundError; a
// present-but-truncated file fails closed with a *KVCorruptError. Get takes no
// mutex: an atomic rename means it observes either the whole old file or the whole
// new one, never a partial write. The returned slice is a fresh copy.
func (s *kvStore) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	if err := storage.ValidateName(key); err != nil {
		return nil, 0, err
	}
	path, perr := s.pathFor(key)
	if perr != nil {
		return nil, 0, perr
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is validated and contained under kv
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, &storage.KeyNotFoundError{Key: key}
	}
	if err != nil {
		return nil, 0, &KVIOError{Op: "read", Path: path, Cause: err}
	}
	if len(data) < revHeaderSize {
		return nil, 0, &KVCorruptError{Path: path, Cause: errShortRevHeader}
	}
	rev := binary.LittleEndian.Uint64(data[:revHeaderSize])
	out := make([]byte, len(data)-revHeaderSize)
	copy(out, data[revHeaderSize:])
	return out, rev, nil
}

// Put performs a revision compare-and-swap. It validates the key first
// (*InvalidNameError). Under the per-key mutex it reads the current revision from
// disk (0 when the key is absent); expectedRev must equal it — 0 for an absent key,
// so expectedRev 0 is create-only. On mismatch it returns a *ConflictError{Name:
// key, Expected: expectedRev} and leaves state untouched. On success it stages the
// new content (rev header at current+1, then val) in a sibling temp file, fsyncs
// it, atomically renames it over the target, fsyncs the parent directory, and
// returns the new revision.
func (s *kvStore) Put(ctx context.Context, key string, expectedRev uint64, val []byte) (uint64, error) {
	if err := storage.ValidateName(key); err != nil {
		return 0, err
	}
	path, perr := s.pathFor(key)
	if perr != nil {
		return 0, perr
	}

	m := s.lockFor(key)
	m.Lock()
	defer m.Unlock()

	cur, exists, rerr := readRev(path)
	if rerr != nil {
		return 0, rerr
	}
	if expectedRev != cur {
		return 0, &storage.ConflictError{Name: key, Expected: expectedRev}
	}
	newRev := cur + 1

	content := make([]byte, revHeaderSize+len(val))
	binary.LittleEndian.PutUint64(content[:revHeaderSize], newRev)
	copy(content[revHeaderSize:], val)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, &KVIOError{Op: "mkdir", Path: dir, Cause: err}
	}
	if err := writeFileAtomic(dir, path, content, kvIOErr); err != nil {
		return 0, err
	}
	// A brand-new key may have created intermediate directories (e.g. kv/sessions),
	// so on create fsync the ancestor chain up to kv; an overwrite only needs the
	// immediate parent (the target dirent) made durable.
	if !exists {
		if err := fsyncDirChain(dir, s.kv, kvIOErr); err != nil {
			return 0, err
		}
	} else if err := fsyncDir(dir, kvIOErr); err != nil {
		return 0, err
	}
	return newRev, nil
}

// Keys returns the keys whose string has prefix, lexicographically ascending and
// duplicate-free. It walks the kv tree, mapping each committed file's path back to
// its key and skipping in-progress temp files. An empty prefix returns all keys.
// Per the contract, Keys does NOT validate prefix — a partial-segment prefix is a
// substring filter, not a valid name. Keys takes no lock: a concurrent Put either
// has not yet renamed (its temp file is skipped) or has committed (the new file is
// listed), so the snapshot is always internally consistent.
func (s *kvStore) Keys(ctx context.Context, prefix string) ([]string, error) {
	out, err := walkKeys(s.kv, prefix)
	if err != nil {
		return nil, &KVIOError{Op: "walk", Path: s.kv, Cause: err}
	}
	return out, nil
}

// kvIOErr adapts *KVIOError to the constructor writeFileAtomic expects, so the
// shared atomic-write helper surfaces KV-typed IO errors.
func kvIOErr(op, path string, cause error) error {
	return &KVIOError{Op: op, Path: path, Cause: cause}
}

// Delete removes key's file under the per-key mutex; it validates the key first
// (*InvalidNameError) and is idempotent, so deleting an absent key succeeds. After
// Delete the key is absent (Get reports *KeyNotFoundError) and its revision counter
// is forgotten (a re-created key restarts at rev 1).
func (s *kvStore) Delete(ctx context.Context, key string) error {
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
		return &KVIOError{Op: "remove", Path: path, Cause: err}
	}
	return nil
}

// readRev reads the persisted revision header from the file at path, failing
// CLOSED on a present-but-truncated file. An absent file is the legitimate "no
// revision yet" state and yields (0, false, nil).
func readRev(path string) (rev uint64, exists bool, err error) {
	f, oerr := os.Open(path) // #nosec G304 -- path is validated and contained under kv
	if errors.Is(oerr, fs.ErrNotExist) {
		return 0, false, nil
	}
	if oerr != nil {
		return 0, false, &KVIOError{Op: "open", Path: path, Cause: oerr}
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = &KVIOError{Op: "close", Path: path, Cause: cerr}
		}
	}()

	var hdr [revHeaderSize]byte
	if _, rerr := io.ReadFull(f, hdr[:]); rerr != nil {
		// io.EOF (empty file) or io.ErrUnexpectedEOF (1..7 bytes): the file exists but
		// cannot hold a revision, so fail closed rather than report rev 0.
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			return 0, false, &KVCorruptError{Path: path, Cause: errShortRevHeader}
		}
		return 0, false, &KVIOError{Op: "read", Path: path, Cause: rerr}
	}
	return binary.LittleEndian.Uint64(hdr[:]), true, nil
}
