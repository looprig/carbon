package journal

import "context"

// RecordReplayer is the journal's FULL read side: it opens an ordered cursor over a
// session's log and surfaces EVERY record — events, commands, AND fences — in sequence
// order. It is the data seam the transcript export consumes: the narrower EventReplayer
// yields enduring events only and therefore DROPS every CommandRecord (the user's gate
// decisions). Reading the whole log in sequence instead yields events and commands
// interleaved in append/causal order — the merged stream the transcript builder needs.
// The concrete implementation lives in a backend package (same inputs as EventReplayer).
type RecordReplayer interface {
	// Open binds a cursor over the WHOLE session log (every record kind) positioned at
	// req.From. Only the cold path (Follow:false) need be implemented; Follow:true
	// returns a typed *FollowUnsupportedError, matching EventReplayer.
	Open(ctx context.Context, req ReplayRequest) (RecordCursor, error)
}

// RecordCursor yields a session's journal records in sequence order. Next returns the
// next decoded JournalRecord (an EventRecord, CommandRecord, or FenceRecord) with its
// sequence, io.EOF once the cold backlog is drained, or a typed error on a
// malformed/missing/corrupt record. Close releases the underlying reader; it is
// idempotent and safe to call after an error. It is the all-records counterpart to
// EventCursor (which yields events only).
type RecordCursor interface {
	// Next returns the next record and its sequence, or io.EOF when the cold backlog is
	// exhausted. A decode/read error fails secure: the cursor surfaces the typed error
	// rather than skipping or zero-valuing the record.
	Next(ctx context.Context) (JournalRecord, uint64, error)
	// Close tears down the reader. Idempotent: a second call is a no-op.
	Close() error
}
