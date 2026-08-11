// Package transport is a generic, connection-bound HTTP client for the inference seam.
// It composes an transport.Endpoint (connection identity), an route.Router (request
// routing), an codec.Codec (request encoding + response decoding), an OPTIONAL
// codec.StreamDecoder (streaming), and an auth.Authenticator (credentials)
// into an inference.Client. It owns HTTP mechanics only: it does not hardcode a method,
// a chat path, a Content-Type, a streaming Accept header, or a stream framing such as
// SSE — the Router supplies method+URL+route headers, the encoder supplies the body and
// its headers, and the StreamDecoder owns wire framing.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/credentials/httpauth"
	"github.com/looprig/inference"
	auth "github.com/looprig/inference/auth"
	codec "github.com/looprig/inference/codec"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/model"

	route "github.com/looprig/inference/route"
	stream "github.com/looprig/inference/stream"
)

// Client is a connection-bound inference.Client: one Codec x one Endpoint x one Router
// x one Authenticator, with an optional StreamDecoder. It performs the same ordered
// pre-I/O guards for both Invoke and Stream — binding check, then Model.Validate — then
// routes, encodes, authorizes, and executes, mapping transport failures to
// *failure.NetworkError and non-2xx responses to *failure.APIError.
type Client struct {
	ep     Endpoint
	router route.Router
	enc    codec.RequestEncoder
	dec    codec.ResponseDecoder
	stream codec.StreamDecoder // nil ⇒ streaming unsupported
	// roundTripper is caller-owned. When set, it is installed as-is on both
	// HTTP clients; the transport never clones, closes, or synchronizes it.
	roundTripper  http.RoundTripper
	tlsRootCAs    *x509.CertPool
	invokeTimeout time.Duration
	// auth is the legacy constructor default. Call-scoped callers use
	// InvokeWithAuth/StreamWithAuth and supply a fresh authorizer per concrete
	// wire attempt.
	auth httpauth.Authorizer
	// hcInvoke and hcStream are deliberately separate http.Clients: Invoke's
	// response is atomic (read whole via a bounded reader, nothing partial to protect),
	// so it is safe to bound with a real whole-request Timeout. Stream must never
	// carry a whole-request Timeout — that would abort a long-lived body
	// mid-flight — so it keeps only the connect/TLS/response-header budget below.
	hcInvoke *http.Client
	hcStream *http.Client
}

// Compile-time proof that Client honors the inference.Client contract.
var _ inference.Client = (*Client)(nil)

// RequestBuildError is a failure to CONSTRUCT the outbound request — a router that
// cannot build a route, or a malformed method/URL. It is a request-configuration error,
// kept strictly distinct from *failure.NetworkError (reserved for hc.Do transport
// failures) so errors.As never misclassifies a config bug as a transport fault. Unwrap
// exposes the underlying cause (e.g. a route.MissingModelError or a net/http error).
type RequestBuildError struct {
	Err error
}

func (e *RequestBuildError) Error() string { return "transport: build request: " + e.Err.Error() }
func (e *RequestBuildError) Unwrap() error { return e.Err }

// UnsupportedStreamingError is returned by Stream, before any I/O, when the Client has
// no StreamDecoder (the codec is a plain Codec, not a StreamingCodec, and no decoder was
// injected). Fail-closed and typed so callers can errors.As it. APIFormat records the
// bound endpoint's format for diagnosis.
type UnsupportedStreamingError struct {
	APIFormat model.APIFormat
}

func (e *UnsupportedStreamingError) Error() string {
	return fmt.Sprintf("transport: streaming unsupported for API format %q: no StreamDecoder (codec is not a StreamingCodec and none was injected)", e.APIFormat)
}

// Timeout budget for the risky connection-setup phases of a request, shared by
// both Invoke and Stream. The per-request deadline for the body is the caller's
// context (every I/O call takes ctx) on top of whichever budget below applies.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 90 * time.Second

	// streamResponseHeaderTimeout bounds only time-to-first-byte for Stream: a
	// stream should start promptly even under load, and once headers arrive the
	// body is free to run long — there is no whole-request Timeout to fight with.
	streamResponseHeaderTimeout = 60 * time.Second

	// defaultInvokeTimeout is Invoke's whole-request ceiling. Unlike Stream,
	// Invoke's response is atomic (read in full before it is usable), so bounding
	// the entire call — not just the header wait — is safe and correct: there is
	// no partial progress a longer wait would risk losing. 60s (Stream's header-only
	// budget) is too short for a loaded local model or a slow cloud minute; this is
	// deliberately more generous, and overridable per Client via WithInvokeTimeout.
	defaultInvokeTimeout = 5 * time.Minute
)

// Option customizes a Client at construction.
type Option func(*Client)

// WithStreamDecoder sets or overrides the Client's StreamDecoder. Use it to enable
// streaming for a plain Codec, or to route streaming through a caller-supplied decoder
// (e.g. an NDJSON- or custom-framer-backed decoder) different from the codec's own.
func WithStreamDecoder(sd codec.StreamDecoder) Option {
	return func(c *Client) { c.stream = sd }
}

// WithInvokeTimeout overrides Invoke's whole-request timeout (default
// defaultInvokeTimeout). It has no effect on Stream, which never carries a
// whole-request timeout. A non-positive d is a no-op — the default (or a prior
// WithInvokeTimeout) is left in place — since a zero or negative timeout would
// mean "never time out" or "always time out", neither of which this option is
// meant to express.
func WithInvokeTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d <= 0 {
			return
		}
		c.invokeTimeout = d
		c.hcInvoke = newInvokeHTTPClient(d, c.tlsRootCAs)
		c.applyRoundTripper()
	}
}

// WithRoundTripper installs the exact caller-supplied RoundTripper on both the
// invoke and stream HTTP clients. The caller retains ownership: this option
// neither clones nor closes the RoundTripper, and the value must be safe for
// concurrent use by both clients. It must also enforce the caller's desired
// TLS verification policy itself; supplying this option replaces the
// library-created transport (and therefore its dialing, pooling, and TLS
// defaults) rather than wrapping it.
//
// A nil RoundTripper panics during option construction so a caller cannot
// silently fall back to the process-wide default transport.
func WithRoundTripper(rt http.RoundTripper) Option {
	if rt == nil {
		panic("transport.WithRoundTripper: round tripper must not be nil")
	}
	return func(c *Client) {
		c.roundTripper = rt
		c.applyRoundTripper()
	}
}

// WithTLSRootCAs installs a cloned, non-empty trust pool on the library-owned
// invoke and stream transports. It does not replace dialing, proxy, TLS
// minimum-version, pooling, timeout, or redirect policy. The caller must
// populate the pool with the roots it intends to trust before construction. If
// WithRoundTripper is also supplied, that caller-owned transport remains in
// use and is responsible for its own TLS verification policy.
func WithTLSRootCAs(roots *x509.CertPool) Option {
	if roots == nil || roots.Equal(x509.NewCertPool()) {
		panic("transport.WithTLSRootCAs: root pool must be non-empty")
	}
	cloned := roots.Clone()
	return func(c *Client) {
		c.tlsRootCAs = cloned.Clone()
		c.hcInvoke = newInvokeHTTPClient(c.invokeTimeout, c.tlsRootCAs)
		c.hcStream = newStreamHTTPClient(c.tlsRootCAs)
		c.applyRoundTripper()
	}
}

// New constructs a Client bound to ep, routing with router, encoding/decoding with
// codec, authenticating with auth. It is the source-compatible wrapper around
// NewWithAuth. If codec also satisfies codec.StreamDecoder
// (i.e. is a StreamingCodec), it becomes the stream decoder automatically; otherwise
// streaming fails before I/O with *UnsupportedStreamingError unless WithStreamDecoder
// supplies one. router, codec, and auth are required: a nil is a programmer error (the
// explicit "no credentials" value is auth.None()), so New panics rather than sending
// unrouted, unencoded, or silently unauthenticated requests.
func New(ep Endpoint, router route.Router, cdc codec.Codec, authenticator auth.Authenticator, opts ...Option) *Client {
	if authenticator == nil {
		panic("transport.New: auth must not be nil; pass auth.None() for no credentials")
	}
	c := newClient(ep, router, cdc)
	c.auth = authenticator
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewWithAuth constructs a connection-bound client for call-scoped
// authorization. The optional arguments may contain a default
// httpauth.Authorizer (for compatibility with callers that still invoke
// Invoke/Stream directly) and/or Option values. With no default authorizer,
// use InvokeWithAuth or StreamWithAuth for every request. The variadic shape
// keeps this additive constructor compatible with both forms during the
// migration; invalid arguments panic like the legacy constructor's nil checks.
func NewWithAuth(ep Endpoint, router route.Router, cdc codec.Codec, args ...any) *Client {
	c := newClient(ep, router, cdc)
	for _, arg := range args {
		switch value := arg.(type) {
		case Option:
			value(c)
		case httpauth.Authorizer:
			if value == nil {
				panic("transport.NewWithAuth: auth must not be nil; pass httpauth.None() for no credentials")
			}
			c.auth = value
		case nil:
			panic("transport.NewWithAuth: nil option or auth")
		default:
			panic("transport.NewWithAuth: unsupported argument")
		}
	}
	return c
}

// NewWithAuthorizer is the typed constructor spelling for new code that wants
// a legacy default in addition to call-scoped methods.
func NewWithAuthorizer(ep Endpoint, router route.Router, cdc codec.Codec, authorizer httpauth.Authorizer, opts ...Option) *Client {
	if authorizer == nil {
		panic("transport.NewWithAuthorizer: auth must not be nil; pass httpauth.None() for no credentials")
	}
	c := newClient(ep, router, cdc)
	c.auth = authorizer
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func newClient(ep Endpoint, router route.Router, cdc codec.Codec) *Client {
	if router == nil {
		panic("transport.New: router must not be nil")
	}
	if cdc == nil {
		panic("transport.New: codec must not be nil")
	}
	c := &Client{
		ep:            ep,
		router:        router,
		enc:           cdc,
		dec:           cdc,
		invokeTimeout: defaultInvokeTimeout,
		hcInvoke:      newInvokeHTTPClient(defaultInvokeTimeout, nil),
		hcStream:      newStreamHTTPClient(nil),
	}
	// Optional streaming: a StreamingCodec is its own StreamDecoder.
	if sd, ok := cdc.(codec.StreamDecoder); ok {
		c.stream = sd
	}
	return c
}

// applyRoundTripper reapplies a caller-owned transport after an option rebuilds
// one or both library-owned HTTP clients. A nil value leaves the secure,
// library-created transport in place.
func (c *Client) applyRoundTripper() {
	if c.roundTripper == nil {
		return
	}
	c.hcInvoke.Transport = c.roundTripper
	c.hcStream.Transport = c.roundTripper
}

// baseTransport builds the http.Transport settings shared by both the Invoke and
// Stream clients: connect/TLS timeout budget, a TLS 1.2 floor, and connection
// pooling. responseHeaderTimeout is the one setting callers vary between the two
// use cases.
func baseTransport(responseHeaderTimeout time.Duration, roots *x509.CertPool) *http.Transport {
	var rootCAs *x509.CertPool
	if roots != nil {
		rootCAs = roots.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Default TLS with an explicit floor of 1.2 and no InsecureSkipVerify —
		// server certificates are verified.
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// noRedirect is shared by both clients: a 3xx would replay the single-shot
// EncodedRequest.Body, and the transport must never retry or replay a body — a
// redirect is surfaced as its 3xx response (mapped to *failure.APIError) instead.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// parseRetryAfter reads Retry-After's integer-seconds form. The HTTP-date
// form is deliberately unsupported (needs a clock; no provider we bind uses it).
func parseRetryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// newStreamHTTPClient builds the http.Client used by Stream: no whole-request
// Timeout (it would abort a long-lived streaming body mid-flight), just the
// connect/TLS/header timeout budget on the Transport. The body itself is bounded
// only by the caller's context.
func newStreamHTTPClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		CheckRedirect: noRedirect,
		Transport:     baseTransport(streamResponseHeaderTimeout, roots),
	}
}

// newInvokeHTTPClient builds the http.Client used by Invoke, bounded by a real
// whole-request Timeout of d. This is safe (unlike for Stream) because Invoke's
// response is atomic: it is read in full via io.ReadAll before it is usable, so
// there is no partial progress a longer wait could risk losing — a flat deadline
// on the entire call is the correct tool, not just a header-arrival guard.
// ResponseHeaderTimeout is left at 0 (unbounded) on this Transport: the outer
// Client.Timeout already covers header wait plus bounded body read, so a second,
// shorter-or-equal timer here would be redundant.
func newInvokeHTTPClient(d time.Duration, roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:       d,
		CheckRedirect: noRedirect,
		Transport:     baseTransport(0, roots),
	}
}

// Invoke sends a non-streaming request using the legacy constructor's default
// authorizer. New credential-backed callers should use InvokeWithAuth so a
// lease is applied to this concrete request attempt.
func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	resp, err := c.InvokeWithAuth(ctx, req, c.auth)
	return resp, legacyAuthorizationError(err)
}

// InvokeWithAuth sends one non-streaming request with a call-scoped
// authorizer. The request is built from scratch before authorization, so no
// header or body from a previous attempt can accumulate on this attempt.
func (c *Client) InvokeWithAuth(ctx context.Context, req inference.Request, authorizer httpauth.Authorizer) (*inference.Response, error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	if err := requireAuthorizer(authorizer); err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req, codec.RequestModeInvoke)
	if err != nil {
		return nil, err
	}
	if err := authorizer.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	httpResp, err := c.hcInvoke.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	limit := MaxResponseBodyBytes
	if httpResp.StatusCode/100 != 2 {
		limit = MaxErrorResponseBodyBytes
	}
	body, tooLarge, err := readBoundedBody(httpResp.Body, limit)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if tooLarge && httpResp.StatusCode/100 == 2 {
		return nil, &failure.ResponseBodyTooLargeError{Limit: MaxResponseBodyBytes}
	}
	// Non-2xx is mapped to an APIError BEFORE the decoder is invoked; a bounded
	// prefix is parsed transiently and the body is closed (deferred).
	if httpResp.StatusCode/100 != 2 {
		return nil, failure.APIErrorFromResponse(httpResp.StatusCode, body, httpResp.Header, parseRetryAfter(httpResp.Header))
	}
	return c.dec.DecodeResponse(body)
}

// Stream sends a streaming request using the legacy constructor's default
// authorizer. New credential-backed callers should use StreamWithAuth.
func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	reader, err := c.StreamWithAuth(ctx, req, c.auth)
	return reader, legacyAuthorizationError(err)
}

// legacyAuthorizationError preserves the old inference transport contract at
// the legacy Client methods. Before call-scoped authorization, cancellation
// during authorization was reported as a transport failure. New call-scoped
// methods deliberately return httpauth's typed cancellation directly.
func legacyAuthorizationError(err error) error {
	if err == nil {
		return nil
	}
	var networkErr *failure.NetworkError
	if errors.As(err, &networkErr) {
		return err
	}
	if errors.Is(err, httpauth.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &failure.NetworkError{Err: err}
	}
	return err
}

// StreamWithAuth sends one streaming request with a call-scoped authorizer.
// Error responses are bounded and sanitized before returning.
func (c *Client) StreamWithAuth(ctx context.Context, req inference.Request, authorizer httpauth.Authorizer) (*stream.StreamReader[content.Chunk], error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := req.Model.Validate(); err != nil {
		return nil, err
	}
	if c.stream == nil {
		return nil, &UnsupportedStreamingError{APIFormat: c.ep.APIFormat}
	}
	if err := requireAuthorizer(authorizer); err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req, codec.RequestModeStream)
	if err != nil {
		return nil, err
	}
	if err := authorizer.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}
	httpResp, err := c.hcStream.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		body, _, readErr := readBoundedBody(httpResp.Body, MaxErrorResponseBodyBytes)
		if readErr != nil {
			return nil, &failure.NetworkError{Err: fmt.Errorf("transport: reading error body (status %d): %w", httpResp.StatusCode, readErr)}
		}
		return nil, failure.APIErrorFromResponse(httpResp.StatusCode, body, httpResp.Header, parseRetryAfter(httpResp.Header))
	}
	reader, err := c.stream.DecodeStream(httpResp)
	if err != nil {
		// Backstop the StreamDecoder body-ownership contract: a compliant decoder already
		// closed resp.Body before returning an error, but a contract-violating third-party
		// decoder might not. A double Close on an http response body is harmless, so close
		// here to guarantee the connection is never leaked on the highest-risk seam.
		_ = httpResp.Body.Close()
		return nil, err
	}
	return reader, nil
}

// InvokeWithAuthorizer is a descriptive alias for InvokeWithAuth.
func (c *Client) InvokeWithAuthorizer(ctx context.Context, req inference.Request, authorizer httpauth.Authorizer) (*inference.Response, error) {
	return c.InvokeWithAuth(ctx, req, authorizer)
}

// StreamWithAuthorizer is a descriptive alias for StreamWithAuth.
func (c *Client) StreamWithAuthorizer(ctx context.Context, req inference.Request, authorizer httpauth.Authorizer) (*stream.StreamReader[content.Chunk], error) {
	return c.StreamWithAuth(ctx, req, authorizer)
}

const (
	// MaxResponseBodyBytes bounds an atomic successful Invoke response before
	// handing it to a codec. Streaming bodies remain owned by their decoder.
	MaxResponseBodyBytes = 16 << 20
	// MaxErrorResponseBodyBytes bounds transient non-2xx parsing. The body is
	// never retained by failure.APIError.
	MaxErrorResponseBodyBytes = 64 << 10
)

func requireAuthorizer(authorizer httpauth.Authorizer) error {
	if authorizer == nil {
		return &auth.MissingCredentialsError{Credential: "authorizer"}
	}
	return nil
}

func readBoundedBody(body io.Reader, limit int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// checkBinding fails closed when the request's Model names a provider, endpoint, or API
// format that conflicts with the bound Endpoint, before any I/O. Empty request fields
// are wildcards ("use the bound value"), not claims, so only a non-empty field that
// disagrees is a cross-wiring error.
func (c *Client) checkBinding(m model.Model) error {
	providerConflict := m.Provider != "" && m.Provider != c.ep.Provider
	endpointConflict := m.BaseURL != "" && m.BaseURL != c.ep.BaseURL
	formatConflict := m.APIFormat != "" && m.APIFormat != c.ep.APIFormat
	if providerConflict || endpointConflict || formatConflict {
		return &failure.ModelMismatchError{
			BoundProvider:    c.ep.Provider,
			RequestProvider:  m.Provider,
			BoundEndpoint:    c.ep.BaseURL,
			RequestEndpoint:  m.BaseURL,
			BoundAPIFormat:   c.ep.APIFormat,
			RequestAPIFormat: m.APIFormat,
		}
	}
	return nil
}

// buildRequest routes and encodes req into a ctx-bound *http.Request, applying headers
// in order route → encoder (later overrides earlier). Auth headers are applied by the
// caller (Invoke/Stream) after this, so the full precedence is route → encoder → auth.
func (c *Client) buildRequest(ctx context.Context, req inference.Request, mode codec.RequestMode) (*http.Request, error) {
	route, err := c.router.BuildRoute(c.ep.BaseURL, req, mode)
	if err != nil {
		return nil, &RequestBuildError{Err: err}
	}
	enc, err := c.enc.EncodeRequest(req, mode)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, route.Method, route.URL, enc.Body)
	if err != nil {
		return nil, &RequestBuildError{Err: err}
	}
	// EncodedRequest.Body is single-shot: clear the GetBody that NewRequestWithContext
	// auto-populates for a bytes/strings body so net/http can never rewind and replay it
	// on a connection-reuse retry (which it would for an idempotent method a custom Router
	// might return). The transport must never replay a body; retry policy is the caller's.
	httpReq.GetBody = nil
	applyHeaders(httpReq.Header, route.Header) // route headers first
	applyHeaders(httpReq.Header, enc.Header)   // encoder headers override route
	return httpReq, nil
}

// applyHeaders copies every key from src into dst, replacing any existing values for
// that key so a later layer overrides an earlier one while preserving multi-valued
// headers. A nil src is a no-op.
func applyHeaders(dst, src http.Header) {
	for k, vals := range src {
		dst.Del(k)
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}
