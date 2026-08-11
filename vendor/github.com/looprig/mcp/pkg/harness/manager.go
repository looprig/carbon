// This file owns the client sets: it starts every binding's connection,
// decides when its owner may be considered ready, hands out routes to callers,
// reconfigures bindings under a live owner, and closes them.
//
// The Manager is the only thing in this package that holds mutable state, and
// the shape of that state follows the design's central split: Scope decides who
// owns a connection, Visibility decides who may see it. Ownership is what Close
// and CloseLoop act on; visibility is what sessionRoutes answers.

package mcpharness

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/mcp/pkg/client"
)

// DefaultRetirementTimeout bounds how long a retiring route waits for the turns
// still using it. See BindingOp and retire.
const DefaultRetirementTimeout = 30 * time.Second

// bindingState is one binding's live state: its immutable configuration, its
// connection, and the bookkeeping that decides when that connection may close.
//
// It carries its own mutex rather than living under the Manager's. That is not
// an optimization — it is what makes the design's concurrent startup possible:
// a server may elicit during its own initialization, and the callback that
// answers must be able to reach this binding's state while Start is still
// blocked waiting for a different binding.
type bindingState struct {
	mu sync.Mutex
	// binding is immutable while installed. Reconfiguration replaces the whole
	// bindingState; it never mutates one an active turn may be reading.
	binding Binding
	cl      *client.Client
	failure *client.Error
	// disabled marks a binding an operator turned off. Its configuration
	// remains so EnableBinding can start it again.
	disabled bool
	// retiring marks a binding removed from future Loop generations. It still
	// serves the turns already holding its route (design §Binding
	// reconfiguration).
	retiring bool
	// dialing reports that a connect attempt has been launched for this state.
	// It is what makes Close's wait safe: a binding nobody dialed will never
	// close ready, so waiting on it would hang a shutdown forever, while a
	// binding mid-dial must be waited for or the connection it is about to
	// produce leaks.
	dialing bool
	// settled reports that the first connect attempt finished, successfully or
	// not. ready is closed exactly once, when it does.
	settled bool
	ready   chan struct{}

	// inflight counts the active turn references to this route. A retiring
	// route closes when it reaches zero, or when the retirement deadline
	// cancels the turns still holding it.
	inflight int
	idle     chan struct{}

	// eliciting counts this binding's pending elicitations. A server drives it,
	// so it is bounded: see maxPendingElicitations.
	eliciting int
}

func newBindingState(b Binding) *bindingState {
	return &bindingState{binding: b, ready: make(chan struct{})}
}

// settle records the outcome of a connect attempt and releases anything waiting
// on readiness. It is idempotent with respect to ready: a reconnect settles a
// state that is already settled.
func (bs *bindingState) settle(cl *client.Client, err *client.Error) {
	bs.mu.Lock()
	bs.cl, bs.failure = cl, err
	first := !bs.settled
	bs.settled = true
	bs.mu.Unlock()
	if first {
		close(bs.ready)
	}
}

// acquire takes a turn's reference to this route. It fails when the binding has
// no connection or has been retired, which is what stops a call from starting
// on a route that is on its way out (design §Binding reconfiguration: the old
// connection closes after no active turn generation references it).
func (bs *bindingState) acquire() (*client.Client, bool) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.cl == nil || bs.retiring || bs.disabled {
		return nil, false
	}
	bs.inflight++
	return bs.cl, true
}

// release drops a turn's reference and wakes a retirement waiting on the last
// one.
func (bs *bindingState) release() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.inflight--
	if bs.inflight <= 0 && bs.idle != nil {
		close(bs.idle)
		bs.idle = nil
	}
}

// markRetiring removes the route from future generations and returns a channel
// closed when the turns still holding it are done. A route nobody holds is
// already idle, so the returned channel is closed.
func (bs *bindingState) markRetiring() <-chan struct{} {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.retiring = true
	if bs.inflight <= 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if bs.idle == nil {
		bs.idle = make(chan struct{})
	}
	return bs.idle
}

// loopID is the Loop a server-initiated request on this binding is on behalf of.
//
// It is zero for a Session-scoped binding, and that is not a gap: a Session's
// server is shared, and a request it raises — during initialization, or during a
// call from any of several Loops — belongs to the Session, not to whichever Loop
// happened to be first. GateRequest.LoopID and SampleRequest.LoopID both
// document the zero as exactly this case.
//
// It reads bs.binding without the lock, as its callers do: a binding is immutable
// while installed, and reconfiguration replaces the whole bindingState rather
// than mutating this one.
func (bs *bindingState) loopID() uuid.UUID {
	if bs.binding.Scope == ScopeLoop {
		return bs.binding.Loop
	}
	return uuid.UUID{}
}

func (bs *bindingState) client() *client.Client {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.cl
}

// Manager owns a Session's MCP bindings.
//
// It is safe for concurrent use. Its own mutex guards only the binding table —
// which bindings exist — and is never held across a connect, a close, or a call
// into host code, so a server that elicits during initialization can be answered
// while Start is still waiting on some other required server.
type Manager struct {
	deps  Deps
	clock Clock

	// sessionID is the Session these bindings serve, or nil while the Manager is
	// unbound. It is late-bound and atomic because of the ordering attach.go
	// exists for: an application that fingerprints its MCP configuration starts
	// this Manager before its Session exists, so a status published on a client's
	// event goroutine may race the BindSession that gives it somewhere to go.
	sessionID atomic.Pointer[uuid.UUID]

	// ctx is the Manager's lifetime context: Close cancels it, which cancels
	// startup, in-flight requests, background readers, and reconnect work
	// (design §Shutdown). It is deliberately NOT derived from Start's ctx —
	// Start's context bounds the wait for required bindings, while an optional
	// binding goes on connecting after Start returns.
	ctx    context.Context
	cancel context.CancelFunc
	// wg tracks the background goroutines Close must wait for.
	wg sync.WaitGroup

	mu       sync.Mutex
	states   map[client.Name]*bindingState
	started  bool
	closed   bool
	retireIn time.Duration
	// elicitIn is the overall wall-clock bound on one elicitation. It is a
	// field rather than a constant for the same reason retireIn is: a test must
	// be able to drive the deadline without waiting out DefaultElicitationTimeout.
	elicitIn time.Duration

	// afterReplaceConnect, when non-nil, is called by applyReplace after a
	// replacement's dial has settled and before the install decision. It is nil
	// in production; a concurrency test uses it to land a Close (or another op)
	// in the one window that decision must survive, so the guard is exercised
	// deterministically rather than by hoping the race lands. It is set once,
	// before the Reconfigure that reads it, so it carries no lock.
	afterReplaceConnect func()
}

// NewManager validates the bindings and returns a Manager that has not connected
// anything yet.
//
// Every binding is validated up front, and duplicate names are rejected: a
// binding name qualifies tool identities and permission identities, so two
// bindings sharing one would make both ambiguous — the model would see one name
// for two authorities.
func NewManager(bindings []Binding, deps Deps) (*Manager, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	states := make(map[client.Name]*bindingState, len(bindings))
	for i, b := range bindings {
		if err := b.Validate(); err != nil {
			return nil, fmt.Errorf("bindings[%d]: %w", i, err)
		}
		name := client.Name(b.Name)
		if _, dup := states[name]; dup {
			return nil, fmt.Errorf("bindings[%d]: duplicate binding name %q", i, b.Name)
		}
		states[name] = newBindingState(b)
	}
	deps = deps.normalized()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		deps:     deps,
		clock:    deps.Clock,
		ctx:      ctx,
		cancel:   cancel,
		states:   states,
		retireIn: DefaultRetirementTimeout,
		elicitIn: DefaultElicitationTimeout,
	}
	// A Deps that named a Session binds the Manager at birth, so the one-phase
	// composition is attached before anything can publish. A Deps that did not is
	// the discover-then-create flow, and BindSession closes it (see attach.go).
	if !deps.SessionID.IsZero() {
		id := deps.SessionID
		m.sessionID.Store(&id)
	}
	return m, nil
}

// BindingFailure is one binding's classified startup failure.
type BindingFailure struct {
	// Binding names the binding that failed.
	Binding string
	// Class classifies the failure.
	Class client.FailureClass
	// Message is the failure's bounded, normalized text.
	Message string
}

// StartupError reports every required binding that did not become ready. It is
// aggregated rather than first-wins because a user fixing their configuration
// should see all of it at once: three servers with three different problems is
// three rounds of trial and error if they are reported one at a time (design
// §Required and optional servers).
type StartupError struct {
	Failures []BindingFailure
}

func (e *StartupError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", f.Binding, f.Message, f.Class))
	}
	return fmt.Sprintf("mcp: %d required binding(s) failed to start: %s", len(e.Failures), strings.Join(parts, "; "))
}

// ErrManagerClosed is returned by every operation on a closed Manager.
var ErrManagerClosed = errors.New("mcp: manager is closed")

// ErrAlreadyStarted is returned by a second Start.
var ErrAlreadyStarted = errors.New("mcp: manager is already started")

// Start connects every binding concurrently and returns once the required ones
// have settled.
//
// The concurrency is the contract, not an optimization (design §Concurrent
// startup): one slow optional server must not delay an unrelated one's
// discovery, and Start must not hold anything a callback needs — a server may
// elicit or authenticate during its own initialization, and the answer has to
// route while startup is still in progress. That is why the binding table's lock
// is released before the wait and why every per-binding fact lives under the
// binding's own lock.
//
// It returns a *StartupError naming every required binding that failed. An
// optional binding that fails degrades only itself and leaves the owner usable.
// Optional bindings that have not settled keep connecting in the background
// after Start returns; Close waits for them.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if m.started {
		m.mu.Unlock()
		return ErrAlreadyStarted
	}
	m.started = true
	states := slices.Collect(maps.Values(m.states))
	m.mu.Unlock()

	for _, bs := range states {
		m.startConnect(bs)
	}

	var failures []BindingFailure
	for _, bs := range states {
		if !bs.binding.Required {
			continue
		}
		if f, ok := m.awaitRequired(ctx, bs); ok {
			failures = append(failures, f)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	// Deterministic order: a report a user reads twice should read the same
	// twice. Startup order is concurrent and says nothing.
	sort.Slice(failures, func(i, j int) bool { return failures[i].Binding < failures[j].Binding })
	return &StartupError{Failures: failures}
}

// awaitRequired waits for one required binding to settle and reports its failure
// if it has one. ok is false when the binding is ready.
func (m *Manager) awaitRequired(ctx context.Context, bs *bindingState) (BindingFailure, bool) {
	select {
	case <-bs.ready:
		bs.mu.Lock()
		err := bs.failure
		bs.mu.Unlock()
		if err == nil {
			return BindingFailure{}, false
		}
		return BindingFailure{Binding: bs.binding.Name, Class: err.Class, Message: err.Msg}, true
	case <-ctx.Done():
		return BindingFailure{
			Binding: bs.binding.Name,
			Class:   client.FailureStartupTimeout,
			Message: "startup did not complete before the deadline",
		}, true
	case <-m.ctx.Done():
		return BindingFailure{
			Binding: bs.binding.Name,
			Class:   client.FailureShutdown,
			Message: "manager closed during startup",
		}, true
	}
}

// startConnect dials one binding on a background goroutine tracked by m.wg.
func (m *Manager) startConnect(bs *bindingState) {
	bs.mu.Lock()
	bs.dialing = true
	bs.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.connect(m.ctx, bs)
	}()
}

// connect dials one binding and records the outcome. It never returns an error:
// a binding's failure is its own state, and whether that failure matters to the
// owner is Required's business, not this function's.
func (m *Manager) connect(ctx context.Context, bs *bindingState) {
	bs.mu.Lock()
	def, disabled := bs.binding.Server, bs.disabled
	bs.mu.Unlock()
	if disabled {
		bs.settle(nil, client.NewError(client.FailureInvalidConfig, def.Name, "start", "binding is disabled", nil))
		return
	}
	cl, err := client.Connect(ctx, def, m.handlersFor(bs))
	bs.settle(cl, asClientError(def.Name, "start", err))
}

// asClientError narrows a Connect failure to the typed error the status surface
// records.
//
// A *client.Error passes through with the class it claimed. Anything else is
// this module violating its own contract — Connect classifies everything it
// returns — so rather than invent a class for it, the failure is reported as
// indeterminate: an unclassified error is precisely a failure whose nature is
// not known, and saying so is better than picking a plausible-looking class that
// an operator would then act on.
func asClientError(name client.Name, op string, err error) *client.Error {
	if err == nil {
		return nil
	}
	var typed *client.Error
	if errors.As(err, &typed) {
		return typed
	}
	return client.NewError(client.FailureIndeterminate, name, op, "connect returned an unclassified error", err)
}

// BindingStatus is a binding's observable state: its configuration posture plus
// its connection's status. Every field is safe metadata (design §Lifecycle and
// readiness) — it is designed to be logged, rendered, and shipped to telemetry
// as-is.
type BindingStatus struct {
	// Name is the binding name.
	Name string
	// Scope names the connection's owner.
	Scope Scope
	// Loop is the owning Loop for a loop-scoped binding, zero otherwise.
	Loop uuid.UUID
	// Required reports the binding's startup posture.
	Required bool
	// Enabled is false for a binding an operator disabled.
	Enabled bool
	// Retiring reports that the binding is out of future generations and is
	// serving only the turns that still hold it.
	Retiring bool
	// Client is the connection's status, already redacted and bounded. It is
	// the zero value with State StateConfigured before a connection exists.
	Client client.Status
}

// Status returns a snapshot of every binding, in a deterministic name order so
// that a rendering of it does not reshuffle between reads.
func (m *Manager) Status() []BindingStatus {
	m.mu.Lock()
	states := slices.Collect(maps.Values(m.states))
	m.mu.Unlock()

	out := make([]BindingStatus, 0, len(states))
	for _, bs := range states {
		out = append(out, bs.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (bs *bindingState) status() BindingStatus {
	bs.mu.Lock()
	b, cl, failure, disabled, retiring := bs.binding, bs.cl, bs.failure, bs.disabled, bs.retiring
	bs.mu.Unlock()

	st := BindingStatus{
		Name:     b.Name,
		Scope:    b.Scope,
		Loop:     b.Loop,
		Required: b.Required,
		Enabled:  !disabled,
		Retiring: retiring,
	}
	switch {
	case cl != nil:
		st.Client = cl.Status()
	default:
		// No connection: report the binding's own posture rather than a zero
		// status that would read as "configured" for a binding that failed.
		st.Client = client.Status{
			Binding:        client.Name(b.Name),
			State:          client.StateConfigured,
			TransportKind:  b.Server.Transport.Kind(),
			RedactedOrigin: b.Server.Transport.RedactedOrigin(),
		}
		if failure != nil {
			st.Client.State = client.StateFailed
			st.Client.Failure = &client.Failure{Class: failure.Class, Message: failure.Msg}
		}
	}
	return st
}

// sessionRoutes returns the session-scoped bindings the identified Loop may
// consume, in deterministic name order.
//
// This is where Visibility is enforced, and it is separate from loopRoutes
// because the two answer different questions: this one is "which shared servers
// may this Loop see", and that is a policy the Session set.
func (m *Manager) sessionRoutes(loopID uuid.UUID, loopName string) []*bindingState {
	return m.routes(func(b Binding) bool {
		return b.Scope == ScopeSession && b.permits(loopID, loopName)
	})
}

// loopRoutes returns the bindings the identified Loop owns.
//
// It matches on the owning Loop's ID alone, which is the whole of design
// §Delegation: a delegate has its own Loop ID, so it never sees its parent's
// private bindings — not because anything filters them out, but because it was
// never their owner.
func (m *Manager) loopRoutes(loopID uuid.UUID) []*bindingState {
	return m.routes(func(b Binding) bool {
		return b.Scope == ScopeLoop && b.permits(loopID, "")
	})
}

// routes returns the live, consumable bindings matching keep. A disabled or
// retiring binding is never returned: both are out of future generations by
// definition.
func (m *Manager) routes(keep func(Binding) bool) []*bindingState {
	m.mu.Lock()
	states := slices.Collect(maps.Values(m.states))
	m.mu.Unlock()

	out := make([]*bindingState, 0, len(states))
	for _, bs := range states {
		bs.mu.Lock()
		b, skip := bs.binding, bs.disabled || bs.retiring
		bs.mu.Unlock()
		if skip || !keep(b) {
			continue
		}
		out = append(out, bs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].binding.Name < out[j].binding.Name })
	return out
}

// Close shuts the whole Manager down: every binding, whatever its scope.
//
// It cancels startup, in-flight requests, pending elicitations, background
// readers, reconnect work, and stdio subprocesses by cancelling the Manager's
// lifetime context, then closes each connection and waits — within ctx's bound —
// for the background goroutines to finish (design §Shutdown). It is idempotent:
// a second Close returns nil without touching anything.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	states := slices.Collect(maps.Values(m.states))
	m.mu.Unlock()

	// Cancel first, close second. The cancel is what unblocks a connect still
	// in its handshake and a reader parked on a socket; closing a client that
	// is still dialing would otherwise wait for work that has no reason to
	// stop.
	m.cancel()
	err := m.closeStates(ctx, states)
	return errors.Join(err, m.waitBackground(ctx))
}

// CloseLoop closes the bindings the identified Loop owns.
//
// It matches on ownership, which is what makes design §Delegation hold without a
// special case: a parent's shutdown never reaches a delegate's bindings, because
// those name the delegate's Loop as their owner. Session-scoped bindings are
// untouched however many of this Loop's turns were using them — the Session owns
// those, and another Loop may be mid-call on one right now.
func (m *Manager) CloseLoop(ctx context.Context, loopID uuid.UUID) error {
	if loopID.IsZero() {
		return fmt.Errorf("mcp: CloseLoop: loopID is zero")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	var owned []*bindingState
	for name, bs := range m.states {
		if bs.binding.Scope == ScopeLoop && bs.binding.Loop == loopID {
			owned = append(owned, bs)
			delete(m.states, name)
		}
	}
	m.mu.Unlock()
	return m.closeStates(ctx, owned)
}

// closeStates closes every state concurrently and joins their errors. Closing is
// concurrent for the same reason starting is: one server that will not go away
// quietly must not hold up the rest of the shutdown.
func (m *Manager) closeStates(ctx context.Context, states []*bindingState) error {
	errs := make([]error, len(states))
	var wg sync.WaitGroup
	for i, bs := range states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait for a connect attempt that is actually in flight: closing a
			// client that does not exist yet would leak the connection the
			// attempt is about to produce. The Manager's context is already
			// cancelled, so the attempt is on its way to failing.
			//
			// A binding nobody dialed is skipped rather than waited for. Its
			// ready will never close — there is no goroutine to close it — so
			// waiting would hang the shutdown of a Manager that was simply
			// never started.
			bs.mu.Lock()
			dialing := bs.dialing
			bs.mu.Unlock()
			if dialing {
				select {
				case <-bs.ready:
				case <-ctx.Done():
				}
			}
			if cl := bs.client(); cl != nil {
				errs[i] = cl.Close(ctx)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// waitBackground waits for the Manager's goroutines within ctx's bound. A
// shutdown that has to give up still returns: an unbounded wait would turn one
// wedged server into a Session that cannot exit (design §Shutdown: "waits for
// owned resources to finish within a bound").
func (m *Manager) waitBackground(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mcp: shutdown did not finish within the bound: %w", ctx.Err())
	}
}
