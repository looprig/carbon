package gemini

import (
	"fmt"

	model "github.com/looprig/inference/model"
)

// CounterStateReason classifies an unusable Counter or CountContext boundary.
type CounterStateReason string

const (
	CounterStateNilReceiver          CounterStateReason = "nil counter"
	CounterStateNilContext           CounterStateReason = "nil context"
	CounterStateMissingEndpoint      CounterStateReason = "missing endpoint"
	CounterStateMissingAuthenticator CounterStateReason = "missing authenticator"
	CounterStateMissingHTTPDoer      CounterStateReason = "missing HTTP doer"
	CounterStateInvalidTimeout       CounterStateReason = "invalid timeout"
)

// CounterStateError rejects an invalid counter before request encoding or I/O.
type CounterStateError struct {
	Reason CounterStateReason
}

func (e *CounterStateError) Error() string {
	return "gemini: invalid countTokens counter state: " + string(e.Reason)
}

// CounterRequestReason classifies failure to build the countTokens envelope
// from the already encoded complete GenerateContentRequest.
type CounterRequestReason string

const (
	CounterRequestGenerateBodyInvalid CounterRequestReason = "generateContentRequest body is not one JSON object"
	CounterRequestModelEncodingFailed CounterRequestReason = "model resource JSON encoding failed"
	CounterRequestModelCollision      CounterRequestReason = "generateContentRequest already contains model"
)

// CounterRequestError reports a local countTokens envelope invariant failure.
// It never carries request bytes or the model name.
type CounterRequestError struct {
	Reason CounterRequestReason
	Err    error
}

func (e *CounterRequestError) Error() string {
	if e.Err != nil {
		return "gemini: build countTokens request: " + string(e.Reason) + ": " + e.Err.Error()
	}
	return "gemini: build countTokens request: " + string(e.Reason)
}

func (e *CounterRequestError) Unwrap() error { return e.Err }

// CounterEndpointReason classifies an endpoint that cannot safely carry a
// countTokens request. Values never include the rejected endpoint or credentials.
type CounterEndpointReason string

const (
	CounterEndpointMalformed         CounterEndpointReason = "malformed endpoint"
	CounterEndpointMissingHost       CounterEndpointReason = "missing endpoint host"
	CounterEndpointCredentials       CounterEndpointReason = "endpoint contains credentials"
	CounterEndpointUnsupportedScheme CounterEndpointReason = "unsupported endpoint scheme"
	CounterEndpointInsecureTransport CounterEndpointReason = "plaintext endpoint is not loopback"
	CounterEndpointNonASCIIHost      CounterEndpointReason = "endpoint host is not ASCII"
	CounterEndpointInvalidHost       CounterEndpointReason = "invalid endpoint host"
	CounterEndpointAmbiguousPath     CounterEndpointReason = "ambiguous escaped endpoint path"
)

// CounterEndpointError rejects unsafe counter routing before any request I/O.
// The raw endpoint is deliberately omitted because it may contain credentials.
type CounterEndpointError struct {
	Reason CounterEndpointReason
}

func (e *CounterEndpointError) Error() string {
	return "gemini: invalid countTokens endpoint: " + string(e.Reason)
}

// CounterResponseReason classifies a countTokens response that cannot produce a
// trustworthy normalized input-token count.
type CounterResponseReason string

const (
	CounterResponseMalformed      CounterResponseReason = "malformed response"
	CounterResponseMissingCount   CounterResponseReason = "missing totalTokens"
	CounterResponseInvalidCount   CounterResponseReason = "invalid totalTokens"
	CounterResponseDuplicateField CounterResponseReason = "duplicate response field"
	CounterResponseBodyTooLarge   CounterResponseReason = "response body too large"
)

// CounterResponseField identifies one field in a countTokens response.
type CounterResponseField string

const CounterResponseFieldTotalTokens CounterResponseField = "totalTokens"

// CounterResponseFieldReason classifies an ambiguous response field.
type CounterResponseFieldReason string

const CounterResponseFieldDuplicate CounterResponseFieldReason = "duplicate"

// CounterResponseFieldError reports a field-level response ambiguity without
// retaining provider-controlled values.
type CounterResponseFieldError struct {
	Field  CounterResponseField
	Reason CounterResponseFieldReason
}

func (e *CounterResponseFieldError) Error() string {
	return "gemini: countTokens response field " + string(e.Field) + ": " + string(e.Reason)
}

// CounterResponseError reports an invalid successful countTokens response.
// Provider payload bytes are deliberately omitted so Error never leaks request
// or response data; Err carries only a typed/safe parsing cause.
type CounterResponseError struct {
	Reason CounterResponseReason
	Err    error
}

func (e *CounterResponseError) Error() string {
	if e.Err != nil {
		return "gemini: countTokens response: " + string(e.Reason) + ": " + e.Err.Error()
	}
	return "gemini: countTokens response: " + string(e.Reason)
}

func (e *CounterResponseError) Unwrap() error { return e.Err }

// UnsupportedAPIFormatError is a fail-closed rejection, before any I/O, of a
// request whose Model.APIFormat this client cannot honor. This client encodes only
// the Gemini generateContent dialect. Provider.supportsAPIFormat currently admits
// only APIFormatGemini for ProviderGoogle, so a ValidateModel-passing Google Model
// can never reach this guard — it is defense-in-depth (Open/Closed): should a second
// Google dialect ever be admitted upstream, this keeps the client from silently
// Gemini-encoding a request it does not understand. Carries the offending format so
// callers can branch via errors.As.
type UnsupportedAPIFormatError struct {
	APIFormat model.APIFormat
}

func (e *UnsupportedAPIFormatError) Error() string {
	return fmt.Sprintf("gemini: API format %q is not implemented; this client encodes only the Gemini dialect (%q)", e.APIFormat, model.APIFormatGemini)
}

// RequestBuildError is a failure to CONSTRUCT the outbound HTTP request (a
// malformed endpoint/URL), kept distinct from *failure.NetworkError (reserved for
// transport failures out of hc.Do) so errors.As never misclassifies a config bug
// as a transport fault. Unwrap exposes the net/http cause.
type RequestBuildError struct {
	Err error
}

func (e *RequestBuildError) Error() string { return "gemini: build request: " + e.Err.Error() }
func (e *RequestBuildError) Unwrap() error { return e.Err }
