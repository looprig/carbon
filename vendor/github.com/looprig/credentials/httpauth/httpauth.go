// Package httpauth contains call-scoped HTTP request authorizers. It depends
// only on secrets and deliberately does not import the credentials root,
// keeping the protocol boundary usable by sources and transports alike.
package httpauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/looprig/secrets"
)

var (
	ErrInvalidHeaderName  = errors.New("httpauth: invalid header name")
	ErrInvalidHeaderValue = errors.New("httpauth: invalid header value")
	ErrZeroSecret         = errors.New("httpauth: zero secret")
	ErrNilRequest         = errors.New("httpauth: nil request")
	ErrNilContext         = errors.New("httpauth: nil context")
	ErrCanceled           = errors.New("httpauth: authorization canceled")
)

const MaxHeaderNameLength = 256

type errorReason uint8

const (
	errorReasonInvalid errorReason = iota
	errorReasonName
	errorReasonValue
	errorReasonZero
	errorReasonNilRequest
	errorReasonNilContext
)

func normalizeReason(value string) errorReason {
	switch value {
	case "name":
		return errorReasonName
	case "value":
		return errorReasonValue
	case "zero":
		return errorReasonZero
	case "nil request":
		return errorReasonNilRequest
	case "nil context":
		return errorReasonNilContext
	default:
		return errorReasonInvalid
	}
}

func reasonString(value errorReason) string {
	switch value {
	case errorReasonName:
		return "name"
	case errorReasonValue:
		return "value"
	case errorReasonZero:
		return "zero"
	case errorReasonNilRequest:
		return "nil request"
	case errorReasonNilContext:
		return "nil context"
	default:
		return "invalid"
	}
}

func formatSafe(state fmt.State, message string) {
	_, _ = state.Write([]byte(message))
}

func (e *invalidHeaderNameError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

type invalidHeaderNameError struct{ reason errorReason }

// InvalidHeaderNameError reports a malformed HTTP field name without
// retaining the caller's input.
type InvalidHeaderNameError = invalidHeaderNameError

func (e *invalidHeaderNameError) Error() string {
	if e == nil {
		return ErrInvalidHeaderName.Error()
	}
	return "httpauth: invalid header name (" + reasonString(e.reason) + ")"
}
func (e *invalidHeaderNameError) Unwrap() error { return ErrInvalidHeaderName }
func (e *invalidHeaderNameError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *invalidHeaderNameError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *invalidHeaderNameError) GoString() string               { return e.Error() }

type invalidHeaderValueError struct{ reason errorReason }

// InvalidHeaderValueError reports an authority that cannot safely become an
// HTTP header value.
type InvalidHeaderValueError = invalidHeaderValueError

func (e *invalidHeaderValueError) Error() string {
	if e == nil {
		return ErrInvalidHeaderValue.Error()
	}
	return "httpauth: invalid header value (" + reasonString(e.reason) + ")"
}
func (e *invalidHeaderValueError) Unwrap() error { return ErrInvalidHeaderValue }
func (e *invalidHeaderValueError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *invalidHeaderValueError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *invalidHeaderValueError) GoString() string               { return e.Error() }
func (e *invalidHeaderValueError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

type zeroSecretError struct{}

// ZeroSecretError reports use of an invalid zero secrets.Secret.
type ZeroSecretError = zeroSecretError

func (e *zeroSecretError) Error() string                  { return ErrZeroSecret.Error() }
func (e *zeroSecretError) Unwrap() error                  { return ErrZeroSecret }
func (e *zeroSecretError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *zeroSecretError) GoString() string               { return e.Error() }
func (e *zeroSecretError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

type nilRequestError struct{}

// NilRequestError reports an attempt to authorize a nil request.
type NilRequestError = nilRequestError

func (e *nilRequestError) Error() string                  { return ErrNilRequest.Error() }
func (e *nilRequestError) Unwrap() error                  { return ErrNilRequest }
func (e *nilRequestError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *nilRequestError) GoString() string               { return e.Error() }
func (e *nilRequestError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

type nilContextError struct{}

func (e *nilContextError) Error() string                  { return ErrNilContext.Error() }
func (e *nilContextError) Unwrap() error                  { return ErrNilContext }
func (e *nilContextError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *nilContextError) GoString() string               { return e.Error() }
func (e *nilContextError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

type canceledError struct{ cause error }

func (e *canceledError) Error() string { return ErrCanceled.Error() }
func (e *canceledError) Unwrap() error {
	if e == nil || e.cause == nil {
		return context.Canceled
	}
	return e.cause
}
func (e *canceledError) Is(target error) bool {
	if target == ErrCanceled {
		return true
	}
	return errors.Is(e.Unwrap(), target)
}
func (e *canceledError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *canceledError) GoString() string               { return e.Error() }
func (e *canceledError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

func contextError(ctx context.Context) error {
	if ctx == nil {
		return &nilContextError{}
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &canceledError{cause: context.DeadlineExceeded}
		}
		return &canceledError{cause: context.Canceled}
	}
	return nil
}

// Authorizer applies one immutable authority snapshot to one concrete
// request attempt.
type Authorizer interface {
	Authorize(context.Context, *http.Request) error
}

type noneAuthorizer struct{}

var none = noneAuthorizer{}

// None returns an immutable no-op authorizer for explicitly unauthenticated
// local transports.
func None() Authorizer { return none }

func (noneAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request == nil {
		return &nilRequestError{}
	}
	return contextError(ctx)
}
func (noneAuthorizer) String() string                   { return "httpauth: none" }
func (a noneAuthorizer) Format(state fmt.State, _ rune) { formatSafe(state, a.String()) }
func (a noneAuthorizer) GoString() string               { return a.String() }
func (a noneAuthorizer) LogValue() slog.Value           { return slog.StringValue(a.String()) }

type headerAuthorizer struct {
	name  string
	value string
}

// readSecretBytes is kept as a package-internal seam so tests can verify that
// the mutable copy returned by Secret.Bytes is cleared at the consumption
// boundary. Authorizers retain only the immutable string conversion.
var readSecretBytes = func(value secrets.Secret) []byte { return value.Bytes() }

// Header constructs an immutable authorizer that sets one HTTP field. The
// authority is copied from Secret.Bytes only at construction, never retained
// in a caller-owned mutable buffer.
func Header(name string, value secrets.Secret) (Authorizer, error) {
	canonical, err := canonicalHeaderName(name)
	if err != nil {
		return nil, err
	}
	bytes := readSecretBytes(value)
	if len(bytes) == 0 {
		clear(bytes)
		return nil, &zeroSecretError{}
	}
	if !validHeaderValue(bytes) {
		clear(bytes)
		return nil, &invalidHeaderValueError{reason: errorReasonValue}
	}
	authority := string(bytes)
	clear(bytes)
	return &headerAuthorizer{name: canonical, value: authority}, nil
}

// NewHeader is an explicit constructor alias for callers that prefer the
// conventional New prefix.
func NewHeader(name string, value secrets.Secret) (Authorizer, error) {
	return Header(name, value)
}

func (a *headerAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request == nil {
		return &nilRequestError{}
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	deleteHeaderCaseInsensitive(request.Header, a.name)
	// Set is intentional: each concrete attempt has exactly one current
	// authority value and never accumulates stale retries.
	request.Header.Set(a.name, a.value)
	return nil
}

func (a *headerAuthorizer) String() string {
	if a == nil {
		return "httpauth: nil header authorizer"
	}
	return "httpauth: header authorizer (" + a.name + ")"
}
func (a *headerAuthorizer) Format(state fmt.State, _ rune) { formatSafe(state, a.String()) }
func (a *headerAuthorizer) GoString() string               { return a.String() }
func (a *headerAuthorizer) LogValue() slog.Value           { return slog.StringValue(a.String()) }

type bearerAuthorizer struct{ value string }

// Bearer constructs an immutable Authorization: Bearer authorizer.
func Bearer(value secrets.Secret) (Authorizer, error) {
	bytes := readSecretBytes(value)
	if len(bytes) == 0 {
		clear(bytes)
		return nil, &zeroSecretError{}
	}
	if !validHeaderValue(bytes) {
		clear(bytes)
		return nil, &invalidHeaderValueError{reason: errorReasonValue}
	}
	authority := string(bytes)
	clear(bytes)
	return &bearerAuthorizer{value: authority}, nil
}

// NewBearer is an explicit constructor alias for callers that prefer the New
// naming convention.
func NewBearer(value secrets.Secret) (Authorizer, error) { return Bearer(value) }

func (a *bearerAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request == nil {
		return &nilRequestError{}
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	deleteHeaderCaseInsensitive(request.Header, "Authorization")
	request.Header.Set("Authorization", "Bearer "+a.value)
	return nil
}

func (a *bearerAuthorizer) String() string {
	if a == nil {
		return "httpauth: nil bearer authorizer"
	}
	return "httpauth: bearer authorizer"
}
func (a *bearerAuthorizer) Format(state fmt.State, _ rune) { formatSafe(state, a.String()) }
func (a *bearerAuthorizer) GoString() string               { return a.String() }
func (a *bearerAuthorizer) LogValue() slog.Value           { return slog.StringValue(a.String()) }

func canonicalHeaderName(name string) (string, error) {
	if len(name) == 0 || len(name) > MaxHeaderNameLength {
		return "", &invalidHeaderNameError{reason: errorReasonName}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isHeaderToken(c) {
			return "", &invalidHeaderNameError{reason: errorReasonName}
		}
	}
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if canonical == "" {
		return "", &invalidHeaderNameError{reason: errorReasonName}
	}
	return canonical, nil
}

func isHeaderToken(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
	}
}

func validHeaderValue(value []byte) bool {
	for _, c := range value {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

func deleteHeaderCaseInsensitive(header http.Header, name string) {
	for key := range header {
		if strings.EqualFold(key, name) {
			delete(header, key)
		}
	}
}
