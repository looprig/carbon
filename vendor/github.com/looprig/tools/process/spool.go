package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/looprig/tools/internal/atomicfile"
)

// spoolVersion is the current on-disk spool format version. Only version 1
// exists today.
const spoolVersion = 1

// spoolSuffix names the on-disk file for one process's disk spool, keyed by
// its Handle, beneath a private resource root.
const spoolSuffix = ".spool.json"

// ErrSpoolClosed reports that an operation was attempted against a Spool
// after Close or Remove. It is a plain sentinel, not a *Error, because it
// signals local misuse of the Spool value's lifetime rather than a
// model-facing supervision failure.
var ErrSpoolClosed = errors.New("process: spool is closed")

// spoolWire is the on-disk JSON shape for a Spool's durable state. Payload
// is exactly the currently retained window: the []byte field is
// base64-encoded automatically by encoding/json.
type spoolWire struct {
	Version      int    `json:"version"`
	TotalBytes   int64  `json:"total_bytes"`
	RetainedFrom int64  `json:"retained_from"`
	Payload      []byte `json:"payload"`
}

// Spool is the bounded, durable, cursor-addressed disk retention window for
// one process's combined stdout+stderr byte stream (spec "Output capture
// and storage"). It stores only the currently retained window plus a
// monotonically increasing TotalBytes counter; an append that would exceed
// the configured ceiling drops the oldest retained bytes rather than
// blocking or failing (spec: "The spool is a bounded retention window, not
// a hard cap on the process"). Every Append durably persists the new
// retained window via atomicfile.Replace before returning success, so the
// spool file is never left in a partially-written state that a concurrent
// reader could observe.
//
// A Spool is safe for concurrent use by multiple goroutines.
type Spool struct {
	path    string
	ceiling int64

	mu           sync.Mutex
	closed       bool
	totalBytes   int64
	retainedFrom int64
	payload      []byte
}

// OpenSpool opens (or, if none exists yet, prepares to create) the spool for
// process h beneath root. ceiling bounds the retained window in bytes; a
// non-positive ceiling defaults to DefaultMaxProcessSpoolBytes (process/
// config.go). OpenSpool never creates the on-disk file itself — the file is
// created by the first Append.
//
// A truncated or otherwise inconsistent on-disk spool (malformed JSON, an
// unrecognized version, impossible cursor bounds, or a payload whose length
// does not match its declared retained window) is reported as
// CodeSpoolCorrupt.
func OpenSpool(root string, h Handle, ceiling int64) (*Spool, error) {
	if ceiling <= 0 {
		ceiling = DefaultMaxProcessSpoolBytes
	}
	path, rel, err := resourcePath(root, h, spoolSuffix)
	if err != nil {
		return nil, err
	}

	s := &Spool{path: path, ceiling: ceiling}

	data, err := readResourceFile(root, rel)
	switch {
	case err == nil:
		if err := s.hydrate(data); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fresh spool: nothing retained yet.
	default:
		return nil, Wrap(CodeSpoolCorrupt, err)
	}
	return s, nil
}

func (s *Spool) hydrate(data []byte) error {
	var w spoolWire
	if err := json.Unmarshal(data, &w); err != nil {
		return Wrap(CodeSpoolCorrupt, err)
	}
	if w.Version != spoolVersion {
		return Wrap(CodeSpoolCorrupt, fmt.Errorf("unknown spool version %d", w.Version))
	}
	if w.TotalBytes < 0 || w.RetainedFrom < 0 || w.RetainedFrom > w.TotalBytes {
		return Wrap(CodeSpoolCorrupt, fmt.Errorf("invalid spool cursor bounds: total=%d retained_from=%d", w.TotalBytes, w.RetainedFrom))
	}
	if int64(len(w.Payload)) != w.TotalBytes-w.RetainedFrom {
		return Wrap(CodeSpoolCorrupt, fmt.Errorf("payload length %d does not match retained window %d", len(w.Payload), w.TotalBytes-w.RetainedFrom))
	}
	s.totalBytes = w.TotalBytes
	s.retainedFrom = w.RetainedFrom
	s.payload = w.Payload
	return nil
}

// Append durably adds data to the end of the combined output stream and
// returns the resulting global cursor (equal to TotalBytes after the
// append). Append order determines the global cursor: cursors are assigned
// strictly in the order Append is called, regardless of whether the bytes
// originated from stdout or stderr.
//
// If retaining data would exceed the configured ceiling, Append drops
// exactly enough of the oldest retained bytes to fit and still succeeds;
// TotalBytes keeps counting every byte ever appended, independent of what
// remains retained. Append never fails, blocks, or refuses data because of
// volume — a process is never terminated for output volume (spec).
func (s *Spool) Append(data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrSpoolClosed
	}
	if len(data) == 0 {
		return s.totalBytes, nil
	}

	newPayload := make([]byte, 0, len(s.payload)+len(data))
	newPayload = append(newPayload, s.payload...)
	newPayload = append(newPayload, data...)

	newTotal := s.totalBytes + int64(len(data))
	newRetainedFrom := s.retainedFrom
	if int64(len(newPayload)) > s.ceiling {
		drop := int64(len(newPayload)) - s.ceiling
		newPayload = newPayload[drop:]
		newRetainedFrom += drop
	}

	if err := s.persist(newTotal, newRetainedFrom, newPayload); err != nil {
		return 0, err
	}

	s.totalBytes = newTotal
	s.retainedFrom = newRetainedFrom
	s.payload = newPayload
	return s.totalBytes, nil
}

func (s *Spool) persist(total, retainedFrom int64, payload []byte) error {
	w := spoolWire{Version: spoolVersion, TotalBytes: total, RetainedFrom: retainedFrom, Payload: payload}
	data, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("process: marshal spool: %w", err)
	}
	return atomicfile.Replace(s.path, data, 0o600)
}

// Read returns up to maxBytes of retained output starting at cursor, the
// exclusive cursor immediately after the returned data (nextCursor), and
// whether cursor fell before the earliest retained byte (gap). When gap is
// true, the returned data begins at the earliest retained byte rather than
// at cursor, exactly as the spec describes for both the in-memory window
// and the disk spool. A non-positive maxBytes returns every retained byte
// from the (possibly gap-adjusted) start.
//
// A cursor beyond TotalBytes reports CodeCursorAhead. A negative cursor
// reports CodeInvalidArguments.
func (s *Spool) Read(cursor int64, maxBytes int) (data []byte, nextCursor int64, gap bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, 0, false, ErrSpoolClosed
	}
	if cursor < 0 {
		return nil, 0, false, New(CodeInvalidArguments)
	}
	if cursor > s.totalBytes {
		return nil, s.totalBytes, false, New(CodeCursorAhead)
	}

	start := cursor
	gap = cursor < s.retainedFrom
	if gap {
		start = s.retainedFrom
	}

	offset := int(start - s.retainedFrom)
	end := len(s.payload)
	if maxBytes > 0 && end-offset > maxBytes {
		end = offset + maxBytes
	}

	chunk := append([]byte(nil), s.payload[offset:end]...)
	return chunk, start + int64(len(chunk)), gap, nil
}

// TotalBytes reports the monotonically increasing count of every byte ever
// appended to the spool, independent of what remains retained.
func (s *Spool) TotalBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

// RetainedFrom reports the cursor of the earliest byte currently retained
// on disk.
func (s *Spool) RetainedFrom() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retainedFrom
}

// Close marks the Spool closed, rejecting further Append/Read calls. It
// does not remove the on-disk file. Close is idempotent: calling it more
// than once, including after Remove, is a no-op.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Remove closes the Spool and deletes its on-disk file, if any. Remove is
// idempotent: calling it more than once, or calling it when no file was
// ever written (e.g. a process that produced no output), is a no-op rather
// than an error.
func (s *Spool) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
