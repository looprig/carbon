// Package streamablehttp is the MCP Streamable HTTP transport: it speaks the
// protocol to a remote server over HTTP POST, with server-to-client messages
// arriving on SSE streams.
//
// # What this transport owns, and what it does not
//
// It owns the HTTP client. It does not own the HTTP protocol: unlike the stdio
// transport — which declines the SDK's command transport because that transport
// starts the process itself, leaving no seam for a host that confines its
// servers — this package uses the SDK's StreamableClientTransport, because the
// SDK's HTTP transport has the seam stdio's lacked. It takes an *http.Client,
// and an *http.Client is enough to own everything this module needs to own:
//
//   - credentials, via a RoundTripper that asks the auth seam per request;
//   - TLS, via that client's transport, which this package builds or vets;
//   - timeouts, likewise;
//   - what may be buffered, via the response bodies that RoundTripper returns.
//
// What is left over is the part that is fiddly and that nobody should write
// twice: the Mcp-Session-Id handshake, the Mcp-Protocol-Version header, the
// standalone SSE stream, resumption by Last-Event-ID, and the DELETE that ends
// a session. That is protocol, the SDK implements it, and this package uses the
// SDK for what the SDK is for.
//
// # Retries
//
// A tool call is never retried. Not "retried carefully" — never.
//
// This is a property of the wiring and not a promise made in a comment. The
// SDK's client re-sends a POST in exactly one place: when it is holding an
// OAuthHandler and a request comes back 401 or 403, it authorizes and sends the
// same POST again. This transport leaves OAuthHandler nil — permanently, and
// the auth seam here is a RoundTripper precisely so that it can stay nil — so
// the POST carrying a call is issued once and its failure is reported, whatever
// it was. There is no path that resends it, so there is no path that can
// double-execute a tool that charges a credit card.
//
// What is retried is reading. A dropped SSE stream is reconnected with
// Last-Event-ID, which resumes the byte stream the server is already committed
// to sending; the request that caused it is not re-issued. That is the
// protocol's own resumption and it is safe by construction: it re-reads a
// reply, it does not re-run a call.
//
// # Trust
//
// The server is untrusted and remote, which makes it strictly worse than the
// stdio child: it is anonymous until TLS says otherwise, it can hold a stream
// open forever, it can answer a small request with an unbounded body, and it can
// tell this client to go somewhere else. So TLS is verified always, cleartext is
// refused to anything but loopback, every response body is bounded before it is
// buffered, every stream is bounded per frame — because a total on a stream that
// is designed to live for a session is not a limit, it is an expiry date — and
// every frame is on a clock, because a server that starts a message and stops is
// otherwise a socket held forever.
//
// A redirect that leaves the configured origin is refused outright, credential
// or no credential. This one is worth stating plainly because getting it wrong
// makes this package *worse* than the http.Client underneath it: the stdlib
// strips Authorization across origins, and a transport that attaches
// credentials in a RoundTripper — as this one does, so that they can be
// refreshed per request — runs below that logic and would put the header back.
// See httpsec.RedirectGuard.
package streamablehttp

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

// kind is the transport kind this package reports. It matches the package name
// and the spec's name for the transport; "http" would be a claim about a
// protocol rather than about which of MCP's two HTTP transports this is (the
// other being pkg/transport/sse).
const kind = "streamablehttp"

// displayName is how this transport names itself in an error's message. It is
// prose where kind is an identifier, and it is this module's own word: nothing a
// server sends ever reaches it.
const displayName = "streamable HTTP"

// The contracts this package exists to satisfy.
var (
	_ client.TransportFactory = (*factory)(nil)
	_ protocol.Conn           = (*httpconn.Conn)(nil)
)

// streamReconnects is how many times a dropped SSE stream is reconnected before
// the session is failed. It is stated here rather than left to the SDK's default
// so that the number is a decision of this package's: reconnecting a read is
// safe (see the package comment), and five attempts with the SDK's jittered
// backoff spans about a minute of a server being briefly unreachable.
//
// It bounds reconnects that make no progress. A stream that keeps delivering
// events resets the count, so a long, healthy session is never cut off by it.
const streamReconnects = 5

// Config configures a Streamable HTTP transport.
type Config struct {
	// Endpoint is the MCP server's URL: https://, or http:// for a loopback
	// host only. It may carry a path and a query — "https://h/mcp?v=2" is an
	// ordinary endpoint — and neither ever appears in RedactedOrigin or in an
	// error, because a query string is a place people put tokens.
	//
	// It is validated once, by New, against exactly the rules auth.Key demands
	// of an origin, so a URL this transport accepts is a URL the token store
	// can key by.
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
	// TLS 1.2 floor rather than being used as-is — so this package's guarantees
	// hold for a caller's client too, and the caller's client is not mutated. A
	// non-zero Timeout is refused: it is a deadline on a whole exchange, and it
	// would sever the streams this transport is built on.
	//
	// The default client honors the proxy environment (HTTP_PROXY, HTTPS_PROXY,
	// NO_PROXY), as http.DefaultTransport and pkg/auth do. That is worth knowing
	// for a transport this deliberate about what may see a credential: those
	// variables can route this traffic through a host the endpoint does not
	// name. It stays the default because it is what every Go HTTP client does
	// and operators rely on it, and because the exposure is narrow — an https
	// endpoint stays end-to-end encrypted through a proxy's CONNECT, so the
	// proxy sees the host but not the credential, and loopback is exempt.
	// Supply an HTTPClient whose transport sets Proxy: nil to opt out.
	HTTPClient *http.Client
	// Timeouts bounds the network. Zero fields select their defaults.
	Timeouts Timeouts
}

// Timeouts bounds every wait the HTTP layer performs. Every field is explicit
// and defaulted; none is ever zero in a running transport.
//
// There is deliberately no whole-request deadline for MCP traffic. The obvious
// one — http.Client.Timeout — cancels a response while its body is being read,
// which for this transport means killing the SSE stream a session is listening
// on, and killing a long tool call that is answering exactly as intended. Those
// two are bounded where the knowledge is: ResponseHeader catches a server that
// is not answering, the caller's context bounds the call (which is what the
// client's own Timeouts.Request sets), and Frame catches the case neither of
// those sees — a server that answers, starts a message, and stops.
//
// Between them the bounds cover a request end to end: getting a socket (Dial,
// TLSHandshake), getting an answer (ResponseHeader), getting each message of it
// (Frame), and letting go (Request, IdleConn).
type Timeouts struct {
	// Dial bounds the TCP connect. Zero means DefaultDialTimeout.
	Dial time.Duration
	// TLSHandshake bounds the TLS handshake. Zero means
	// DefaultTLSHandshakeTimeout.
	TLSHandshake time.Duration
	// ResponseHeader bounds the wait for a response's headers, on every
	// request. This is the bound that catches a server which accepts a request
	// and then says nothing — it stops before the body, so it costs a stream
	// nothing. Zero means DefaultResponseHeaderTimeout.
	ResponseHeader time.Duration
	// Frame bounds how long one wire frame may take to arrive, measured from
	// its first byte to its last. Zero means DefaultFrameTimeout.
	//
	// This is the "idle timeout" the transport needs, expressed in the only
	// unit that means anything for a stream that is idle by design. A bound on
	// silence would be wrong: a healthy SSE stream says nothing for hours at a
	// time, and cutting it off for that would break the sessions it is meant to
	// protect. Silence inside a frame the server has already begun is the thing
	// that is never legitimate, and that is what this bounds — see frames.go.
	//
	// It is a completion deadline and not an idle one, deliberately: a deadline
	// that any byte resets is a deadline a server dribbling one byte at a time
	// never trips, which is the attack. A frame that legitimately needs longer
	// than this is a very large frame on a very slow link; raise it.
	Frame time.Duration
	// IdleConn bounds how long a pooled connection is kept alive unused. It is
	// about sockets this transport is not using; for the bound on a server that
	// has gone quiet mid-message, see Frame. Zero means
	// DefaultIdleConnTimeout.
	IdleConn time.Duration
	// Request bounds a whole request that cannot stream — in practice the DELETE
	// that ends the session at Close, which is the one request this transport
	// makes whose response is not, and cannot become, a stream. Zero means
	// DefaultRequestTimeout.
	//
	// It is the bound on how long a server can drag out a shutdown, and that is
	// not only Close's problem: the SDK closes the session itself when a
	// handshake fails, from inside the call, so a cancelled Initialize does not
	// return until the DELETE it triggers has finished or hit this. A server
	// that accepts a DELETE and never answers therefore delays a cancelled
	// caller by up to this long. Lower it if a caller must come back sooner;
	// it cannot be removed, because a DELETE with no bound would hang shutdown
	// outright.
	Request time.Duration
}

// Defaults applied when the corresponding Timeouts field is zero.
const (
	// DefaultDialTimeout bounds the TCP connect.
	DefaultDialTimeout = 10 * time.Second
	// DefaultTLSHandshakeTimeout bounds the TLS handshake.
	DefaultTLSHandshakeTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout bounds the wait for response headers. It is
	// generous because a legitimate tool call can be slow to start answering,
	// and a server that has said nothing for this long is not going to.
	DefaultResponseHeaderTimeout = 30 * time.Second
	// DefaultFrameTimeout bounds one frame's arrival. It is generous enough for
	// a large result over a poor link — the frame cap is a few mebibytes, and a
	// minute of it is a floor of tens of kilobytes a second — and short enough
	// that a server which has stopped mid-message is noticed the same minute.
	DefaultFrameTimeout = 60 * time.Second
	// DefaultIdleConnTimeout bounds an unused pooled connection.
	DefaultIdleConnTimeout = 90 * time.Second
	// DefaultRequestTimeout bounds a whole non-streaming request — the DELETE
	// that releases the session. It is shorter than the other bounds on
	// purpose: nothing reads the DELETE's answer, a server that has not
	// released a session in this long is not going to, and this is the bound a
	// cancelled caller waits out (see Timeouts.Request), so a generous value
	// here is paid for by the caller who already gave up.
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
//
// The mapping is explicit rather than a shared struct because the two types have
// different jobs: this one is a caller's configuration surface, with the prose
// and the defaults that belong to it, and httpsec.Timeouts is what the
// enforcement reads.
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
	// endpoint is the validated request URL, path and query included.
	endpoint string
	// origin is the precomputed RedactedOrigin: scheme://host[:port] and
	// nothing else. It never depends on anything that could change, so it
	// cannot drift from what was validated.
	origin   string
	headers  []auth.Header
	provider auth.HeaderProvider
	times    Timeouts
	// base is the vetted HTTP transport every connection's client is built on.
	// It is shared: an *http.Transport is a connection pool and is safe for
	// concurrent use, and sharing it is what lets two connections to one server
	// reuse a socket.
	base *http.Transport
}

// New validates cfg and returns a Streamable HTTP TransportFactory.
//
// It fails closed: every violation is a *client.Error of class
// FailureInvalidConfig, and no connection is attempted, no credential is
// fetched and no name is resolved until Connect. Config errors name the
// offending field, and the endpoint's origin — which is not a secret — but
// never a header's value, a query string, or a token, which may be.
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

// Kind implements client.TransportFactory.
func (f *factory) Kind() string { return kind }

// RedactedOrigin implements client.TransportFactory. It is the endpoint's
// origin — scheme://host[:port] — and nothing else.
//
// The path and the query are withheld, and that is the whole point rather than
// tidiness: "?access_token=..." is a real thing that appears in real
// configuration, and RedactedOrigin's output is destined for logs. Headers and
// the auth provider are not consulted at all.
func (f *factory) RedactedOrigin() string { return f.origin }

// Connect returns a connection ready to be initialized. It performs no I/O:
// there is no process to start and no socket to open until a request is made,
// and the handshake is Initialize's job — so a server that is unreachable is
// reported by the operation that discovered it rather than by construction.
//
// ctx bounds nothing here for that reason; it is checked so that an already
// cancelled caller does not receive a connection it never asked to keep.
func (f *factory) Connect(ctx context.Context, cfg protocol.ConnectConfig) (protocol.Conn, error) {
	if err := ctx.Err(); err != nil {
		class := client.FailureCancelled
		if errors.Is(err, context.DeadlineExceeded) {
			class = client.FailureStartupTimeout
		}
		return nil, client.NewError(class, "", httpconn.OpConnect,
			"the streamable HTTP transport was not connected", err)
	}

	// Per connection, not per factory: diags is where a failure's cause is
	// recorded on its way past, and two connections must not read each other's.
	d := &httpsec.Diagnostics{}
	session := protocol.NewSession(&mcp.StreamableClientTransport{
		Endpoint:   f.endpoint,
		HTTPClient: f.httpClient(d, httpsec.NewWireLimits(cfg.Wire)),
		MaxRetries: streamReconnects,
		// OAuthHandler is deliberately left nil, and this is load-bearing: it is
		// the SDK's only path that re-sends a POST, and a re-sent POST is a
		// re-run tool call. Credentials reach a request through the RoundTripper
		// instead — see the package comment.
	}, cfg)
	return httpconn.New(session, d, displayName, f.endpoint, f.origin), nil
}

// httpClient builds the client for one connection: the shared, vetted transport
// underneath, wrapped in this connection's own RoundTripper.
//
// The wrapper is per-connection because what it carries is: the credentials for
// this binding, and the diagnostics buffer this connection's errors are
// classified from. The pool underneath is shared, so the wrapping costs a
// struct and not a socket.
func (f *factory) httpClient(d *httpsec.Diagnostics, w httpsec.WireLimits) *http.Client {
	return &http.Client{
		// No Timeout, by design: see Timeouts. The bounds live on the transport
		// underneath and in the RoundTripper, where a stream can be told apart
		// from an exchange.
		CheckRedirect: httpsec.RedirectGuard(f.origin),
		Transport: &httpsec.RoundTripper{
			Base:     f.base,
			Headers:  f.headers,
			Provider: f.provider,
			Wire:     w,
			Request:  f.times.Request,
			Frame:    f.times.Frame,
			Diags:    d,
			// The endpoint's origin, pinned. This transport's URLs are all its
			// own — the SDK derives them from the configured endpoint — so this
			// is belt to CheckRedirect's braces here, and the load-bearing guard
			// in pkg/transport/sse, where a server supplies a URL directly.
			Origin: f.origin,
		},
	}
}
