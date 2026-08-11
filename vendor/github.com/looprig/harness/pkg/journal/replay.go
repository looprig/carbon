package journal

import (
	"context"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// StartPos is the closed value type naming where a replay begins: the log beginning
// (every record) or a specific sequence. It is a value, not an interface, so a caller
// cannot smuggle a third start mode past a switch; the two constructors Beginning and
// FromSeq are the only ways to build one, and Seq reads it back.
type StartPos struct {
	// fromSeq is 0 for Beginning and the (1-based) inclusive start sequence for FromSeq.
	// Log sequences are 1-based, so 0 unambiguously means "from the start".
	fromSeq uint64
}

// Beginning starts a replay at the log's first record.
func Beginning() StartPos { return StartPos{fromSeq: 0} }

// FromSeq starts a replay at sequence seq, inclusive — the dormant-snapshot hook
// (resume after a snapshot's last applied sequence). A seq of 0 is equivalent to
// Beginning (there is no sequence 0).
func FromSeq(seq uint64) StartPos { return StartPos{fromSeq: seq} }

// Seq returns the inclusive start sequence, or 0 for Beginning. It is how a backend
// replayer reads the requested start position off a ReplayRequest.
func (p StartPos) Seq() uint64 { return p.fromSeq }

// ReplayRequest selects which of a session's records to replay and how. Which records
// (events only, or a single loop's) is derived from SessionID + LoopID; how far back
// from From; whether to keep tailing from Follow. The concrete filtering is the
// backend replayer's job — this is the backend-neutral request it honors.
type ReplayRequest struct {
	// SessionID is the session whose log is replayed (required; a zero id yields a
	// setup error rather than a replay over every session).
	SessionID uuid.UUID
	// LoopID, when non-zero, narrows an event replay to that single loop; zero
	// replays the session's events plus every loop's events.
	LoopID uuid.UUID
	// From is where the backlog read begins: Beginning or FromSeq(n).
	From StartPos
	// Follow keeps the cursor live after the backlog drains (tailing new appends). A
	// backend that implements only the cold path returns a typed *FollowUnsupportedError
	// from Open rather than silently behaving as a cold cursor.
	Follow bool
}

// EventReplayer is the journal's read side: it opens an ordered cursor over a session's
// Enduring events. It is the narrow counterpart to SessionJournal (the write side) — a
// caller that only reads history depends on Open alone. The concrete implementation
// lives in a backend package (e.g. pkg/sessionstore over storage).
type EventReplayer interface {
	// Open binds a cursor over the session's events selected by req and positioned at
	// req.From.
	Open(ctx context.Context, req ReplayRequest) (EventCursor, error)
}

// EventCursor yields a session's Enduring events in sequence order. Next returns the
// next decoded event with its sequence, io.EOF once the backlog is drained (cold mode),
// or a typed error on a malformed/missing/corrupt record. Close releases the underlying
// reader; it is idempotent and safe to call after an error.
type EventCursor interface {
	// Next returns the next event and its sequence, or io.EOF when the cold backlog is
	// exhausted. A decode/read error fails secure: the cursor surfaces the typed error
	// rather than skipping or zero-valuing the record.
	Next(ctx context.Context) (event.Event, uint64, error)
	// Close tears down the reader. Idempotent: a second call is a no-op.
	Close() error
}

// FollowUnsupportedError is returned by Open when ReplayRequest.Follow is true and the
// backend implements only the cold (Follow:false) backlog read. Failing closed with a
// typed error is preferable to silently behaving as a cold cursor that EOFs at the
// current tip. It carries the log identifier the replay targeted.
type FollowUnsupportedError struct {
	Stream string
}

func (e *FollowUnsupportedError) Error() string {
	return "journal: live follow (Follow:true) is not implemented for " + strconv.Quote(e.Stream)
}
