package journal

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// NilJournalError reports that a JournalEventAppender was constructed over a nil
// SessionJournal — a composition-wiring bug. The checked constructor fails loud with
// this typed error rather than letting the nil surface as a panic at the first append.
type NilJournalError struct{}

func (*NilJournalError) Error() string {
	return "journal: JournalEventAppender requires a non-nil SessionJournal"
}

// catalogUpdater is the narrow seam the appender notifies AFTER a successful durable
// append so the derived session catalog can index the event (best-effort). It is a
// single-method interface (Interface Segregation): the appender depends on
// UpdateOnEvent alone, never the full Catalog. The *Catalog satisfies it; the nop
// default (nopCatalogUpdater) keeps existing wiring and headless mode unchanged.
//
// Its contract is best-effort by design: UpdateOnEvent must NEVER return a non-nil
// error (the catalog is derivable, so a failed index is logged and swallowed inside
// it). The appender therefore ignores its return, and a catalog failure can never
// affect the append's success/failure semantics.
type catalogUpdater interface {
	UpdateOnEvent(ctx context.Context, ev event.Event, seq uint64) error
}

// nopCatalogUpdater is the default catalogUpdater: it indexes nothing. It is the safe
// default so an appender constructed without a catalog (existing tests, headless mode)
// behaves exactly as before — no catalog, no extra I/O.
type nopCatalogUpdater struct{}

func (nopCatalogUpdater) UpdateOnEvent(context.Context, event.Event, uint64) error { return nil }

// JournalEventAppender adapts a SessionJournal (the write side) to the narrow
// "append one Enduring event" seam the session hub depends on. The hub holds an
// unexported eventAppender interface (AppendEvent(ctx, event.Event) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in via
// hub.WithAppender without the hub ever importing the journal package (Dependency
// Inversion). Beyond the journal it holds an optional catalog updater (nop by default):
// after a successful Append it notifies the catalog best-effort so the replay-free
// session index stays current. One responsibility: wrap the event in an EventRecord
// (which self-derives its subject from the event's scope+coordinates and its idempotency
// id from the EventID), append it, then best-effort index it.
type JournalEventAppender struct {
	journal SessionJournal
	catalog catalogUpdater
}

// AppenderOption configures a JournalEventAppender at construction. Applied in order
// over a defaults struct (nop catalog), so a later option overrides an earlier one.
type AppenderOption func(*JournalEventAppender)

// WithCatalog injects the catalog updater the appender notifies after a successful
// append (best-effort). A nil updater is ignored (the nop default is kept), so the
// appender owns its invariant — it never holds a nil catalog and never nil-derefs.
func WithCatalog(c catalogUpdater) AppenderOption {
	return func(a *JournalEventAppender) {
		if c != nil {
			a.catalog = c
		}
	}
}

// NewJournalEventAppender wraps journal as an event appender. It does NOT guard
// against a nil journal — use NewJournalEventAppenderChecked at the composition root
// where a wiring bug must fail loud. This unchecked form exists for call sites that
// have already validated the journal (and for the structural-satisfaction assertion).
func NewJournalEventAppender(journal SessionJournal, opts ...AppenderOption) *JournalEventAppender {
	a := &JournalEventAppender{journal: journal, catalog: nopCatalogUpdater{}}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// NewJournalEventAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring
// the failure to a nil-deref at the first append.
func NewJournalEventAppenderChecked(journal SessionJournal, opts ...AppenderOption) (*JournalEventAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return NewJournalEventAppender(journal, opts...), nil
}

// AppendEvent durably appends one Enduring event: it wraps ev in an EventRecord and
// calls the journal's Append, returning the underlying typed error unchanged (the hub
// maps it onto a SessionPersistenceFault — never swallowed). The EventRecord routes a
// session-scoped event to the session subject and a loop-scoped event to its loop
// event subject, and uses the event's EventID as the Nats-Msg-Id (idempotency). An
// Ephemeral event is never appended by the hub; if one were passed, the serializer's
// event.MarshalEvent fails closed inside Append, so this path stays fail-secure.
//
// It returns the assigned durable journal sequence so the hub can ride it on the LIVE
// delivery (event.Delivery.JournalSeq) — the sequence NEVER enters the persisted event
// codec. ONLY after the durable append succeeds does it best-effort notify the catalog
// (with the same sequence) so the replay-free session index stays current. The catalog
// update is the soft tail: its error is swallowed inside UpdateOnEvent and cannot change
// this method's return — the durable append stays strict, the catalog is derivable. On an
// append failure the catalog is NOT touched (the event did not durably land) and seq 0 is
// returned alongside the error.
//
// When the underlying journal additionally satisfies IdempotentJournal (the optional
// dedup seam), a redelivered event whose EventID already names a durable record is
// detected there and reported as AppendResult.Appended=false; this method then
// returns the ORIGINAL sequence without re-notifying the catalog — a duplicate was
// already indexed by its first, genuine append, so republishing it a second time
// would be redundant. A journal that does not implement the optional interface
// behaves exactly as before (every successful Append notifies the catalog).
func (a *JournalEventAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	seq, _, err := a.AppendEventResult(ctx, ev)
	return seq, err
}

// AppendEventResult is the result-preserving event append seam used by the Hub
// trusted publication path. Appended is true only when this call created a new
// durable frame; an identical idempotent retry returns the original sequence and
// Appended=false. The legacy AppendEvent method above deliberately discards only
// this boolean so existing callers retain their API and error behavior.
func (a *JournalEventAppender) AppendEventResult(ctx context.Context, ev event.Event) (uint64, bool, error) {
	rec := NewEventRecord(ev)
	if idem, ok := a.journal.(IdempotentJournal); ok {
		result, err := idem.AppendIdempotent(ctx, rec)
		if err != nil {
			return 0, false, err
		}
		if !result.Appended {
			return result.Sequence, false, nil
		}
		_ = a.catalog.UpdateOnEvent(ctx, ev, result.Sequence)
		return result.Sequence, true, nil
	}
	seq, err := a.journal.Append(ctx, rec)
	if err != nil {
		return 0, false, err
	}
	// Best-effort, post-success: UpdateOnEvent never returns a non-nil error by
	// contract, so the catalog can never fail the append. The return is ignored.
	_ = a.catalog.UpdateOnEvent(ctx, ev, seq)
	return seq, true, nil
}

// JournalCommandAppender adapts a SessionJournal to the narrow "append one command"
// seam the session depends on for the intent log. The session holds an unexported
// commandAppender interface (AppendCommand(ctx, CommandRecord) error); this type
// satisfies it structurally, so the composition root (Phase 10) wires it in without
// the session importing journal internals beyond the CommandRecord constructor
// (Dependency Inversion). It carries no state beyond the journal — one method, one
// responsibility: append a CommandRecord (which the SESSION built with the dispatch
// target loopID, since a command — Interrupt/Shutdown especially — does not carry its
// own routing) and return the underlying typed error.
//
// Unlike the event appender, the SESSION treats this seam as AUDIT-ONLY: a non-nil
// error is logged and the dispatch proceeds (losing a command record must never block
// the user's action). This façade itself never swallows — it returns the journal's
// error unchanged so the session owns the log-and-proceed decision.
type JournalCommandAppender struct {
	journal SessionJournal
}

// NewJournalCommandAppender wraps journal as a command appender. Like the event
// appender's unchecked form, it does NOT guard against a nil journal — use
// NewJournalCommandAppenderChecked at the composition root where a wiring bug must
// fail loud.
func NewJournalCommandAppender(journal SessionJournal) *JournalCommandAppender {
	return &JournalCommandAppender{journal: journal}
}

// NewJournalCommandAppenderChecked is the fail-loud constructor for the composition
// root: it returns a typed *NilJournalError if journal is nil rather than deferring the
// failure to a nil-deref at the first append.
func NewJournalCommandAppenderChecked(journal SessionJournal) (*JournalCommandAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalCommandAppender{journal: journal}, nil
}

// AppendCommand appends one intent-log command record: it calls the journal's Append
// with the session-built CommandRecord and returns the underlying typed error
// unchanged (the session logs+proceeds — audit-only — never faulting the session on a
// command-append failure). The CommandRecord routes to the target loop's command
// (intent-log) subject and uses the command's CommandID as the Nats-Msg-Id
// (idempotency). The returned sequence is discarded — the session needs only the
// success/failure signal.
func (a *JournalCommandAppender) AppendCommand(ctx context.Context, rec CommandRecord) error {
	_, err := a.journal.Append(ctx, rec)
	return err
}

// JournalGateAppender adapts a SessionJournal to the session gate directory's
// strict durable append seam. GatePreparedRecord is appended as a private record;
// GateOpened and GateResolved are public Enduring events and are wrapped in
// EventRecord exactly like JournalEventAppender.
type JournalGateAppender struct {
	journal SessionJournal
}

// NewJournalGateAppender wraps journal as a gate appender. Like the other unchecked
// appender constructors, it expects a validated journal; use
// NewJournalGateAppenderChecked at composition roots.
func NewJournalGateAppender(journal SessionJournal) *JournalGateAppender {
	return &JournalGateAppender{journal: journal}
}

// NewJournalGateAppenderChecked fails loud on nil journal so composition wiring bugs
// surface at construction instead of the first gate operation.
func NewJournalGateAppenderChecked(journal SessionJournal) (*JournalGateAppender, error) {
	if journal == nil {
		return nil, &NilJournalError{}
	}
	return &JournalGateAppender{journal: journal}, nil
}

func (a *JournalGateAppender) AppendGatePrepared(ctx context.Context, rec GatePreparedRecord) error {
	_, err := a.journal.Append(ctx, rec)
	return err
}

func (a *JournalGateAppender) AppendGateOpened(ctx context.Context, ev event.GateOpened) error {
	_, err := a.journal.Append(ctx, NewEventRecord(ev))
	return err
}

func (a *JournalGateAppender) AppendGateResolved(ctx context.Context, ev event.GateResolved) error {
	_, err := a.journal.Append(ctx, NewEventRecord(ev))
	return err
}
