// This file is the one place this transport declines to use the SDK as-is, and
// the reason is a real defect rather than a preference.
//
// # The problem
//
// The legacy protocol's session *is* a hanging GET: the stream the server sends
// every reply on is opened once, at connect, and must live for as long as the
// session does. The SDK's SSEClientTransport opens that GET with the context
// passed to its Connect (see its use of http.NewRequestWithContext), and keeps
// reading the body afterwards — so the stream is bound to the lifetime of the
// call that established it.
//
// That is fine if Connect is handed a background context, and fatal if it is
// handed a bounded one. It is handed a bounded one: internal/protocol's Session
// connects the transport from Initialize, and pkg/client calls Initialize under
// a context bounded by Timeouts.Startup and cancels it the moment startup
// returns. The result is a session that completes its handshake and then dies —
// every subsequent request answered "session not found" by the server, because
// the server tore the session down when the GET it was keyed to went away.
//
// A test proves it end to end (TestSessionOutlivesTheContextThatConnectedIt);
// without this wrapper the fixture's own tool calls 404.
//
// # The fix, and its shape
//
// The two things the SDK conflates are genuinely different and both are wanted:
//
//   - Connecting must be bounded by the caller's context. A server that will not
//     answer the GET, or send its endpoint event, must not hang startup — that
//     is what Timeouts.Startup is for.
//   - The stream, once established, must outlive that context and end only when
//     the session is closed.
//
// So the context handed to the SDK is detached from the caller's, and the
// caller's is bridged onto it only for the duration of the connect (see
// context.AfterFunc). After Connect returns, the only thing that can end the
// stream is Close — which is exactly the contract a session needs, and exactly
// what the SDK's Connection interface says Close is for.
//
// This is a wrapper and not a reimplementation. Everything about the legacy
// protocol — the endpoint event, the POSTs, the framing — stays the SDK's, and
// the security posture stays internal/httpsec's. What is corrected here is the
// lifetime of one HTTP request, which is not the SDK's protocol knowledge and is
// not something this module can express through its API.

package sse

import (
	"context"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The contracts this file satisfies.
var (
	_ mcp.Transport  = (*sessionTransport)(nil)
	_ mcp.Connection = (*sessionConn)(nil)
)

// sessionTransport wraps an mcp.Transport so that the connection it yields
// outlives the context that established it.
type sessionTransport struct {
	inner mcp.Transport
}

// Connect establishes the stream under a context of the session's own.
//
// ctx bounds the connect and nothing after it: the returned connection holds the
// cancel, and Close is what uses it. A caller that abandons ctx while Connect is
// still running gets the connect abandoned too, which is the bound it asked for.
func (t *sessionTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// Detached first, then bridged: the derived context must not inherit the
	// caller's cancellation, because inheriting it is the whole defect. The
	// bridge below re-adds it for the connect alone.
	sessCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	// While Connect runs, the caller's context still ends it. stop() takes the
	// bridge down the moment Connect returns, so from then on nothing but Close
	// can cancel sessCtx.
	stop := context.AfterFunc(ctx, cancel)

	conn, err := t.inner.Connect(sessCtx)
	stop()
	if err != nil {
		cancel()
		return nil, err
	}
	// A caller's context that ended *during* Connect may have cancelled sessCtx
	// before stop() ran, leaving a connection whose stream is already dead. It
	// is reported as the failure it is rather than returned as a session that
	// will not work.
	if err := sessCtx.Err(); err != nil {
		cancel()
		_ = conn.Close()
		return nil, err
	}
	return &sessionConn{Connection: conn, cancel: cancel}, nil
}

// sessionConn is a connection whose Close also releases the session's context,
// which is what ends the hanging GET.
type sessionConn struct {
	mcp.Connection
	cancel context.CancelFunc
	// once guards the cancel: the SDK documents that Close may be called
	// multiple times, potentially concurrently.
	once sync.Once
}

// Close ends the session and the stream underneath it.
func (c *sessionConn) Close() error {
	err := c.Connection.Close()
	// After the SDK's Close, not before: cancelling first would race the
	// transport's own teardown and turn an orderly close into a cancelled read.
	c.once.Do(c.cancel)
	return err
}
