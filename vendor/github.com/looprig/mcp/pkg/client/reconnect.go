// This file implements bounded reconnection: a binding whose connection died
// for a transient reason builds a new one, without disturbing what it has
// already promised its callers.
//
// # What a reconnect is allowed to assume
//
// Nothing. The design's gate is four conjunctive conditions — the owner is live,
// the failure is classified transient, policy permits it, and the retry budget
// (count, delay, total) is not spent — and every one of them is a way of saying
// that reconnecting is not the default response to a connection that stopped
// working. A binding that reconnects through an auth denial is a lockout; one
// that reconnects forever is a storm; one that reconnects after its owner is
// gone is a leak.
//
// # What a reconnect must not do: replay
//
// A tool call that was in flight when the connection died has an indeterminate
// outcome. The request may never have arrived, may have arrived and run, or may
// have run and had its reply lost — and nothing at this layer can tell those
// apart, because the only evidence is on the other side of a connection that no
// longer exists. So the call is reported as FailureIndeterminate and never
// retried. This is the design's rule and it is the reason a reconnect is not
// transparent: re-sending "delete the branch" because the first reply went
// missing is how a resilience feature deletes a branch twice.
//
// Reads are different in kind but treated the same way here: they are not
// replayed either, because the caller is the layer that knows whether re-reading
// is what it wants. What differs is the class it is told — a lost read is a
// closed transport, not an indeterminate effect.
//
// # What survives a reconnect
//
// The adopted generation. A new connection discovers a complete candidate, and
// that candidate waits for the caller's safe boundary exactly like one from a
// list-change notification. Swapping a Loop's toolset because a socket blinked
// would be the same mid-turn mutation the candidate/adopted split exists to
// prevent — the connection changed, but nothing the model was told has.

package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/mcp/internal/catalog"
	"github.com/looprig/mcp/internal/lifecycle"
	"github.com/looprig/mcp/internal/protocol"
)

// Operation names carried by the errors and events in this file.
const opReconnect = "reconnect"

// transientConnectionLoss reports whether a failure class means "the connection
// is gone, and trying again might work".
//
// The set is deliberately small. A closed transport and a framing failure are
// the two ways a live connection stops being one for a reason that is not about
// what was asked: a server exited, a socket dropped, a stream desynchronized.
//
// Everything else is excluded, and the exclusions matter more than the
// inclusions. An auth failure is not transient — reconnecting through one
// retries a credential that was just refused, which is how a client locks an
// account out on the host's behalf. A server protocol error is not transient —
// the connection is fine and the server is wrong, so a new connection would
// reach the same server and get the same answer. A deadline or a cancellation is
// the caller's own decision, not a fault at all.
func transientConnectionLoss(class FailureClass) bool {
	switch class {
	case FailureTransportClosed, FailureFraming:
		return true
	default:
		return false
	}
}

// noteFailure inspects a failed request and starts a reconnect if the failure
// says the connection is gone.
//
// It returns err unchanged: it is an observer on an error path, not a step in
// it. A caller's error is what happened to the caller's request, whatever the
// binding decides to do about the connection afterwards.
func (c *Client) noteFailure(epoch uint64, err error) error {
	class, ok := ClassOf(err)
	if !ok || !transientConnectionLoss(class) {
		return err
	}
	c.connectionLost(epoch, class, err)
	return err
}

// connectionLost records a lost connection and asks the reconnect worker for a
// pass.
//
// One disconnection produces one report, however many requests observe it: a
// binding with eight calls in flight has lost one connection, not eight, and an
// application told otherwise would be told something false. The collapsing is
// done by claimLoss, which admits exactly one caller per connection — so of the
// eight, one degrades the binding, emits, and signals, and the other seven
// return here having done nothing at all.
func (c *Client) connectionLost(epoch uint64, class FailureClass, cause error) {
	// One atomic step decides everything: is this report about the connection
	// the binding is using now, and is it the first report about it?
	//
	// It has to be one step. Checking the epoch and then transitioning the
	// machine is two, and the gap between them is long enough to lose a race
	// that matters: a request descheduled after the check resumes to find that
	// the reconnect it triggered has already installed a new connection and put
	// the binding back in service, and then degrades it — which dials away a
	// working connection, and, if the retries run out, fails a binding whose
	// connection was never broken. That is not a hypothetical; it is what this
	// code did, and it surfaced as an intermittent hang under load rather than
	// as anything that pointed at itself.
	if !c.claimLoss(epoch) {
		return
	}

	// The claimer, and only the claimer, moves the machine. This is safe against
	// the swap without holding a lock across it, because a swap cannot happen
	// before this point: a reconnect starts only on the signal below, the signal
	// comes only from a claimer, and there is exactly one claimer per
	// connection. Every other observer of the same disconnection has already
	// returned above.
	//
	// Best-effort: the binding may be closing, in which case shutdown wins and
	// there is nothing to reconnect.
	if err := c.machine.To(lifecycle.StateDegraded); err != nil {
		return
	}

	c.emit(ConnectionLost{
		Binding:  c.def.Name,
		Class:    class,
		Message:  failureMessage(asError(c.def.Name, opReconnect, class, cause)),
		Adopted:  c.adoptedNumber(),
		Retrying: !c.def.Reconnect.Disabled,
		At:       time.Now(),
	})

	if c.def.Reconnect.Disabled {
		// Policy refuses. The binding stays degraded and keeps serving nothing
		// but errors until its owner closes it — which is what "do not
		// reconnect" means, and is a legitimate posture for a server whose
		// re-establishment costs something the application would rather decide
		// about itself.
		return
	}
	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

// claimLoss reports whether this caller is the one that gets to act on the loss
// of connection epoch: it must name the binding's current connection, and no
// other caller must have claimed it already.
//
// It is the whole of the concurrency control on the reporting path, and it works
// because both halves of the question are answered under the one lock that the
// swap also takes (see swapConn). A request holding a retired epoch cannot claim
// anything, and of the several requests that observe one disconnection, exactly
// one leaves here with a claim.
//
// The claim is released by the swap, not by the claimer: it is a fact about a
// connection ("this one's death has been reported"), so it lives exactly as long
// as the connection does. A binding that never reconnects therefore never
// reports the same disconnection twice, however many requests fail on it.
//
// The lossHook seam fires on arrival, before the claim, and is nil outside tests
// (see the field). That position is deliberate: a caller suspended there has not
// claimed anything yet, so a test can hold one reporter without stalling the
// reconnect that another is driving.
func (c *Client) claimLoss(epoch uint64) bool {
	if c.lossHook != nil {
		c.lossHook()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.connEpoch || c.lossReported {
		return false
	}
	c.lossReported = true
	return true
}

// asError renders an arbitrary cause as an *Error of the given class.
//
// An untyped cause gets an explicit message rather than its own text, for the
// reason failureMessage documents: what reaches an event must be text this
// module wrote about a failure it classified, never a transport's rendering of a
// URL it was given.
func asError(binding Name, op string, class FailureClass, cause error) *Error {
	var typed *Error
	if errors.As(cause, &typed) {
		return typed
	}
	return NewError(class, binding, op, "the binding's connection failed", cause)
}

// runReconnector is the reconnect worker. Like the refresher it is the only
// thing that does its job, so two reconnects can never race to install two
// connections.
func (c *Client) runReconnector(ctx context.Context) {
	defer close(c.reconnectorDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.reconnectCh:
			c.reconnectWithRetry(ctx)
		}
	}
}

// reconnectWithRetry re-establishes the connection under the binding's bounded
// policy: attempts, delay and total, whichever runs out first.
//
// A binding that exhausts its budget is failed, not degraded. The distinction is
// what an operator acts on: degraded says "still serving, still trying", and
// failed says "this binding is not coming back by itself".
func (c *Client) reconnectWithRetry(ctx context.Context) {
	budget := newRetrySchedule(c.def.Reconnect.RetryPolicy, time.Now())
	for attempt := 1; ; attempt++ {
		// Every request that saw the same disconnection signalled, so a pass may
		// arrive for a connection an earlier pass already rebuilt. A binding
		// that is serving has nothing to reconnect, and dialing it again would
		// throw away a working connection to prove it.
		if c.machine.State() == lifecycle.StateReady {
			return
		}
		delay, ok := budget.next(time.Now())
		if !ok {
			c.reconnectExhausted()
			return
		}
		if !sleepCtx(ctx, delay) {
			return
		}
		// The owner may have closed while this was waiting out a backoff, which
		// is the design's first condition: never reconnect for an owner that is
		// gone.
		if !c.ownerLive() {
			return
		}
		c.setReconnectAttempt(attempt)

		if err := c.reconnectOnce(ctx); err == nil {
			c.setReconnectAttempt(0)
			return
		} else if ctx.Err() != nil {
			return
		} else {
			c.recordFailure(asError(c.def.Name, opReconnect, FailureTransportClosed, err))
		}
	}
}

// ownerLive reports whether the binding is still one a reconnect could serve.
func (c *Client) ownerLive() bool {
	switch c.machine.State() {
	case lifecycle.StateClosing, lifecycle.StateClosed:
		return false
	default:
		return true
	}
}

// reconnectExhausted marks a binding whose retry budget is spent.
func (c *Client) reconnectExhausted() {
	if !c.ownerLive() {
		return
	}
	// Best-effort: a machine that moved on is not dragged back.
	_ = c.machine.To(lifecycle.StateFailed)
	c.emit(ConnectionLost{
		Binding:  c.def.Name,
		Class:    FailureTransportClosed,
		Message:  "the connection could not be re-established within the binding's reconnect policy",
		Adopted:  c.adoptedNumber(),
		Retrying: false,
		At:       time.Now(),
	})
}

// reconnectOnce makes one attempt at a new logical connection.
//
// It follows the design's post-reconnect sequence: initialize a new connection,
// discover a complete candidate, compare server and catalog identity, leave the
// adopted generation alone, and let the caller adopt at its own boundary.
//
// It fails closed and unwinds completely, exactly as startup does. Every failure
// path closes the connection it opened before returning — a half-established
// connection owned by nobody is the one outcome worse than staying disconnected,
// because it is a live subprocess or socket that nothing will ever close.
func (c *Client) reconnectOnce(ctx context.Context) error {
	if err := c.toReconnecting(); err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.def.Timeouts.Startup)
	defer cancel()

	conn, err := c.def.Transport.Connect(dialCtx, protocol.ConnectConfig{
		Client:       protocol.ClientIdentity{Name: ClientName, Version: ClientVersion, Title: ClientTitle},
		Capabilities: c.caps,
		Bounds:       c.def.Limits.bounds(),
		Wire:         c.def.Limits.wire(),
		OnLog:        c.logAdapter(),
		// The new connection's notifications must reach the same binding: a
		// reconnected server that changes its tools is no less a change.
		OnListChanged: c.onListChanged,
		// And its resource-update notifications: a subscription's deliveries must
		// survive a reconnect no differently from list-change notifications.
		OnResourceUpdated: c.onResourceUpdated,
		// And so must its requests: what this host can serve is a property of
		// the host, not of which socket the server is on. A binding that served
		// elicitation before a reconnect and not after would advertise a
		// capability (c.caps is re-advertised verbatim) with nothing behind it.
		OnElicit: c.elicitAdapter(),
		OnSample: c.sampleAdapter(),
		// And its roots: a reconnected server must learn the same workspace view.
		OnRoots: c.rootsAdapter(),
	})
	if err != nil {
		return c.classify(dialCtx, opReconnect, err, FailureTransportClosed)
	}
	if isNilConn(conn) {
		return NewError(FailureTransportClosed, c.def.Name, opReconnect,
			"the transport did not establish a usable session", nil)
	}

	// From here every path either installs conn or closes it. There is no
	// third option, and no return between here and the swap that skips the
	// unwind.
	res, err := conn.Initialize(dialCtx)
	if err != nil {
		return c.abandon(ctx, conn, c.classify(dialCtx, opReconnect, err, FailureServerProtocol))
	}
	if res.ProtocolVersion == "" {
		return c.abandon(ctx, conn, NewError(FailureServerProtocol, c.def.Name, opReconnect,
			"the handshake settled no protocol version", nil))
	}

	gen, err := catalog.Discover(dialCtx, conn, catalog.Config{
		Binding:    string(c.def.Name),
		Number:     c.reserveGeneration(),
		Handshake:  res,
		Limits:     c.def.Limits.catalog(),
		Tolerances: c.def.Compat.tolerances(),
	})
	if err != nil {
		return c.abandon(ctx, conn, c.classify(dialCtx, opReconnect, err, discoveryClass(err)))
	}

	// Before the swap: swapConn installs the new identity, so asking afterwards
	// would compare the new server with itself and never see any drift.
	drift := c.identityDrift(res)

	old, err := c.swapConn(conn, res)
	if err != nil {
		return c.abandon(ctx, conn, err)
	}
	// The connection this one replaces. Closing it is this goroutine's job
	// alone: it is the only holder of the reference now.
	c.closeQuietly(ctx, old)

	if c.wantsLogs() && res.Capabilities.Logging {
		if err := conn.SetLogLevel(dialCtx, string(c.def.LogLevel)); err != nil {
			c.recordFailure(NewError(FailureServerProtocol, c.def.Name, opReconnect,
				"the server refused to set a log level; its logs will not arrive", err))
		}
	}

	c.emit(ConnectionRestored{
		Binding:    c.def.Name,
		Server:     ServerIdentity{Name: res.Server.Name, Version: res.Server.Version, Title: res.Server.Title},
		Drift:      drift,
		Adopted:    c.adoptedNumber(),
		Generation: gen.Number(),
		At:         time.Now(),
	})

	// publish compares the candidate with what is adopted and moves the binding
	// back to ready. The adopted generation is untouched either way: a
	// reconnected server is a new connection, not a new agreement about what the
	// caller's Loops may see.
	c.publish(gen)
	return nil
}

// toReconnecting moves the binding into StateReconnecting, tolerating the case
// where it is already there.
//
// The retry loop calls this once per attempt, and a machine has no
// self-transitions (see internal/lifecycle) — so the second attempt's move is
// refused not because anything is wrong but because the first attempt already
// made it. Treating that refusal as "someone else moved the machine" would end
// the loop after one try, silently turning every bounded policy into a single
// attempt. What a refusal means here is decided by looking at where the machine
// actually is, not by whether the move was legal.
func (c *Client) toReconnecting() error {
	if c.machine.State() == lifecycle.StateReconnecting {
		return nil
	}
	if err := c.machine.To(lifecycle.StateReconnecting); err != nil {
		// Genuinely refused: the machine is somewhere a reconnect cannot start
		// from, which for this worker means the binding is closing.
		return NewError(FailureShutdown, c.def.Name, opReconnect, "the binding is not reconnectable", err)
	}
	return nil
}

// abandon closes a connection that failed to become the binding's, and returns
// the failure that condemned it.
func (c *Client) abandon(ctx context.Context, conn protocol.Conn, out error) error {
	c.closeQuietly(ctx, conn)
	return out
}

// closeQuietly closes a connection the client is discarding, bounding the wait
// and dropping the error.
//
// The error is dropped because there is no one to tell and nothing to do: the
// connection is already being thrown away, and a failure to close it cleanly
// changes neither. It is recorded nowhere rather than surfaced as the binding's
// Failure, which must describe why the binding is in the state it is in — not
// how tidily a superseded connection went.
func (c *Client) closeQuietly(ctx context.Context, conn protocol.Conn) {
	if conn == nil {
		return
	}
	// A context of its own: the caller's may already be done (a cancelled
	// reconnect still has to release what it opened), and a transport given a
	// dead context cannot shut down cleanly.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.def.Timeouts.Startup)
	defer cancel()
	_ = conn.Close(closeCtx)
}

// swapConn installs a new connection and returns the one it replaces.
//
// It refuses a binding that is shutting down, and that refusal is not
// theoretical: Close cancels this worker and waits for it, but the wait is
// bounded, so a reconnect blocked in a dial can outlive it. Installing then
// would hand a live connection to a Client nobody will ever close again. The
// caller closes what it could not install.
func (c *Client) swapConn(conn protocol.Conn, res protocol.InitializeResult) (protocol.Conn, error) {
	if !c.ownerLive() {
		return nil, NewError(FailureShutdown, c.def.Name, opReconnect, "the binding is shutting down", nil)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under the lock, against the same lock Close's teardown takes: the
	// pre-lock check is a moment that has passed, and Close moves the machine to
	// Closing before closeConn takes this lock. If the binding closed in that
	// window, installing conn would hand a live connection to a Client whose
	// teardown has already run and will never run again — the caller closes what
	// it could not install.
	if !c.ownerLive() {
		return nil, NewError(FailureShutdown, c.def.Name, opReconnect, "the binding is shutting down", nil)
	}
	old := c.conn
	c.conn = conn
	// A new connection is a new epoch: every request still holding the old one
	// is now talking about a connection that has been retired, and its report
	// of that connection's death must not touch this one. The new connection
	// starts with its own death unreported — the claim belongs to a connection,
	// not to the binding.
	c.connEpoch++
	c.lossReported = false
	c.server = ServerIdentity{Name: res.Server.Name, Version: res.Server.Version, Title: res.Server.Title}
	c.protocolVersion = string(res.ProtocolVersion)
	c.initResult = res
	return old, nil
}

// identityDrift reports how a reconnected server's identity differs from the one
// this binding first connected to, as bounded, safe text — or empty when it is
// the same server.
//
// It is reported, not enforced. The design says to compare server and catalog
// identity after a reconnect; it does not say to refuse the connection, and
// refusing would be wrong: a server that was restarted with a new version is the
// normal reason to be reconnecting at all. What the drift changes is the
// caller's decision about the candidate that came with it, which is why this
// travels on the event that announces it.
//
// Only the name and version are compared. Title is cosmetic, and a server that
// re-words its own title has not become a different server.
func (c *Client) identityDrift(res protocol.InitializeResult) string {
	c.mu.Lock()
	was := c.server
	c.mu.Unlock()

	now := ServerIdentity{Name: res.Server.Name, Version: res.Server.Version}
	if was.Name == now.Name && was.Version == now.Version {
		return ""
	}
	return boundMessage(fmt.Sprintf("the server now identifies as %q version %q, was %q version %q",
		now.Name, now.Version, was.Name, was.Version))
}

// setReconnectAttempt records which attempt is in flight, for Status. Zero means
// none.
func (c *Client) setReconnectAttempt(n int) {
	c.mu.Lock()
	c.reconnectAttempt = n
	c.mu.Unlock()
}

// startReconnector launches the reconnect worker, on the same terms as the
// refresher: after startup has succeeded, cancelled only by Close.
func (c *Client) startReconnector() {
	ctx, cancel := context.WithCancel(context.Background())
	c.stopReconnector = cancel
	c.reconnectorDone = make(chan struct{})
	go c.runReconnector(ctx)
}

// stopReconnecting ends the reconnect worker and waits, bounded by ctx, for it
// to exit — so that a reconnect cannot install a connection into a binding that
// Close has already released. swapConn is what makes the bounded wait safe: a
// worker that outlives it still cannot install anything.
func (c *Client) stopReconnecting(ctx context.Context) {
	if c.stopReconnector == nil {
		return
	}
	c.stopReconnector()
	select {
	case <-c.reconnectorDone:
	case <-ctx.Done():
	}
}
