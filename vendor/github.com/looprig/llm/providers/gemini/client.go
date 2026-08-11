// Package gemini is a bespoke client for Google's Gemini generateContent API. It
// satisfies inference.Client for both the non-streaming (generateContent) and
// streaming (streamGenerateContent, SSE) paths. Gemini is not plain OpenAI-over-HTTP:
// the model id lives in the URL path with a ":generateContent" method suffix, and the
// credential is an "x-goog-api-key" HEADER (not a Bearer token) — so the generic
// transport client (which assumes a static /chat/completions path and Bearer auth)
// cannot serve it. The JSON body + per-event decoding are delegated to the shared
// geminiapi Codec; only wire routing (URL + header + SSE endpoint) lives here.
package gemini

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	failure "github.com/looprig/inference/failure"

	codec "github.com/looprig/inference/codec"
	geminicodec "github.com/looprig/inference/codec/geminiapi"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/llm"
)

// Compile-time proof that Client honors the inference.Client contract.
var _ inference.Client = (*Client)(nil)

const (
	// defaultBaseURL is the Gemini generateContent v1beta root. The per-model path
	// (/models/<name>:generateContent) is appended by buildRequest.
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// apiKeyHeader is the header Gemini authenticates with — a raw key, NOT a Bearer
	// token, which is why this client uses auth.Header rather than auth.Key.
	// #nosec G101 -- HTTP header NAME, not a credential; the value is the caller's key at runtime.
	apiKeyHeader = "x-goog-api-key"

	// URL fragments. The model id is a single PathEscaped segment; the ":method"
	// suffix is appended literally after it (url.PathEscape leaves ":" alone, and it
	// is a valid non-first path-segment character, so it stays literal on the wire).
	pathModelsPrefix            = "/models/"
	methodGenerateContent       = "generateContent"
	methodStreamGenerateContent = "streamGenerateContent"
	methodCountTokens           = "countTokens"
	// sseQuery selects server-sent-events framing for the streaming endpoint.
	sseQuery = "alt=sse"

	contentTypeJSON = "application/json"
	acceptSSE       = "text/event-stream"

	// maxAPIErrorBodyBytes bounds provider-controlled diagnostic payloads while
	// retaining a useful prefix in failure.APIError.
	maxAPIErrorBodyBytes = 1 << 20
)

// Timeout budget for the connect/TLS/header phases, mirroring the generic
// transport client's hygiene. There is deliberately no whole-request
// http.Client.Timeout: the per-request deadline is the caller's context, and
// omitting it keeps the client forward-compatible with a long-lived SSE body.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 60 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 90 * time.Second
)

// Client is a Gemini generateContent inference client. It owns one Authenticator
// (the x-goog-api-key header) and one http.Client, and is safe for concurrent use
// (both are immutable after construction). Binding is by provider: a request whose
// Model.Provider is not ProviderGoogle is rejected pre-I/O with
// *failure.ModelMismatchError. The endpoint base is fixed at construction.
type Client struct {
	endpoint string // scheme://host[/v1beta] base; New binds the Gemini default
	auth     auth.Authenticator
	codec    geminicodec.Codec
	hc       *http.Client
}

// New constructs a Gemini client authenticated with key. It fails closed with
// *llm.AuthRequiredError when key is empty — no Client and no network object are
// created — matching the auto.New credential contract for an AuthAPIKey provider.
func New(key auth.APIKey) (inference.Client, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderGoogle, Kind: auth.AuthAPIKey}
	}
	return newClient(key, defaultBaseURL), nil
}

// newClient wires a Client for a validated key + endpoint base. endpoint is the
// scheme://host[/v1beta] base; New supplies the Gemini default, tests override it
// to reach an httptest.Server. It builds the x-goog-api-key authenticator once and
// the phase-bounded, TLS>=1.2 http.Client.
func newClient(key auth.APIKey, endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		auth:     auth.Header(key, apiKeyHeader),
		hc:       newHTTPClient(),
	}
}

// newHTTPClient constructs the immutable phase-bounded transport shared by the
// inference client and the separately constructed countTokens counter.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			ExpectContinueTimeout: expectContinueTimeout,
			IdleConnTimeout:       idleConnTimeout,
			ForceAttemptHTTP2:     true,
		},
	}
}

// Invoke sends a non-streaming generateContent request and returns the decoded
// response. Ordered, all pre-I/O guards first (preflight), then: build the
// ctx-bound POST to <base>/models/<name>:generateContent, set the x-goog-api-key
// header, do, map transport/non-2xx failures, and decode the Gemini response.
func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	body, err := c.preflight(req, codec.RequestModeInvoke)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req.Model.Name, methodGenerateContent, "", body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", contentTypeJSON)
	if err := c.auth.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}

	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode/100 != 2 {
		return nil, providerAPIError(httpResp)
	}
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	return c.codec.DecodeResponse(respBody)
}

// Stream sends a streamGenerateContent request and, on 2xx, returns a chunk reader
// that de-frames the SSE body and decodes each event via the shared codec. The body
// is identical to Invoke's (Gemini streaming is an endpoint + ?alt=sse concern, not
// a body field). Same ordered pre-I/O guards. A non-2xx status maps to
// *failure.APIError (body drained + closed first); on 2xx the codec's DecodeStream
// takes ownership of the response body and closes it via the returned reader's Close.
// Gemini SSE has no [DONE] sentinel — the sse framer returns io.EOF at body end,
// which the reader surfaces normally.
func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	body, err := c.preflight(req, codec.RequestModeStream)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.buildRequest(ctx, req.Model.Name, methodStreamGenerateContent, sseQuery, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", acceptSSE)
	if err := c.auth.Authorize(ctx, httpReq); err != nil {
		return nil, err
	}

	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		return nil, providerAPIError(httpResp)
	}
	// DecodeStream owns httpResp.Body on success (closed via the reader's Close). On
	// the (practically unreachable) framer error it closes the body itself; close here
	// too as a harmless backstop so the connection is never leaked.
	reader, err := c.codec.DecodeStream(httpResp)
	if err != nil {
		if closeErr := httpResp.Body.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return reader, nil
}

// preflight runs the ordered fail-closed guards shared by Invoke and Stream and, on
// success, encodes the request body: (1) provider binding, (2) ValidateModel (the
// provider truth table + structural checks), (3) the Gemini API-format guard, then
// (4) the Gemini body encode. No I/O happens here, so a guard failure never opens a
// connection. The mode is accepted for call-site symmetry; the Gemini body is
// mode-independent (streaming is an endpoint/query concern), so it is not consulted.
func (c *Client) preflight(req inference.Request, _ codec.RequestMode) ([]byte, error) {
	if err := c.checkBinding(req.Model); err != nil {
		return nil, err
	}
	if err := llm.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if req.Model.APIFormat != model.APIFormatGemini {
		return nil, &UnsupportedAPIFormatError{APIFormat: req.Model.APIFormat}
	}
	return geminicodec.EncodeRequest(req)
}

// checkBinding fails closed when the request's Model names a provider other than
// Google, before any I/O. The endpoint base is fixed at construction, so the
// enforceable binding is the provider.
func (c *Client) checkBinding(m model.Model) error {
	if llm.Provider(m.Provider) != llm.ProviderGoogle {
		return &failure.ModelMismatchError{
			BoundProvider:   model.ProviderName(llm.ProviderGoogle),
			RequestProvider: m.Provider,
			BoundEndpoint:   c.endpoint,
			RequestEndpoint: m.BaseURL,
		}
	}
	return nil
}

// buildRequest constructs the ctx-bound POST to
// <endpoint>/models/<escaped model id>:<method>[?<query>] with a JSON Content-Type.
// The model id is path-escaped so it is a single URL path segment; the ":method"
// suffix is appended literally (url.PathEscape does not escape ":", and ":" is a
// valid non-first path-segment character, so it stays literal on the wire). Caller
// sets Accept (application/json for invoke, text/event-stream for stream).
func (c *Client) buildRequest(ctx context.Context, modelName, method, query string, body []byte) (*http.Request, error) {
	return buildRequest(ctx, c.endpoint, modelName, method, query, body)
}

func buildRequest(ctx context.Context, endpoint, modelName, method, query string, body []byte) (*http.Request, error) {
	rawURL := endpoint + pathModelsPrefix + url.PathEscape(modelName) + ":" + method
	if query != "" {
		rawURL += "?" + query
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, &RequestBuildError{Err: err}
	}
	httpReq.Header.Set("Content-Type", contentTypeJSON)
	return httpReq, nil
}

// providerAPIError returns the shared bounded non-2xx representation. A body
// read failure remains a transport failure rather than a partial API error.
func providerAPIError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAPIErrorBodyBytes))
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	return failure.APIErrorFromResponse(response.StatusCode, body, response.Header, 0)
}
