// Package sse is the legacy HTTP+SSE transport, and it exists for exactly one
// reason: there are MCP servers that predate Streamable HTTP and still work.
//
// # Compatibility-only
//
// This transport implements the 2024-11-05 spec's HTTP+SSE transport, which the
// specification has since replaced with Streamable HTTP (pkg/transport/-
// streamablehttp) and removed. Do not choose it for anything new. It is here so
// that a server nobody is going to update is reachable, and for nothing else.
//
// It is opt-in and cannot be reached by accident. Nothing selects it, nothing
// falls back to it, and no configuration ever "upgrades" or "downgrades" into
// it: a caller gets this transport by importing this package and calling New,
// which is a decision with a name on it. That is deliberate. A client that
// silently accepted a legacy transport when a modern one failed would let a
// server choose which protocol it is talked to over, and the design lists
// accepting legacy SSE "only when explicitly configured" among the tolerances
// that are safe *because* they are explicit.
//
// # Compatibility-only is not permission to be lax
//
// Being the legacy option buys this transport nothing. It does not weaken
// validation, auth, limits, or cancellation: it shares them, as code, with
// Streamable HTTP (internal/httpsec, internal/httpconn) rather than restating
// them. TLS is verified with a 1.2 floor, cleartext is refused to anything but
// loopback, credentials are attached per request and never sent off-origin,
// bodies are bounded whole, streams are bounded per frame and put on a clock,
// and no non-idempotent request is ever retried.
//
// # The endpoint event, which is this transport's real hazard
//
// The legacy protocol has a shape Streamable HTTP does not: the client GETs a
// stream, and the *server's first event tells the client where to POST*. The
// SDK implements this faithfully — it resolves that event's data as a URL
// reference against the endpoint — which means a server can name an absolute
// URL and be believed.
//
// That is an origin change chosen by the server, and it does not go anywhere
// near an http.Client's CheckRedirect, because it is not a redirect: it is a
// fresh request to whatever the server said. A transport that pinned only
// redirects would hand this module's credentials to any host a legacy server
// cared to name.
//
// So the origin is pinned in the RoundTripper, where every request passes
// however its URL was chosen (httpsec.RoundTripper.Origin), and the pin is
// checked before a credential is attached. Both guards are installed: the
// redirect guard refuses the stdlib's hops, and the RoundTripper's refuses the
// server's. See TestPostEndpointCannotLeaveTheOrigin.
//
// # Retries
//
// A tool call is never retried, and here that is a property of what the SDK's
// SSE client does not have rather than of anything switched off: it has no
// OAuthHandler to re-send a POST on a 401, and no stream resumption to replay.
// A POST carrying a call is issued once, and its failure is reported.
//
// A dropped stream is not reconnected either. The legacy protocol has no
// Last-Event-ID resumption, so there is nothing safe to resume: the session
// ends and the client above rebuilds it, which is the client's decision and not
// this transport's to make quietly.
package sse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/httpconn"
	"github.com/looprig/mcp/internal/httpsec"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/pkg/auth"
	"github.com/looprig/mcp/pkg/client"
)

// kind is the transport kind this package reports. It names the legacy
// transport, which is the other of MCP's two HTTP transports (see
// pkg/transport/streamablehttp).
const kind = "sse"

// displayName is how this transport names itself in an error's message. It is
// prose where kind is an identifier, and it is this module's own word: nothing
// a server sends ever reaches it.
const displayName = "SSE"

// The contracts this package exists to satisfy.
var (
	_ client.TransportFactory = (*factory)(nil)
	_ protocol.Conn           = (*httpconn.Conn)(nil)
)

// Config configures a legacy SSE transport.
//
// It is deliberately the same shape as streamablehttp.Config. The transports
// differ in the protocol they speak, not in what a caller has to decide, and a
// caller moving off this one should have nothing to rewrite but the import.
type Config struct {
	// Endpoint is the server's SSE URL: https://, or http:// for a loopback
	// host only. It is the URL the stream is GET from; where messages are POSTed
	// is the server's to say, within this origin (see the package comment).
	//
	// It may carry a path and a query, and neither ever appears in
	// RedactedOrigin or in an error, because a query string is a place people
	// put tokens.
	//
	// It is validated once, by New, against exactly the rules auth.Key demands
	// of an origin, so a URL this transport accepts is a URL the token store can
	// key by.
	Endpoint string
	// Headers are static application-supplied headers, attached to every
	// request. A value may be a credential and is treated as one.
	//
	// They are applied before Auth, so a provider and a static header naming the
	// same field resolve in the provider's favour: the live credential wins over
	// the configured one.
	Headers []auth.Header
	// Auth supplies credential headers per request. Nil means no auth headers.
	//
	// It is consulted on every request rather than once per connection, which is
	// what makes an expiring credential work: a provider that refreshes returns
	// the new value on the next request without the connection noticing.
	Auth auth.HeaderProvider
	// HTTPClient is the client to use. Nil selects a client built from Timeouts
	// with TLS 1.2 as its floor, which is the expected case.
	//
	// A supplied client is vetted, not trusted: New refuses one whose transport
	// disables certificate verification, and the transport is cloned and given a
	// TLS 1.2 floor rather than being used as-is. A non-zero Timeout is refused:
	// it is a deadline on a whole exchange, and it would sever the stream this
	// transport's whole session hangs on.
	HTTPClient *http.Client
	// Timeouts bounds the network. Zero fields select their defaults.
	Timeouts Timeouts
}

// Timeouts bounds every wait this transport's HTTP layer performs. Every field
// is explicit and defaulted; none is ever zero in a running transport.
//
// There is deliberately no whole-request deadline for MCP traffic, for the
// reason streamablehttp.Timeouts gives and which is sharper here: this
// transport's session *is* a hanging GET, so an http.Client.Timeout would kill
// every session on a timer.
type Timeouts struct {
	// Dial bounds the TCP connect. Zero means DefaultDialTimeout.
	Dial time.Duration
	// TLSHandshake bounds the TLS handshake. Zero means
	// DefaultTLSHandshakeTimeout.
	TLSHandshake time.Duration
	// ResponseHeader bounds the wait for a response's headers, on every request.
	// This is the bound that catches a server which accepts a request and then
	// says nothing. Zero means DefaultResponseHeaderTimeout.
	ResponseHeader time.Duration
	// Frame bounds how long one wire frame may take to arrive, measured from its
	// first byte to its last. Zero means DefaultFrameTimeout.
	//
	// It is a completion deadline and not an idle one: a healthy SSE stream is
	// silent for hours by design, so a bound on silence would break the sessions
	// it is meant to protect, while silence *inside* a frame the server has
	// already begun is never legitimate.
	Frame time.Duration
	// IdleConn bounds how long a pooled connection is kept alive unused. Zero
	// means DefaultIdleConnTimeout.
	IdleConn time.Duration
	// Request bounds a whole request that cannot stream. Zero means
	// DefaultRequestTimeout.
	//
	// This transport makes no such request of its own — it GETs a stream and
	// POSTs messages — so it is close to vestigial here, and it is kept because
	// the bound belongs to the shared HTTP layer and a zero would mean
	// "unbounded" to it.
	Request time.Duration
}

// Defaults applied when the corresponding Timeouts field is zero. They match
// streamablehttp's: the network is the same network.
const (
	// DefaultDialTimeout bounds the TCP connect.
	DefaultDialTimeout = 10 * time.Second
	// DefaultTLSHandshakeTimeout bounds the TLS handshake.
	DefaultTLSHandshakeTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout bounds the wait for response headers.
	DefaultResponseHeaderTimeout = 30 * time.Second
	// DefaultFrameTimeout bounds one frame's arrival.
	DefaultFrameTimeout = 60 * time.Second
	// DefaultIdleConnTimeout bounds an unused pooled connection.
	DefaultIdleConnTimeout = 90 * time.Second
	// DefaultRequestTimeout bounds a whole non-streaming request.
	DefaultRequestTimeout = 10 * time.Second
)

// withDefaults returns a copy with every zero field replaced by its default.
func (t Timeouts) withDefaults() Timeouts {
	pick := func(v, def time.Duration) time.Duration {
		if v == 0 {
			return def
		}
		return v
	}
	return Timeouts{
		Dial:           pick(t.Dial, DefaultDialTimeout),
		TLSHandshake:   pick(t.TLSHandshake, DefaultTLSHandshakeTimeout),
		ResponseHeader: pick(t.ResponseHeader, DefaultResponseHeaderTimeout),
		Frame:          pick(t.Frame, DefaultFrameTimeout),
		IdleConn:       pick(t.IdleConn, DefaultIdleConnTimeout),
		Request:        pick(t.Request, DefaultRequestTimeout),
	}
}

// httpsec projects the defaulted timeouts onto the narrow view the shared HTTP
// layer enforces. Call it on a defaulted value: httpsec expects positive fields
// and this package's defaults are what make them so.
func (t Timeouts) httpsec() httpsec.Timeouts {
	return httpsec.Timeouts{
		Dial:           t.Dial,
		TLSHandshake:   t.TLSHandshake,
		ResponseHeader: t.ResponseHeader,
		Frame:          t.Frame,
		IdleConn:       t.IdleConn,
		Request:        t.Request,
	}
}

// validate reports the first negative field, naming it. Zero is valid (it means
// "use the default").
func (t Timeouts) validate() error {
	for _, f := range []struct {
		name  string
		value time.Duration
	}{
		{"Timeouts.Dial", t.Dial},
		{"Timeouts.TLSHandshake", t.TLSHandshake},
		{"Timeouts.ResponseHeader", t.ResponseHeader},
		{"Timeouts.Frame", t.Frame},
		{"Timeouts.IdleConn", t.IdleConn},
		{"Timeouts.Request", t.Request},
	} {
		if f.value < 0 {
			return fmt.Errorf("%s: negative duration %v", f.name, f.value)
		}
	}
	return nil
}

// factory is the TransportFactory. It is immutable once New has returned it,
// which is what makes Kind, RedactedOrigin and Connect safe to call
// concurrently.
type factory struct {
	// endpoint is the validated stream URL, path and query included.
	endpoint string
	// origin is the precomputed RedactedOrigin: scheme://host[:port] and nothing
	// else. It is also the pin every request is checked against.
	origin   string
	headers  []auth.Header
	provider auth.HeaderProvider
	times    Timeouts
	// base is the vetted HTTP transport every connection's client is built on.
	// It is shared: an *http.Transport is a connection pool and is safe for
	// concurrent use.
	base *http.Transport
}

// New validates cfg and returns a legacy SSE TransportFactory.
//
// Calling it is the explicit opt-in the package comment describes: this is the
// only way to get this transport, and there is no path that reaches it without
// a caller naming it.
//
// It fails closed: every violation is a *client.Error of class
// FailureInvalidConfig, and no connection is attempted, no credential is fetched
// and no name is resolved until Connect. Config errors name the offending field,
// and the endpoint's origin — which is not a secret — but never a header's
// value, a query string, or a token, which may be.
func New(cfg Config) (client.TransportFactory, error) {
	fail := func(msg string) error {
		return client.NewError(client.FailureInvalidConfig, "", httpconn.OpNew, msg, nil)
	}

	endpoint, origin, err := httpsec.ResolveEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, fail(err.Error())
	}
	for i, h := range cfg.Headers {
		if err := h.Validate(); err != nil {
			// The auth error already names the header and withholds its value;
			// the index is added because a caller's slice is what it must fix.
			return nil, fail(fmt.Sprintf("Headers[%d]: %s", i, err.Error()))
		}
	}
	if err := cfg.Timeouts.validate(); err != nil {
		return nil, fail(err.Error())
	}
	base, err := httpsec.VetTransport(cfg.HTTPClient, cfg.Timeouts.withDefaults().httpsec())
	if err != nil {
		return nil, fail(err.Error())
	}

	return &factory{
		endpoint: endpoint,
		origin:   origin,
		// A detached copy: a Definition is immutable after validation, and a
		// caller that keeps its slice must not be able to rewrite what was
		// validated onto a live request.
		headers:  slices.Clone(cfg.Headers),
		provider: cfg.Auth,
		times:    cfg.Timeouts.withDefaults(),
		base:     base,
	}, nil
}

// Kind returns the transport kind.
func (f *factory) Kind() string { return kind }

// RedactedOrigin returns the endpoint's origin: scheme://host[:port], with the
// path, the query and any userinfo left out. It is safe to log.
func (f *factory) RedactedOrigin() string { return f.origin }

// Connect establishes one legacy SSE session.
//
// It does no I/O: the SDK's transport connects on the first use, which is
// Initialize. What this builds is the client that use will go through.
func (f *factory) Connect(ctx context.Context, cfg protocol.ConnectConfig) (protocol.Conn, error) {
	if err := ctx.Err(); err != nil {
		class := client.FailureCancelled
		if errors.Is(err, context.DeadlineExceeded) {
			class = client.FailureStartupTimeout
		}
		return nil, client.NewError(class, "", httpconn.OpConnect,
			"the SSE transport was not connected", err)
	}

	// Per connection, not per factory: diags is where a failure's cause is
	// recorded on its way past, and two connections must not read each other's.
	d := &httpsec.Diagnostics{}
	// Wrapped, not used raw: the SDK binds the session's hanging GET to the
	// context that connects it, and the context that connects it is a bounded
	// startup deadline. See stream.go — without this the handshake succeeds and
	// every request after it is answered "session not found".
	session := protocol.NewSession(&sessionTransport{inner: &mcp.SSEClientTransport{
		Endpoint:   f.endpoint,
		HTTPClient: f.httpClient(d, httpsec.NewWireLimits(cfg.Wire)),
	}}, cfg)
	return httpconn.New(session, d, displayName, f.endpoint, f.origin), nil
}

// httpClient builds the client for one connection: the shared, vetted transport
// underneath, wrapped in this connection's own RoundTripper.
//
// The wrapper is per-connection because what it carries is: the credentials for
// this binding, and the diagnostics buffer this connection's errors are
// classified from. The pool underneath is shared, so the wrapping costs a struct
// and not a socket.
func (f *factory) httpClient(d *httpsec.Diagnostics, w httpsec.WireLimits) *http.Client {
	return &http.Client{
		// No Timeout, by design: see Timeouts. This transport's session is a
		// hanging GET, and a whole-exchange deadline would end it on a clock.
		CheckRedirect: httpsec.RedirectGuard(f.origin),
		Transport: &httpsec.RoundTripper{
			Base:     f.base,
			Headers:  f.headers,
			Provider: f.provider,
			Wire:     w,
			Request:  f.times.Request,
			Frame:    f.times.Frame,
			Diags:    d,
			// Load-bearing here in a way it is not for Streamable HTTP, whose
			// URLs are all derived from its own config. This transport POSTs to
			// a URL the *server* named in its endpoint event, so this pin is the
			// only thing standing between a legacy server and this module's
			// credentials. See the package comment.
			Origin: f.origin,
		},
	}
}
