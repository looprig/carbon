package journal

import "context"

// SessionJournal is the single serialized writer for one session's durable log.
// Append encodes a JournalRecord's payload, persists it under single-writer fencing,
// and returns the assigned sequence. It is the only thing that writes a session's
// log; callers funnel every event, command, and fence through it so the log stays a
// totally-ordered, gap-free record of the session.
//
// The interface is intentionally narrow (one method): a caller that only needs to
// persist a record must not depend on any log-management surface. The concrete
// implementation lives in a backend package (e.g. pkg/sessionstore over storage),
// wired at the composition root — this package owns only the contract.
type SessionJournal interface {
	// Append serializes rec, persists it under the next expected sequence, and
	// returns the assigned sequence. ctx bounds the caller's willingness to wait; the
	// implementation additionally carries a per-append deadline independent of ctx so
	// one stuck call cannot wedge the serialized writer forever. Appends are totally
	// ordered: the returned sequences are strictly monotonic across calls.
	Append(ctx context.Context, rec JournalRecord) (seq uint64, err error)
}
