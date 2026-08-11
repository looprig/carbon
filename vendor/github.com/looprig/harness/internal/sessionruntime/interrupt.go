package sessionruntime

import (
	"context"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// interrupt.go is the session-runtime hierarchical-interruption core (design §"Session-wide
// interrupt ordering"). It implements HIERARCHICAL SELECTION over FLAT DELIVERY: a scope
// (session-wide / a loop's delegate subtree / a single owned child) is resolved to a set of
// target loop ids UNDER THE SESSION LOCK, the whole set is marked interrupt-pending BEFORE any
// command is sent (mark-before-fanout), then one-hop command.Interrupt commands are delivered
// to every target CONCURRENTLY. A cancelled turn arms an admission barrier that holds the marks
// until the session next reaches idle (SessionIdle durably appended). A configured checkpoint
// controller refines that edge with a generation-specific accepted/committed/faulted outcome.
// While a loop is interrupt-pending
// the delegation admission paths refuse NEW machine-delegate work (start/send) — so a parent whose
// interrupted delegate wait resolves cannot open a fresh DELEGATE step in the race window — while
// human (AgencyUser) input remains accepted and queued, while loop-scoped execution admission
// prevents it from starting until every applicable barrier generation releases.
//
// SCOPE / RESIDUAL: the marks gate both machine delegate admission and loop-scoped execution
// admission. A step admitted before mark-before-fanout may continue only until its actor receives
// the interrupt and cancels the turn; accepted work queued afterward cannot emit TurnStarted until
// every applicable barrier generation releases.

// InterruptReleasePolicy is the pluggable admission-barrier release seam (Dependency Inversion).
// After an interrupt fan-out cancels at least one running turn, the session holds every target's
// interrupt-pending mark until AwaitRelease returns, then clears them. The default policy
// (sessionIdleRelease) releases once the session reaches idle — WaitIdle returns only after the
// hub has durably appended SessionIdle, so "release after idle" and "release after SessionIdle is
// appended" are the same edge. Workspace-backed sessions use checkpoint-controller sweep
// outcomes directly; this seam remains for headless/custom session-runtime composition.
type InterruptReleasePolicy interface {
	// AwaitRelease blocks until the interrupt admission barrier should release. ctx is the
	// session lifetime. The returned error is advisory: the marks are cleared once AwaitRelease
	// returns regardless of the error (fail-open on RELEASE, so a barrier can never wedge
	// admission forever). Implementations must be safe for concurrent use.
	AwaitRelease(ctx context.Context) error
}

// defaultInterruptReleaseBound caps how long the default policy waits for the session-idle
// edge before failing open. It exists to make the fail-open guarantee in InterruptReleasePolicy
// TRUE: the session-idle edge depends on EVERY live loop reaching idle, so a single loop that
// does not observe cancellation (a tool or provider ignoring ctx) would otherwise hold the
// barrier for the session's whole lifetime — and with it the retained human input on every
// OTHER loop, which is accepted and queued but cannot be admitted. That presents to the user as
// an interrupt that appears to work followed by messages that queue forever.
//
// It is deliberately far longer than a healthy interrupt-to-idle settle (cancel the stream, tear
// down in-flight tools, append the terminal), so a well-behaved session ALWAYS releases on the
// real SessionIdle edge and this bound never fires. It is a wedge backstop, not a deadline.
const defaultInterruptReleaseBound = 30 * time.Second

// sessionIdleRelease is the default InterruptReleasePolicy: it releases the admission barrier
// once the session next reaches idle, or after bound elapses — whichever comes first. A hub-less
// struct-literal session (a test seam) has no idle model, so it parks on ctx instead of
// nil-dereferencing the hub. A non-positive bound means defaultInterruptReleaseBound; tests set
// it directly to exercise the backstop without waiting out the production value.
type sessionIdleRelease struct {
	session *Session
	bound   time.Duration
}

// AwaitRelease blocks until the session reaches idle or the bound elapses. Timing out is NOT an
// error the caller must handle: armInterruptBarrier clears the marks once this returns whatever
// the error, which is exactly the documented fail-open-on-release behavior. The returned error
// stays advisory so a caller could log the distinction.
func (r sessionIdleRelease) AwaitRelease(ctx context.Context) error {
	if r.session.hub == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	bound := r.bound
	if bound <= 0 {
		bound = defaultInterruptReleaseBound
	}
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	return r.session.WaitIdle(bounded)
}

// loopInterruptPending reports whether loopID is under an interrupt admission barrier — a loop
// whose current turn was interrupted and whose NEW machine-delegate admission is refused until
// the barrier releases. It is the gate the delegation admission paths (scopedController.start /
// send) consult so a parent whose interrupted delegate wait resolves cannot open a fresh delegate
// step in the interrupt's race window. Reads under loopsMu; a nil/absent entry (a loop never
// marked, or one whose barriers all released) reads false safely.
func (s *Session) loopInterruptPending(loopID uuid.UUID) bool {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.interruptPending[loopID] > 0
}

// markInterruptPendingLocked marks every snapshot loop interrupt-pending, incrementing a
// REFCOUNT per loop. The caller MUST hold loopsMu (write): the target set is selected and marked
// under the SAME lock hold, so the whole set is pending BEFORE any interrupt command is sent
// (mark-before-fanout). The refcount lets OVERLAPPING interrupt scopes (a session-wide interrupt
// overlapping a subtree interrupt) each hold a shared loop independently — a barrier release
// decrements, and a shared loop stays pending until the LAST holding barrier releases. This
// matters once Task 16's workspace policy staggers release across scopes. The map is lazily
// allocated so a struct-literal session is safe.
func (s *Session) markInterruptPendingLocked(snapshot []loopSnapshot) {
	if s.interruptPending == nil {
		s.interruptPending = make(map[uuid.UUID]int, len(snapshot))
	}
	for _, ls := range snapshot {
		s.interruptPending[ls.loopID]++
	}
}

// clearInterruptPending releases ONE barrier's hold on ids — decrementing each loop's refcount
// and deleting the entry only when it reaches zero (the last holding barrier released). Under
// loopsMu, idempotent (a missing/already-zero id is a no-op), so a double release or a clear
// after the loops are gone is harmless.
func (s *Session) clearInterruptPending(ids []uuid.UUID) {
	s.loopsMu.Lock()
	changed := false
	for _, id := range ids {
		if s.interruptPending[id] <= 1 {
			if s.interruptPending[id] > 0 {
				changed = true
			}
			delete(s.interruptPending, id)
			continue
		}
		s.interruptPending[id]--
		changed = true
	}
	if changed && s.interruptChanged != nil {
		close(s.interruptChanged)
		s.interruptChanged = make(chan struct{})
	}
	s.loopsMu.Unlock()
}

// liveLoopSnapshotLocked returns every registered loop paired with its handle. Caller holds
// loopsMu. It is the session-wide interrupt's target set (design: "marks every live loop").
func (s *Session) liveLoopSnapshotLocked() []loopSnapshot {
	snapshot := make([]loopSnapshot, 0, len(s.loops))
	for lid, h := range s.loops {
		snapshot = append(snapshot, loopSnapshot{loopID: lid, handle: h})
	}
	return snapshot
}

// subtreeSnapshotLocked returns root plus every loop below it in the delegate tree (a BFS over
// the registry parent links), or ok=false if root is not registered. Caller holds loopsMu. It is
// the controller interrupt's target set (design: "marks one loop and its delegate subtree"): a
// sibling, an ancestor, or an unrelated loop is never included. The `seen` guard bounds the walk
// against a corrupt (cyclic) registry — the registry is a tree, so a cycle is impossible, but an
// unbounded walk must never be possible.
func (s *Session) subtreeSnapshotLocked(root uuid.UUID) ([]loopSnapshot, bool) {
	rootHandle, ok := s.loops[root]
	if !ok {
		return nil, false
	}
	children := make(map[uuid.UUID][]uuid.UUID, len(s.loops))
	for id, h := range s.loops {
		children[h.parent.LoopID] = append(children[h.parent.LoopID], id)
	}
	snapshot := []loopSnapshot{{loopID: root, handle: rootHandle}}
	seen := map[uuid.UUID]struct{}{root: {}}
	frontier := []uuid.UUID{root}
	for len(frontier) > 0 {
		parent := frontier[0]
		frontier = frontier[1:]
		for _, childID := range children[parent] {
			if _, dup := seen[childID]; dup {
				continue
			}
			seen[childID] = struct{}{}
			snapshot = append(snapshot, loopSnapshot{loopID: childID, handle: s.loops[childID]})
			frontier = append(frontier, childID)
		}
	}
	return snapshot, true
}

// preparedInterrupt is one target's command.Interrupt built in fanoutInterrupt's sequential
// phase, ready for its concurrent send + ack-wait. ack is the bidirectional receive end of the
// command's send-only Ack channel (kept here so deliverInterrupt can read the reply).
type preparedInterrupt struct {
	handle *loopHandle
	cmd    command.Interrupt
	ack    <-chan bool
}

// interruptOutcome is one target loop's fan-out result: whether it cancelled a running turn, or
// a transport error (ctx cancelled during the send/ack).
type interruptOutcome struct {
	cancelled bool
	err       error
}

// fanoutInterrupt delivers a command.Interrupt to every snapshot loop CONCURRENTLY and returns
// whether ANY loop reported it cancelled a running turn. Delivery is two-phase: the id mint +
// audit intent-append run sequentially on the caller goroutine (the appender is not required to
// be concurrency-safe), then one goroutine PER target performs the send and ack-wait so a slow
// actor never blocks the others (design: "deliver one-hop commands concurrently"). agency stamps
// the command — AgencyUser for the human session interrupt, AgencyMachine for a programmatic
// controller interrupt. ctx bounds the whole fan-out; a ctx cancellation returns
// (false, *SessionError{SessionContextDone}). A per-loop id-gen failure SKIPS that loop
// (best-effort, mirroring Shutdown) rather than failing the whole interrupt.
func (s *Session) fanoutInterrupt(ctx context.Context, snapshot []loopSnapshot, agency identity.Agency) (bool, error) {
	targets := make([]preparedInterrupt, 0, len(snapshot))
	for _, ls := range snapshot {
		id, err := s.newCommandID()
		if err != nil {
			// One loop's id-gen failure never aborts the whole interrupt (best-effort, mirroring
			// Shutdown). Asymmetry recorded on purpose: this loop was already marked
			// interrupt-pending under the lock but is skipped here, so it is never sent an
			// interrupt yet stays gated until the barrier releases. That is fail-secure (deny new
			// machine work by default) and id-gen failure is ~unreachable (crypto/rand), so the
			// stranded-gate window is acceptable.
			continue
		}
		ack := make(chan bool, 1)
		cmd := command.Interrupt{Header: command.Header{CommandID: id, Agency: agency, CreatedAt: s.stampNow()}, Ack: ack}
		// Intent log (audit-only): one record per loop, appended BEFORE this loop's send; a
		// failure is logged and the fan-out proceeds.
		s.appendCommand(ctx, ls.loopID, cmd)
		targets = append(targets, preparedInterrupt{handle: ls.handle, cmd: cmd, ack: ack})
	}

	results := make(chan interruptOutcome, len(targets))
	for _, t := range targets {
		go func() { results <- s.deliverInterrupt(ctx, t) }()
	}

	var anyCancelled bool
	var firstErr error
	for range targets {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		anyCancelled = anyCancelled || r.cancelled
	}
	if firstErr != nil {
		return false, firstErr
	}
	return anyCancelled, nil
}

// deliverInterrupt sends one prepared interrupt to its loop and waits the ack, escaping on the
// loop's Done (a stopped actor never wedges the send/wait) and ctx.Done(). It is the per-target
// body fanoutInterrupt runs concurrently. A loop that already exited reports "not cancelled".
func (s *Session) deliverInterrupt(ctx context.Context, t preparedInterrupt) interruptOutcome {
	backend := t.handle.backend
	select {
	case commandSinkFor(backend, t.cmd) <- t.cmd:
	case <-backend.DoneChan():
		return interruptOutcome{}
	case <-ctx.Done():
		return interruptOutcome{err: &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}}
	}
	select {
	case cancelled := <-t.ack:
		return interruptOutcome{cancelled: cancelled}
	case <-backend.DoneChan():
		return interruptOutcome{}
	case <-ctx.Done():
		return interruptOutcome{err: &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}}
	}
}

// runInterrupt is the shared interrupt core behind Session.Interrupt and interruptSubtree. It
// (1) selects the target set and marks it interrupt-pending under a SINGLE loopsMu hold
// (mark-before-fanout), (2) delivers command.Interrupt to every target concurrently, and (3) arms
// the admission barrier when at least one running turn was cancelled — holding the marks until the
// release policy fires, then clearing them. A fully-idle interrupt (no turn cancelled) is
// FAIL-QUIET: it clears the marks immediately, arms no barrier, and returns false. It returns
// whether any turn was cancelled, a channel that closes once the barrier has released the marks
// (nil when no barrier was armed), and a typed error. The exported entry points discard the barrier
// channel; in-package tests await it to assert release deterministically.
func (s *Session) runInterrupt(ctx context.Context, selectLocked func() ([]loopSnapshot, bool), agency identity.Agency) (bool, <-chan struct{}, error) {
	any, barrier, err := s.runInterruptWithBarrier(ctx, selectLocked, agency)
	if barrier == nil {
		return any, nil, err
	}
	return any, barrier.done, err
}

// runInterruptWithBarrier is runInterrupt's owned-barrier form. Most interrupt scopes expose
// only the completion channel and let the session/checkpoint release policy clear their mark. A
// persistent-agent stop has a narrower lifecycle: it must release its own mark at the target's
// LoopIdle edge, without leaving the later policy goroutine able to consume an overlapping scope's
// refcount. Returning the barrier gives that caller a single-owner release operation.
func (s *Session) runInterruptWithBarrier(ctx context.Context, selectLocked func() ([]loopSnapshot, bool), agency identity.Agency) (bool, *interruptBarrier, error) {
	s.loopsMu.Lock()
	snapshot, ok := selectLocked()
	if ok {
		s.markInterruptPendingLocked(snapshot)
	}
	s.loopsMu.Unlock()
	if !ok {
		return false, nil, &SessionError{Kind: SessionLoopNotFound}
	}
	// design §15's third cancellation trigger: the loop or turn is
	// interrupted. Every active permission review routed to a target loop is
	// cancelled before fan-out even begins — a no-op for any loop with no
	// active review.
	for _, ls := range snapshot {
		s.cancelPermissionReviewsForLoop(ls.loopID)
	}
	var checkpointSweep *interruptCheckpointSweep
	if s.checkpoints != nil {
		// Register before fan-out: a fast actor may publish TurnInterrupted and
		// derive SessionIdle before its interrupt acknowledgement is collected.
		checkpointSweep = s.checkpoints.beginInterruptSweep()
	}

	ids := make([]uuid.UUID, len(snapshot))
	for i, ls := range snapshot {
		ids[i] = ls.loopID
	}

	any, err := s.fanoutInterrupt(ctx, snapshot, agency)
	if err != nil {
		checkpointSweep.cancel()
		s.clearInterruptPending(ids) // a cancelled fan-out must not strand the admission barrier
		return false, nil, err
	}
	if !any {
		checkpointSweep.cancel()
		s.clearInterruptPending(ids) // fail-quiet: nothing was running, hold no barrier
		return false, nil, nil
	}
	return true, s.armInterruptBarrier(ids, checkpointSweep), nil
}

// interruptBarrier owns exactly one interrupt-pending ref per target id. Release is idempotent
// because the policy waiter and an owner such as StopAgent may race to settle the same scope.
type interruptBarrier struct {
	session         *Session
	ids             []uuid.UUID
	checkpointSweep *interruptCheckpointSweep
	cancel          context.CancelFunc
	done            chan struct{}
	releaseOnce     sync.Once
}

// armInterruptBarrier holds the interrupt-pending marks for ids until the release policy returns,
// then clears them. It runs the wait on the SESSION lifetime (not the interrupt caller's ctx, which
// may be short-lived) so the barrier tracks the session reaching idle. It returns a channel that
// closes once the marks are cleared.
func (s *Session) armInterruptBarrier(ids []uuid.UUID, checkpointSweep *interruptCheckpointSweep) *interruptBarrier {
	policy := s.interruptRelease
	if policy == nil {
		policy = sessionIdleRelease{session: s}
	}
	barrierCtx, cancel := context.WithCancel(s.sessionCtx)
	b := &interruptBarrier{
		session:         s,
		ids:             append([]uuid.UUID(nil), ids...),
		checkpointSweep: checkpointSweep,
		cancel:          cancel,
		done:            make(chan struct{}),
	}
	go func() {
		defer close(b.done)
		if checkpointSweep != nil {
			_, _ = checkpointSweep.await(barrierCtx)
		} else {
			_ = policy.AwaitRelease(barrierCtx)
		}
		b.release()
	}()
	return b
}

// release settles this barrier's one ownership unit. It also cancels the wait context so the
// policy goroutine exits when an owner releases at a narrower lifecycle edge than session idle.
func (b *interruptBarrier) release() {
	if b == nil {
		return
	}
	b.releaseOnce.Do(func() {
		b.cancel()
		if b.checkpointSweep != nil {
			b.checkpointSweep.cancel()
		}
		b.session.clearInterruptPending(b.ids)
	})
}

// interruptSubtree cancels the current turn of rootLoopID AND every loop below it in the delegate
// tree (the loop.Controller.Interrupt scope), marking the whole subtree interrupt-pending before
// fan-out and arming the admission barrier. Returns SessionLoopNotFound if root is not registered.
// It is machine-originated (a programmatic controller action), never attributed to a human.
func (s *Session) interruptSubtree(ctx context.Context, root uuid.UUID) error {
	_, _, err := s.runInterrupt(ctx, func() ([]loopSnapshot, bool) {
		return s.subtreeSnapshotLocked(root)
	}, identity.AgencyMachine)
	return err
}

// interruptSingle applies the session's mark-before-fanout and queue-disposition
// guarantees to exactly one loop. It is the persistent-agent StopAgent scope: the
// target's active turn and machine queue are interrupted, while descendants and
// siblings remain untouched.
func (s *Session) interruptSingle(ctx context.Context, loopID uuid.UUID) error {
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{loopID: {}}}})
	if err != nil {
		return err
	}
	defer func() { _ = sub.Close() }()

	any, barrier, err := s.runInterruptWithBarrier(ctx, func() ([]loopSnapshot, bool) {
		handle, ok := s.loops[loopID]
		if !ok {
			return nil, false
		}
		return []loopSnapshot{{loopID: loopID, handle: handle}}, true
	}, identity.AgencyMachine)
	if err != nil || !any {
		return err
	}
	// The fan-out acknowledgement means cancelTurn was invoked, not that the
	// target has finished its terminal and queued-input disposition. StopAgent
	// must not report idle, or permit immediate reuse, until that target-specific
	// edge commits. The normal interrupt barrier remains armed for the broader
	// session/checkpoint policy; consume this scope's one ref as soon as the
	// target itself is idle so a parent tool turn cannot deadlock the stop.
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				return &SessionError{Kind: SessionLoopExited}
			}
			if _, idle := delivery.Event.(event.LoopIdle); idle {
				barrier.release()
				return nil
			}
		case <-ctx.Done():
			return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		case <-s.sessionCtx.Done():
			return &SessionError{Kind: SessionContextDone, Cause: s.sessionCtx.Err()}
		}
	}
}
