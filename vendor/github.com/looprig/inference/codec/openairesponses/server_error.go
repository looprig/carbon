package openairesponses

import (
	"encoding/json"
	"errors"
	"net/http"

	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/wire/jsonbody"
)

// writeResponsesError encodes err as a native Responses error envelope
// (`{"error":{"code":...,"message":...}}` — this codec's own best-effort
// choice; the real Responses API's exact pre-header error shape is not
// verified against a live endpoint, see the package doc) and writes it with
// the classified HTTP status. It never panics, regardless of err's concrete
// type (including nil).
func writeResponsesError(w http.ResponseWriter, err error) {
	status, code, message := classifyError(err)
	body, marshalErr := json.Marshal(wireErrorEnvelope{Error: wireResponseError{Code: code, Message: message}})
	if marshalErr != nil {
		// Marshaling a plain string-only struct cannot realistically fail, but
		// WriteError must never panic: fall back to a fixed, valid envelope.
		body = []byte(`{"error":{"code":"api_error","message":"internal encode error"}}`)
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// wireErrorEnvelope is the native Responses error envelope, reusing the
// existing wireResponseError wire struct (types.go) for the nested `error`
// object.
type wireErrorEnvelope struct {
	Error wireResponseError `json:"error"`
}

// classifyError maps an arbitrary Go error to the HTTP status and a short
// machine-readable code, used by both writeResponsesError (pre-header) and
// StreamEncoder.Fail (post-header, via a native response.failed event
// carrying the same envelope shape). A nil err classifies as a generic 500
// api_error so callers never need a nil guard before calling this.
func classifyError(err error) (status int, code string, message string) {
	if err == nil {
		return http.StatusInternalServerError, "api_error", "unknown error"
	}

	var decErr *ServerDecodeError
	if errors.As(err, &decErr) {
		return http.StatusBadRequest, "invalid_request_error", decErr.Error()
	}
	var dupErr *DuplicateKeyError
	if errors.As(err, &dupErr) {
		return http.StatusBadRequest, "invalid_request_error", dupErr.Error()
	}
	var blockErr *UnsupportedBlockError
	if errors.As(err, &blockErr) {
		return http.StatusBadRequest, "invalid_request_error", blockErr.Error()
	}
	var convErr *UnsupportedConversationError
	if errors.As(err, &convErr) {
		return http.StatusBadRequest, "invalid_request_error", convErr.Error()
	}
	var chunkErr *UnsupportedChunkError
	if errors.As(err, &chunkErr) {
		return http.StatusBadRequest, "invalid_request_error", chunkErr.Error()
	}
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status < 100 || status > 599 {
			status = http.StatusBadGateway
		}
		return status, codeForStatus(status), err.Error()
	}

	return http.StatusInternalServerError, "api_error", err.Error()
}

// codeForStatus maps an HTTP status to a short error code for statuses this
// codec did not otherwise classify from a typed error (e.g. an opaque
// upstream *failure.APIError).
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
