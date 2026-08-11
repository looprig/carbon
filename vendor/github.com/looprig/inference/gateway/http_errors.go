package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/looprig/inference"
	"github.com/looprig/inference/failure"
)

// This file defines the HTTP status classification this package owns (see
// the package doc for the full table) and the typed errors that back it.
//
// Classification mechanism: codec.ServerCodec.WriteError is owned by each
// ingress codec (e.g. anthropicapi.Codec), and this package cannot modify
// an already-committed codec package to teach it about gateway-level error
// types. anthropicapi's classifyError (server_error.go) already classifies
// by concrete type via errors.As, with one deliberately generic case:
// *failure.APIError, whose Status field is trusted verbatim (clamped to a
// valid HTTP status range). That is the one reusable, already-existing
// cross-package classification mechanism, so this package's writeError
// helper (handler.go) wraps every gateway-recognized error in a
// *failure.APIError carrying the status computed here before handing it to
// a codec's WriteError. Errors this package does NOT recognize (chiefly: a
// codec's own native decode errors) are hard-required to be passed straight
// to that SAME codec's WriteError unwrapped -- see handler.go's decode step
// -- since that codec already classifies its own error types correctly and
// wrapping would only discard that fidelity.
//
// No new cross-package interface (e.g. an HTTPClassified/HTTPStatus()
// contract on codec.ServerCodec) was added for this: one was not needed
// because the *failure.APIError mechanism above already exists and already
// covers every status this package needs to produce. See the package doc
// for a note on why that path was chosen over adding one.

// MethodNotAllowedError reports an HTTP method this gateway does not accept
// for any configured route. Every inference route this gateway serves is
// POST-only, so method validation happens once, generically, before codec
// selection.
type MethodNotAllowedError struct{ Method string }

func (e *MethodNotAllowedError) Error() string {
	return "gateway: method not allowed: " + e.Method
}

// UnsupportedContentTypeError reports a request whose Content-Type is not
// application/json. Every currently bundled dialect is JSON-bodied, so this
// gateway enforces the constraint once, generically, before codec selection
// -- ahead of, and instead of, any per-codec Content-Type check a codec's own
// DecodeRequest might otherwise perform (which would classify as 400, not
// the 415 this boundary requires).
type UnsupportedContentTypeError struct{ ContentType string }

func (e *UnsupportedContentTypeError) Error() string {
	return "gateway: unsupported content type: " + e.ContentType
}

// RequestTooLargeError reports a request body exceeding Config.MaxRequestBody.
type RequestTooLargeError struct{ Limit int64 }

func (e *RequestTooLargeError) Error() string {
	return fmt.Sprintf("gateway: request body exceeds %d byte limit", e.Limit)
}

// NoMatchingCodecError reports that no configured ServerCodec recognized a
// request's method and path.
type NoMatchingCodecError struct {
	Method string
	Path   string
}

func (e *NoMatchingCodecError) Error() string {
	return "gateway: no route for " + e.Method + " " + e.Path
}

// AmbiguousCodecMatchError reports that more than one configured ServerCodec
// matched the same concrete request. This indicates a caller misconfiguration
// (overlapping MatchRequest implementations registered together), not a
// client error.
type AmbiguousCodecMatchError struct {
	Method string
	Path   string
	Count  int
}

func (e *AmbiguousCodecMatchError) Error() string {
	return "gateway: ambiguous codec match for " + e.Method + " " + e.Path
}

// ConcurrencyLimitExceededError reports that Config.MaxConcurrent in-flight
// requests were already admitted when this request arrived. Admission is
// reject-on-full, never queued.
type ConcurrencyLimitExceededError struct{}

func (e *ConcurrencyLimitExceededError) Error() string {
	return "gateway: concurrency limit exceeded"
}

// CountTokensUnavailableError reports that the count_tokens route was
// invoked but the Handler was constructed with a nil Config.ContextCounter.
type CountTokensUnavailableError struct{}

func (e *CountTokensUnavailableError) Error() string {
	return "gateway: count_tokens is unavailable: no ContextCounter is configured"
}

// UpstreamInvocationError reports that Target.Client.Invoke, Target.Client.Stream, or a
// configured contextcount.ContextCounter.CountContext returned an error, or
// that Target.Client.Invoke never returned before the request's context
// deadline. Its Error() message is deliberately generic -- it never includes
// the wrapped error's own message -- because that message originates from an
// upstream provider client the gateway does not control and may contain
// upstream-secret material (e.g. an echoed Authorization header from a
// misbehaving transport). Callers that need the underlying detail for
// internal diagnostics use errors.Unwrap/errors.As, never the HTTP response.
type UpstreamInvocationError struct {
	Err              error
	DeadlineExceeded bool
}

func (e *UpstreamInvocationError) Error() string {
	if e.DeadlineExceeded {
		return "gateway: upstream invocation deadline exceeded"
	}
	return "gateway: upstream invocation failed"
}

func (e *UpstreamInvocationError) Unwrap() error { return e.Err }

// ResponseEncodeError reports that a codec's WriteResponse failed after a
// successful upstream invocation. By the time this can occur the codec has
// typically already written HTTP headers (and possibly a partial body), so
// there is usually nothing further this package can do over the wire; the
// type exists for classification completeness and for callers that inspect
// it via errors.As for internal diagnostics.
type ResponseEncodeError struct{ Err error }

func (e *ResponseEncodeError) Error() string { return "gateway: response encode failed" }
func (e *ResponseEncodeError) Unwrap() error { return e.Err }

// classifyStatus reports the HTTP status this package assigns to err and
// whether err is one this package recognizes at all. A recognized error is
// always wrapped in a *failure.APIError carrying this status before being
// handed to a codec's WriteError (see writeError in handler.go); an
// unrecognized error is passed to WriteError completely unwrapped, trusting
// the codec that produced it (or that will render it) to classify it
// correctly on its own -- this is the "passthrough" behavior for a codec's
// native decode errors.
func classifyStatus(err error) (int, bool) {
	switch {
	case as[*AuthenticationError](err):
		return http.StatusUnauthorized, true
	case as[*RequestTooLargeError](err):
		return http.StatusRequestEntityTooLarge, true
	case as[*MethodNotAllowedError](err):
		return http.StatusMethodNotAllowed, true
	case as[*UnsupportedContentTypeError](err):
		return http.StatusUnsupportedMediaType, true
	case as[*NoMatchingCodecError](err):
		return http.StatusNotFound, true
	case as[*RouteNotFoundError](err):
		return http.StatusNotFound, true
	case as[*UnknownRouteError](err):
		return http.StatusNotFound, true
	case as[*AmbiguousCodecMatchError](err):
		return http.StatusInternalServerError, true
	case as[*ConfigError](err):
		return http.StatusInternalServerError, true
	case as[*ConcurrencyLimitExceededError](err):
		return http.StatusTooManyRequests, true
	case as[*CountTokensUnavailableError](err):
		return http.StatusServiceUnavailable, true
	case as[*ResponseEncodeError](err):
		return http.StatusInternalServerError, true
	case as[*UpstreamInvocationError](err):
		return classifyUpstreamStatus(err), true

	// "Unsupported semantic feature" passthrough: inference.ValidateRequestFeatures
	// returns one of these typed errors, none of which the already-committed
	// anthropicapi.classifyError recognizes today. They are reused as-is (no
	// new gateway-level wrapper type is defined for them -- hence
	// "passthrough" in the design doc's category list), but this package
	// still actively classifies them to 400 via the *failure.APIError
	// mechanism so every codec's WriteError reports the right status.
	case as[*inference.StructuredOutputConflictError](err),
		as[*inference.ImageInputUnsupportedError](err),
		as[*inference.StructuredOutputUnsupportedError](err),
		as[*inference.StructuredOutputWithToolsUnsupportedError](err),
		as[*inference.SchemaValidationError](err):
		return http.StatusBadRequest, true

	default:
		return 0, false
	}
}

// as reports whether err's chain contains a value of type T, without
// exposing the recovered value to the caller -- classifyStatus only needs
// the yes/no answer.
func as[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// apiError creates a *failure.APIError carrying status -- the
// mechanism an already-committed codec's WriteError/classifyError already
// consults (see the file doc comment above). Every gateway-recognized error
// type's Error() string is deliberately free of secret material (bearer
// tokens, upstream credentials), so using it verbatim as the wrapped
// message is safe.
func apiError(status int, err error) error {
	return failure.NewAPIErrorWithStatusText(status, "", "", 0, err.Error())
}

// classifyUpstreamStatus maps an *UpstreamInvocationError to the specific
// upstream-failure status: 504 when the request's context deadline was
// exceeded, the upstream's own classified status when it wrapped a
// *failure.APIError with a valid status, 503 when it wrapped a
// *failure.NetworkError (a connectivity failure -- the target itself is
// unavailable), and 502 (classified upstream/provider failure) otherwise.
func classifyUpstreamStatus(err error) int {
	var upstream *UpstreamInvocationError
	if !errors.As(err, &upstream) {
		return http.StatusBadGateway
	}
	if upstream.DeadlineExceeded {
		return http.StatusGatewayTimeout
	}
	if upstream.Err == nil {
		return http.StatusBadGateway
	}
	if errors.Is(upstream.Err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var apiErr *failure.APIError
	if errors.As(upstream.Err, &apiErr) {
		status := apiErr.Status
		if status < 100 || status > 599 {
			return http.StatusBadGateway
		}
		return status
	}
	var netErr *failure.NetworkError
	if errors.As(upstream.Err, &netErr) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}
