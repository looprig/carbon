package process

import "sync"

// Buffer is the in-memory rolling window over one process's combined
// stdout+stderr byte stream (spec "Output capture and storage": "The
// in-memory window is optimized for recent polling. The spool is the
// bounded source of truth for completed output and cursor recovery"). Buffer
// and Spool (spool.go) share one cursor addressing scheme -- raw,
// monotonically increasing byte offsets into a single combined
// append-ordered stream, never rune or codepoint indexes -- but are two
// independent stores: Buffer is a fixed-capacity ring kept entirely in
// memory for cheap recent-output polling. It never persists to disk and
// never reads from or delegates to a Spool; a Buffer alone is not the
// source of truth for anything older than its own capacity.
//
// A Buffer is safe for concurrent use by multiple goroutines.
type Buffer struct {
	mu sync.Mutex

	capacity int64
	ring     []byte // len(ring) == capacity, lazily allocated by the first Append
	total    int64  // every byte ever appended, independent of what remains retained
}

// NewBuffer returns a Buffer with the given capacity in bytes. A
// non-positive capacity defaults to DefaultMaxProcessInMemoryBytes
// (config.go), mirroring OpenSpool's non-positive-ceiling default.
func NewBuffer(capacity int64) *Buffer {
	if capacity <= 0 {
		capacity = DefaultMaxProcessInMemoryBytes
	}
	return &Buffer{capacity: capacity}
}

// Append durably (in the in-memory sense -- see the type doc for what
// "durable" does not mean here) adds data to the end of the combined output
// stream and returns the resulting global cursor (equal to TotalBytes after
// the append). Append order determines the global cursor, exactly as for
// Spool.Append.
//
// Once the retained window would exceed capacity, Append overwrites the
// oldest retained bytes in place (true ring wraparound: no reallocation,
// no growth beyond capacity) rather than blocking or failing; TotalBytes
// keeps counting every byte ever appended, independent of what remains
// retained. A single Append whose data is itself longer than capacity is
// handled the same way an equivalent sequence of smaller Appends would be:
// only the last capacity bytes of data end up retained.
func (b *Buffer) Append(chunk []byte) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(chunk) == 0 {
		return b.total, nil
	}
	if b.ring == nil {
		b.ring = make([]byte, b.capacity)
	}

	// writeAt is the global cursor of data[0]. When chunk is longer than
	// capacity, every byte before the last `capacity` bytes of chunk would
	// be overwritten by the remainder of this same Append before Append
	// returns, so skip writing them at all -- the ring position math below
	// is unaffected either way, since ring positions repeat every
	// `capacity` bytes.
	writeAt := b.total
	data := chunk
	if int64(len(data)) > b.capacity {
		skip := int64(len(data)) - b.capacity
		data = data[skip:]
		writeAt += skip
	}

	pos := int(writeAt % b.capacity)
	n := copy(b.ring[pos:], data)
	if n < len(data) {
		copy(b.ring, data[n:])
	}

	b.total += int64(len(chunk))
	return b.total, nil
}

// retainedFromLocked reports the cursor of the earliest byte currently
// retained. Callers must hold b.mu.
func (b *Buffer) retainedFromLocked() int64 {
	if b.total <= b.capacity {
		return 0
	}
	return b.total - b.capacity
}

// Read returns up to maxBytes of retained output starting at cursor, the
// exclusive cursor immediately after the returned data (nextCursor), and
// whether cursor fell before the earliest retained byte (gap). This
// mirrors Spool.Read's exact signature and semantics: when gap is true, the
// returned data begins at the earliest retained byte rather than at cursor.
// A non-positive maxBytes returns every retained byte from the (possibly
// gap-adjusted) start.
//
// A cursor beyond TotalBytes reports CodeCursorAhead. A negative cursor
// reports CodeInvalidArguments.
func (b *Buffer) Read(cursor int64, maxBytes int) (data []byte, nextCursor int64, gap bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < 0 {
		return nil, 0, false, New(CodeInvalidArguments)
	}
	if cursor > b.total {
		return nil, b.total, false, New(CodeCursorAhead)
	}

	retainedFrom := b.retainedFromLocked()
	start := cursor
	gap = cursor < retainedFrom
	if gap {
		start = retainedFrom
	}

	avail := b.total - start
	n := avail
	if maxBytes > 0 && int64(maxBytes) < n {
		n = int64(maxBytes)
	}

	out := make([]byte, n)
	if n > 0 {
		pos := int(start % b.capacity)
		copied := copy(out, b.ring[pos:])
		if int64(copied) < n {
			copy(out[copied:], b.ring[:n-int64(copied)])
		}
	}
	return out, start + n, gap, nil
}

// TotalBytes reports the monotonically increasing count of every byte ever
// appended, independent of what remains retained.
func (b *Buffer) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// RetainedFrom reports the cursor of the earliest byte currently retained
// in memory.
func (b *Buffer) RetainedFrom() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retainedFromLocked()
}

// Capacity reports the maximum number of retained bytes b was constructed
// with.
func (b *Buffer) Capacity() int64 {
	return b.capacity
}
