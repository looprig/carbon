// Package sessionadapter adapts harness sessions to the terminal Agent contract.
package sessionadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/tui"
)

const sessionAdapterInitShutdownTimeout = 10 * time.Second

// shouldExposeEvent applies the harness's fail-closed visibility and requested
// class/loop filter before an event can affect adapter projections or consumers.
func shouldExposeEvent(filter event.EventFilter, ev event.Event) bool {
	return event.ShouldDeliver(filter, ev)
}

// sessionAdapter adapts a Rig session.SessionController to the tui.Agent surface. It
// owns three things and nothing else: the live session controller, replay dependencies used
// for cold restore and durable gap repair, and a concurrency-safe gate index that folds
// GateOpened/GateResolved events into a (loopID, toolExecutionID)→GateID map so the TUI's
// per-loop Approve/Deny/ProvideAnswer calls can resolve the harness-minted gate id. It holds
// no root context or GC ticker — the rig owns the session lifetime, workspace snapshots, and
// GC — so Close is a single session Shutdown.
type sessionAdapter struct {
	sess     session.SessionController
	replayer journal.EventReplayer // nil for a new/headless session with no replay
	// openReplay reopens the product event replayer at an inclusive journal sequence.
	// sessionstore binds FromSeq when OpenEventReplayer is constructed, so recovery must
	// retain this factory rather than trying to reposition the already-used cold cursor.
	openReplay func(uint64) (journal.EventReplayer, error)
	// replayedThrough is the highest durable sequence consumed by coldReplay. A reconnecting
	// subscription starts after it so events that happened between cold replay and Subscribe
	// are recovered instead of being silently skipped.
	replayedThrough uint64

	isRestore bool
	backlog   []event.Event // restored public all-loop Enduring history; nil for new/headless

	// mu guards the gate indexes below. foldGate mutates them from the cold replay (at
	// construction) and from every Subscribe forwarding goroutine; the gate-reply trio reads
	// them. gateKey pairs the loop with the per-call tool-execution id so the SAME
	// ToolExecutionID appearing in two loops resolves to two distinct gates.
	mu      sync.Mutex
	forward map[gateKey]gate.ID
	reverse map[gate.ID]gateKey

	shutdownOnce sync.Once
	shutdownErr  error
}

// Agent is the reusable harness-session adapter consumed by terminal applications.
type Adapter = sessionAdapter

// New wraps a newly created session. Fresh sessions have no replay backlog. It is the
// plain test/explicitly-ephemeral constructor; production clients that promise enduring
// delivery must use NewWithReplay or Restore.
func New(sess session.SessionController) *Adapter {
	return &sessionAdapter{
		sess:    sess,
		forward: make(map[gateKey]gate.ID),
		reverse: make(map[gate.ID]gateKey),
	}
}

// NewWithReplay wraps a newly created session and cold-replays the public
// enduring events already committed during rig construction. It is intended for
// interactive clients that subscribe after primers have been created.
func NewWithReplay(ctx context.Context, sess session.SessionController, store ReplayOpener) (*Adapter, error) {
	return newSessionAdapter(ctx, sess, store, true)
}

// Restore wraps a restored session and reconstructs its public enduring backlog
// and open-gate index before returning it to the TUI.
func Restore(ctx context.Context, sess session.SessionController, store ReplayOpener) (*Adapter, error) {
	return newSessionAdapter(ctx, sess, store, true)
}

// gateKey identifies an open gate by the loop that opened it plus the per-call
// tool-execution id, so the same ToolExecutionID in two loops keys two distinct gates.
type gateKey struct {
	loopID          uuid.UUID
	toolExecutionID uuid.UUID
}

// replayOpener is the narrow slice of the session store the adapter needs: open a durable
// public event replayer for one session. *sessionstore.Store satisfies it.
type ReplayOpener interface {
	OpenEventReplayer(uuid.UUID, sessionstore.ReplayRequest) (journal.EventReplayer, error)
}

// replayOpener is retained internally while the migrated tests exercise the same
// narrow contract.
type replayOpener = ReplayOpener

// newSessionAdapter wraps a live rig session controller as a tui.Agent. A restored session
// performs one unnarrowed cold replay, filtering visibility before folding every requested
// loop's gate events and materializing public all-loop Enduring history for TUI projections.
func newSessionAdapter(ctx context.Context, sess session.SessionController, store ReplayOpener, isRestore bool) (*sessionAdapter, error) {
	if !isRestore {
		return New(sess), nil
	}
	a := &sessionAdapter{
		sess:      sess,
		isRestore: isRestore,
		forward:   make(map[gateKey]gate.ID),
		reverse:   make(map[gate.ID]gateKey),
	}
	if store == nil {
		return nil, a.failInitialization(errors.New("sessionadapter: restored session requires replay store"))
	}
	a.openReplay = func(fromSeq uint64) (journal.EventReplayer, error) {
		return store.OpenEventReplayer(sess.SessionID(), sessionstore.ReplayRequest{FromSeq: fromSeq})
	}
	if replayer, err := a.openReplay(0); err != nil {
		return nil, a.failInitialization(err)
	} else if replayer == nil {
		return nil, a.failInitialization(errDurableReplayUnavailable)
	} else {
		a.replayer = replayer
	}
	if err := a.coldReplay(ctx); err != nil {
		return nil, a.failInitialization(err)
	}
	return a, nil
}

// failInitialization releases controller ownership after the rig has successfully created or
// restored it but the adapter cannot finish initialization. Cleanup is bounded and detached
// from the caller because replay failures commonly arrive through an already-canceled context.
func (a *sessionAdapter) failInitialization(primary error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionAdapterInitShutdownTimeout)
	defer cancel()
	return errors.Join(primary, a.Close(ctx))
}

// coldReplay drains the public durable log once (all loops), applying the same fail-closed
// product filter used by live delivery before gate folds and backlog materialization.
func (a *sessionAdapter) coldReplay(ctx context.Context) error {
	if a.replayer == nil {
		return nil
	}
	cursor, err := a.replayer.Open(ctx, journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close() }()

	for {
		ev, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err // typed fail-secure error (object missing/corrupt) — surfaced unchanged
		}
		if !shouldExposeEvent(event.EventFilter{Enduring: event.LoopScope{All: true}}, ev) {
			if seq > a.replayedThrough {
				a.replayedThrough = seq
			}
			continue
		}
		if seq > a.replayedThrough {
			a.replayedThrough = seq
		}
		a.foldGate(ev)
		if ev.Class() == event.Enduring {
			a.backlog = append(a.backlog, ev)
		}
	}
}

// foldGate updates the gate index from a single event: a GateOpened inserts the
// (loopID, toolExecutionID)→GateID forward entry and the reverse removal entry; a
// GateResolved removes both by the resolved gate id. Any other event is ignored. It is
// concurrency-safe (mu) so the cold replay and every Subscribe forwarder can call it.
func (a *sessionAdapter) foldGate(ev event.Event) {
	switch e := ev.(type) {
	case event.GateOpened:
		key := gateKey{loopID: e.EventHeader().LoopID, toolExecutionID: e.Gate.Subject.ToolExecutionID}
		a.mu.Lock()
		a.forward[key] = e.Gate.ID
		a.reverse[e.Gate.ID] = key
		a.mu.Unlock()
	case event.GateResolved:
		a.mu.Lock()
		if key, ok := a.reverse[e.GateID]; ok {
			delete(a.forward, key)
			delete(a.reverse, e.GateID)
		}
		a.mu.Unlock()
	}
}

// Submit delivers a multimodal user message fire-and-forget to the ACTIVE loop and returns
// the InputID the resulting Reply events carry (Cause.CommandID).
func (a *sessionAdapter) Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error) {
	return a.sess.Submit(ctx, blocks)
}

// SubmitToLoop delivers a user message fire-and-forget to a SPECIFIC loop (the focused
// loop) and returns the InputID the resulting Reply events carry.
func (a *sessionAdapter) SubmitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error) {
	return a.sess.SubmitToLoop(ctx, loopID, blocks)
}

// ActiveLoopID returns the session's current default input target directly.
func (a *sessionAdapter) ActiveLoopID() uuid.UUID { return a.sess.ActiveLoop().ID() }

// AcceptsImages reports whether the CURRENT model bound to loopID accepts image blocks. It
// is dynamic and per-target (a multi-loop session runs heterogeneous models) and fails
// closed (false) for an unknown loop.
func (a *sessionAdapter) AcceptsImages(loopID uuid.UUID) bool {
	handle, ok := a.sess.Loop(loopID)
	if !ok {
		return false
	}
	return handle.Model().Caps.AcceptsImages
}

// Subscribe returns a filtered wrapping subscription over the session fan-in. Restored and
// durable-bootstrap adapters apply the caller's product filter before folding
// GateOpened/GateResolved into the adapter index and forwarding, so a live gate is answerable
// the instant the TUI observes its request; a hub loss is repaired from the journal before
// live forwarding resumes. New's explicitly-ephemeral path retains the lightweight wrapper.
func (a *sessionAdapter) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	if a.openReplay != nil {
		return newReplayingSubscription(replayingSubscriptionConfig{
			ctx:        context.Background(),
			sessionID:  a.sess.SessionID(),
			filter:     filter,
			initialSeq: a.replayedThrough,
			subscribe:  a.sess.SubscribeEvents,
			openReplay: a.openReplay,
			foldGate:   a.foldGate,
		})
	}
	inner, err := a.sess.SubscribeEvents(filter)
	if err != nil {
		return nil, err
	}
	sub := &gateFoldingSubscription{inner: inner, out: make(chan event.Delivery), done: make(chan struct{})}
	go func() {
		defer close(sub.out)
		for delivery := range inner.Events() {
			if !shouldExposeEvent(filter, delivery.Event) {
				continue
			}
			a.foldGate(delivery.Event)
			select {
			case sub.out <- delivery:
			case <-sub.done:
				return
			}
		}
	}()
	return sub, nil
}

// ReplayBacklog returns the restored session's public all-loop Enduring history materialized
// at construction, in session order. A NEW/headless session returns nil (the TUI skips the
// repaint). ctx is accepted for the contract; the backlog was folded once at restore.
func (a *sessionAdapter) ReplayBacklog(_ context.Context) ([]event.Event, error) {
	if !a.isRestore {
		return nil, nil
	}
	return a.backlog, nil
}

// SessionID returns the underlying session's id (the composition root prints it and keys the
// catalog on it).
func (a *sessionAdapter) SessionID() uuid.UUID { return a.sess.SessionID() }

// Controller returns the wrapped harness controller for application-level operations
// that are intentionally outside the terminal Agent contract.
func (a *sessionAdapter) Controller() session.SessionController { return a.sess }

// Interrupt cancels the running turn, returning true if a turn was cancelled.
func (a *sessionAdapter) Interrupt(ctx context.Context) (bool, error) { return a.sess.Interrupt(ctx) }

// GateNotOpenError reports that no open gate matches the (loop, tool-execution) a gate reply
// addressed. It is fail-secure: an Approve/Deny/ProvideAnswer for a call with no live gate
// touches nothing and returns this. It is errors.As-able.
type GateNotOpenError struct {
	LoopID          uuid.UUID
	ToolExecutionID uuid.UUID
}

func (e *GateNotOpenError) Error() string {
	return "sessionadapter: no open gate for loop " + e.LoopID.String() + " tool-execution " + e.ToolExecutionID.String()
}

// gateIDFor resolves the harness gate id of the open gate opened by loopID for callID (the
// per-call tool-execution id), from the adapter-owned index. An unmatched call fails secure
// with *GateNotOpenError.
func (a *sessionAdapter) gateIDFor(loopID, callID uuid.UUID) (gate.ID, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.forward[gateKey{loopID: loopID, toolExecutionID: callID}]; ok {
		return id, nil
	}
	return gate.ID{}, &GateNotOpenError{LoopID: loopID, ToolExecutionID: callID}
}

// Approve resolves a pending tool-call permission gate with the chosen approval action
// — gate.ApprovalApprove or gate.ApprovalApproveAlwaysWorkspace. loopID names the
// gate-opening loop; callID is the tool-execution id. The action string is sent in the
// clear with no values (the one combined prompt carries no scope and no per-capability
// payload); the session validates it against the gate's ApprovalControls and, for the
// always-workspace action, persists the displayed candidates before execution.
func (a *sessionAdapter) Approve(ctx context.Context, loopID, callID uuid.UUID, action gate.ApprovalAction) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: string(action),
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Deny resolves a pending tool-call permission gate by failing it closed (fail-secure)
// with the exact gate.ApprovalDeny action.
func (a *sessionAdapter) Deny(ctx context.Context, loopID, callID uuid.UUID) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: string(gate.ApprovalDeny),
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// ProvideAnswer supplies the user's reply to a pending AskUser request.
func (a *sessionAdapter) ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error {
	gateID, err := a.gateIDFor(loopID, callID)
	if err != nil {
		return err
	}
	rawAnswer, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: "answer",
		Values: map[string]json.RawMessage{"answer": rawAnswer},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// RespondGate answers a host-raised gate — form or open-url — named directly by
// gateID.
//
// It performs no gate-index lookup, and that is the point: the index exists to
// recover a gate id the TUI never saw, because a permission or AskUser prompt is
// built from a per-kind request event that carries only a ToolExecutionID. A
// host-raised gate is observed through GateOpened, which carries the gate id
// itself, so looking it up would be inventing a round trip through a key (the
// tool execution) that an integration's gate need not even have.
//
// Values are passed through unchanged. The session validates them against the
// gate's authoritative schema and refuses an action the gate never offered, so
// this adapter neither validates nor sanitizes — doing so here would be a second,
// divergent copy of a rule the session already owns.
func (a *sessionAdapter) RespondGate(ctx context.Context, gateID gate.ID, action string, values map[string]json.RawMessage) error {
	return a.sess.RespondGate(ctx, gate.GateResponse{
		GateID: gateID,
		Action: action,
		Values: values,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
}

// Close shuts the session down exactly once (the rig owns every other lifetime concern — the
// workspace lease, snapshots, and GC — so there is no second root or watcher cancel).
func (a *sessionAdapter) Close(ctx context.Context) error {
	a.shutdownOnce.Do(func() { a.shutdownErr = a.sess.Shutdown(ctx) })
	return a.shutdownErr
}

// CompactToLoop forwards a manual compaction request to the exact loop selected
// by the TUI. The Session owns command identity and error semantics.
func (a *sessionAdapter) CompactToLoop(ctx context.Context, loopID uuid.UUID) (uuid.UUID, error) {
	return a.sess.CompactToLoop(ctx, loopID)
}

// gateFoldingSubscription is the lightweight wrapper used for plain New sessions. Its
// forwarding goroutine filters before folding each visible gate transition into the adapter
// index and handing the event to the consumer channel; Close tears down both the inner
// subscription and the forwarder so a consumer that stops reading cannot leak the goroutine.
type gateFoldingSubscription struct {
	inner event.Subscription
	out   chan event.Delivery
	done  chan struct{}
	once  sync.Once
}

func (s *gateFoldingSubscription) Events() <-chan event.Delivery { return s.out }

func (s *gateFoldingSubscription) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.inner.Close()
}

func (s *gateFoldingSubscription) Err() error { return s.inner.Err() }

// compile-time assertions: the adapter satisfies the tui.Agent surface, and the
// wrapping subscription satisfies event.Subscription.
var (
	_ tui.Agent          = (*sessionAdapter)(nil)
	_ event.Subscription = (*gateFoldingSubscription)(nil)
)
