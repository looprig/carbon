// Package httpsec is the HTTP security layer shared by this module's two
// network transports: the client they build, the client they will accept from a
// caller, and the RoundTripper through which every request and every response
// passes.
//
// It exists as one package because there is exactly one right answer to each of
// its questions, and two copies of an answer is two things to keep right. The
// hazard is not the writing, it is the drift: a second copy that is correct on
// the day it is written and a release behind six months later is worse than no
// copy at all, because it looks like it was thought about. So Streamable HTTP
// and legacy SSE differ in their protocol and share every one of their
// guarantees:
//
//   - TLS is verified, always, with a 1.2 floor — on a client this package
//     built and on a client a caller supplied (see VetTransport).
//   - Credentials are attached per request, so an expiring one is refreshed
//     without a connection noticing, and are never sent to an origin other
//     than the configured one (see RoundTripper.Origin).
//   - Nothing unbounded is buffered: a non-streaming body is capped whole, and
//     a stream is capped per frame, because a total on a stream designed to
//     live for a session is not a limit but an expiry date.
//   - A server that starts a frame and stops is on a clock (see frames.go).
//
// The RoundTripper is where the guarantees actually live. The SDK above it
// composes MCP out of HTTP requests; this is the one place that sees all of
// them, so it is the only place that can attach a credential to each one, bound
// what each one may return, and record why one failed.
//
// This package must not import the MCP go-sdk, and does not: what it owns is
// HTTP, and the SDK's business is the protocol carried over it.
package httpsec

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/looprig/mcp/internal/limits"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/pkg/auth"
)

// MinTLSVersion is the floor for every HTTPS connection a transport built on
// this package makes, on a client it built and on a client it was given alike.
const MinTLSVersion = tls.VersionTLS12

// MaxRedirects caps a redirect chain. A legitimate MCP endpoint does not need
// one at all — every hop here stays on the configured origin, so a chain is at
// most a path or scheme normalization.
const MaxRedirects = 5

// Timeouts bounds every wait the HTTP layer performs. It is this package's
// narrow view: each transport exports its own documented Timeouts and maps it
// onto this, because the defaults and the prose belong to the transport a
// caller configures, while the enforcement belongs here.
//
// Every field is expected positive; a transport passes a defaulted value.
type Timeouts struct {
	// Dial bounds the TCP connect.
	//
	// It is applied to every transport this package builds, and to a supplied
	// transport that has no DialContext of its own. A caller who installs a
	// DialContext owns its timeout: that field is how a proxy or a custom
	// resolver is configured, so this package fills it in when it is absent
	// rather than overwriting a dialing policy someone chose on purpose.
	Dial time.Duration
	// TLSHandshake bounds the TLS handshake.
	TLSHandshake time.Duration
	// ResponseHeader bounds the wait for a response's headers. It stops before
	// the body, so it costs a stream nothing.
	ResponseHeader time.Duration
	// Frame bounds how long one wire frame may take to arrive, measured from
	// its first byte to its last. It is a completion deadline and not an idle
	// one: a deadline any byte resets is a deadline a server dribbling one byte
	// at a time never trips, which is the attack.
	Frame time.Duration
	// IdleConn bounds how long a pooled connection is kept alive unused.
	IdleConn time.Duration
	// Request bounds a whole request that cannot stream.
	Request time.Duration
}

// RedirectGuard returns the http.Client CheckRedirect that refuses to leave
// origin, or to go round forever.
//
// This is not a hardening nicety; without it a transport here is strictly worse
// than the http.Client it wraps. The stdlib strips Authorization when a redirect
// crosses origins — and then this package's RoundTripper, which runs *below*
// that logic and attaches credentials to every request it sees, puts it back. A
// 302 from a configured endpoint to an attacker's host would hand over the
// bearer token, in cleartext if the redirect said http. A test proves both
// halves, because the failure is invisible from the happy path.
//
// The policy is origin-pinning, and it is deliberately stricter than the scheme
// check pkg/auth's defaultHTTPClient applies to its own flows:
//
//   - An MCP endpoint is configuration. The caller named one server; a server
//     that answers "I am actually over there" is not a redirect to follow, it is
//     a different server, and only the caller can decide to talk to it.
//   - The credential is keyed by origin. auth.Key binds a token to the origin it
//     was minted for, so sending it to another origin is incoherent by
//     construction — whatever the scheme, whatever the trust.
//   - Allowing cross-origin https "because TLS" would still send a token for
//     server A to server B. The scheme is not what makes that wrong.
//
// So same-origin redirects pass — "/mcp" to "/mcp/" is an ordinary thing for a
// server to do, and the credential is not going anywhere new — and everything
// else is refused. Refusing costs a caller nothing but an explicit config
// change, which is the point: it makes the move deliberate.
//
// Canonicalization is auth.CanonicalOrigin, the same function a transport's New
// validated its endpoint with, which is what makes config time and run time
// agree. A hop to a non-loopback http host does not need its own check: it fails
// to match the origin, and would fail CanonicalOrigin too.
//
// It is belt to RoundTripper.Origin's braces, and both are wanted. This one
// refuses the hop before it is made, which is the better error and the earlier
// stop; the RoundTripper's catches a URL that reached the client without ever
// being a redirect. Even a same-origin chain is bounded: a server that bounces a
// request between two of its own paths forever is a hang, and a hang is what
// every bound here exists to prevent.
func RedirectGuard(origin string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		target, err := auth.CanonicalOrigin(req.URL.String())
		if err != nil {
			// The URL is not repeated: a redirect target is server-controlled
			// and a query string is a place tokens live.
			return fmt.Errorf("refusing a redirect to an unusable URL: %s", err.Error())
		}
		if target != origin {
			return fmt.Errorf("refusing a redirect from %s to %s: an MCP endpoint's origin is configuration, "+
				"and a credential minted for one origin is never sent to another", origin, target)
		}
		return nil
	}
}

// ResolveEndpoint validates rawURL and splits it into the URL to request and
// the origin to display.
//
// The validation is auth.CanonicalOrigin's, not a second opinion: it is the
// function that decides what an origin is for the token store, and a transport
// that accepted a URL the store cannot key — or refused one it can — would put
// a credential and the server it is for in disagreement about what "the server"
// means. It brings the loopback rule with it, which is the rule that matters:
// cleartext is for a server on this machine, and tokens do not cross a network
// unencrypted.
//
// The request URL keeps its path and query; only the origin is derived. The two
// are separate values because they have separate audiences: one is sent, the
// other is shown.
func ResolveEndpoint(rawURL string) (endpoint, origin string, err error) {
	const field = "Endpoint"
	if rawURL == "" {
		return "", "", fmt.Errorf("%s is empty", field)
	}
	origin, err = auth.CanonicalOrigin(rawURL)
	if err != nil {
		// The auth error is already bounded, normalized and secret-free — it is
		// built to be logged — and it never renders its cause. Its text is the
		// message; the URL it describes is not repeated, because a query string
		// is not something to put in an error.
		return "", "", fmt.Errorf("%s is not usable: %s", field, err.Error())
	}
	// Parsed again only to normalize the request URL. CanonicalOrigin has
	// already accepted it, so this cannot fail; the error is checked rather
	// than discarded because "cannot fail" is a claim about today's code.
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return "", "", fmt.Errorf("%s is not a valid URL", field)
	}
	// Rebuilt from the canonical origin rather than reusing the caller's
	// scheme/host spelling: "HTTPS://Example.COM:0443/mcp" and
	// "https://example.com/mcp" must not become two different endpoints when
	// they are one server, and the origin is the spelling the token store uses.
	u.Scheme = ""
	u.Host = ""
	u.User = nil
	u.Fragment = ""
	return origin + u.String(), origin, nil
}

// WireLimits is this package's normalized view of protocol.WireLimits: every
// field is positive.
type WireLimits struct {
	MaxBody  int
	MaxFrame int
}

// Defaults applied when a WireLimits field is not positive.
//
// They exist because a transport may legitimately be driven without the client
// above it — a test, or an application composing its own — and limits.BoundedReader
// treats a non-positive bound as a programmer error, not as "unbounded". The
// values match client.DefaultLimits; they are restated rather than imported
// because pkg/client imports this direction, not the other.
const (
	// DefaultMaxBodyBytes caps one non-streaming response body.
	DefaultMaxBodyBytes = 16 << 20
	// DefaultMaxFrameBytes caps one SSE event.
	DefaultMaxFrameBytes = 4 << 20
)

// NewWireLimits normalizes what the client passed. A non-positive field means the
// caller did not set one — client.Connect always passes normalized values — and
// gets this package's default; it never means unbounded, which is not a setting.
func NewWireLimits(w protocol.WireLimits) WireLimits {
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return WireLimits{
		MaxBody:  pick(w.MaxBodyBytes, DefaultMaxBodyBytes),
		MaxFrame: pick(w.MaxFrameBytes, DefaultMaxFrameBytes),
	}
}

// VetTransport returns the *http.Transport every connection's client is built
// on, from the caller's client if it supplied one.
//
// A supplied client is neither trusted nor mutated. It is read, refused if it
// contradicts this package's guarantees, and otherwise cloned — so a caller that
// supplies a client to pin a CA or route through a proxy gets that, without also
// getting the ability to turn certificate verification off by accident, and
// without this package reaching into a value the caller still holds.
func VetTransport(c *http.Client, t Timeouts) (*http.Transport, error) {
	if c == nil {
		return DefaultTransport(t), nil
	}
	if c.Timeout != 0 {
		return nil, fmt.Errorf(
			"HTTPClient.Timeout is %v: it must be zero, because it bounds a whole exchange including the body — "+
				"it would sever the SSE streams this transport reads and cut off slow tool calls; "+
				"use Timeouts.ResponseHeader to bound a server that does not answer", c.Timeout)
	}
	if c.Transport == nil {
		return DefaultTransport(t), nil
	}
	base, ok := c.Transport.(*http.Transport)
	if !ok {
		// A custom RoundTripper. There is no way to read its TLS posture, so
		// there is no way to promise anything about it — and promising anyway is
		// the fail-open this module does not do. The refusal names the way in:
		// a caller wanting custom behavior supplies an *http.Transport, which
		// carries a Proxy, a DialContext and a TLSClientConfig between them.
		return nil, fmt.Errorf("HTTPClient.Transport is %T: it must be *http.Transport, "+
			"because this transport must be able to verify that TLS is configured safely", c.Transport)
	}
	if base.TLSClientConfig != nil && base.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("HTTPClient.Transport has TLSClientConfig.InsecureSkipVerify set: " +
			"certificate verification is not optional; to trust a private CA, set TLSClientConfig.RootCAs")
	}

	// Cloned, so what follows edits this package's copy and not the caller's
	// live client. Transport.Clone deep-copies TLSClientConfig, so the floor set
	// below cannot reach back either.
	base = base.Clone()
	if base.TLSClientConfig == nil {
		base.TLSClientConfig = &tls.Config{MinVersion: MinTLSVersion}
	} else if base.TLSClientConfig.MinVersion < MinTLSVersion {
		// Raised, not refused: zero is what a caller who never thought about it
		// leaves behind (it means "the stdlib's floor"), and refusing a client
		// that is merely unopinionated would make supplying a CA pool needlessly
		// painful. A caller who deliberately asked for TLS 1.0 gets 1.2 anyway,
		// which is the direction this package is allowed to move.
		base.TLSClientConfig.MinVersion = MinTLSVersion
	}
	// The timeouts are this package's contract, not the caller's: they are
	// stated as defaults on Timeouts and documented as always applying. Fields
	// the caller set are only overwritten where this package promises a bound.
	base.TLSHandshakeTimeout = t.TLSHandshake
	base.ResponseHeaderTimeout = t.ResponseHeader
	base.IdleConnTimeout = t.IdleConn
	if base.DialContext == nil {
		// A nil DialContext is not a caller's dialing policy — it is net/http's
		// zero-value fallback, which has no timeout at all and would leave the
		// connect bounded only by the OS TCP stack. That is the common shape
		// here: someone clones http.DefaultTransport's idea to pin a CA and
		// never thinks about dialing. Filling it in is what makes Timeouts.Dial
		// true on this path rather than only on DefaultTransport's.
		//
		// A caller who DID supply a DialContext keeps it, timeout and all. It is
		// the one field a caller has a real reason to own — a proxy, a custom
		// resolver, a test dialer — and overwriting it would break the use case
		// VetTransport exists to permit. See Timeouts.Dial, which states this
		// split.
		base.DialContext = (&net.Dialer{
			Timeout:   t.Dial,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	return base, nil
}

// DefaultTransport builds the transport used when the caller supplies no
// client. It is http.DefaultTransport's shape with this package's bounds and a
// TLS floor, rather than http.DefaultTransport itself: that one is shared with
// the whole process, and editing it would be editing everyone's.
func DefaultTransport(t Timeouts) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   t.Dial,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       t.IdleConn,
		TLSHandshakeTimeout:   t.TLSHandshake,
		ResponseHeaderTimeout: t.ResponseHeader,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			// Never InsecureSkipVerify. There is no configuration that sets it,
			// which is the strongest form of "never": a caller who needs a
			// private CA supplies RootCAs on their own client's transport.
			MinVersion: MinTLSVersion,
		},
	}
}

// RoundTripper attaches credentials to every request and bounds every response.
//
// It is one connection's, not one factory's, because the Diagnostics it writes
// belong to one session's failure.
type RoundTripper struct {
	// Base is the vetted transport underneath. Required.
	Base http.RoundTripper
	// Headers are static application-supplied headers, attached to every
	// request. A value may be a credential and is treated as one.
	Headers []auth.Header
	// Provider supplies credential headers per request. Nil means none.
	Provider auth.HeaderProvider
	// Wire bounds what may be buffered off the network.
	Wire WireLimits
	// Request bounds a whole non-streaming exchange; see a transport's
	// Timeouts.Request.
	Request time.Duration
	// Frame bounds one frame's arrival on a stream; see Timeouts.Frame.
	Frame time.Duration
	// Diags records a failure's cause on its way past. Required.
	Diags *Diagnostics
	// Origin, when non-empty, is the only origin this RoundTripper will send a
	// request to. Anything else is refused before a credential is attached.
	//
	// It is not a duplicate of the redirect guard, and the difference is the
	// reason it exists. CheckRedirect sees an origin change the *stdlib* makes,
	// following a 3xx. It does not see one a *server* makes by handing the
	// transport above it a new URL to send to — which is exactly what the legacy
	// SSE transport's "endpoint" event is: the SDK resolves that event's data as
	// a URL reference against the endpoint, and an absolute one lands wherever
	// the server says, with this module's credentials on it.
	//
	// This is the guard that covers both, because every request passes through
	// here however its URL was chosen.
	Origin string
}

// RoundTrip implements http.RoundTripper.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// A new request must not inherit a *previous* request's response status. If a
	// prior request's status had ended the session there would be no request now,
	// so a status that outlived it did not end the session and cannot be the
	// reason a later request — a genuine transport loss, say — fails. Only the
	// status is cleared here; the stream-scoped and session-scoped causes are
	// left, see resetPerRequest.
	rt.Diags.resetPerRequest()

	// Cloned before anything is written: RoundTrip must not modify the request
	// it is given, and the SDK builds requests it may still be holding.
	req = req.Clone(req.Context())

	// Before the credential, always. The point of this check is that a request
	// to the wrong origin never carries one, so it has to run first — a refusal
	// after attachCredentials would be a refusal of a request that had already
	// been built with the secret in it.
	if err := rt.checkOrigin(req); err != nil {
		return nil, err
	}
	if err := rt.attachCredentials(req); err != nil {
		return nil, err
	}

	// A request that cannot stream gets a deadline of its own. Every other
	// request's response may be an SSE stream, and a deadline on one of those is
	// a deadline on the session; those are bounded by ResponseHeaderTimeout and
	// by the caller's context instead. See Timeouts.
	var cancel context.CancelFunc
	if !mayStream(req) {
		var ctx context.Context
		ctx, cancel = context.WithTimeout(req.Context(), rt.Request)
		req = req.WithContext(ctx)
	}

	resp, err := rt.Base.RoundTrip(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		rt.Diags.RecordStatus(resp.StatusCode)
	}
	resp.Body = rt.boundBody(resp, cancel)
	return resp, nil
}

// ErrForeignOrigin reports a request aimed somewhere other than the configured
// origin. Transports classify it into their own taxonomy; this package has none.
var ErrForeignOrigin = errors.New("httpsec: refusing a request to a foreign origin")

// checkOrigin refuses a request that would leave the configured origin.
//
// A credential is minted for an origin and belongs to that origin. Whatever
// decided this request's URL — this module's config, a redirect the stdlib
// followed, or a URL a server sent in an event — the question of whether to
// send it is the same one, so it is answered in the one place that sees every
// request rather than at each of the doors a URL can come in through.
func (rt *RoundTripper) checkOrigin(req *http.Request) error {
	if rt.Origin == "" {
		return nil
	}
	origin, err := auth.CanonicalOrigin(req.URL.String())
	if err != nil {
		// The URL is not repeated: it may be server-controlled, and a query
		// string is a place tokens live. The auth error is already bounded,
		// normalized and secret-free.
		return fmt.Errorf("%w: the target is not a usable URL: %s", ErrForeignOrigin, err.Error())
	}
	if origin != rt.Origin {
		// Origins are not secret and naming both is the whole diagnostic.
		return fmt.Errorf("%w: %s is not %s: an MCP endpoint's origin is configuration, "+
			"and a credential minted for one origin is never sent to another", ErrForeignOrigin, origin, rt.Origin)
	}
	return nil
}

// attachCredentials writes the static headers and then the provider's onto req.
//
// Order matters and is documented on Config.Headers: the provider is applied
// second and with Set, so a live credential wins over a configured one naming
// the same field.
//
// Every header is validated before it is written, including the provider's. The
// static ones were validated by New, but a provider is application code that
// mints a value per request, and a value with a newline in it appends headers of
// the application's choosing to this request. Validating here is what makes that
// impossible rather than unlikely.
func (rt *RoundTripper) attachCredentials(req *http.Request) error {
	for _, h := range rt.Headers {
		req.Header.Set(h.Name(), h.Value())
	}
	if rt.Provider == nil {
		return nil
	}

	// The request's context, so a credential that needs I/O to mint is bounded
	// by the caller who wanted the request.
	headers, err := rt.Provider.Headers(req.Context())
	if err != nil {
		return rt.rejectCredentials(err)
	}
	for i, h := range headers {
		if err := h.Validate(); err != nil {
			return rt.rejectCredentials(fmt.Errorf("auth header provider returned an invalid header at index %d: %w", i, err))
		}
		req.Header.Set(h.Name(), h.Value())
	}
	return nil
}

// rejectCredentials records an auth failure and returns it.
//
// It is recorded as well as returned because returning is not enough: the SDK
// flattens some of its error paths with %v, so the chain this error travels in
// may not survive to reach classify. The recording is what does.
func (rt *RoundTripper) rejectCredentials(err error) error {
	rt.Diags.RecordAuthError(err)
	return err
}

// boundBody wraps resp.Body so that nothing unbounded is ever buffered, and so
// that a non-streaming request's deadline is released when its body is done.
//
// The bound depends on what the body is, and the difference is not a detail: a
// non-streaming body is one message and is capped whole, while an SSE stream is
// a session's worth of messages and is capped per frame. Capping a stream whole
// would work perfectly and then kill a healthy session in an hour, which is the
// kind of bug that reaches production.
func (rt *RoundTripper) boundBody(resp *http.Response, cancel context.CancelFunc) io.ReadCloser {
	body := resp.Body
	if isEventStream(resp.Header.Get("Content-Type")) {
		// The frame clock closes the body to unblock a parked Read, so it is
		// handed the body itself as its interrupt.
		fr := NewFrameReader(body, rt.Wire.MaxFrame, rt.Diags, rt.Frame, func() { _ = body.Close() })
		return &boundedBody{Reader: fr, closer: body, cancel: cancel, stop: fr.close}
	}
	// A non-streaming body needs no clock of its own: it is the answer to a
	// request, so it dies with that request's context — which every caller
	// bounds, and which the DELETE gets from Timeouts.Request. The stream is the
	// case with no such owner, which is why it is the case with the clock.
	r := &recordingReader{
		r:     limits.BoundedReader(body, rt.Wire.MaxBody),
		diags: rt.Diags,
	}
	return &boundedBody{Reader: r, closer: body, cancel: cancel}
}

// mayStream reports whether req's response could be an SSE stream.
//
// It is a question about this transport's own traffic, not a general one: the
// SDK POSTs messages (whose response is JSON or a stream, decided by the
// server), GETs the standalone stream, and DELETEs the session. Only the DELETE
// is certain not to stream, so only the DELETE gets a whole-request deadline.
// The test is by exclusion — anything unrecognized is treated as possibly
// streaming — because the cost of being wrong runs one way: a missing deadline
// on a request that had another bound anyway, versus a severed session.
func mayStream(req *http.Request) bool {
	return req.Method != http.MethodDelete
}

// isEventStream reports whether a Content-Type names SSE, ignoring parameters
// ("text/event-stream; charset=utf-8") and case, both of which are the server's
// choice and neither of which changes what the body is.
func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "text/event-stream"
}

// boundedBody is the response body handed back to the SDK: bounded reads, the
// original's Close, and the release of a deadline this package took out.
type boundedBody struct {
	io.Reader
	closer io.Closer
	// cancel releases the whole-request context, or is nil for a request that
	// did not get one.
	cancel context.CancelFunc
	// stop releases the frame clock, or is nil for a body that has none. A
	// timer left armed on a finished stream is a goroutine holding a body close
	// aimed at whatever connection the pool hands out next.
	stop func()
}

// Close closes the underlying body and releases the request's deadline.
//
// Close, not EOF: a caller that stops reading early — which http.Client itself
// does on some paths — must still release both, and only Close is guaranteed to
// happen. It is safe to call twice; both halves are idempotent.
func (b *boundedBody) Close() error {
	if b.stop != nil {
		b.stop()
	}
	err := b.closer.Close()
	if b.cancel != nil {
		b.cancel()
	}
	return err
}

// recordingReader reports an over-limit read to Diagnostics as it happens.
//
// It exists because the error does not survive the trip: the SDK reads a JSON
// body with io.ReadAll and renders a failure with %v, which flattens the chain,
// so by the time classify sees it the *limits.OverLimitError is a string. This
// catches it at the only point where it is still a value.
type recordingReader struct {
	r     io.Reader
	diags *Diagnostics
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	var over *limits.OverLimitError
	if errors.As(err, &over) {
		rr.diags.RecordLimitError(over)
	}
	return n, err
}

// Diagnostics is what one connection recorded about its own failure, on the way
// past, at the point where the cause was still a value.
//
// It is a record and not a channel: nothing waits on it, and nothing acts on it
// except classify, after something has already failed.
//
// Its fields fall into two lifetimes, because a failure describes either one
// request or the whole session. The status is request-scoped: it is written
// synchronously for a single request's response line and cleared at the head of
// the next request (see resetPerRequest), so a later transport loss is never
// labelled with an earlier request's 401. The limit, stall and auth causes are
// session-scoped and first-writer-wins: a stall or over-limit body ends the
// session it streams on, and a refused credential recurs on every request until
// it changes — so the first of each is the one that explains why the session is
// over, and it is kept. (The limit and stall are also written asynchronously,
// from the SDK's body-read goroutines, which is the second reason not to clear
// them per request: doing so would erase a live stream's genuine cause the
// moment a concurrent request began.)
//
// It is safe for concurrent use: the SDK reads its streams on goroutines of its
// own, so a status and a body can be recorded from two places at once.
type Diagnostics struct {
	mu sync.Mutex
	// lastStatus is the last HTTP error status seen, or 0.
	//
	// Only statuses >= 400 are recorded, so a later success cannot erase the
	// failure that is being explained. It is a diagnostic and is treated as one:
	// classify consults it only after the session has already failed, and only
	// when nothing more specific was recorded.
	lastStatus int
	authErr    error
	limitErr   error
	stallErr   error
}

// resetPerRequest clears the status a single request's response recorded, so a
// fresh request starts explaining nothing but itself.
//
// Only the status is cleared, and the reason is where each cause is recorded.
// The status is written synchronously in RoundTrip, for this request's own
// response line, before the request returns — so clearing it at the head of the
// next request is coherent: no other request is writing it. The limit and stall
// are written asynchronously, from the goroutines on which the SDK reads
// response bodies, and those reads outlive the RoundTrip that started them and
// overlap the next request's; clearing them here would erase a *live* stream's
// genuine mid-frame stall the moment a second request began, which is a real
// regression, not a hypothetical. They are session-scoped instead: a stall or an
// over-limit body ends the session it is read on, and first-writer-wins keeps
// that first fatal cause.
//
// The auth failure is likewise session-scoped, but for a different reason: a
// refused credential is not a property of one request, and every request until
// the credential changes fails the same way. Clearing it would make a chain of
// requests each re-discover the same lockout as a fresh mystery.
func (d *Diagnostics) resetPerRequest() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastStatus = 0
}

// RecordStallError keeps the first stalled frame. Like the others it is a
// record of a cause, written where the cause was still a value.
func (d *Diagnostics) RecordStallError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stallErr == nil {
		d.stallErr = err
	}
}

func (d *Diagnostics) StallError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stallErr
}

func (d *Diagnostics) RecordStatus(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastStatus = status
}

func (d *Diagnostics) Status() (int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastStatus, d.lastStatus != 0
}

// RecordAuthError keeps the first auth failure, not the last: once credentials
// are refused, every request after it fails the same way, and the first one is
// the one that explains the session.
func (d *Diagnostics) RecordAuthError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authErr == nil {
		d.authErr = err
	}
}

// AuthError returns the recorded auth failure as an *auth.Error, or nil.
//
// A provider is application code and may return anything. A failure that is not
// an *auth.Error is still an auth failure — the provider was asked for a
// credential and did not produce one — so it is reported as one, with the class
// that says exactly that much and no more.
func (d *Diagnostics) AuthError() *auth.Error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.authErr == nil {
		return nil
	}
	var aerr *auth.Error
	if errors.As(d.authErr, &aerr) {
		return aerr
	}
	// Wrapped, not rendered: the provider's text is not something this package
	// can vouch for, and auth.Error never renders its cause.
	return auth.NewError(auth.ClassFailed, "headers",
		"the auth header provider did not return credentials", d.authErr)
}

func (d *Diagnostics) RecordLimitError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.limitErr == nil {
		d.limitErr = err
	}
}

func (d *Diagnostics) LimitError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.limitErr
}
