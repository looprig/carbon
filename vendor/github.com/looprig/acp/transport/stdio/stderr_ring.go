package stdio

import "sync"

// stderrRingCapacity bounds how much of a child's stderr is retained for
// diagnosis. Stderr is never parsed for protocol meaning — this ring exists
// purely to surface the tail of it on abnormal exit.
const stderrRingCapacity = 8 * 1024

// stderrRing is a bounded io.Writer that retains only the last
// stderrRingCapacity bytes written to it. It is safe for concurrent Write and
// Bytes calls, since a child's stderr may be drained by one goroutine while
// Wait reads the tail from another.
type stderrRing struct {
	mu  sync.Mutex
	buf []byte
}

func newStderrRing() *stderrRing {
	return &stderrRing{}
}

func (r *stderrRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if drop := len(r.buf) - stderrRingCapacity; drop > 0 {
		r.buf = append([]byte(nil), r.buf[drop:]...)
	}
	return len(p), nil
}

// Bytes returns a copy of the retained tail, at most stderrRingCapacity bytes.
func (r *stderrRing) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}
