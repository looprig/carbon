package anthropicapi

import (
	"encoding/json"
	"errors"
	"net/http"

	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/wire/jsonbody"
)

// ServerDecodeError reports a native Messages request body this codec cannot
// decode into the provider-neutral vocabulary: malformed shape, a missing
// required field, or a recognized-but-unsupported feature (e.g. a tool_choice
// variant or thinking mode the neutral Request cannot represent). Reason is a
// short machine-checkable diagnostic code; Detail elaborates for logs/messages.
type ServerDecodeError struct {
	Reason string
	Detail string
}

func (e *ServerDecodeError) Error() string {
	msg := "anthropicapi: invalid request: " + e.Reason
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// DuplicateKeyError reports a request body with a duplicate JSON object member
// name. encoding/json silently takes the last occurrence; this codec rejects the
// request instead so a client cannot smuggle a semantically different value past
// a naive review of the first occurrence.
type DuplicateKeyError struct {
	Key string
}

func (e *DuplicateKeyError) Error() string {
	return "anthropicapi: duplicate JSON object key " + e.Key
}

// StreamTerminatedError is returned by StreamEncoder.WriteChunk, Finish, or Fail
// once the stream has already been terminated by a prior Finish or Fail call, per
// the single-termination-ownership rule in codec.StreamEncoder.
type StreamTerminatedError struct{}

func (e *StreamTerminatedError) Error() string {
	return "anthropicapi: stream already terminated"
}

// UnsupportedChunkError is returned when a content.Chunk has a concrete type this
// dialect's stream encoder does not model. content.Chunk is a sealed interface, so
// this only guards against future variants added to the vocabulary.
type UnsupportedChunkError struct {
	Chunk string
}

func (e *UnsupportedChunkError) Error() string {
	return "anthropicapi: unsupported stream chunk type " + e.Chunk
}

// writeMessageError encodes err as the native Anthropic error envelope
// (`{"type":"error","error":{"type":...,"message":...}}`) and writes it with the
// classified HTTP status. It never panics, regardless of err's concrete type
// (including nil).
func writeMessageError(w http.ResponseWriter, err error) {
	status, wireType, message := classifyError(err)
	body, marshalErr := json.Marshal(wireErrorResponse{
		Type:  responseTypeError,
		Error: anthropicError{Type: wireType, Message: message},
	})
	if marshalErr != nil {
		// Marshaling a plain string-only struct cannot realistically fail, but
		// WriteError must never panic: fall back to a fixed, valid envelope.
		body = []byte(`{"type":"error","error":{"type":"api_error","message":"internal encode error"}}`)
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// wireErrorResponse is the native Anthropic error envelope, reusing the existing
// anthropicError wire struct (types.go) for the nested `error` object.
type wireErrorResponse struct {
	Type  string         `json:"type"`
	Error anthropicError `json:"error"`
}

// classifyError maps an arbitrary Go error to the HTTP status and Anthropic
// `error.type` wire value used by both WriteError (pre-header) and
// StreamEncoder.Fail (post-header, via a native `error` SSE event carrying the
// same envelope shape). A nil err classifies as a generic 500 api_error so
// callers never need a nil guard before calling this.
func classifyError(err error) (status int, wireType string, message string) {
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
		return status, wireTypeForStatus(status), err.Error()
	}

	return http.StatusInternalServerError, "api_error", err.Error()
}

// wireTypeForStatus maps an HTTP status to Anthropic's closest native
// error.type label for statuses this codec did not otherwise classify from a
// typed error (e.g. an opaque upstream *failure.APIError).
func wireTypeForStatus(status int) string {
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
	case 529: // Anthropic's own "overloaded" status, not a stdlib http constant.
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
