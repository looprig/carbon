package fsstore

import (
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

// This file implements storage.Ledger over the local filesystem: one append-only
// file per named ledger, laid out as <root>/streams/<name>.log where the name's
// '/'-separated segments become directory components. Each record is an
// encodeFrame frame (see frame.go); the file is the concatenation of its frames.
//
// # Locking model
//
// Two layers guard every ledger, and the append critical section holds both:
//
//   - In-process: a top-level mutex guards the name->ledgerFile registry; each
//     ledgerFile carries its own mutex that serializes all operations
//     (Append/Read/Tip/Delete) on that one name. Registry mutex is only ever held
//     to look a ledgerFile up, never while its per-file mutex is held, so the two
//     never deadlock.
//   - Cross-process: an exclusive advisory lock (flock LOCK_EX) is taken on the
//     ledger file for the duration of each Append and Delete critical section and
//     released before the call returns — a PER-APPEND lock, never held open across
//     calls. Sessions are effectively single-writer, so per-append locking keeps
//     the fd lifecycle simple and lets the OS reclaim a crashed writer's lock
//     immediately (the next writer's LOCK_EX proceeds the instant the dead holder's
//     fd is closed by the kernel).
//
// Reads (Read/Tip) deliberately take NO advisory lock. They snapshot the tip by
// scanning a private in-memory copy of the file. This is safe ONLY under the
// backend's operating assumptions: a single writer per name (the Leaser enforces
// this in practice) that only ever appends whole frames beyond the current end
// and never rewrites a committed frame. Under those assumptions a reader that
// races an in-flight pwrite observes the newly appended region only partially — a
// short trailing frame — which the scan treats as a torn tail and stops at,
// yielding a valid bounded snapshot rather than corruption. (Absent single-writer
// discipline, a concurrent rewrite of committed bytes could instead surface a CRC
// mismatch and fail the read closed — the safe direction, never silent data
// loss.) The append-only invariant also means a snapshot stays valid for the life
// of the cursor.
//
// ctx is accepted to satisfy the storage.Ledger contract but, like the memstore
// reference backend, local filesystem syscalls are not interrupted mid-call.

// logExt is the on-disk suffix appended to every ledger file's mapped path.
const logExt = ".log"

// errNonContiguousSeq is the leaf cause recorded in a LedgerCorruptError when a
// frame's decoded sequence does not follow its predecessor (a gap or reordering
// that truncation cannot repair).
var errNonContiguousSeq = errors.New("fsstore: non-contiguous ledger sequence")

// LedgerCorruptError reports that a ledger file could not be trusted: a frame at
// Seq is present-but-invalid (CRC mismatch, over-ceiling length) or the sequence
// is non-contiguous. Unlike a torn tail, this is not recoverable by truncation,
// so every operation fails closed rather than silently drop data. Cause carries
// the underlying frame fault or errNonContiguousSeq.
type LedgerCorruptError struct {
	Path  string
	Seq   uint64
	Cause error
}

func (e *LedgerCorruptError) Error() string {
	return "fsstore: ledger " + strconv.Quote(e.Path) + " corrupt at seq " + strconv.FormatUint(e.Seq, 10)
}

// Unwrap exposes the underlying frame fault (a *FrameError) or errNonContiguousSeq.
func (e *LedgerCorruptError) Unwrap() error { return e.Cause }

// LedgerPathError reports that a ledger Name mapped to a filesystem location
// outside the store root. It is defense in depth: ValidateName already forbids
// the '..' and empty segments that could escape, so a valid name never triggers
// it — a triggered LedgerPathError means an unvalidated name reached path mapping.
type LedgerPathError struct {
	Name string
	Path string
}

func (e *LedgerPathError) Error() string {
	return "fsstore: ledger name " + strconv.Quote(e.Name) + " maps outside the store root: " + strconv.Quote(e.Path)
}

// LedgerRootError reports that newLedgerStore was given an unusable root (for
// example, an empty path).
type LedgerRootError struct {
	Root   string
	Reason string
}

func (e *LedgerRootError) Error() string {
	return "fsstore: invalid ledger root " + strconv.Quote(e.Root) + ": " + e.Reason
}

// LedgerIOError wraps an underlying filesystem failure (open, read, write, fsync,
// truncate, mkdir, stat) with the operation and path for diagnosis. Cause is the
// os/syscall error and is exposed via Unwrap.
type LedgerIOError struct {
	Op    string
	Path  string
	Cause error
}

func (e *LedgerIOError) Error() string {
	return "fsstore: ledger " + e.Op + " " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying filesystem error.
func (e *LedgerIOError) Unwrap() error { return e.Cause }

// ledgerStore is the filesystem Ledger backend: it addresses many named ledgers
// rooted at streams, tracking one ledgerFile per name it has touched.
type ledgerStore struct {
	root    string // cleaned store root; <root>/streams holds the ledgers
	streams string // cleaned <root>/streams
	mu      sync.Mutex
	files   map[string]*ledgerFile
}

// Compile-time proof that *ledgerStore honors the Ledger contract.
var _ storage.Ledger = (*ledgerStore)(nil)

// ledgerFile holds the per-name serialization mutex and the cached tip observed
// after the last write. tip/size are authoritative only while recovered is true
// AND the on-disk file size still matches size; a mismatch (a cross-process
// append or truncation) forces a re-scan. By construction, whenever recovered is
// true, size equals the byte offset just past the last good frame (no torn tail
// is ever cached), so an append can extend the file at size directly.
type ledgerFile struct {
	path      string
	mu        sync.Mutex
	recovered bool
	tip       uint64
	size      int64
}

// newLedgerStore creates <root> and <root>/streams at 0700 and returns a ledger
// backend rooted there. It rejects an empty root and wraps any mkdir failure in a
// typed error.
func newLedgerStore(root string) (*ledgerStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, &LedgerRootError{Root: root, Reason: "empty root"}
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return nil, &LedgerIOError{Op: "mkdir", Path: clean, Cause: err}
	}
	streams := filepath.Join(clean, "streams")
	if err := os.MkdirAll(streams, 0o700); err != nil {
		return nil, &LedgerIOError{Op: "mkdir", Path: streams, Cause: err}
	}
	return &ledgerStore{root: clean, streams: streams, files: make(map[string]*ledgerFile)}, nil
}

// Close releases the ledger backend's in-process state: it drops the cached
// name->ledgerFile registry (the tip/size metadata observed after the last write).
// The backend holds no long-lived file descriptors — every Append/Read/Tip/Delete
// opens and closes its fd within the call (the per-append advisory-lock model) — so
// there are no open handles to release; Close relinquishes the in-memory registry
// and makes backend shutdown explicit. It is idempotent: it resets the registry to
// an empty map, so a second call (or a stray operation after Close) is safe rather
// than a nil-map panic.
func (s *ledgerStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = make(map[string]*ledgerFile)
	return nil
}

// ledgerIOErr adapts *LedgerIOError to the ioErrFunc the shared fsync helpers
// expect, so a directory-fsync failure on a ledger path surfaces as a ledger-typed
// IO error rather than another backend's.
func ledgerIOErr(op, path string, cause error) error {
	return &LedgerIOError{Op: op, Path: path, Cause: cause}
}

// pathFor maps a storage name to its ledger file path under streams and verifies
// the result stays within streams (containment / defense in depth). The name's
// '/'-separated segments become path components; the file gains the .log suffix.
// Containment is LEXICAL: it assumes an exclusively-owned root created at 0700 and
// does NOT resolve symlinks, so a symlink planted inside streams by another writer
// could still redirect a mapped path. Owning the store root is the deployment's
// responsibility.
func (s *ledgerStore) pathFor(name string) (string, error) {
	full := filepath.Clean(filepath.Join(s.streams, filepath.FromSlash(name)))
	// The trailing separator in the prefix rejects both an escaping path and the
	// streams root itself (an empty name), and defends against a sibling directory
	// whose name merely starts with "streams".
	if !strings.HasPrefix(full, s.streams+string(os.PathSeparator)) {
		return "", &LedgerPathError{Name: name, Path: full}
	}
	return full + logExt, nil
}

// fileFor returns the ledgerFile tracking name, creating the registry entry on
// first use. It validates the name->path mapping (containment) before touching
// the registry.
func (s *ledgerStore) fileFor(name string) (*ledgerFile, error) {
	path, err := s.pathFor(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lf := s.files[name]
	if lf == nil {
		lf = &ledgerFile{path: path}
		s.files[name] = lf
	}
	return lf, nil
}

// Append commits payload as the record after sequence expected (CAS on the tip).
// It serializes per-name in-process and takes an exclusive cross-process advisory
// lock for the critical section, recovers a torn tail before reading the tip, and
// fsyncs the file (and, on first creation, the ancestor directory chain up to
// streams) before advancing the cached tip. A CAS mismatch returns *ConflictError
// with state untouched; a filesystem backend never returns *AmbiguousError.
func (s *ledgerStore) Append(ctx context.Context, name string, expected uint64, payload []byte) (err error) {
	if verr := storage.ValidateName(name); verr != nil {
		return verr
	}
	lf, ferr := s.fileFor(name)
	if ferr != nil {
		return ferr
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()

	dir := filepath.Dir(lf.path)
	// Best-effort, race-free: ensure the directory tree exists before opening
	// (idempotent). The durability-critical CREATION decision is NOT made here —
	// it is derived below from on-disk state read under the advisory lock, so a
	// cross-process Delete racing this MkdirAll cannot skip the dir fsync.
	if merr := os.MkdirAll(dir, 0o700); merr != nil {
		return &LedgerIOError{Op: "mkdir", Path: dir, Cause: merr}
	}

	f, oerr := os.OpenFile(lf.path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- lf.path is validated and contained under streams
	if oerr != nil {
		return &LedgerIOError{Op: "open", Path: lf.path, Cause: oerr}
	}
	// Defers run LIFO: unlock (registered second) releases the advisory lock
	// before close (registered first) releases the fd.
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = &LedgerIOError{Op: "close", Path: lf.path, Cause: cerr}
		}
	}()
	if lerr := lockFileEx(f); lerr != nil {
		return lerr
	}
	defer func() {
		if uerr := unlockFile(f); uerr != nil && err == nil {
			err = uerr
		}
	}()

	if rerr := lf.refresh(f); rerr != nil {
		return rerr
	}
	// Authoritative creation decision from on-disk state observed under the lock:
	// an empty ledger (no good frames) means its directory entries may be newly
	// created and need an fsync to be crash-durable. A torn-to-zero recovery also
	// lands here, making the dir fsync redundant — a negligible cost.
	created := lf.size == 0
	if expected != lf.tip {
		return &storage.ConflictError{Name: name, Expected: expected}
	}

	frame, eerr := encodeFrame(lf.tip+1, payload)
	if eerr != nil {
		return eerr // oversize: a definite failure, never ambiguous
	}
	if _, werr := f.WriteAt(frame, lf.size); werr != nil {
		return &LedgerIOError{Op: "write", Path: lf.path, Cause: werr}
	}
	if serr := f.Sync(); serr != nil {
		return &LedgerIOError{Op: "fsync", Path: lf.path, Cause: serr}
	}
	if created {
		// On first creation the new dirents may reach up several levels (e.g.
		// streams/sessions/<uuid>.log creates streams/sessions), so fsync the
		// ancestor chain from the file's directory up to and including streams.
		if derr := fsyncDirChain(dir, s.streams, ledgerIOErr); derr != nil {
			return derr
		}
	}
	lf.tip++
	lf.size += int64(len(frame))
	return nil
}

// refresh brings lf's cached tip/size in sync with the on-disk file, scanning and
// (if a torn tail is found) truncating it back to the last good frame. It is
// called only from Append, under both the per-file mutex and the advisory lock,
// so the file is quiescent. The fast path skips the scan when the cache is valid
// and the file size is unchanged.
//
// The st.Size()==lf.size fast path is sound only under the single-writer-per-name
// invariant (the Leaser provides it in practice): it detects a cross-process
// change by size alone, so a Delete+recreate that happened to land an identical
// byte count would stale the cache. Under single-writer discipline that sequence
// cannot occur. (An inode-number comparison would close even that gap but needs a
// platform-specific syscall, so it is intentionally not done here.)
func (lf *ledgerFile) refresh(f *os.File) error {
	st, serr := f.Stat()
	if serr != nil {
		return &LedgerIOError{Op: "stat", Path: lf.path, Cause: serr}
	}
	if lf.recovered && st.Size() == lf.size {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return &LedgerIOError{Op: "seek", Path: lf.path, Cause: err}
	}
	data, rerr := io.ReadAll(f)
	if rerr != nil {
		return &LedgerIOError{Op: "read", Path: lf.path, Cause: rerr}
	}
	lastSeq, goodEnd, scerr := scanFrames(data, lf.path)
	if scerr != nil {
		return scerr // LedgerCorruptError: fail closed
	}
	if goodEnd < int64(len(data)) {
		// A torn tail (crash mid-append): roll the file back to the last good frame.
		if terr := f.Truncate(goodEnd); terr != nil {
			return &LedgerIOError{Op: "truncate", Path: lf.path, Cause: terr}
		}
		if fserr := f.Sync(); fserr != nil {
			return &LedgerIOError{Op: "fsync", Path: lf.path, Cause: fserr}
		}
	}
	lf.tip = lastSeq
	lf.size = goodEnd
	lf.recovered = true
	return nil
}

// Read returns a bounded cursor over records with sequence >= from, observing the
// tip as of this call. It snapshots the file into memory, so the cursor is immune
// to later appends, truncations, or deletion. from < 1 clamps to the first
// record; from > tip and an absent ledger both yield an immediately-drained
// cursor. A corrupt file fails closed.
func (s *ledgerStore) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	lf, ferr := s.fileFor(name)
	if ferr != nil {
		return nil, ferr
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()

	data, err := os.ReadFile(lf.path) // #nosec G304 -- path is validated and contained under streams
	if errors.Is(err, fs.ErrNotExist) {
		return &drainedCursor{}, nil // absent == empty
	}
	if err != nil {
		return nil, &LedgerIOError{Op: "read", Path: lf.path, Cause: err}
	}
	lastSeq, _, scerr := scanFrames(data, lf.path)
	if scerr != nil {
		return nil, scerr
	}
	if from < 1 {
		from = 1
	}
	if from > lastSeq {
		return &drainedCursor{}, nil
	}
	return &recordCursor{data: data, from: from, tip: lastSeq, nextSeq: 1, path: lf.path}, nil
}

// Tip returns the current tip for name (0 if the file is absent). It scans the
// file read-only, so a torn tail is reported as the last good sequence without
// mutating the file; a corrupt file fails closed.
func (s *ledgerStore) Tip(ctx context.Context, name string) (uint64, error) {
	if err := storage.ValidateName(name); err != nil {
		return 0, err
	}
	lf, ferr := s.fileFor(name)
	if ferr != nil {
		return 0, ferr
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()

	data, err := os.ReadFile(lf.path) // #nosec G304 -- path is validated and contained under streams
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, &LedgerIOError{Op: "read", Path: lf.path, Cause: err}
	}
	lastSeq, _, scerr := scanFrames(data, lf.path)
	if scerr != nil {
		return 0, scerr
	}
	return lastSeq, nil
}

// Delete removes the ledger file; it is idempotent (an absent file succeeds). It
// takes the advisory lock before unlinking so an in-flight cross-process append
// completes first, then resets the in-process cache so a later Append re-creates
// the ledger from empty.
func (s *ledgerStore) Delete(ctx context.Context, name string) (err error) {
	if verr := storage.ValidateName(name); verr != nil {
		return verr
	}
	lf, ferr := s.fileFor(name)
	if ferr != nil {
		return ferr
	}
	lf.mu.Lock()
	defer lf.mu.Unlock()

	f, oerr := os.OpenFile(lf.path, os.O_RDWR, 0) // #nosec G304 -- lf.path is validated and contained under streams
	switch {
	case oerr == nil:
		if lerr := lockFileEx(f); lerr != nil {
			// Surface a close failure alongside the lock error without masking it:
			// the lock error stays independently discoverable via errors.As.
			if cerr := f.Close(); cerr != nil {
				return errors.Join(lerr, &LedgerIOError{Op: "close", Path: lf.path, Cause: cerr})
			}
			return lerr
		}
		rmErr := os.Remove(lf.path)
		if uerr := unlockFile(f); uerr != nil && rmErr == nil {
			rmErr = uerr
		}
		if cerr := f.Close(); cerr != nil && rmErr == nil {
			rmErr = &LedgerIOError{Op: "close", Path: lf.path, Cause: cerr}
		}
		if rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return &LedgerIOError{Op: "remove", Path: lf.path, Cause: rmErr}
		}
	case errors.Is(oerr, fs.ErrNotExist):
		// Already absent: idempotent no-op.
	default:
		return &LedgerIOError{Op: "open", Path: lf.path, Cause: oerr}
	}

	lf.recovered = false
	lf.tip = 0
	lf.size = 0
	return nil
}

// scanFrames walks the concatenated frames in data, verifying that sequences are
// contiguous from 1. It returns the last good sequence and goodEnd, the byte
// offset just past the last whole frame. A torn tail (an incomplete final frame)
// stops the walk cleanly at goodEnd (recoverable by truncation); a corrupt frame
// or a sequence gap returns a *LedgerCorruptError (fail closed).
func scanFrames(data []byte, path string) (lastSeq uint64, goodEnd int64, err error) {
	off := 0
	var expect uint64 = 1
	for off < len(data) {
		seq, _, n, derr := decodeFrame(data[off:])
		if derr != nil {
			var fe *FrameError
			if errors.As(derr, &fe) && fe.IsTorn() {
				return lastSeq, int64(off), nil // torn tail: good up to off
			}
			return 0, 0, &LedgerCorruptError{Path: path, Seq: expect, Cause: derr}
		}
		if seq != expect {
			return 0, 0, &LedgerCorruptError{Path: path, Seq: seq, Cause: errNonContiguousSeq}
		}
		off += n
		lastSeq = seq
		expect++
	}
	return lastSeq, int64(off), nil
}

// fsyncDir opens dir and fsyncs it so a newly created child file's directory
// entry is durable across a crash. It wraps any fault via ioErr so the caller's
// own backend-typed error surfaces (a KV/Blob Put's dir fsync must not report a
// ledger error), mirroring how writeFileAtomic already takes an ioErrFunc.
func fsyncDir(dir string, ioErr ioErrFunc) (err error) {
	// dir derives from a ValidateName-checked, containment-verified path (pathFor),
	// so it cannot escape the store root.
	d, oerr := os.Open(dir) // #nosec G304 -- dir derives from a validated, containment-checked store path
	if oerr != nil {
		return ioErr("open-dir", dir, oerr)
	}
	defer func() {
		if cerr := d.Close(); cerr != nil && err == nil {
			err = ioErr("close-dir", dir, cerr)
		}
	}()
	if serr := d.Sync(); serr != nil {
		return ioErr("fsync-dir", dir, serr)
	}
	return nil
}

// fsyncDirChain fsyncs every directory from leafDir up to and including stopDir,
// so that on a first-ever ledger creation the new directory entries — which may
// span several levels (e.g. a new streams/sessions holding the .log file) — are
// all durable across a crash. leafDir is always stopDir or a descendant of it (it
// derives from a path contained under stopDir), so the walk terminates at stopDir;
// the filesystem-root guard is defensive belt-and-braces only. ioErr threads the
// caller's backend-typed IO error down to each per-directory fsync.
func fsyncDirChain(leafDir, stopDir string, ioErr ioErrFunc) error {
	for dir := leafDir; ; {
		if err := fsyncDir(dir, ioErr); err != nil {
			return err
		}
		if dir == stopDir {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root: stop rather than loop
			return nil
		}
		dir = parent
	}
}

// drainedCursor is the empty cursor returned for an absent ledger or a from
// beyond the tip: it yields io.EOF immediately.
type drainedCursor struct{}

func (drainedCursor) Next(ctx context.Context) (storage.Record, error) {
	return storage.Record{}, io.EOF
}

func (drainedCursor) Close() error { return nil }

// recordCursor is a bounded, single-pass reader over an in-memory snapshot of a
// ledger file taken at Read time. It walks frames from the start, skipping those
// below from, and stops once it has yielded the record at tip — so it never
// touches any torn tail beyond the snapshot and never observes a later append.
type recordCursor struct {
	data    []byte
	off     int
	from    uint64
	tip     uint64
	nextSeq uint64 // 1-based sequence of the frame at off
	path    string
}

// Next decodes and returns the next record with sequence in [from, tip], handing
// back the fresh payload copy that decodeFrame produced, or io.EOF once the
// snapshot's tip has been reached.
func (c *recordCursor) Next(ctx context.Context) (storage.Record, error) {
	for c.nextSeq <= c.tip {
		seq, payload, n, derr := decodeFrame(c.data[c.off:])
		if derr != nil {
			// The snapshot was validated up to tip at Read time, so a fault here
			// means the committed region is unreadable: fail closed.
			return storage.Record{}, &LedgerCorruptError{Path: c.path, Seq: c.nextSeq, Cause: derr}
		}
		if seq != c.nextSeq {
			return storage.Record{}, &LedgerCorruptError{Path: c.path, Seq: seq, Cause: errNonContiguousSeq}
		}
		c.off += n
		cur := c.nextSeq
		c.nextSeq++
		if cur >= c.from {
			return storage.Record{Seq: seq, Payload: payload}, nil
		}
	}
	return storage.Record{}, io.EOF
}

// Close releases the cursor. The snapshot is an in-memory copy, so there is
// nothing to release; the method exists to honor the Cursor contract.
func (c *recordCursor) Close() error { return nil }
