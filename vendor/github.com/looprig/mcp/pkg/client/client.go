// This file implements Client: one MCP server binding, its lifecycle, and the
// startup sequence that establishes it.
//
// Startup is deliberately linear and fails closed. Every step either advances
// the lifecycle machine or unwinds completely — there is no partially connected
// Client, and no error path that returns one. Because shutdown can legally
// overtake startup at any step (see internal/lifecycle), a refused transition
// is treated as "someone else is closing this" and unwinds, never as a bug.

package client

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/looprig/mcp/internal/catalog"
	"github.com/looprig/mcp/internal/lifecycle"
	"github.com/looprig/mcp/internal/limits"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/internal/sched"
)

// The identity this module presents to servers. It is cosmetic — a server
// learns who is calling, not what the caller may do.
const (
	// ClientName is the MCP client name this module reports.
	ClientName = "looprig-mcp"
	// ClientVersion is the module version reported alongside ClientName.
	ClientVersion = "0.1.0"
	// ClientTitle is the human-readable client name.
	ClientTitle = "looprig MCP client"
)

// Operation names carried by the errors this file returns.
const (
	opConnect    = "connect"
	opInitialize = "initialize"
	opDiscover   = "discover"
	opClose      = "close"
)

// Client is one connection to one MCP server. It is safe for concurrent use.
// A Client is always either usable or closed: Connect never returns one that
// failed to start.
type Client struct {
	// def is the normalized definition. It is written once, before the Client
	// escapes Connect, and never mutated afterwards.
	def Definition

	machine *lifecycle.Machine
	// unwatch deregisters the lifecycle watcher. Close nils it out to release
	// the watcher's reference to this Client; it is only ever touched inside
	// closeOnce.
	unwatch func()
	// logHandler is the application's log callback, captured at construction.
	// It is read-only after that, so it needs no lock.
	logHandler LogHandler
	// eventHandler is the application's event callback, captured at
	// construction and read-only afterwards. See Client.emit.
	eventHandler EventHandler
	// elicitHandler is the application's elicitation handler, captured at
	// construction and read-only afterwards. Nil means this binding serves no
	// elicitation and advertises no such capability. See Client.elicitAdapter.
	elicitHandler ElicitationHandler
	// samplingHandler is the application's sampling handler, captured at
	// construction and read-only afterwards. Nil means this binding serves no
	// sampling and advertises no such capability. See Client.sampleAdapter.
	samplingHandler SamplingHandler
	// rootsProvider is the application's roots provider, captured at
	// construction and read-only afterwards. Nil means this binding exposes no
	// roots and advertises no such capability. See Client.rootsAdapter.
	rootsProvider RootsProvider
	// samples bounds the sampling this binding will serve: how much at once, and
	// how deep. It is created with the Client and immutable afterwards (it does
	// its own locking).
	samples *sampleGate
	// sched admits this binding's requests: it serializes what must be
	// serialized and bounds everything. It is created with the Client and is
	// immutable afterwards (it does its own locking).
	sched *sched.Scheduler

	// refreshCh carries a request for a catalog refresh pass. It is buffered to
	// one: that buffer is what coalesces duplicate notifications (see
	// signalRefresh).
	refreshCh chan struct{}
	// stopRefresher ends the refresh worker; refresherDone is closed when it
	// has. Both are nil until the worker starts, which is after the binding is
	// ready — a client that never reached ready has no worker to stop.
	stopRefresher context.CancelFunc
	refresherDone chan struct{}

	// reconnectCh carries a request for a reconnect pass, buffered to one for
	// the same reason as refreshCh: every request that observes one lost
	// connection reports the same loss, and they collapse onto one pass.
	reconnectCh chan struct{}
	// stopReconnector ends the reconnect worker; reconnectorDone is closed when
	// it has. See stopRefresher for why both are nil before the binding is
	// ready.
	stopReconnector context.CancelFunc
	reconnectorDone chan struct{}

	// closeOnce guards the whole shutdown sequence, making Close idempotent.
	closeOnce sync.Once
	// lossHook, when non-nil, runs at the head of claimLoss, before the claim.
	// It is a test seam and nothing sets it in production.
	//
	// It exists because the race it exposes is otherwise untestable: the window
	// it stands in is a few instructions wide, it needs a specific interleaving
	// of three goroutines to matter, and the bug it guards against showed up in
	// roughly one full-suite run in ten — as an unexplained hang, not as
	// anything that named itself. A guard whose regression test relies on that
	// happening again is not a guard. See TestLossClaimIsAtomicWithTheSwap.
	lossHook func()
	// caps are the client capabilities settled at Connect. A reconnect
	// re-advertises exactly these: what the host can serve is a property of the
	// host, so it must not drift across a reconnection.
	caps protocol.ClientCapabilities

	// mu guards the conn handle and the observable metadata below. The
	// lifecycle state itself lives in the machine, which does its own locking.
	mu sync.Mutex
	// conn is the binding's current connection. It is guarded because Close and
	// the reconnect worker both reach for it from other goroutines.
	conn protocol.Conn
	// connEpoch identifies which connection conn is: it is incremented every
	// time one replaces another.
	//
	// It exists because a request outlives the connection it was issued on. A
	// call that started on connection 1, and reports its death after connection
	// 2 is already serving, is describing a connection that is already gone —
	// and without an epoch to say so, that stale report degrades a healthy
	// binding and starts a reconnect against a connection nothing is wrong
	// with. The epoch is what makes "this connection is lost" a claim about a
	// specific connection rather than about whatever is current when the report
	// lands.
	connEpoch       uint64
	server          ServerIdentity
	protocolVersion string
	failure         *Failure
	lastChange      time.Time
	// initResult is what the handshake settled. A refresh needs it — discovery
	// is gated on the server's advertised capabilities — and re-running the
	// handshake to recover it is not an option, so it is kept.
	initResult protocol.InitializeResult
	// generation is the adopted catalog, or nil before discovery has published
	// one. The Generation it points at is immutable, so only the pointer needs
	// guarding.
	generation *catalog.Generation
	// candidate is the validated generation waiting for the caller to adopt it,
	// or nil when there is none. Only the refresh path writes it; only Adopt
	// promotes it. See refresh.go.
	candidate *catalog.Generation
	// lastGeneration is the highest ordinal handed out so far. See
	// reserveGeneration.
	lastGeneration uint64
	// stale holds the catalog families a server has announced a change to and
	// which have not been refetched since.
	stale map[catalog.Family]struct{}
	// reconnectAttempt is the reconnect attempt currently in flight, or 0 when
	// the binding is not reconnecting.
	reconnectAttempt int
	// lossReported records that the current connection's death has already been
	// claimed by one request, so the others that observe it stay quiet. It is
	// cleared by the swap that installs a new connection. See claimLoss.
	lossReported bool
}

// Connect establishes a binding: it validates def, opens a transport, performs
// the MCP handshake, and returns a ready Client. The whole sequence is bounded
// by def.Timeouts.Startup (or the caller's ctx, whichever fires first).
//
// It fails closed at every step. On any error it returns a nil Client and an
// *Error bound to def.Name — the transport, if one was opened, is always
// closed before returning, so a failed Connect leaks nothing.
//
// ctx bounds startup only; it does not govern the returned Client's lifetime.
// Cancelling it after Connect returns has no effect — use Close.
func Connect(ctx context.Context, def Definition, h Handlers) (*Client, error) {
	// Validate before anything is built or contacted: a malformed definition
	// must not reach a transport.
	if err := def.Validate(); err != nil {
		return nil, err
	}
	caps, err := h.advertised(def.Name, def.Capabilities)
	if err != nil {
		return nil, err
	}

	c := newClient(def.normalized(), h)
	c.caps = caps

	startCtx, cancel := context.WithTimeout(ctx, c.def.Timeouts.Startup)
	defer cancel()

	if err := c.start(startCtx, caps); err != nil {
		// The unwind already closed the transport and recorded the failure.
		c.unwatch()
		return nil, err
	}
	return c, nil
}

// newClient builds an unstarted Client in StateConfigured with its lifecycle
// watcher already registered, so no transition can be missed by a caller's
// event handler.
func newClient(def Definition, h Handlers) *Client {
	c := &Client{
		def:           def,
		machine:       lifecycle.NewMachine(),
		logHandler:    h.Log,
		eventHandler:  h.Event,
		elicitHandler: h.Elicitation,
		refreshCh:     make(chan struct{}, 1),
		reconnectCh:   make(chan struct{}, 1),
		stale:         make(map[catalog.Family]struct{}),

		samplingHandler: h.Sampling,
		rootsProvider:   h.Roots,
		samples:         newSampleGate(def.Limits.MaxSamplingDepth, def.Limits.MaxSamplingConcurrency),
		sched: sched.New(sched.Config{
			MaxConcurrent: def.Limits.MaxConcurrentRequests,
			// The application's decision, and only ever the application's. A
			// server's tool annotations may inform it — an application is free
			// to read them and configure accordingly — but nothing a server
			// says reaches this field, so no server can widen its own
			// concurrency by claiming to be safe.
			AllowParallel: def.AllowParallelCalls,
		}),
	}
	c.unwatch = c.machine.Watch(func(from, to lifecycle.State) {
		now := time.Now()
		c.mu.Lock()
		c.lastChange = now
		c.mu.Unlock()
		c.emit(StateChanged{
			Binding: def.Name,
			From:    fromLifecycle(from),
			To:      fromLifecycle(to),
			At:      now,
		})
	})
	return c
}

// start runs the startup sequence: configured -> starting -> discovering ->
// ready.
//
// Authentication (StateAuthenticating) is skipped: no transport in this module
// authenticates yet, and the lifecycle permits starting -> discovering
// directly.
func (c *Client) start(ctx context.Context, caps protocol.ClientCapabilities) error {
	if err := c.to(lifecycle.StateStarting, opConnect); err != nil {
		return err
	}

	conn, err := c.def.Transport.Connect(ctx, protocol.ConnectConfig{
		Client:            protocol.ClientIdentity{Name: ClientName, Version: ClientVersion, Title: ClientTitle},
		Capabilities:      caps,
		Bounds:            c.def.Limits.bounds(),
		Wire:              c.def.Limits.wire(),
		OnLog:             c.logAdapter(),
		OnListChanged:     c.onListChanged,
		OnResourceUpdated: c.onResourceUpdated,
		OnElicit:          c.elicitAdapter(),
		OnSample:          c.sampleAdapter(),
		OnRoots:           c.rootsAdapter(),
	})
	if err != nil {
		return c.fail(ctx, opConnect, err, FailureTransportClosed)
	}
	if isNilConn(conn) {
		// A transport that reports success with no connection is broken; there
		// is nothing to close and nothing to initialize.
		return c.fail(ctx, opConnect, nil, FailureTransportClosed)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	res, err := conn.Initialize(ctx)
	if err != nil {
		return c.fail(ctx, opInitialize, err, FailureServerProtocol)
	}
	if res.ProtocolVersion == "" {
		// Defence in depth: internal/protocol already rejects this, but a Conn
		// is an interface and the client must not proceed on a handshake that
		// never settled a protocol version.
		return c.fail(ctx, opInitialize, nil, FailureServerProtocol)
	}

	c.mu.Lock()
	c.server = ServerIdentity{Name: res.Server.Name, Version: res.Server.Version, Title: res.Server.Title}
	c.protocolVersion = string(res.ProtocolVersion)
	c.initResult = res
	c.mu.Unlock()

	// From here on the conn is established, so every failure — including a
	// refused transition, which means shutdown overtook startup — unwinds
	// through the same path that closes it. The alternative is an open
	// connection owned by nobody.
	if err := c.to(lifecycle.StateDiscovering, opDiscover); err != nil {
		return c.unwind(ctx, err)
	}
	if err := c.discover(ctx, conn, res); err != nil {
		return err
	}
	// Adoption happens before ready (design's discovery step 9): a caller that
	// observes StateReady can always read the catalog that goes with it.
	if err := c.to(lifecycle.StateReady, opDiscover); err != nil {
		return c.unwind(ctx, err)
	}
	// Last, once the binding is established and no failure path can still
	// unwind it: a worker started earlier would be one more thing every unwind
	// had to remember to stop, and the only work it has is on a live connection
	// anyway. From here the goroutine's lifetime is exactly Close's.
	c.startRefresher()
	c.startReconnector()
	return nil
}

// startRefresher launches the refresh worker. It is called once, from the end of
// a successful startup.
func (c *Client) startRefresher() {
	// Deliberately not derived from the startup context: startup's context
	// bounds startup, and the worker outlives it. Cancellation comes from Close
	// and from nowhere else.
	ctx, cancel := context.WithCancel(context.Background())
	c.stopRefresher = cancel
	c.refresherDone = make(chan struct{})
	go c.runRefresher(ctx)
}

// stopRefreshing ends the refresh worker and waits for it to exit, so that no
// refresh can still be in flight against a connection Close is about to shut.
//
// It is bounded by ctx: a worker wedged in a fetch against a server that has
// stopped answering must not hold shutdown open forever. Abandoning the wait is
// safe — the worker's own context is already cancelled, so it ends as soon as
// its fetch does, and it touches nothing Close releases (closing a conn under an
// in-flight request is the transport's business, and it reports it as a closed
// connection).
func (c *Client) stopRefreshing(ctx context.Context) {
	if c.stopRefresher == nil {
		return
	}
	c.stopRefresher()
	select {
	case <-c.refresherDone:
	case <-ctx.Done():
	}
}

// discover fetches the initial catalog and adopts it, and sets the server's log
// level if this binding wants logs.
//
// A discovery failure is terminal for startup: a binding with no catalog has
// nothing to offer, and pretending otherwise would produce a ready Client whose
// every call fails. It unwinds like any other startup step, so the transport is
// closed and the caller gets a typed error.
func (c *Client) discover(ctx context.Context, conn protocol.Conn, res protocol.InitializeResult) error {
	gen, err := catalog.Discover(ctx, conn, catalog.Config{
		Binding: string(c.def.Name),
		// The first ordinal this binding hands out, and the same counter every
		// later refresh draws from: generations are numbered by one source, so
		// no two of them can ever share a number.
		Number:     c.reserveGeneration(),
		Handshake:  res,
		Limits:     c.def.Limits.catalog(),
		Tolerances: c.def.Compat.tolerances(),
	})
	if err != nil {
		return c.fail(ctx, opDiscover, err, discoveryClass(err))
	}

	c.mu.Lock()
	c.generation = gen
	c.mu.Unlock()

	// An MCP server sends no log messages until a level is set, so a binding
	// that wanted them and never asked would sit in silence and be unable to
	// tell it from a quiet server. Only ask when there is both somewhere to
	// deliver them (see wantsLogs) and a server that advertised logging.
	if c.wantsLogs() && res.Capabilities.Logging {
		if err := conn.SetLogLevel(ctx, string(c.def.LogLevel)); err != nil {
			// Logs are diagnostics. A server that will not enable them is not a
			// reason to refuse an otherwise-working binding, so this is
			// recorded and startup continues.
			c.recordFailure(NewError(FailureServerProtocol, c.def.Name, opDiscover,
				"the server refused to set a log level; its logs will not arrive", err))
		}
	}
	return nil
}

// discoveryClass maps a discovery failure onto the class a caller branches on.
//
// The two catalog classes are genuinely different to a caller: an over-limit
// catalog is well-formed and might be accepted with a raised bound, while a
// defective one is broken whatever the bounds are.
func discoveryClass(err error) FailureClass {
	var over *limits.OverLimitError
	if errors.As(err, &over) {
		return FailureCatalogOverLimit
	}
	var defect *catalog.DefectError
	if errors.As(err, &defect) {
		return FailureCatalogInvalid
	}
	return FailureServerProtocol
}

// logAdapter wraps the application's log handler into the neutral callback the
// connection takes, tagging each record with the binding. A nil handler stays
// nil, so a log arriving for a binding that wants none is dropped at the
// boundary rather than converted first.
func (c *Client) logAdapter() func(protocol.LogRecord) {
	if !c.wantsLogs() {
		return nil
	}
	return func(r protocol.LogRecord) {
		if c.logHandler != nil {
			c.logHandler(LogMessage{
				Binding: c.def.Name,
				Level:   LogLevel(r.Level),
				Logger:  r.Logger,
				Text:    r.Text,
			})
		}
		c.emit(ServerLog{
			Binding: c.def.Name,
			Level:   LogLevel(r.Level),
			Logger:  r.Logger,
			Text:    r.Text,
			At:      time.Now(),
		})
	}
}

// wantsLogs reports whether anything on this binding would receive a server log.
//
// Either handler counts, and that is the whole reason this exists: a server
// sends no logs at all until a level is asked for, so an application that
// observes a binding through events alone would otherwise install an Event
// handler, never install a Log handler, and sit in a silence indistinguishable
// from a quiet server.
func (c *Client) wantsLogs() bool {
	return c.logHandler != nil || c.eventHandler != nil
}

// isNilConn reports whether a transport handed back no connection.
//
// A nil check alone is not enough: a transport returning a typed nil pointer
// (var s *session; return s, nil) produces a non-nil interface holding nil,
// which would panic on the first method call — inside Connect, on the caller's
// goroutine. protocol.Conn is a sealed, module-internal boundary, so that is an
// in-module transport bug rather than hostile input; it is still checked here,
// because turning a transport's bug into a typed error the caller can classify
// costs three lines, and a panic out of Connect costs the host its process.
func isNilConn(conn protocol.Conn) bool {
	if conn == nil {
		return true
	}
	v := reflect.ValueOf(conn)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// to moves the machine, converting a refused transition into a typed error. A
// *lifecycle.TransitionError here does not mean the sequence is wrong: it means
// a concurrent Close legally moved the machine first, so the sequence has been
// overtaken and its caller must unwind.
func (c *Client) to(next lifecycle.State, op string) *Error {
	err := c.machine.To(next)
	if err == nil {
		return nil
	}
	var te *lifecycle.TransitionError
	if errors.As(err, &te) {
		return NewError(FailureShutdown, c.def.Name, op, "binding is shutting down", err)
	}
	return NewError(FailureIndeterminate, c.def.Name, op, "lifecycle transition failed", err)
}

// fail classifies a failed startup step and unwinds it.
func (c *Client) fail(ctx context.Context, op string, err error, fallback FailureClass) error {
	return c.unwind(ctx, c.classify(ctx, op, err, fallback))
}

// unwind abandons startup: it records the failure, marks the binding failed and
// closes the transport, returning the *Error Connect gives the caller.
//
// Every failing path routes through here, including a refused transition — the
// one path where the failure is that someone else is already closing. Skipping
// the close there would be the worst version of the bug: the conn is
// established, Connect returns nil, and nothing else holds a reference to it.
// The transport is therefore closed regardless of what the machine says, and
// closing it twice is impossible — closeConn takes the conn out of the Client
// under the lock, so whoever takes it is the only one who closes it and a racing
// Close finds nothing left to do. See closeConn for why that is ownership rather
// than a sync.Once.
func (c *Client) unwind(ctx context.Context, out *Error) error {
	c.recordFailure(out)
	// A refused transition means shutdown is already running. Either way the
	// caller gets the failure that actually happened, not the transition's.
	_ = c.machine.To(lifecycle.StateFailed)
	// The startup context may already be done — indeed on the timeout and
	// cancellation paths it always is — which would stop a transport from
	// closing cleanly. Give the close its own bounded context.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.def.Timeouts.Startup)
	defer cancel()
	_ = c.closeConn(closeCtx)
	return out
}

// classify turns a step's failure into a typed *Error.
//
// An *Error from a transport or Conn is authoritative and passes through
// untouched: it knows what went wrong better than its caller does. Otherwise
// context errors are read first — a startup that ran out of time is a startup
// timeout, and a cancelled caller is a cancellation, whatever the step called
// the resulting error — and everything else takes the step's fallback class.
func (c *Client) classify(ctx context.Context, op string, err error, fallback FailureClass) *Error {
	var typed *Error
	if err != nil && errors.As(err, &typed) {
		return typed
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return NewError(FailureCancelled, c.def.Name, op, "startup was cancelled", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return NewError(FailureStartupTimeout, c.def.Name, op, "startup exceeded "+c.def.Timeouts.Startup.String(), err)
	}
	if err == nil {
		// A step that failed without an error of its own: say what the client
		// observed rather than render an empty message.
		return NewError(fallback, c.def.Name, op, "the "+c.def.Transport.Kind()+" transport did not establish a usable session", nil)
	}
	return NewError(fallback, c.def.Name, op, "", err)
}

// recordFailure stores a bounded summary of err for Status.
func (c *Client) recordFailure(err *Error) {
	f := &Failure{Class: err.Class, Message: err.Msg}
	if f.Message == "" {
		// Error renders the wrapped cause when Msg is empty, bounding it as it
		// goes; Status must carry the same bounded text, never the raw cause.
		f.Message = boundMessage(err.Error())
	}
	c.mu.Lock()
	c.failure = f
	c.mu.Unlock()
}

// closeConn closes the binding's current connection, at most once however many
// paths race for it.
//
// The guard is ownership, not a sync.Once: the conn is taken out of the Client
// under the lock, and whoever takes it is the one who closes it. Everyone else
// finds nothing and returns. That is what a Once cannot do here — a binding may
// have several connections over its life (reconnect retires one and installs
// another), and a Once would let the first close swallow every later one,
// leaking every connection after the first.
func (c *Client) closeConn(ctx context.Context) error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn == nil {
		return nil
	}
	// Never call into a transport while holding the lock: it is foreign code and
	// its close may take as long as its peer does.
	if err := conn.Close(ctx); err != nil {
		return NewError(FailureTransportClosed, c.def.Name, opClose, "closing the transport failed", err)
	}
	return nil
}

// Status returns a snapshot of the binding's observable state. It carries safe
// metadata only — no credentials, no payloads — and is a value, so the caller
// may keep or mutate it freely.
func (c *Client) Status() Status {
	// The transport's own accessors are called outside c.mu: they are foreign
	// code, and nothing the client guards should be held across a call into it.
	s := Status{
		Binding:        c.def.Name,
		State:          fromLifecycle(c.machine.State()),
		TransportKind:  c.def.Transport.Kind(),
		RedactedOrigin: c.def.Transport.RedactedOrigin(),
		StaleFamilies:  c.staleFamilies(),
		CompatProfile:  c.def.Compat.String(),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	s.ProtocolVersion = c.protocolVersion
	s.Server = c.server
	s.LastChange = c.lastChange
	if c.generation != nil {
		s.CatalogGeneration = c.generation.Number()
		s.CatalogDigest = c.generation.Digest().String()
	}
	if c.candidate != nil {
		s.CandidateGeneration = c.candidate.Number()
		s.CandidateDigest = c.candidate.Digest().String()
	}
	s.ReconnectAttempt = c.reconnectAttempt
	if c.failure != nil {
		// Copy: a caller must not be able to rewrite the client's own record.
		f := *c.failure
		s.Failure = &f
	}
	return s
}

// Close shuts the binding down and releases its transport. It is idempotent and
// safe to call concurrently: the first call performs the shutdown and reports
// any error closing the transport; later calls do nothing and return nil.
//
// Closing a binding that already failed is not an error — shutdown is
// reachable from every non-terminal state, and a caller must never have to know
// which state it is in to release the resources.
func (c *Client) Close(ctx context.Context) error {
	var (
		first bool
		err   error
	)
	c.closeOnce.Do(func() {
		first = true
		// Both transitions may be refused (the binding is already closed, or
		// terminal). Shutdown is best-effort by design: the transport must be
		// released regardless of what the machine says.
		_ = c.machine.To(lifecycle.StateClosing)
		// Reject new work, then cancel what is in flight, then release what
		// they were using. Closing the conn first would tear the transport out
		// from under live requests, which loses their replies and makes the
		// peer exit on a read error rather than a clean stop.
		c.sched.Shutdown()
		c.stopRefreshing(ctx)
		c.stopReconnecting(ctx)
		err = c.closeConn(ctx)
		_ = c.machine.To(lifecycle.StateClosed)
		c.unwatch()
		// Drop the watcher's reference to this Client along with the callback
		// it captured, so neither outlives the binding.
		c.unwatch = nil
	})
	if !first {
		return nil
	}
	return err
}
