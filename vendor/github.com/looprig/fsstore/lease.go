package fsstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/looprig/storage"
)

// This file implements storage.Leaser over the local filesystem: one lock file
// per named lease, laid out as <root>/leases/<name>.lock where the name's
// '/'-separated segments become directory components.
//
// # Exclusion model
//
// Acquire opens a FRESH fd to the lock file and takes a NON-BLOCKING exclusive
// advisory lock (flock LOCK_EX|LOCK_NB) on it, held open for the whole life of the
// grant and released on Release. Because an advisory lock binds to the open file
// description, a second Acquire — even in the same process — opens its own fd whose
// non-blocking lock conflicts, so a live holder makes the second Acquire fail with
// *LeaseHeldError. The OS releases the lock when the holding fd is closed OR when
// the process exits/crashes, so a dead holder's lease is reclaimed automatically
// with no stale-lock cleanup. That reclaim is the backend's native liveness
// mechanism and its scope is PER-HOST: it fences holders on this machine (and any
// sharing the underlying filesystem with working advisory locks), not across a
// network partition.
//
// # Epoch persistence
//
// The lock file's contents ARE the persisted epoch counter: an ASCII decimal
// followed by '\n' (e.g. "7\n"). An empty or absent file means "no lease has ever
// been granted" (epoch 0). On a successful acquire the holder reads the counter
// under the lock, increments it, writes it back and fsyncs the file, so epochs are
// strictly increasing across grants of the same name AND survive a graceful process
// restart (the fsync makes the new value durable; a fresh store re-reads it). The
// counter is only ever advanced under the exclusive lock, so a lock-free advisory
// read — used to fill LeaseHeldError.HolderEpoch — observes a committed value; a
// torn or unparseable read there fails soft to 0 (the epoch is informational, the
// refusal itself is authoritative).
//
// ctx is accepted to satisfy the storage.Leaser/Lease contract but local
// filesystem syscalls are not interrupted mid-call.

// leaseExt is the on-disk suffix appended to every lock file's mapped path.
const leaseExt = ".lock"

// maxEpochBytes bounds the advisory read of the epoch counter. A uint64 is at most
// 20 decimal digits plus a trailing '\n'; 32 bytes leaves generous headroom while
// keeping the read tiny.
const maxEpochBytes = 32

// errBadEpoch is the leaf cause recorded in a LeaseCorruptError when a lock file's
// persisted counter is present but not a valid decimal uint64.
var errBadEpoch = errors.New("fsstore: lease epoch counter is not a valid decimal")

// LeaseRootError reports that newLeaserStore was given an unusable root (for
// example, an empty path).
type LeaseRootError struct {
	Root   string
	Reason string
}

func (e *LeaseRootError) Error() string {
	return "fsstore: invalid lease root " + strconv.Quote(e.Root) + ": " + e.Reason
}

// LeasePathError reports that a lease Name mapped to a filesystem location outside
// the store's leases directory. It is defense in depth: ValidateName already
// forbids the '..' and empty segments that could escape, so a valid name never
// triggers it — a triggered LeasePathError means an unvalidated name reached path
// mapping.
type LeasePathError struct {
	Name string
	Path string
}

func (e *LeasePathError) Error() string {
	return "fsstore: lease name " + strconv.Quote(e.Name) + " maps outside the store root: " + strconv.Quote(e.Path)
}

// LeaseIOError wraps an underlying filesystem failure (mkdir, open, read, write,
// truncate, fsync, close) with the operation and path for diagnosis. Cause is the
// os/syscall error and is exposed via Unwrap.
type LeaseIOError struct {
	Op    string
	Path  string
	Cause error
}

func (e *LeaseIOError) Error() string {
	return "fsstore: lease " + e.Op + " " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying filesystem error.
func (e *LeaseIOError) Unwrap() error { return e.Cause }

// LeaseCorruptError reports that a lock file's persisted epoch counter could not be
// trusted: it is present but not a valid decimal uint64. Acquire fails closed
// rather than reset the counter to 0 (which would let a stale holder's epoch be
// reissued, breaking the strictly-increasing guarantee). Cause is errBadEpoch.
type LeaseCorruptError struct {
	Path  string
	Cause error
}

func (e *LeaseCorruptError) Error() string {
	return "fsstore: lease " + strconv.Quote(e.Path) + " has a corrupt epoch counter"
}

// Unwrap exposes the underlying cause (errBadEpoch).
func (e *LeaseCorruptError) Unwrap() error { return e.Cause }

// leaserStore is the filesystem Leaser backend: it grants exclusive, epoch-fenced
// ownership of names, each mapped to a lock file rooted at <root>/leases. It holds
// no per-name in-process state — the OS advisory lock is the sole arbiter of
// exclusion, and the on-disk counter is the sole source of epoch truth — so the
// store itself needs no mutex.
type leaserStore struct {
	root   string // cleaned store root; <root>/leases holds the lock files
	leases string // cleaned <root>/leases
}

// Compile-time proof that *leaserStore honors the Leaser contract and *fsLease the
// Lease contract.
var (
	_ storage.Leaser = (*leaserStore)(nil)
	_ storage.Lease  = (*fsLease)(nil)
)

// newLeaserStore creates <root> and <root>/leases at 0700 and returns a leaser
// backend rooted there. It rejects an empty root and wraps any mkdir failure in a
// typed error.
func newLeaserStore(root string) (*leaserStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &LeaseRootError{Root: root, Reason: "empty root"}
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, &LeaseIOError{Op: "mkdir", Path: clean, Cause: err}
	}
	leases := filepath.Join(clean, "leases")
	if err := os.MkdirAll(leases, 0o700); err != nil {
		return nil, &LeaseIOError{Op: "mkdir", Path: leases, Cause: err}
	}
	return &leaserStore{root: clean, leases: leases}, nil
}

// pathFor maps a storage name to its lock file path under leases and verifies the
// result stays within leases (containment / defense in depth). The name's
// '/'-separated segments become path components; the file gains the .lock suffix.
// Containment is LEXICAL: it assumes an exclusively-owned root created at 0700 and
// does NOT resolve symlinks. Owning the store root is the deployment's
// responsibility (identical to the ledger backend's model).
func (s *leaserStore) pathFor(name string) (string, error) {
	full := filepath.Clean(filepath.Join(s.leases, filepath.FromSlash(name)))
	// The trailing separator in the prefix rejects both an escaping path and the
	// leases root itself (an empty name), and defends against a sibling directory
	// whose name merely starts with "leases".
	if !strings.HasPrefix(full, s.leases+string(os.PathSeparator)) {
		return "", &LeasePathError{Name: name, Path: full}
	}
	return full + leaseExt, nil
}

// Acquire grants exclusive ownership of name. It validates the name
// (*InvalidNameError), maps and contains it to a lock file, opens a fresh fd, and
// takes a non-blocking exclusive advisory lock:
//   - held elsewhere -> *LeaseHeldError with the holder's persisted epoch (advisory
//     read; the fd is closed).
//   - granted -> the persisted counter is read, incremented, written back and
//     fsynced (durable, strictly increasing), and an fsLease is returned holding
//     the locked fd, the granted epoch, and an open Lost() channel.
//
// A filesystem backend never returns *AmbiguousError: a failed acquire is always
// definite (fail closed, no partial grant).
func (s *leaserStore) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	path, perr := s.pathFor(name)
	if perr != nil {
		return nil, perr
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &LeaseIOError{Op: "mkdir", Path: dir, Cause: err}
	}

	f, oerr := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- path is validated and contained under leases
	if oerr != nil {
		return nil, &LeaseIOError{Op: "open", Path: path, Cause: oerr}
	}

	acquired, lerr := tryLockFileEx(f)
	if lerr != nil {
		// A genuine flock fault (or unsupported platform): fail closed. Surface a
		// close failure alongside without masking the lock error.
		if cerr := f.Close(); cerr != nil {
			return nil, errors.Join(lerr, &LeaseIOError{Op: "close", Path: path, Cause: cerr})
		}
		return nil, lerr
	}
	if !acquired {
		// A live holder owns the lock. Read its epoch without the lock (advisory) —
		// the counter is only advanced under the lock, so a committed value is
		// observed; a torn/unreadable read fails soft to 0.
		holder := readHolderEpoch(f)
		if cerr := f.Close(); cerr != nil {
			return nil, errors.Join(&storage.LeaseHeldError{Name: name, HolderEpoch: holder}, &LeaseIOError{Op: "close", Path: path, Cause: cerr})
		}
		return nil, &storage.LeaseHeldError{Name: name, HolderEpoch: holder}
	}

	// Locked. Advance the persisted epoch counter under the lock.
	epoch, rerr := readEpoch(f, path)
	if rerr != nil {
		return nil, releaseOnError(f, path, rerr)
	}
	epoch++
	if werr := writeEpoch(f, path, epoch); werr != nil {
		return nil, releaseOnError(f, path, werr)
	}
	return &fsLease{path: path, epoch: epoch, f: f, lost: make(chan struct{})}, nil
}

// releaseOnError unwinds a half-acquired lease: it drops the advisory lock and
// closes the fd, then returns the original cause. It is used only on the failure
// path after the lock was taken but before an fsLease was returned, so the caller
// never leaks a locked fd. A cleanup failure is joined onto cause so neither is
// lost, but cause stays independently discoverable via errors.As.
func releaseOnError(f *os.File, path string, cause error) error {
	uerr := unlockFile(f)
	cerr := f.Close()
	if uerr != nil {
		cause = errors.Join(cause, uerr)
	}
	if cerr != nil {
		cause = errors.Join(cause, &LeaseIOError{Op: "close", Path: path, Cause: cerr})
	}
	return cause
}

// readEpoch reads and parses the persisted epoch counter from f, failing CLOSED on
// an unreadable file or a present-but-unparseable counter. An empty (or absent,
// just-created) file is the legitimate "no epoch yet" state and yields 0.
func readEpoch(f *os.File, path string) (uint64, error) {
	buf := make([]byte, maxEpochBytes)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, &LeaseIOError{Op: "read", Path: path, Cause: err}
	}
	epoch, ok := parseEpoch(buf[:n])
	if !ok {
		return 0, &LeaseCorruptError{Path: path, Cause: errBadEpoch}
	}
	return epoch, nil
}

// readHolderEpoch reads the persisted counter best-effort for LeaseHeldError, WITHOUT
// holding the lock. The epoch is informational (the refusal is authoritative), so
// any read fault or unparseable content is reported as an unknown 0 rather than
// failing the refusal.
func readHolderEpoch(f *os.File) uint64 {
	buf := make([]byte, maxEpochBytes)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0
	}
	epoch, ok := parseEpoch(buf[:n])
	if !ok {
		return 0
	}
	return epoch
}

// parseEpoch decodes a persisted counter. An empty/whitespace slice is the valid
// "no epoch yet" state (0, true); a well-formed decimal is (v, true); any other
// content is (0, false) so callers can fail closed or fall soft as appropriate.
func parseEpoch(b []byte) (uint64, bool) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, true
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// writeEpoch persists epoch as the lock file's entire contents ("<decimal>\n") and
// fsyncs it, so the advanced counter is durable across a process restart. It writes
// at offset 0 and truncates to the written length (the decimal width is
// non-decreasing across grants, so the truncate is normally a no-op; it defends
// against any shorter prior content).
func writeEpoch(f *os.File, path string, epoch uint64) error {
	content := []byte(strconv.FormatUint(epoch, 10) + "\n")
	if _, err := f.WriteAt(content, 0); err != nil {
		return &LeaseIOError{Op: "write", Path: path, Cause: err}
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		return &LeaseIOError{Op: "truncate", Path: path, Cause: err}
	}
	if err := f.Sync(); err != nil {
		return &LeaseIOError{Op: "fsync", Path: path, Cause: err}
	}
	return nil
}

// fsLease is a single grant of a leaserStore name. Its epoch is fixed at
// construction; f is the fd holding the advisory lock; lost closes exactly once
// when the grant ends via Release. The OS also releases the advisory lock if the
// process dies, so the grant's liveness is scoped to this host (a crash reclaims
// the lease without Release ever running).
type fsLease struct {
	path  string
	epoch uint64

	mu       sync.Mutex    // guards released, f, and the lost closure
	f        *os.File      // the locked fd; closed on Release
	lost     chan struct{} // closed on Release
	released bool
}

// Epoch returns the fixed, strictly-increasing epoch stamped on this grant.
func (l *fsLease) Epoch() uint64 { return l.epoch }

// Lost returns a channel closed when ownership ends. On this backend that is
// Release, or — undetected by this channel — process death, which the OS reclaims
// by releasing the flock. The channel closes at most once.
func (l *fsLease) Lost() <-chan struct{} { return l.lost }

// Release ends the grant: it drops the advisory lock, closes the fd, and closes
// Lost(). It is idempotent — a second Release is a no-op returning nil (guarded by
// the released flag under the mutex). The lock/fd are dropped BEFORE Lost() is
// closed so a waiter that reacts to Lost() by re-acquiring finds the lock already
// free. A cleanup failure is reported, but the lease is torn down regardless (Lost
// is closed) so the caller never observes a half-released lease. The ctx exists
// only to satisfy the Lease contract.
func (l *fsLease) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	uerr := unlockFile(l.f)
	cerr := l.f.Close()
	close(l.lost)

	if uerr != nil {
		return uerr
	}
	if cerr != nil {
		return &LeaseIOError{Op: "close", Path: l.path, Cause: cerr}
	}
	return nil
}
