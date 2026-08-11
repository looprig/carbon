// client.go implements Client: the connection runtime that drives a foreign
// ACP agent as a client, over a subprocess spawned via acp/transport/stdio
// and a protocol.Conn built on top of it. It owns the lazy, start-once
// connection lifecycle (Task 5.1's "start-once connection state machine": a
// not-started -> starting -> started progression where concurrent Dial
// callers share one in-flight attempt and a failed attempt resets so a later
// call can retry), the registered client-served ACP method handlers
// (session/update, permission requests, filesystem, and terminal
// operations — see dispatch.go), and the live-session registry Session
// objects are tracked under.
//
// acp/client is pure wire layer (see acp/CLAUDE.md): it imports only
// acp/protocol and acp/transport/stdio, never harness or core.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/looprig/acp/protocol"
	"github.com/looprig/acp/transport/stdio"
)

// LoadTimeout is the default bound session/load's response is awaited under
// once replay updates have started streaming (see session.go's LoadSession
// and the design doc's "Replay-idle tolerance in the client"): a foreign
// agent may stream a full replay but be slow to resolve the call itself.
// Rather than synthesizing a response from wall-clock heuristics, a hung
// load fails typed at this deadline. Options.LoadTimeout overrides it per
// Client.
const LoadTimeout = 90 * time.Second

// dialState is the start-once connection state machine's current phase.
type dialState int

const (
	// dialIdle: not connected, no attempt in flight. The zero value, so a
	// freshly constructed Client starts here.
	dialIdle dialState = iota
	// dialDialing: exactly one attempt is in flight; concurrent Dial callers
	// observe this state and wait on the shared attempt's outcome rather than
	// starting their own.
	dialDialing
	// dialReady: connected; Client.conn/agent/proc are valid to use.
	dialReady
	// dialClosed: Close has been called, or the connection died and Client
	// gave up on it. Terminal: Dial always fails once here.
	dialClosed
)

// dialAttempt is the shared handle concurrent Dial callers wait on while one
// of them owns the in-flight connection attempt.
type dialAttempt struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

// Client is one connection to a foreign ACP agent, spawned as a subprocess
// and driven over acp/protocol's JSON-RPC layer. The zero value is not
// usable; construct with New.
type Client struct {
	cmd  stdio.Command
	opts Options

	// attemptConnect performs one full connection attempt: spawning the
	// subprocess (or, in tests, substituting an in-process transport),
	// wiring the protocol.Conn, registering this Client's client-served
	// method handlers, and completing the "initialize" handshake. New wires
	// this to spawnAndConnect; white-box tests may replace it directly to
	// exercise the state machine (dial_internal_test.go) or the dispatcher
	// logic (over an in-process net.Pipe peer) without a real subprocess.
	attemptConnect func(ctx context.Context) error

	mu     sync.Mutex
	state  dialState
	waiter *dialAttempt
	proc   *stdio.Proc
	conn   *protocol.Conn
	agent  *protocol.AgentConn
	// initRes remains private; callers read its agent information and raw
	// metadata through InitializeMetadata, which returns defensive copies.
	initRes *protocol.InitializeResponse

	sessionsMu sync.Mutex
	sessions   map[protocol.SessionID]*Session
	// sessionsClosed is guarded by sessionsMu. Once terminal teardown has
	// snapshotted the registry, no later operation may add a Session to the
	// replacement map; otherwise its update pump could outlive Client.Close.
	sessionsClosed bool

	droppedUpdates uint64

	// done and doneOnce back Done(): done is created once in New (so Done()
	// itself never needs a nil check or lock) and closed exactly once, by
	// whichever terminal path (watchDeath's connection-death observation, or
	// an explicit Close) reaches its close(c.done) first. sync.Once is what
	// makes "exactly once" true under concurrent close/death rather than
	// merely "usually once": both paths may run concurrently (Close racing a
	// death watchDeath just observed), and closing an already-closed channel
	// panics, so the guard is load-bearing, not defensive boilerplate.
	done     chan struct{}
	doneOnce sync.Once
}

// New constructs a Client that will spawn cmd and negotiate opts's
// capabilities the first time Dial (the package function, or the (*Client)
// method of the same name) is called. No process is started and no I/O
// happens until then.
func New(cmd stdio.Command, opts Options) *Client {
	c := &Client{
		cmd:      cmd,
		opts:     opts.withDefaults(),
		sessions: make(map[protocol.SessionID]*Session),
		done:     make(chan struct{}),
	}
	c.attemptConnect = c.spawnAndConnect
	return c
}

// Dial constructs a Client via New and connects it, for callers that want a
// single spawn-and-initialize call rather than New's lazy, dial-later
// lifecycle (which foreignloops/driver/acp uses to dial on first Spawn).
func Dial(ctx context.Context, cmd stdio.Command, opts Options) (*Client, error) {
	c := New(cmd, opts)
	if err := c.Dial(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Dial runs the start-once connection state machine: the first caller (per
// idle period) becomes the attempt's owner and actually connects; any caller
// that arrives while an attempt is already in flight shares that one
// attempt's outcome instead of starting its own. A failed attempt resets the
// state to idle so that a later call — by the same or a different caller —
// starts a genuinely fresh attempt. Once connected, Dial is a fast no-op
// until the connection is closed or dies.
//
// A context canceled while waiting on someone else's attempt unblocks with
// ctx.Err() without affecting that attempt, which every other caller may
// still be waiting on.
func (c *Client) Dial(ctx context.Context) error {
	for {
		c.mu.Lock()
		switch c.state {
		case dialReady:
			c.mu.Unlock()
			return nil
		case dialClosed:
			c.mu.Unlock()
			return &ClosedError{}
		case dialDialing:
			w := c.waiter
			c.mu.Unlock()
			select {
			case <-w.done:
				continue // state has since changed (ready, or idle for retry)
			case <-ctx.Done():
				return ctx.Err()
			}
		default: // dialIdle
			attemptCtx, cancel := context.WithCancel(ctx)
			w := &dialAttempt{done: make(chan struct{}), cancel: cancel}
			c.waiter = w
			c.state = dialDialing
			c.mu.Unlock()

			err := c.attemptConnect(attemptCtx)

			c.mu.Lock()
			if c.state == dialClosed {
				// Close wins the attempt's terminal race. In particular, do
				// not let a successful attempt resurrect the Client to ready.
				err = &ClosedError{}
			} else if err != nil {
				c.state = dialIdle
			} else {
				c.state = dialReady
			}
			c.waiter = nil
			c.mu.Unlock()

			cancel()
			w.err = err
			close(w.done)
			return err
		}
	}
}

// spawnAndConnect is attemptConnect's production implementation: it spawns
// cmd as a real subprocess via acp/transport/stdio and hands the resulting
// pipes to finishConnect.
func (c *Client) spawnAndConnect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The Dial attempt context belongs to the handshake, not to the
	// successfully connected client. Dial cancels that context as soon as
	// finishConnect returns; binding exec.CommandContext to it would kill a
	// healthy ACP child immediately after initialize. Failed handshakes still
	// go through finishConnect's explicit proc.Kill path.
	proc, err := stdio.Spawn(context.WithoutCancel(ctx), c.cmd)
	if err != nil {
		return err
	}
	return c.finishConnect(ctx, proc, proc.Stdout, proc.Stdin)
}

// finishConnect wires a protocol.Conn over r/w, registers this Client's
// client-served method handlers, and completes "initialize". It is the
// shared tail end of every attemptConnect implementation — production
// (spawnAndConnect) and any in-process test double alike — so capability
// negotiation and handshake logic is exercised identically by both. On
// success it stores proc/conn/agent/initRes and starts watchDeath; on
// failure it tears down whatever it started (never leaking the conn or the
// process) and returns a typed error (wrapConnError: a peer that dies
// before, during, or right after answering "initialize" surfaces here as a
// *protocol.ConnClosedError, normalized to *ClosedError like every other
// Client method's connection-loss error).
func (c *Client) finishConnect(ctx context.Context, proc *stdio.Proc, r io.Reader, w io.Writer) error {
	conn := protocol.NewConn(r, w, protocol.ConnOptions{})
	c.registerClientHandlers(conn)
	agent := protocol.NewAgentConn(conn)

	resp, err := agent.Initialize(ctx, c.buildInitializeRequest())
	if err != nil {
		_ = conn.Close()
		if proc != nil {
			_ = proc.Kill()
		}
		return wrapConnError(err)
	}

	c.mu.Lock()
	if c.state == dialClosed {
		c.mu.Unlock()
		_ = conn.Close()
		if proc != nil {
			_ = proc.Kill()
		}
		return &ClosedError{}
	}
	c.proc, c.conn, c.agent, c.initRes = proc, conn, agent, resp
	c.mu.Unlock()

	go c.watchDeath(conn, proc)
	return nil
}

// watchDeath waits for conn to end (explicit Close, peer disconnect, or a
// transport failure) and then transitions the Client to dialClosed, fails
// every tracked Session's update stream, and ensures the subprocess is
// reaped. Close's own explicit teardown calls proc.Kill() too; Kill is
// idempotent, so calling it again here is always safe and guarantees the
// process is reaped even when the connection died on its own rather than
// through an explicit Close.
func (c *Client) watchDeath(conn *protocol.Conn, proc *stdio.Proc) {
	<-conn.Done()

	c.mu.Lock()
	c.state = dialClosed
	c.mu.Unlock()
	c.closeDone()

	c.closeAllSessions()

	if proc != nil {
		_ = proc.Kill()
	}
}

// Done returns a channel that is closed once this Client reaches a terminal
// closed state: an explicit Close call, or watchDeath observing the
// connection end on its own (peer disconnect or transport failure). It is
// safe to read from immediately after New, before any Dial — it simply
// never closes until the Client is actually terminated, including the case
// where Close is called before Dial ever succeeded (see Close's own doc: it
// transitions to closed and tears down whatever partial state exists,
// unconditionally, once called). A merely FAILED Dial attempt that leaves
// the Client retryable (see Dial's start-once state machine) does not close
// this channel: only a genuine terminal transition does. This lets a caller
// (in particular the ACP launch layer's owned-proxy lifecycle) react to
// unexpected child death without polling Client state.
func (c *Client) Done() <-chan struct{} { return c.done }

// closeDone closes c.done exactly once, however many terminal paths
// (watchDeath, Close, or both concurrently) reach it.
func (c *Client) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// currentAgent returns the connected AgentConn, or a typed error if the
// Client has never dialed successfully or is closed.
func (c *Client) currentAgent() (*protocol.AgentConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case dialReady:
		return c.agent, nil
	case dialClosed:
		return nil, &ClosedError{}
	default:
		return nil, &NotDialedError{}
	}
}

// loadTimeout returns the effective session/load deadline: Options.LoadTimeout
// if set, otherwise the package LoadTimeout default.
func (c *Client) loadTimeout() time.Duration {
	if c.opts.LoadTimeout > 0 {
		return c.opts.LoadTimeout
	}
	return LoadTimeout
}

// Close tears down the connection: it stops accepting new Dial attempts,
// fails every tracked Session's update stream, closes the protocol
// connection, and kills the subprocess (SIGINT, grace period, then
// SIGKILL — see stdio.Proc.Kill), waiting for it to be reaped. It is
// idempotent. If ctx is done before teardown completes, Close returns
// ctx.Err() but teardown continues in the background rather than being
// abandoned, so no goroutine or process is ever leaked.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.state == dialClosed {
		c.mu.Unlock()
		return nil
	}
	conn, proc := c.conn, c.proc
	var cancel context.CancelFunc
	if c.state == dialDialing && c.waiter != nil {
		cancel = c.waiter.cancel
	}
	c.state = dialClosed
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.closeDone()

	c.closeAllSessions()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if proc != nil {
			_ = proc.Kill()
		}
		if conn != nil {
			_ = conn.Close()
		}
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DroppedUpdates reports how many inbound session/update notifications
// named a sessionId this Client has no registered Session for (an unknown or
// already-closed session), and so could not be routed anywhere. This mirrors
// protocol.Conn.DroppedNotifications: a diagnostic counter, never a silent
// failure with no way to observe it.
func (c *Client) DroppedUpdates() uint64 {
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()
	return c.droppedUpdates
}

// SetModelCapability is unforgeable proof that this Client's negotiated
// "initialize" response advertised — under a caller-checked _meta key — the
// non-standard "session/set_model" extension some ACP adapters implement
// (see Session.SetModel). The zero value is not proof of anything and
// SetModel refuses it; the only way to obtain a granted SetModelCapability
// is ProveSetModelCapability, which checks the real bytes this Client
// received during the handshake rather than trusting any caller assertion.
type SetModelCapability struct {
	granted bool
}

// ProveSetModelCapability reports whether this Client's stored "initialize"
// response `_meta` contains key as a present, non-null top-level field, and
// returns a SetModelCapability recording the answer (ok is the same
// boolean, returned separately so a caller can branch without inspecting
// the capability value's private state).
//
// Different ACP adapters that implement the unstable session/set_model
// extension are expected to advertise it under different, adapter-specific
// _meta keys — there is no pinned schema for this extension in
// protocol/types_gen.go to standardize one — so the caller (a connector in
// acp/launch, which knows its specific adapter's documented key) supplies
// the exact key to check. This method's only job is making that check
// unforgeable and centralized: it is always evaluated against the real
// initialize response, never a value a caller could fabricate directly, and
// it deliberately never calls session/set_model itself to "check" for
// support — some ACP adapters answer an unrecognized method with a bare
// `{}` success rather than a JSON-RPC error, which would make a speculative
// probe silently misread as support.
//
// ok is false both when the key is genuinely absent (or _meta is empty or
// not a JSON object) and when this Client has never dialed successfully;
// SetModel refuses either way.
func (c *Client) ProveSetModelCapability(key string) (proof SetModelCapability, ok bool) {
	c.mu.Lock()
	resp := c.initRes
	c.mu.Unlock()
	if resp == nil || len(resp.Meta) == 0 {
		return SetModelCapability{}, false
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(resp.Meta, &meta); err != nil {
		return SetModelCapability{}, false
	}
	raw, present := meta[key]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return SetModelCapability{}, false
	}
	return SetModelCapability{granted: true}, true
}
