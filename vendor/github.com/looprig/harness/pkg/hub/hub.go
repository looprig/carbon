// Package hub implements the session-level event fan-in: a publish/subscribe hub
// with a federated-quiescence model. Loops publish events through the narrow
// eventPublisher contract; consumers (TUI/CLI now, a durable journal later)
// subscribe with an EventFilter. The hub aggregates loop activity into a single
// sessionState so a headless run can WaitIdle without any session goroutine.
//
// Concurrency contract: mu guards the subscriber set, sessionState
// (active/phase), and WaitIdle waiter registry. activityMu serializes each
// active-set mutation with its derived durable edge but never guards state itself.
// The critical section under mu only applies state and copies subscribers; mu is
// always released before durable I/O, workspace boundaries, reporting, or delivery.
package hub

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// Hub is the session's event fan-in. It is owned by Session; loops see only
// its PublishEvent method (the narrow eventPublisher), and consumers see only
// SubscribeEvents. ExpectTurn/CancelExpectTurn/StopSession are session-owned (the
// session calls them; loops depend on the eventPublisher interface, which excludes
// them).
type Hub struct {
	sessionID uuid.UUID

	// publishMu is the construction-abort admission seal for durable publication.
	// AbortSession closes admission atomically and returns publishDrained so the
	// session can retain journal ownership until every already-admitted publisher
	// has left the appender path.
	publishMu      sync.Mutex
	publishAborted error
	publishes      int
	publishDrained chan struct{}

	// activityMu orders every active-set mutation with its derived durable edge.
	// It may be held across I/O, unlike mu; no state is read or written under this
	// lock alone. A failed Hustle Idle→Active acquisition transfers ownership to
	// its partial lease until Release rolls back the uncommitted insertion. A native
	// loop may reserve it before acquiring its first-step checkpoint reader; the
	// reservation's opaque publisher commits the matching TurnStarted without
	// re-locking. Generic publication never discovers or consumes reservations.
	activityMu sync.Mutex

	// mu guards subs, state, and waiters together. One lock keeps the
	// subscriber-set snapshot consistent with the active/phase transition.
	mu      sync.RWMutex
	subs    map[*EventSubscription]struct{}
	state   sessionState
	waiters map[chan error]struct{}
	// waiterFailure is a sticky fault observed by every WaitIdle caller until the
	// owning recoverable operation clears its exact generation token. Later faults
	// overwrite both value and token, so stale recovery cannot erase them.
	waiterFailure           error
	waiterFailureToken      uint64
	waiterFailureGeneration uint64
	// Idle boundary generations close the WaitIdle fast-path window between an
	// in-memory Active→Idle transition and its native durable completion. A generation
	// prevents an older overlapping boundary from clearing a newer pending edge.
	idleBoundaryGeneration uint64
	idleBoundaryPending    uint64

	// The durable-tap trio (Dependency Inversion: all three are interfaces/seams
	// injected via Option; the bare New installs nop/real-clock defaults so existing
	// callers and headless mode are unchanged). They are immutable after construction
	// and read without the lock — appender.AppendEvent is the durable write the hub
	// runs OUTSIDE mu (no I/O under the lock); factory mints headers for synthesized
	// session events; reporter is the fail-secure escalation seam.
	appender       eventAppender
	factory        *event.Factory
	reporter       FaultReporter
	idleBoundary   sessionIdleBoundary
	commitObserver func(event.Event)
}

// New builds an idle hub for sessionID. The returned hub has no subscribers and a
// zero-value (idle, empty) sessionState. Without options it installs the durable-tap
// defaults: a nop appender (persists nothing, never fails — headless/no-persistence
// mode), a real-clock/real-uuid event Factory, and a nop fault reporter. The
// composition root (Phase 10) injects the real trio via WithAppender/WithFactory/
// WithFaultReporter.
func New(sessionID uuid.UUID, opts ...Option) *Hub {
	publishDrained := make(chan struct{})
	close(publishDrained)
	h := &Hub{
		sessionID:      sessionID,
		subs:           make(map[*EventSubscription]struct{}),
		state:          newSessionState(),
		waiters:        make(map[chan error]struct{}),
		appender:       nopEventAppender{},
		factory:        event.NewFactory(uuid.New, time.Now),
		reporter:       nopFaultReporter{},
		idleBoundary:   immediateSessionIdleBoundary{},
		publishDrained: publishDrained,
	}
	for _, opt := range opts {
		opt(h)
	}
	if boundary, ok := h.reporter.(sessionIdleBoundary); ok {
		h.idleBoundary = boundary
	}
	return h
}

// SubscribeEvents registers a new subscription with the given filter and returns
// its handle. The subscriber reads ev from sub.Events(); it must Close the
// subscription when done.
func (h *Hub) SubscribeEvents(filter event.EventFilter) (*EventSubscription, error) {
	sub := newSubscription(filter, h.unsubscribe)
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub, nil
}

// unsubscribe removes a subscription from the set under the write lock. It is the
// subscription's onClose callback, fired on the first terminal (Close or fail), so
// a torn-down subscription does not linger in the fan-out set. Idempotent: a
// second delete of an absent key is a no-op.
func (h *Hub) unsubscribe(sub *EventSubscription) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// PublishEvent is the durable tap (design "Hub tap algorithm"): for an Enduring
// event it appends BEFORE applying it to hub state (durable-first, fail-secure),
// then applies the quiescence transition (which may DERIVE a session event D), then
// mints+appends D, then delivers the triggering event followed by D — in causal
// order. The precise ordering, honoring the lock rule (no I/O under the hub lock):
//
//  1. Ephemeral event: never persisted — fan out only (the unchanged path).
//  2. Enduring event: appender.AppendEvent(ev) OUTSIDE the lock — via
//     appendEventResult, which also asks an appender that implements the optional
//     eventAppenderResult extension whether the append durably persisted a NEW frame.
//     On error → ReportFault, deliver NOTHING, return (do not apply a transition for
//     an event that did not persist). On a deduplicated retry (Appended=false) →
//     apply NOTHING and deliver NOTHING; the event was already applied and delivered
//     by its original, genuinely new append, so this call reports the same success
//     without repeating either.
//  3. Under the lock: apply ev's active/phase mutation, which may derive D
//     (SessionActive/SessionIdle); snapshot the subscriber set. Unlock.
//  4. If D was derived: mint it (Factory: EventID+CreatedAt) and append it OUTSIDE
//     the lock. On error → ReportFault, deliver NEITHER ev nor D, return.
//  5. Deliver live, outside the lock (existing fan-out policy): ev, then D. If D was
//     a SessionIdle (the Active->Idle edge), wake WaitIdle waiters AFTER its durable
//     append — never before (a failed append must not falsely report idle).
//
// Subscriber delivery never blocks a publisher: each send into the bounded egress
// channel is non-blocking. Activity-affecting publishers do serialize with other
// activity transitions until their derived durable edge completes. It returns nil
// even with no subscribers (the headless case) — the sessionState transition still
// runs.
//
// After SessionStopped, the event is still appended+delivered (filtered) but no
// longer mutates active/phase and never derives SessionIdle/SessionActive.
func (h *Hub) PublishEvent(ctx context.Context, ev event.Event) error {
	return h.publishEvent(ctx, ev, false)
}

// PublishEventChecked is the construction-transaction variant of PublishEvent. It keeps
// the same durable-first reporting semantics but also returns the persistence fault to a
// caller that must not make an object reachable unless its creation event committed.
func (h *Hub) PublishEventChecked(ctx context.Context, ev event.Event) error {
	return h.publishEvent(ctx, ev, true)
}

func (h *Hub) publishEvent(ctx context.Context, ev event.Event, checked bool) error {
	return h.publishEventWithActivity(ctx, ev, checked, false)
}

func (h *Hub) publishEventWithActivity(ctx context.Context, ev event.Event, checked, activityReserved bool) error {
	_, err := h.publishEventWithActivityResult(ctx, ev, checked, activityReserved)
	return err
}

// publishEventWithActivityResult reports whether the primary event reached its
// durable append independently from any later failure stamping or appending the
// derived session activity edge.
func (h *Hub) publishEventWithActivityResult(ctx context.Context, ev event.Event, checked, activityReserved bool) (bool, error) {
	if err := validatePublicPublication(ev); err != nil {
		return false, err
	}
	if err := h.beginPublish(); err != nil {
		if checked {
			return false, err
		}
		return false, nil
	}
	defer h.finishPublish()
	if _, mutatesActivity := activeMutation(ev); mutatesActivity && !activityReserved {
		h.activityMu.Lock()
		defer h.activityMu.Unlock()
	}
	// (1)+(2) Ephemeral: no append, seq stays 0. Enduring: append before apply,
	// fail-secure; capture the durable sequence to ride the live delivery.
	var seq uint64
	if ev.Class() == event.Enduring {
		s, appended, err := h.appendEventResult(ctx, ev)
		if err != nil {
			fault := &SessionPersistenceFault{Event: ev, Cause: err}
			h.reporter.ReportFault(ctx, fault)
			if checked {
				return false, fault
			}
			return false, nil
		}
		if !appended {
			// Deduplicated retry: the underlying journal already indexed this event's
			// idempotency id under a genuinely new, earlier append, which already
			// applied its state mutation and delivered it live. Report the same success
			// this call would have reported had it been the original — but apply
			// nothing and broadcast nothing a second time.
			return true, nil
		}
		seq = s
	}
	committed := true

	// (3) Apply under the lock; derive the at-most-one session event D; snapshot subs.
	subs, derived, idleGeneration := h.applyAndSnapshot(ev)
	if _, active := derived.(event.SessionActive); active {
		if observer, ok := h.idleBoundary.(sessionActivationObserver); ok {
			observer.SessionActivated()
		}
	}

	// (4) Mint + durably append a derived session event before it (or ev) goes live.
	// On failure neither ev nor D is delivered, and the fault is raised. D carries its
	// OWN append sequence.
	var derivedSeq uint64
	if derived != nil {
		stamped, err := h.factory.Stamp(derived.EventHeader())
		if err != nil {
			fault := &SessionPersistenceFault{Event: derived, Cause: err}
			h.reporter.ReportFault(ctx, fault)
			if checked {
				return committed, fault
			}
			return committed, nil
		}
		derived = withHeader(derived, stamped)
		// SessionIdle is committed through the narrow native boundary so it can acquire
		// the workspace permit before this append and finish its accepted snapshot before
		// WaitIdle acknowledges. The continuation retains hub ownership of append+fanout.
		if idle, ok := derived.(event.SessionIdle); ok {
			commit := func() error {
				ds, appendErr := h.appender.AppendEvent(ctx, idle)
				if appendErr != nil {
					fault := &SessionPersistenceFault{Event: idle, Cause: appendErr}
					h.reporter.ReportFault(ctx, fault)
					return fault
				}
				h.observeCommit(ev)
				h.deliver(subs, ev, seq)
				h.deliver(subs, idle, ds)
				return nil
			}
			err = h.idleBoundary.CommitSessionIdle(ctx, idle, commit)
			h.completeIdleBoundary(idleGeneration, err == nil)
			if err != nil {
				if checked {
					return committed, err
				}
				return committed, nil
			}
			return committed, nil
		}
		ds, err := h.appender.AppendEvent(ctx, derived)
		if err != nil {
			fault := &SessionPersistenceFault{Event: derived, Cause: err}
			h.reporter.ReportFault(ctx, fault)
			if checked {
				return committed, fault
			}
			return committed, nil
		}
		derivedSeq = ds
	}

	// (5) Deliver live in causal order, then wake idle waiters AFTER the durable
	// append of the SessionIdle edge.
	h.observeCommit(ev)
	h.deliver(subs, ev, seq)
	if derived != nil {
		h.deliver(subs, derived, derivedSeq)
		h.signalIdleIfEdge(derived)
	}
	return committed, nil
}

func (h *Hub) observeCommit(ev event.Event) {
	if h.commitObserver != nil {
		h.commitObserver(ev)
	}
}

// PublishInternalEventChecked durably appends one recognized private
// hustle-lifecycle or permission-review event. It deliberately bypasses
// quiescence mutation, workspace idle boundaries, and subscriber delivery:
// the separate hustle activity lease owns the blocking state, while this
// method owns only the private audit record.
func (h *Hub) PublishInternalEventChecked(ctx context.Context, ev event.Event) error {
	if err := h.validateInternalPublication(ev); err != nil {
		return err
	}
	if err := h.beginPublish(); err != nil {
		return err
	}
	defer h.finishPublish()

	if _, err := h.appender.AppendEvent(ctx, ev); err != nil {
		fault := &SessionPersistenceFault{Event: ev, Cause: err}
		h.reporter.ReportFault(ctx, fault)
		return fault
	}
	return nil
}

// isInternalAuditEventType reports whether ev is one of the recognized
// private audit-only event types: the ONLY types PublishInternalEventChecked
// may durably append (validateInternalPublication's allowlist), and
// therefore also the ones validatePublicPublication's denylist must reject
// on the public-only path. Both switches are derived from this single
// function so the allow/deny pair cannot silently drift apart — exactly the
// gap that let PermissionReviewStarted/Completed ship reachable only through
// the public path (where they were always rejected as Internal-visibility)
// and never added to the internal allowlist that would have actually
// accepted them. It matches both value and pointer forms for every type,
// mirroring the pre-existing Hustle lifecycle coverage — even though
// review_adapter.go currently only ever publishes the value form, matching
// both here keeps this helper (and validatePublicPublication's denylist,
// which has always matched both forms) correct regardless of which form a
// caller uses.
func isInternalAuditEventType(ev event.Event) bool {
	switch ev.(type) {
	case event.HustleStarted, *event.HustleStarted,
		event.HustleCompleted, *event.HustleCompleted,
		event.HustleFailed, *event.HustleFailed,
		event.PermissionReviewStarted, *event.PermissionReviewStarted,
		event.PermissionReviewCompleted, *event.PermissionReviewCompleted:
		return true
	default:
		return false
	}
}

// appendEventResult calls the injected appender's optional Appended-reporting
// extension (eventAppenderResult) when h.appender implements it, and otherwise falls
// back to the plain AppendEvent surface, treating every successful append as new
// (appended=true) — the backward-compatible default for every appender written before
// the extension existed, and for one that simply does not implement it.
func (h *Hub) appendEventResult(ctx context.Context, ev event.Event) (seq uint64, appended bool, err error) {
	if r, ok := h.appender.(eventAppenderResult); ok {
		return r.AppendEventResult(ctx, ev)
	}
	seq, err = h.appender.AppendEvent(ctx, ev)
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func validatePublicPublication(ev event.Event) error {
	if nilEvent(ev) {
		return &PublishBoundaryError{Reason: PublishBoundaryNilEvent}
	}
	if ev.Visibility() != event.Public {
		return &PublishBoundaryError{
			Reason:    PublishBoundaryVisibility,
			EventType: fmt.Sprintf("%T", ev),
		}
	}
	if isInternalAuditEventType(ev) {
		return &PublishBoundaryError{
			Reason:    PublishBoundaryType,
			EventType: fmt.Sprintf("%T", ev),
		}
	}
	return nil
}

func (h *Hub) validateInternalPublication(ev event.Event) error {
	if nilEvent(ev) {
		return &PublishBoundaryError{Reason: PublishBoundaryNilEvent}
	}
	eventType := fmt.Sprintf("%T", ev)
	if ev.Visibility() != event.Internal {
		return &PublishBoundaryError{Reason: PublishBoundaryVisibility, EventType: eventType}
	}
	if ev.Class() != event.Enduring {
		return &PublishBoundaryError{Reason: PublishBoundaryClass, EventType: eventType}
	}
	if ev.EventHeader().SessionID != h.sessionID {
		return &PublishBoundaryError{Reason: PublishBoundarySession, EventType: eventType}
	}
	if !isInternalAuditEventType(ev) {
		return &PublishBoundaryError{Reason: PublishBoundaryType, EventType: eventType}
	}
	if err := event.ValidateEvent(ev); err != nil {
		return &PublishBoundaryError{Reason: PublishBoundaryInvalid, EventType: eventType, Cause: err}
	}
	return nil
}

func nilEvent(ev event.Event) bool {
	if ev == nil {
		return true
	}
	value := reflect.ValueOf(ev)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func (h *Hub) beginPublish() error {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	if h.publishAborted != nil {
		return &SessionAbortedError{Cause: h.publishAborted}
	}
	if h.publishes == 0 {
		h.publishDrained = make(chan struct{})
	}
	h.publishes++
	return nil
}

func (h *Hub) finishPublish() {
	h.publishMu.Lock()
	h.publishes--
	if h.publishes == 0 {
		close(h.publishDrained)
	}
	h.publishMu.Unlock()
}

// applyAndSnapshot is the locked critical section of a publish: it applies the
// event's active/phase mutation (if any) and copies the subscriber slice. It
// returns the snapshot and the at-most-one RAW (unstamped) derived event to mint +
// append + deliver after ev. No I/O happens here — appending the derived event and
// waking waiters are the caller's job, outside the lock.
func (h *Hub) applyAndSnapshot(ev event.Event) ([]*EventSubscription, event.Event, uint64) {
	mutate, mutates := activeMutation(ev)
	if !mutates {
		// Non-mutating publish: read lock, no applyActivity.
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.snapshotSubsLocked(), nil, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// applyActivity is a no-op (returns nil, never mutates) once SessionStopped, so
	// a post-stop mutating event delivers without flipping phase. The derived event
	// is returned RAW; the caller mints + appends it before delivery (and wakes
	// WaitIdle waiters only after a SessionIdle edge's durable append succeeds).
	derived := h.state.applyActivity(h.sessionID, func() { mutate(&h.state) })
	var idleGeneration uint64
	if _, ok := derived.(event.SessionIdle); ok {
		idleGeneration = h.markIdleBoundaryLocked()
	}
	return h.snapshotSubsLocked(), derived, idleGeneration
}

// snapshotSubsLocked copies the subscriber set into a slice. The caller must hold
// mu (read or write). Delivery iterates the copy outside the lock.
func (h *Hub) snapshotSubsLocked() []*EventSubscription {
	if len(h.subs) == 0 {
		return nil
	}
	out := make([]*EventSubscription, 0, len(h.subs))
	for sub := range h.subs {
		out = append(out, sub)
	}
	return out
}

// activeMutation returns the sessionState mutation an event implies and whether it
// mutates at all. The mutating events:
//   - TurnStarted: add {loop, LoopID}; if Cause.LoopID != 0, also remove its
//     {wake} (the hand-back release; the loop key is added in the same step).
//   - LoopIdle: remove {loop, LoopID}.
//   - TurnFoldedInto / InputCancelled with Cause.LoopID != 0: remove {wake}.
//
// Every other event is non-mutating (token firehose, StepDone, tool/gate, the
// session events themselves, and terminals).
func activeMutation(ev event.Event) (func(*sessionState), bool) {
	switch e := ev.(type) {
	case event.TurnStarted:
		loopID := e.LoopID
		wake := e.Cause.LoopID
		return func(s *sessionState) {
			if !wake.IsZero() {
				s.remove(activityKey{kind: kindWake, id: wake})
			}
			s.add(activityKey{kind: kindLoop, id: loopID})
		}, true
	case event.LoopIdle:
		loopID := e.LoopID
		return func(s *sessionState) {
			s.remove(activityKey{kind: kindLoop, id: loopID})
		}, true
	case event.TurnFoldedInto:
		if e.Cause.LoopID.IsZero() {
			return nil, false
		}
		wake := e.Cause.LoopID
		return func(s *sessionState) { s.remove(activityKey{kind: kindWake, id: wake}) }, true
	case event.InputCancelled:
		if e.Cause.LoopID.IsZero() {
			return nil, false
		}
		wake := e.Cause.LoopID
		return func(s *sessionState) { s.remove(activityKey{kind: kindWake, id: wake}) }, true
	default:
		return nil, false
	}
}

// deliver fans one event out to a snapshot of subscribers, OUTSIDE the lock, wrapping
// it with its durable journal sequence seq (0 for Ephemeral, the append sequence for
// Enduring) in an event.Delivery. Per subscriber it applies the declared-interest
// filter (ShouldDeliver) to the UNWRAPPED event, then a non-blocking send into the
// bounded egress channel. On overflow the class-aware
// policy applies: an Ephemeral event is dropped for that subscriber; an Enduring
// event fails that subscription with a typed loss error (never silently dropped),
// and delivery continues to other subscribers. It never blocks.
func (h *Hub) deliver(subs []*EventSubscription, ev event.Event, seq uint64) {
	for _, sub := range subs {
		if !event.ShouldDeliver(sub.filter, ev) {
			continue
		}
		switch sub.trySend(event.Delivery{Event: ev, JournalSeq: seq}) {
		case sendDelivered, sendClosed:
			// Delivered, or the subscription is already torn down (a Close/fail
			// racing this snapshot) — skip it either way.
			continue
		case sendFull:
			if ev.Class() == event.Ephemeral {
				continue // droppable: reconstructable from a later authoritative event
			}
			// Enduring overflow: fail this subscription, keep delivering to others.
			sub.fail(&SubscriptionLossError{DroppedClass: ev.Class()})
		}
	}
}

// ExpectTurn takes a {wake, subagentLoopID} token at subagent spawn so a finished
// subagent's in-flight hand-back cannot empty active and fire a false SessionIdle.
// It derives SessionActive if the session was idle. It is exported for the session
// (its sole caller); loops depend only on the narrow eventPublisher interface,
// which excludes it, so a loop can never reach it.
//
// There is no triggering EVENT here (the wake token is hub-internal), so only the
// derived session event is durable: it is minted + appended OUTSIDE the lock before
// delivery, fail-secure (a failed append delivers nothing and raises a fault).
func (h *Hub) ExpectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	h.mu.Lock()
	derived := h.state.applyActivity(h.sessionID,
		func() { h.state.add(activityKey{kind: kindWake, id: subagentLoopID}) })
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()
	if _, active := derived.(event.SessionActive); active {
		if observer, ok := h.idleBoundary.(sessionActivationObserver); ok {
			observer.SessionActivated()
		}
	}
	h.appendAndDeliverDerived(ctx, subs, derived, 0)
}

// CancelExpectTurn releases a {wake, subagentLoopID} token when its hand-back is
// rejected or explicitly discarded. It derives SessionIdle if this emptied active.
// Exported for the session only (see ExpectTurn).
func (h *Hub) CancelExpectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	h.mu.Lock()
	derived := h.state.applyActivity(h.sessionID,
		func() { h.state.remove(activityKey{kind: kindWake, id: subagentLoopID}) })
	var idleGeneration uint64
	if _, ok := derived.(event.SessionIdle); ok {
		idleGeneration = h.markIdleBoundaryLocked()
	}
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()
	h.appendAndDeliverDerived(ctx, subs, derived, idleGeneration)
}

// HustleActivityLease owns one blocking hustle entry in the hub's quiescence set.
// Its concrete return type is intentional: sessionruntime adapts it to the narrow
// controller-owned lease interface without weakening Go's invariant return types.
type HustleActivityLease struct {
	hub                     *Hub
	key                     activityKey
	partial                 bool
	holdsActivityTransition bool
	acquisitionErr          error
	releaseOnce             sync.Once
	releaseErr              error
}

type hustleActivityInsertion struct {
	key     activityKey
	subs    []*EventSubscription
	derived event.Event
}

// AcquireHustleActivity inserts runID as blocking session work. The returned lease
// removes exactly that entry. If the Idle->Active edge cannot be durably committed,
// a non-nil partial lease is returned with the fault so the caller can silently
// roll back the in-memory insertion.
func (h *Hub) AcquireHustleActivity(ctx context.Context, runID hustle.RunID) (*HustleActivityLease, error) {
	id := uuid.UUID(runID)
	if id.IsZero() {
		return nil, &HustleActivityError{Reason: HustleActivityInvalidRunID, RunID: runID}
	}
	if err := h.beginPublish(); err != nil {
		return nil, err
	}
	defer h.finishPublish()

	h.activityMu.Lock()
	insertion, err := h.insertHustleActivity(runID, id)
	if err != nil {
		h.activityMu.Unlock()
		return nil, err
	}
	lease := &HustleActivityLease{hub: h, key: insertion.key}
	if _, active := insertion.derived.(event.SessionActive); active {
		if observer, ok := h.idleBoundary.(sessionActivationObserver); ok {
			observer.SessionActivated()
		}
	}
	if err := h.appendAndDeliverDerivedChecked(ctx, insertion.subs, insertion.derived, 0); err != nil {
		lease.partial = true
		lease.holdsActivityTransition = true
		lease.acquisitionErr = err
		return lease, err
	}
	h.activityMu.Unlock()
	return lease, nil
}

func (h *Hub) insertHustleActivity(runID hustle.RunID, id uuid.UUID) (hustleActivityInsertion, error) {
	key := activityKey{kind: kindHustle, id: id}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.phase == SessionStopped {
		return hustleActivityInsertion{}, &HustleActivityError{Reason: HustleActivityStopped, RunID: runID}
	}
	if _, exists := h.state.active[key]; exists {
		return hustleActivityInsertion{}, &HustleActivityError{Reason: HustleActivityDuplicate, RunID: runID}
	}
	derived := h.state.applyActivity(h.sessionID, func() { h.state.add(key) })
	return hustleActivityInsertion{key: key, subs: h.snapshotSubsLocked(), derived: derived}, nil
}

// Release removes this lease's exact run activity at most once. A partial lease
// performs an in-memory rollback only and returns the cached acquisition fault; a
// committed lease persists any resulting Active->Idle edge through the native idle
// boundary and caches that result for later calls.
func (l *HustleActivityLease) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.releaseOnce.Do(func() {
		if l.partial {
			l.rollbackPartial()
			l.releaseErr = l.acquisitionErr
			return
		}
		l.releaseErr = l.releaseCommitted(ctx)
	})
	return l.releaseErr
}

func (l *HustleActivityLease) rollbackPartial() {
	l.hub.mu.Lock()
	l.hub.state.remove(l.key)
	if len(l.hub.state.active) == 0 && l.hub.state.phase == SessionActive {
		l.hub.state.phase = SessionIdle
	}
	l.hub.mu.Unlock()
	if l.holdsActivityTransition {
		l.holdsActivityTransition = false
		l.hub.activityMu.Unlock()
	}
}

func (l *HustleActivityLease) releaseCommitted(ctx context.Context) error {
	if err := l.hub.beginPublish(); err != nil {
		return err
	}
	defer l.hub.finishPublish()
	l.hub.activityMu.Lock()
	defer l.hub.activityMu.Unlock()

	l.hub.mu.Lock()
	derived := l.hub.state.applyActivity(l.hub.sessionID, func() { l.hub.state.remove(l.key) })
	var idleGeneration uint64
	if _, idle := derived.(event.SessionIdle); idle {
		idleGeneration = l.hub.markIdleBoundaryLocked()
	}
	subs := l.hub.snapshotSubsLocked()
	l.hub.mu.Unlock()
	return l.hub.appendAndDeliverDerivedChecked(ctx, subs, derived, idleGeneration)
}

// appendAndDeliverDerived is the create→append→deliver path for a session event
// derived by ExpectTurn/CancelExpectTurn (which have no triggering event of their
// own). It mints the derived event's header, durably appends it before delivery, and
// on a SessionIdle edge wakes WaitIdle waiters only AFTER that append succeeds. Any
// append/mint failure is fail-secure: deliver nothing, raise a fault. A nil derived
// (no edge crossed) is a no-op. Called OUTSIDE the hub lock.
func (h *Hub) appendAndDeliverDerived(ctx context.Context, subs []*EventSubscription, derived event.Event, idleGeneration uint64) {
	_ = h.appendAndDeliverDerivedChecked(ctx, subs, derived, idleGeneration)
}

func (h *Hub) appendAndDeliverDerivedChecked(ctx context.Context, subs []*EventSubscription, derived event.Event, idleGeneration uint64) error {
	if derived == nil {
		return nil
	}
	stamped, err := h.factory.Stamp(derived.EventHeader())
	if err != nil {
		fault := &SessionPersistenceFault{Event: derived, Cause: err}
		h.reporter.ReportFault(ctx, fault)
		return fault
	}
	derived = withHeader(derived, stamped)
	if idle, ok := derived.(event.SessionIdle); ok {
		commit := func() error {
			seq, appendErr := h.appender.AppendEvent(ctx, idle)
			if appendErr != nil {
				fault := &SessionPersistenceFault{Event: idle, Cause: appendErr}
				h.reporter.ReportFault(ctx, fault)
				return fault
			}
			h.deliver(subs, idle, seq)
			return nil
		}
		err := h.idleBoundary.CommitSessionIdle(ctx, idle, commit)
		h.completeIdleBoundary(idleGeneration, err == nil)
		return err
	}
	seq, err := h.appender.AppendEvent(ctx, derived)
	if err != nil {
		fault := &SessionPersistenceFault{Event: derived, Cause: err}
		h.reporter.ReportFault(ctx, fault)
		return fault
	}
	h.deliver(subs, derived, seq)
	h.signalIdleIfEdge(derived)
	return nil
}

func (h *Hub) markIdleBoundaryLocked() uint64 {
	h.idleBoundaryGeneration++
	if h.idleBoundaryGeneration == 0 {
		h.idleBoundaryGeneration++
	}
	h.idleBoundaryPending = h.idleBoundaryGeneration
	return h.idleBoundaryGeneration
}

func (h *Hub) completeIdleBoundary(generation uint64, success bool) {
	h.mu.Lock()
	if generation == 0 || h.idleBoundaryPending != generation {
		h.mu.Unlock()
		return
	}
	h.idleBoundaryPending = 0
	if success && h.state.phase == SessionIdle && len(h.state.active) == 0 {
		h.signalIdleLocked()
	}
	h.mu.Unlock()
}

// signalIdleIfEdge wakes every WaitIdle waiter iff derived is a SessionIdle (the
// Active->Idle edge). It runs OUTSIDE the lock, AFTER the SessionIdle's durable
// append, so a failed append never falsely reports the session idle. Each waiter
// channel is buffered(1), so the wake never blocks; a WaitIdle that registers after
// this point re-reads phase under the lock and returns nil immediately, so no waiter
// is lost in the gap between unlock and wake.
func (h *Hub) signalIdleIfEdge(derived event.Event) {
	if _, ok := derived.(event.SessionIdle); !ok {
		return
	}
	h.mu.Lock()
	h.signalIdleLocked()
	h.mu.Unlock()
}

// withHeader returns a COPY of a hub-derived session event with hdr substituted for
// its Header. The hub derives only SessionActive/SessionIdle (applyActivity) and
// SessionStopped (StopSession); the switch is exhaustive over that set and panics on
// any other type — a programming error, since no other event is ever stamped here.
func withHeader(ev event.Event, hdr event.Header) event.Event {
	switch e := ev.(type) {
	case event.SessionActive:
		e.Header = hdr
		return e
	case event.SessionIdle:
		e.Header = hdr
		return e
	case event.SessionStopped:
		e.Header = hdr
		return e
	default:
		panic("hub: withHeader called on a non-derived event type")
	}
}

// StopSession is the session-owned teardown transition. It is idempotent: if
// already SessionStopped it returns without effect. Otherwise — honoring the durable
// tap's append-before-apply, fail-secure rule — it:
//
//  1. Mints the synthesized SessionStopped (Factory: EventID+CreatedAt) and durably
//     appends it OUTSIDE the lock, BEFORE any state change. On error → ReportFault,
//     do NOT flip phase, wake nobody, deliver nothing, return (the session faults
//     instead of stopping; its FaultReporter wakes WaitIdle waiters with the fault).
//  2. Under the lock: clear active, force phase=SessionStopped (bypassing
//     applyActivity so no SessionIdle is derived), wake every WaitIdle waiter with
//     ErrSessionStopped, snapshot the subscriber set. Unlock.
//  3. Deliver the (already durable) SessionStopped live.
//
// Exported for the session only (see ExpectTurn).
func (h *Hub) StopSession(ctx context.Context) {
	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	// Idempotency pre-check: if already stopped, do nothing (and no second append).
	h.mu.RLock()
	already := h.state.phase == SessionStopped
	h.mu.RUnlock()
	if already {
		return
	}

	// (1) Mint + durably append BEFORE the in-memory stop. Fail-secure on error.
	stopped, err := h.factory.NewHeader()
	if err != nil {
		h.reporter.ReportFault(ctx, &SessionPersistenceFault{Event: event.SessionStopped{}, Cause: err})
		return
	}
	stopped.Coordinates = identity.Coordinates{SessionID: h.sessionID}
	ev := event.SessionStopped{Header: stopped}
	seq, err := h.appender.AppendEvent(ctx, ev)
	if err != nil {
		h.reporter.ReportFault(ctx, &SessionPersistenceFault{Event: ev, Cause: err})
		return
	}

	// (2) The append committed: flip phase, wake waiters, snapshot. A concurrent
	// StopSession that raced the pre-check is made safe by re-checking under the
	// write lock — only the first flip wakes waiters and the second is a no-op (but
	// it already appended a SessionStopped; the stream's EventID dedup absorbs the
	// duplicate, and the read-side dedupes by EventID on replay).
	h.mu.Lock()
	if h.state.phase == SessionStopped {
		h.mu.Unlock()
		return
	}
	h.state.active = make(map[activityKey]struct{})
	h.state.phase = SessionStopped
	h.wakeWaitersLocked(ErrSessionStopped)
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()

	// (3) Deliver the durable SessionStopped live, carrying its append sequence.
	h.deliver(subs, ev, seq)
}

// AbortSession tears down an unpublished/failed construction without appending or
// delivering the normal durable SessionStopped lifecycle event.
func (h *Hub) AbortSession(cause error) <-chan struct{} {
	if cause == nil {
		cause = ErrSessionStopped
	}
	h.publishMu.Lock()
	if h.publishAborted == nil {
		h.publishAborted = cause
	}
	drained := h.publishDrained
	h.publishMu.Unlock()

	h.activityMu.Lock()
	defer h.activityMu.Unlock()
	h.mu.Lock()
	h.state.active = make(map[activityKey]struct{})
	h.state.phase = SessionStopped
	h.wakeWaitersLocked(cause)
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()
	for _, sub := range subs {
		sub.fail(cause)
	}
	return drained
}

// WaitIdle blocks until the session is quiescent (active empty), ctx is done, or
// the session stops. It returns nil on idle, ctx.Err() on cancellation, and
// ErrSessionStopped if the session is or becomes stopped. With no session
// goroutine, waiters are woken by applyActivity (Active->Idle) and StopSession.
//
// Sticky waiter failures are checked under the same lock before the idle fast path or
// waiter registration, closing the fault-before-registration race. Stopped takes
// precedence so every post-stop call returns ErrSessionStopped.
func (h *Hub) WaitIdle(ctx context.Context) error {
	h.mu.Lock()
	switch {
	case h.state.phase == SessionStopped:
		h.mu.Unlock()
		return ErrSessionStopped
	case h.waiterFailure != nil:
		err := h.waiterFailure
		h.mu.Unlock()
		return err
	case len(h.state.active) == 0 && h.idleBoundaryPending == 0:
		h.mu.Unlock()
		return nil
	}
	// Buffered(1) so the waker never blocks holding the lock and a late wake after
	// a ctx-loss is harmless (it lands in the buffer, no goroutine reads it).
	ch := make(chan error, 1)
	h.waiters[ch] = struct{}{}
	h.mu.Unlock()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.waiters, ch)
		h.mu.Unlock()
		return ctx.Err()
	}
}

// IsIdle is the non-blocking quiescence probe used by the session's manual
// checkpoint control plane. It is intentionally narrower than exposing hub state.
func (h *Hub) IsIdle() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state.phase == SessionIdle && len(h.state.active) == 0 && h.idleBoundaryPending == 0
}

// FailWaiters latches err as the current sticky waiter failure, returns its
// monotonically increasing generation token, wakes every WaitIdle waiter, and clears
// the registry. It is the session's escalation lever on a SessionPersistenceFault:
// a faulted session is neither idle nor cleanly stopped, so its blocked WaitIdle
// callers must be released with the fault rather than left hanging or falsely told
// "idle". Exported for the
// session only (its FaultReporter implementation); loops never see it. It takes the
// lock itself (called outside it). A waiter that arrives after this point observes the
// same sticky error before consulting idle state or registering.
func (h *Hub) FailWaiters(err error) uint64 {
	h.mu.Lock()
	h.waiterFailureGeneration++
	if h.waiterFailureGeneration == 0 {
		h.waiterFailureGeneration++
	}
	token := h.waiterFailureGeneration
	h.waiterFailure = err
	h.waiterFailureToken = token
	h.wakeWaitersLocked(err)
	h.mu.Unlock()
	return token
}

// ClearWaiterFailure clears the sticky waiter failure only when token still owns the
// current generation. A stale recovery token is a no-op, preserving any newer
// persistence or root-lease fault.
func (h *Hub) ClearWaiterFailure(token uint64) {
	if token == 0 {
		return
	}
	h.mu.Lock()
	if h.waiterFailureToken == token {
		h.waiterFailure = nil
		h.waiterFailureToken = 0
	}
	h.mu.Unlock()
}

// signalIdleLocked wakes every WaitIdle waiter with a nil error (the Active->Idle
// edge). The caller holds mu. Each waiter channel is buffered(1) and cleared, so
// the send never blocks.
func (h *Hub) signalIdleLocked() { h.wakeWaitersLocked(nil) }

// wakeWaitersLocked sends err to every registered waiter and clears the registry.
// The caller holds mu; each waiter channel is buffered(1) so no send blocks.
func (h *Hub) wakeWaitersLocked(err error) {
	for ch := range h.waiters {
		ch <- err
		delete(h.waiters, ch)
	}
}
