// Package gateway provides a local HTTP compatibility layer that lets
// coding-harness clients speaking different model-API dialects (Anthropic
// Messages, OpenAI Responses, OpenAI Chat Completions, Gemini) reach any
// injected inference.Client/model.Model target.
//
// This file implements Handler.ServeHTTP: the ordered request-processing
// pipeline. For every request it performs, in order:
//
//  1. Validate method, path, content type, and header bounds.
//  2. Authenticate the local gateway token using constant-time comparison.
//  3. Select exactly one ingress codec.
//  4. Apply the bounded body reader before JSON decoding.
//  5. Decode the native request into codec.DecodedRequest.
//  6. Resolve (ingress format, requested model) to a Target.
//  7. Replace the neutral request's model with Target.Model. The inbound
//     alias is never sent upstream as the target model name.
//  8. Validate request features against Target.Model.Caps.
//  9. Apply global concurrency admission.
//  10. Invoke Target.Client.Invoke or Target.Client.Stream.
//  11. Encode the result with the same dialect used for ingress.
//  12. Close all upstream bodies/readers and release admission exactly once.
//
// The harness-facing response reports the requested alias where its dialect
// expects a model field. Internal structured diagnostics may record both the
// alias and Target.ID.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/codec"
	"github.com/looprig/inference/codec/anthropicapi"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/inference/model"
)

// jsonContentType is the only Content-Type this gateway accepts on any
// route: every currently bundled dialect (Anthropic Messages, OpenAI Chat
// Completions/Responses, Gemini) is JSON-bodied.
const jsonContentType = "application/json"

// Handler is an http.Handler implementing the gateway's request-processing
// pipeline (see package doc). It is built with New and is safe for
// concurrent use by multiple goroutines, as any http.Handler must be.
type Handler struct {
	resolver       Resolver
	codecs         map[model.APIFormat]codec.ServerCodec
	authenticate   Authenticator
	contextCounter contextcount.ContextCounter
	maxRequestBody int64
	sem            chan struct{}
}

var _ http.Handler = (*Handler)(nil)

// ServeHTTP implements the ordered pipeline documented on the package.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: method, content type, header bounds. Path validity is judged
	// by codec selection (step 3): there is no separate generic notion of a
	// "valid path" ahead of knowing which codec, if any, owns it.
	if r.Method != http.MethodPost {
		writeGenericError(w, &MethodNotAllowedError{Method: r.Method})
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeGenericError(w, &UnsupportedContentTypeError{ContentType: r.Header.Get("Content-Type")})
		return
	}

	// Step 2: authenticate. Header-length bounding for the Authorization
	// header specifically is enforced inside Authenticate itself.
	if err := h.authenticate.Authenticate(r); err != nil {
		writeGenericError(w, err)
		return
	}

	// Step 3: select exactly one ingress codec. The Anthropic-only
	// count_tokens auxiliary route lives outside the generic
	// codec.ServerCodec surface (see anthropicapi.MatchCountTokensRequest's
	// doc comment), so it is matched first, as its own explicit branch.
	if anthropicapi.MatchCountTokensRequest(r) {
		h.serveCountTokens(w, r)
		return
	}

	ingress, sc, err := h.selectCodec(r)
	if err != nil {
		writeGenericError(w, err)
		return
	}

	h.serveInference(w, r, ingress, sc)
}

// selectCodec implements step 3 for the generic (non-count_tokens) path: it
// calls MatchRequest on every configured codec and requires exactly one
// match. Zero matches is a *NoMatchingCodecError (404); more than one match
// is a *AmbiguousCodecMatchError (500) -- a caller misconfiguration, since
// well-behaved dialect codecs' routes never legitimately overlap.
func (h *Handler) selectCodec(r *http.Request) (model.APIFormat, codec.ServerCodec, error) {
	var matchedFormat model.APIFormat
	var matched codec.ServerCodec
	count := 0
	for format, c := range h.codecs {
		if c.MatchRequest(r) {
			count++
			matchedFormat, matched = format, c
		}
	}
	switch count {
	case 0:
		return "", nil, &NoMatchingCodecError{Method: r.Method, Path: r.URL.Path}
	case 1:
		return matchedFormat, matched, nil
	default:
		return "", nil, &AmbiguousCodecMatchError{Method: r.Method, Path: r.URL.Path, Count: count}
	}
}

// serveInference implements steps 4-12 for a normal (non-count_tokens)
// inference request against the already-selected ingress codec sc.
func (h *Handler) serveInference(w http.ResponseWriter, r *http.Request, ingress model.APIFormat, sc codec.ServerCodec) {
	ctx := r.Context()

	// Step 4: bounded body reader, applied before JSON decoding.
	if err := h.applyBoundedBody(r); err != nil {
		h.writeError(sc, w, err)
		return
	}

	// Step 5: decode. A codec's own decode error is passed straight to that
	// SAME codec's WriteError, unwrapped -- "request decode failure
	// passthrough": the codec that produced the error already classifies its
	// own error types correctly.
	decoded, err := sc.DecodeRequest(r)
	if err != nil {
		sc.WriteError(w, err)
		return
	}

	// Step 6: resolve.
	target, err := h.resolver.Resolve(ctx, ingress, decoded.RequestedModel)
	if err != nil {
		h.writeError(sc, w, err)
		return
	}

	// Step 7: replace the neutral request's model. The harness alias
	// (decoded.RequestedModel) is never sent upstream as the target model.
	decoded.Request.Model = target.Model
	if target.AuthoritativeEffort && decoded.Request.Override != nil {
		decoded.Request.Override.Effort = target.Model.Sampling.Effort
	}

	// Step 8: validate request features against Target.Model.Caps.
	if err := inference.ValidateRequestFeatures(decoded.Request); err != nil {
		h.writeError(sc, w, err)
		return
	}

	// Step 9: global concurrency admission (reject-on-full, never queued).
	if !h.acquire() {
		h.writeError(sc, w, &ConcurrencyLimitExceededError{})
		return
	}
	defer h.release()

	if decoded.Streaming {
		// Step 10/11 (streaming): see serveStreaming (stream.go) for the
		// full incremental pull loop.
		_ = h.serveStreaming(w, r, decoded, target, sc)
		return
	}

	// Step 10: invoke. Never retried.
	resp, err := target.Client.Invoke(ctx, decoded.Request)
	if err != nil {
		h.writeError(sc, w, h.upstreamError(ctx, err))
		return
	}

	// The harness-facing response reports the requested alias where its
	// dialect expects a model field, not the resolved upstream model name.
	if resp != nil {
		resp.Model = decoded.RequestedModel
	}

	// Step 11: encode. Headers/body have typically already been partially
	// written by WriteResponse by the time it can fail, so there is usually
	// nothing further to do over the wire; the failure is only observable to
	// an internal caller that wraps it as a *ResponseEncodeError (see that
	// type's doc comment) via its own instrumentation, which this Handler
	// does not do (it has no logger contract).
	_ = sc.WriteResponse(w, resp)
}

// serveCountTokens implements the Anthropic-only
// POST /v1/messages/count_tokens auxiliary route: it resolves and replaces
// the model exactly as serveInference does, then calls the configured
// contextcount.ContextCounter -- never Target.Client -- and writes the
// result via anthropicapi.WriteCountTokensResponse. The ingress codec for
// every response on this route (success or error) is always
// anthropicapi.Codec{}: the route only exists in the Anthropic dialect.
func (h *Handler) serveCountTokens(w http.ResponseWriter, r *http.Request) {
	sc := anthropicapi.Codec{}
	ctx := r.Context()

	if err := h.applyBoundedBody(r); err != nil {
		h.writeError(sc, w, err)
		return
	}

	decoded, err := anthropicapi.DecodeCountTokensRequest(r)
	if err != nil {
		sc.WriteError(w, err)
		return
	}

	target, err := h.resolver.Resolve(ctx, model.APIFormatAnthropic, decoded.RequestedModel)
	if err != nil {
		h.writeError(sc, w, err)
		return
	}
	decoded.Request.Model = target.Model
	if target.AuthoritativeEffort && decoded.Request.Override != nil {
		decoded.Request.Override.Effort = target.Model.Sampling.Effort
	}

	// Unlike serveInference, this path never calls inference.ValidateRequestFeatures:
	// that check exists to protect Target.Client.Invoke/Stream from a request
	// shaped in a way the target can't handle, and this route never calls
	// Target.Client at all -- only the local ContextCounter below.

	if h.contextCounter == nil {
		h.writeError(sc, w, &CountTokensUnavailableError{})
		return
	}

	if !h.acquire() {
		h.writeError(sc, w, &ConcurrencyLimitExceededError{})
		return
	}
	defer h.release()

	count, err := h.contextCounter.CountContext(ctx, decoded.Request)
	if err != nil {
		h.writeError(sc, w, h.upstreamError(ctx, err))
		return
	}

	// See the encode-failure comment in serveInference: nothing further can
	// be done over the wire once WriteCountTokensResponse has started
	// writing.
	_ = anthropicapi.WriteCountTokensResponse(w, saturatingIntFromTokenCount(count.InputTokens))
}

// saturatingIntFromTokenCount converts a uint64-backed content.TokenCount to
// int, saturating at math.MaxInt rather than silently wrapping. In practice
// count.InputTokens comes from contextcount.Estimator's local heuristic and
// is never remotely close to this bound, but the conversion itself must not
// be able to overflow.
func saturatingIntFromTokenCount(n content.TokenCount) int {
	if uint64(n) > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(n) // #nosec G115 -- bounded by the guard above; cannot overflow
}

// applyBoundedBody implements step 4: it reads r.Body fully, bounded by
// h.maxRequestBody, and replaces r.Body with a fresh reader over the exact
// bytes read so a codec's own DecodeRequest sees byte-identical content to
// what a normal (unbounded) read would have produced. A body exceeding the
// bound fails with a *RequestTooLargeError before any codec ever sees it.
//
// This reads the body itself (via io.LimitReader) rather than installing
// http.MaxBytesReader and letting a codec's own DecodeRequest hit the limit:
// an already-committed codec's decode error wraps the underlying read error
// as a string (its Detail field), losing the concrete error type, so this
// package cannot reliably distinguish "body too large" from any other read
// failure after the fact without inspecting error message text. Bounding
// the read here, before any codec is involved, is deterministic and
// codec-agnostic.
func (h *Handler) applyBoundedBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, h.maxRequestBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(data)) > h.maxRequestBody {
		return &RequestTooLargeError{Limit: h.maxRequestBody}
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return nil
}

// acquire attempts non-blocking admission: a full semaphore means immediate
// rejection, never a queued wait.
func (h *Handler) acquire() bool {
	select {
	case h.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns one admission slot. It must be called exactly once per
// successful acquire.
func (h *Handler) release() { <-h.sem }

// writeError classifies err via classifyStatus and, when recognized, wraps
// it in a *failure.APIError carrying that status before handing it to sc's
// WriteError -- the one reusable classification mechanism an
// already-committed codec's WriteError already consults (see http_errors.go
// for the full explanation). An error classifyStatus does not recognize is
// passed to sc.WriteError completely unwrapped.
func (h *Handler) writeError(sc codec.ServerCodec, w http.ResponseWriter, err error) {
	if status, ok := classifyStatus(err); ok {
		sc.WriteError(w, apiError(status, err))
		return
	}
	sc.WriteError(w, err)
}

// upstreamError wraps err (from Target.Client.Invoke, Target.Client.Stream,
// or a configured contextcount.ContextCounter.CountContext) as the single
// shared *UpstreamInvocationError construction used by every upstream-calling
// site in this package (serveInference's Invoke path, serveCountTokens, and
// serveStreaming's pre-header Stream path in stream.go), so the
// DeadlineExceeded classification logic has exactly one implementation.
func (h *Handler) upstreamError(ctx context.Context, err error) *UpstreamInvocationError {
	return &UpstreamInvocationError{
		Err:              err,
		DeadlineExceeded: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
}

// isJSONContentType reports whether raw is the application/json media type,
// ignoring any parameters (e.g. a charset).
func isJSONContentType(raw string) bool {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	return mediaType == jsonContentType
}

// writeGenericError writes a minimal, dialect-agnostic HTTP error response.
// It is used only for the steps of the pipeline (method/content-type/auth)
// that run before an ingress codec has been selected, so there is no
// dialect-native error envelope available yet to write instead.
func writeGenericError(w http.ResponseWriter, err error) {
	status, ok := classifyStatus(err)
	if !ok {
		status = http.StatusInternalServerError
	}
	http.Error(w, err.Error(), status)
}
