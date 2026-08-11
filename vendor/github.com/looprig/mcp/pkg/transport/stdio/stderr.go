// This file holds the bounded stderr capture.
//
// A child's stderr is untrusted output of unbounded length produced by a peer
// we do not control, and the only thing we want from it is the last thing it
// said before it died. A ring keeps exactly that in constant memory: a server
// that logs a gigabyte and a server that logs one line cost the same.

package stdio

import (
	"io"
	"sync"
)

// ring is a fixed-capacity byte ring that keeps the most recent bytes written
// to it and discards the rest. It is an io.Writer, so it can be the destination
// of a plain io.Copy from the child's stderr pipe, and it is safe for
// concurrent use: the copier writes while a failure path reads.
//
// The tail, not the head, is what is kept: the bytes immediately before a crash
// explain it, the bytes from startup rarely do.
type ring struct {
	mu sync.Mutex
	// buf is the backing store, allocated once at capacity.
	buf []byte
	// n is how many bytes of buf are live (n <= cap).
	n int
	// start is the index of the oldest live byte.
	start int
	// dropped counts bytes overwritten (or skipped) because they aged out.
	dropped int64
}

// newRing returns a ring holding at most capacity bytes. capacity must be
// positive; New rejects a non-positive StderrLimit before this is reached.
func newRing(capacity int) *ring {
	if capacity <= 0 {
		panic("stdio: ring capacity must be positive")
	}
	return &ring{buf: make([]byte, capacity)}
}

// Write records p, dropping whatever no longer fits. It never fails and never
// short-writes: the writer is a copier draining a pipe, and reporting a short
// write to it would stall the child on a full pipe for no benefit.
func (r *ring) Write(p []byte) (int, error) {
	total := len(p)
	r.mu.Lock()
	defer r.mu.Unlock()

	capacity := len(r.buf)
	if total >= capacity {
		// The write alone overflows the ring: keep only its own tail, and
		// account for everything it displaced, including its own head.
		r.dropped += int64(r.n) + int64(total-capacity)
		copy(r.buf, p[total-capacity:])
		r.start = 0
		r.n = capacity
		return total, nil
	}

	end := (r.start + r.n) % capacity
	written := copy(r.buf[end:], p)
	if written < total {
		copy(r.buf, p[written:])
	}
	if r.n+total <= capacity {
		r.n += total
		return total, nil
	}
	// The oldest bytes were just overwritten: advance start past them.
	overflow := r.n + total - capacity
	r.dropped += int64(overflow)
	r.start = (r.start + overflow) % capacity
	r.n = capacity
	return total, nil
}

// Tail returns the last n bytes recorded, or everything held if fewer. n <= 0
// returns nothing.
func (r *ring) Tail(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || r.n == 0 {
		return nil
	}
	if n > r.n {
		n = r.n
	}
	out := make([]byte, 0, n)
	from := (r.start + r.n - n) % len(r.buf)
	if from+n <= len(r.buf) {
		return append(out, r.buf[from:from+n]...)
	}
	out = append(out, r.buf[from:]...)
	return append(out, r.buf[:n-(len(r.buf)-from)]...)
}

// Len reports how many bytes are currently held.
func (r *ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Dropped reports how many bytes were discarded for want of room. It is what
// makes a truncated capture honest: the diagnostic says the server said more
// than is shown.
func (r *ring) Dropped() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// compile-time proof the copier can target a ring directly.
var _ io.Writer = (*ring)(nil)
