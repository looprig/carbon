package anthropic

import "fmt"

type CounterStateReason string

const (
	CounterStateNilReceiver          CounterStateReason = "nil counter"
	CounterStateNilContext           CounterStateReason = "nil context"
	CounterStateMissingEndpoint      CounterStateReason = "missing endpoint"
	CounterStateMissingAuthenticator CounterStateReason = "missing authenticator"
	CounterStateMissingHTTPDoer      CounterStateReason = "missing HTTP doer"
	CounterStateInvalidTimeout       CounterStateReason = "invalid timeout"
)

type CounterStateError struct{ Reason CounterStateReason }

func (e *CounterStateError) Error() string {
	return "anthropic: invalid count-tokens counter state: " + string(e.Reason)
}

type CounterRequestReason string

const (
	CounterRequestEncodeFailed CounterRequestReason = "Messages request encoding failed"
	CounterRequestMalformed    CounterRequestReason = "encoded request is not one JSON object"
)

type CounterRequestError struct {
	Reason CounterRequestReason
	Err    error
}

func (e *CounterRequestError) Error() string {
	if e.Err != nil {
		return "anthropic: build count-tokens request: " + string(e.Reason) + ": " + e.Err.Error()
	}
	return "anthropic: build count-tokens request: " + string(e.Reason)
}

func (e *CounterRequestError) Unwrap() error { return e.Err }

type CounterEndpointReason string

const (
	CounterEndpointMalformed         CounterEndpointReason = "malformed endpoint"
	CounterEndpointMissingHost       CounterEndpointReason = "missing endpoint host"
	CounterEndpointCredentials       CounterEndpointReason = "endpoint contains credentials"
	CounterEndpointUnsupportedScheme CounterEndpointReason = "unsupported endpoint scheme"
	CounterEndpointInsecureTransport CounterEndpointReason = "plaintext endpoint is not loopback"
)

type CounterEndpointError struct{ Reason CounterEndpointReason }

func (e *CounterEndpointError) Error() string {
	return "anthropic: invalid count-tokens endpoint: " + string(e.Reason)
}

type CounterResponseReason string

const (
	CounterResponseMalformed      CounterResponseReason = "malformed response"
	CounterResponseMissingCount   CounterResponseReason = "missing input_tokens"
	CounterResponseInvalidCount   CounterResponseReason = "invalid input_tokens"
	CounterResponseDuplicateField CounterResponseReason = "duplicate response field"
	CounterResponseBodyTooLarge   CounterResponseReason = "response body too large"
)

type CounterResponseField string

const CounterResponseFieldInputTokens CounterResponseField = "input_tokens"

type CounterResponseFieldReason string

const CounterResponseFieldDuplicate CounterResponseFieldReason = "duplicate"

type CounterResponseFieldError struct {
	Field  CounterResponseField
	Reason CounterResponseFieldReason
}

func (e *CounterResponseFieldError) Error() string {
	return fmt.Sprintf("anthropic: count-tokens response field %s: %s", e.Field, e.Reason)
}

type CounterResponseError struct {
	Reason CounterResponseReason
	Err    error
}

func (e *CounterResponseError) Error() string {
	if e.Err != nil {
		return "anthropic: count-tokens response: " + string(e.Reason) + ": " + e.Err.Error()
	}
	return "anthropic: count-tokens response: " + string(e.Reason)
}

func (e *CounterResponseError) Unwrap() error { return e.Err }
