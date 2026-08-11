package hub

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// eventAppender is the hub's narrow durable-write seam: append one Enduring event to
// the session's durable journal, returning the assigned durable journal sequence and a
// typed error if the append did not commit. The hub depends only on this one method
// (Interface Segregation) — never on the full SessionJournal, its stream management, or
// the record wrappers. The composition root (Phase 10) wires a real adapter over
// SessionJournal; the default is the nop appender so existing hub callers and
// headless/no-persistence mode are unchanged.
//
// AppendEvent must be append-BEFORE-apply: the hub calls it before applying the event
// to hub state, outside the hub lock (no I/O under the lock), and treats any non-nil
// error as fail-secure — the live event is not delivered and a SessionPersistenceFault
// is raised. The returned sequence rides only the LIVE delivery (event.Delivery.JournalSeq);
// it never enters the persisted event codec.
type eventAppender interface {
	AppendEvent(ctx context.Context, ev event.Event) (uint64, error)
}

// eventAppenderResult is the OPTIONAL extension of eventAppender an injected appender
// may additionally satisfy to report whether ITS OWN durable append produced a NEW
// frame (Appended=true) or deduplicated an already-durable retry (Appended=false — the
// underlying durable journal recognized the event's idempotency id as already indexed,
// e.g. a redelivered checked process-lifecycle publish). The hub type-asserts h.appender
// for it before applying and delivering an Enduring event's live transition: on
// Appended=false the publish is treated as an already-completed no-op — no sessionState
// mutation, no subscriber broadcast — because the event was already applied and
// delivered by its original, genuinely new append. This preserves the OLD single-method
// appender surface unweakened (Interface Segregation): an appender written before this
// extension existed, or one that simply chooses not to implement it, keeps satisfying
// plain eventAppender and is treated as if every successful append is new — exactly its
// pre-extension behavior.
type eventAppenderResult interface {
	eventAppender
	AppendEventResult(ctx context.Context, ev event.Event) (seq uint64, appended bool, err error)
}

// nopEventAppender is the default appender wired into a hub built without an injected
// one. It persists nothing and never fails, so the hub's fail-secure branch is never
// taken in no-persistence mode — every Enduring event is delivered exactly as before
// the durable tap landed. Headless runs and existing tests use this.
type nopEventAppender struct{}

func (nopEventAppender) AppendEvent(context.Context, event.Event) (uint64, error) { return 0, nil }

// AppendEventResult reports Appended=true: the nop appender persists nothing and
// deduplicates nothing, so every publish it backs is treated as new — headless mode
// keeps delivering every Enduring event exactly as it did before this extension
// existed.
func (nopEventAppender) AppendEventResult(context.Context, event.Event) (uint64, bool, error) {
	return 0, true, nil
}

// Option configures an optional hub dependency at construction. The bare
// New(sessionID) installs the nop appender, a real-clock/real-uuid Factory, and the
// nop fault reporter; an Option overrides one of them. This keeps existing callers
// (hub.New(id)) working while the composition root injects the durable trio.
type Option func(*Hub)

// WithCommitObserver installs a callback invoked after the primary event and any
// derived session event have durably committed, but before either event is fanned
// out to subscribers. It is an observation hook: the callback must not mutate
// durable state or return an error. The session runtime uses it to keep its
// bounded live status view ordered before consumers can observe the same
// committed terminal.
func WithCommitObserver(observer func(event.Event)) Option {
	return func(h *Hub) {
		if observer != nil {
			h.commitObserver = observer
		}
	}
}

// sessionIdleBoundary is the narrow internal collaborator around a derived idle edge.
// The hub discovers it structurally from its already-injected FaultReporter, so no public
// hook/subscriber/watcher API is exposed.
type sessionIdleBoundary interface {
	CommitSessionIdle(context.Context, event.SessionIdle, func() error) error
}

type sessionActivationObserver interface{ SessionActivated() }

type immediateSessionIdleBoundary struct{}

func (immediateSessionIdleBoundary) CommitSessionIdle(_ context.Context, _ event.SessionIdle, commit func() error) error {
	return commit()
}

// withSessionIdleBoundary is package-private and exists only for focused hub tests. Live
// composition discovers the session capability from FaultReporter in New.
func withSessionIdleBoundary(boundary sessionIdleBoundary) Option {
	return func(h *Hub) {
		if boundary != nil {
			h.idleBoundary = boundary
		}
	}
}

// WithAppender injects the durable event appender (the composition root's adapter over
// SessionJournal). A nil appender is ignored (the nop default stays installed) so a
// caller can never accidentally null out the field and skip the nil-safe publish path.
func WithAppender(a eventAppender) Option {
	return func(h *Hub) {
		if a != nil {
			h.appender = a
		}
	}
}

// WithFactory injects the event Factory the hub mints EventID+CreatedAt from for the
// session events it SYNTHESIZES (SessionActive/SessionIdle/SessionStopped). A nil
// factory is ignored (the default real-clock Factory stays). The session-scoped
// events the hub derives currently carry no header identity; this factory stamps them
// so the journal sees a stable idempotency key and creation time.
func WithFactory(f *event.Factory) Option {
	return func(h *Hub) {
		if f != nil {
			h.factory = f
		}
	}
}

// WithFaultReporter injects the fail-secure escalation seam invoked when a required
// durable append fails. A nil reporter is ignored (the nop default stays). The Session
// implements it to reject new Submit/NewLoop and wake WaitIdle waiters.
func WithFaultReporter(r FaultReporter) Option {
	return func(h *Hub) {
		if r != nil {
			h.reporter = r
		}
	}
}
