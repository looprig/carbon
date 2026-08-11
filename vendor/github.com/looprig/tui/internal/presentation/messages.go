package presentation

import (
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// eventMsg carries one delivery pulled from the session-lifetime subscription. JournalSeq is
// retained across the initial restore barrier even though current reducers only consume ev.
type eventMsg struct {
	ev         event.Event
	journalSeq uint64
}

// subscribedMsg carries the session-lifetime EventStream established at startup
// (and re-established on /clear). On a non-nil err the TUI cannot observe the
// session at all, so Update commits a fatal error entry; on success it stores the
// stream and starts the continuous reader.
type subscribedMsg struct {
	sub EventStream
	err error
}

// subClosedMsg signals the subscription's Events channel closed. err is the typed
// termination cause: nil for an intentional Close (e.g. a /clear swap or quit), or
// a *hub.SubscriptionLossError for a hub-forced drop (egress overflow). It is the
// continuous reader's only terminal — there is no per-turn EOF anymore.
type subClosedMsg struct{ err error }

// submitResultMsg reports the outcome of a fire-and-forget Submit. On success it
// carries the loop-assigned inputID and the submitted blocks so Update can record
// the submit (RecordSubmit) — the queued affordance shows once the loop's
// InputQueued event arrives, and the authoritative user row is committed later from
// the TurnStarted/TurnFoldedInto Message (NOT optimistically at submit). A non-nil
// err is a non-fatal send failure Update surfaces as a faint error notice. It is a
// tea.Msg.
type submitResultMsg struct {
	inputID uuid.UUID
	blocks  []content.Block
	err     error
}

// compactResultMsg reports the immediate outcome of a manual compaction request.
// The command id correlates the request with later compaction events; a non-nil
// error is the only immediate user-visible outcome.
type compactResultMsg struct {
	commandID uuid.UUID
	err       error
}

// interruptResultMsg carries the outcome of an Interrupt call.
type interruptResultMsg struct {
	cancelled bool
	err       error
}

// reopenResultMsg carries the freshly opened replacement agent (nil on err) from a /clear
// handoff. The old agent has already been closed before this message is produced.
type reopenResultMsg struct {
	agent   Agent
	err     error
	handoff *reopenHandoff
	// warning is set when the replacement succeeded only after recovering from an earlier,
	// non-fatal failure (a session-browser resume rejected/failed and fell back to a fresh
	// session — see beginSessionResume). nil on an ordinary /clear or successful resume.
	// Meaningful only when err is nil; the caller surfaces it as a committed notice rather
	// than a fatal error.
	warning error
}

// closeForQuitResultMsg reports completion of the exactly-once replacement close deferred by
// ctrl+c during /clear. Update clears model ownership only after consuming this message.
type closeForQuitResultMsg struct {
	handoff *agentCloseHandoff
	err     error
}

// staleReopenCloseMsg reports bounded cleanup of a replacement rejected by the initial
// restore barrier.
type staleReopenCloseMsg struct {
	closing  *staleReopenClose
	closeErr error
}

// systemReadyMsg triggers the initial system "session ready" entry at startup.
type systemReadyMsg struct{}
