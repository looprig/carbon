package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Package errors intentionally expose only closed, package-owned categories.
// None of the typed errors retain caller strings, provider responses, or
// wrapped provider causes: those values may contain credential material.
var (
	ErrInvalidReference  = errors.New("credentials: invalid reference")
	ErrInvalidDescriptor = errors.New("credentials: invalid descriptor")
	ErrInvalidGeneration = errors.New("credentials: invalid generation")
	ErrInvalidFailure    = errors.New("credentials: invalid failure")
	ErrInvalidRecord     = errors.New("credentials: invalid record")
	ErrInvalidScheme     = errors.New("credentials: invalid scheme")
	ErrInvalidUsage      = errors.New("credentials: invalid usage class")
	ErrSourceClosed      = errors.New("credentials: source closed")
	ErrClosed            = ErrSourceClosed
	ErrNilContext        = errors.New("credentials: nil context")
	ErrCanceled          = errors.New("credentials: operation canceled")
)

type reason uint8

const (
	reasonInvalid reason = iota
	reasonLength
	reasonEncoding
	reasonScheme
	reasonPath
	reasonProvider
	reasonName
	reasonTransport
	reasonIssuer
	reasonAudience
	reasonLabel
	reasonUsage
	reasonValue
	reasonZero
	reasonNilReceiver
	reasonSchema
	reasonReference
	reasonDescriptor
	reasonState
	reasonContext
	reasonClosed
)

func normalizeReason(value string) reason {
	switch value {
	case "length":
		return reasonLength
	case "encoding":
		return reasonEncoding
	case "scheme":
		return reasonScheme
	case "path":
		return reasonPath
	case "provider":
		return reasonProvider
	case "name":
		return reasonName
	case "transport":
		return reasonTransport
	case "issuer":
		return reasonIssuer
	case "audience":
		return reasonAudience
	case "label":
		return reasonLabel
	case "usage":
		return reasonUsage
	case "value":
		return reasonValue
	case "zero":
		return reasonZero
	case "nil receiver":
		return reasonNilReceiver
	case "schema":
		return reasonSchema
	case "reference":
		return reasonReference
	case "descriptor":
		return reasonDescriptor
	case "state":
		return reasonState
	case "context":
		return reasonContext
	case "closed":
		return reasonClosed
	default:
		return reasonInvalid
	}
}

func reasonString(value reason) string {
	switch value {
	case reasonLength:
		return "length"
	case reasonEncoding:
		return "encoding"
	case reasonScheme:
		return "scheme"
	case reasonPath:
		return "path"
	case reasonProvider:
		return "provider"
	case reasonName:
		return "name"
	case reasonTransport:
		return "transport"
	case reasonIssuer:
		return "issuer"
	case reasonAudience:
		return "audience"
	case reasonLabel:
		return "label"
	case reasonUsage:
		return "usage"
	case reasonValue:
		return "value"
	case reasonZero:
		return "zero"
	case reasonNilReceiver:
		return "nil receiver"
	case reasonSchema:
		return "schema"
	case reasonReference:
		return "reference"
	case reasonDescriptor:
		return "descriptor"
	case reasonState:
		return "state"
	case reasonContext:
		return "context"
	case reasonClosed:
		return "closed"
	default:
		return "invalid"
	}
}

func formatSafe(state fmt.State, message string) {
	_, _ = state.Write([]byte(message))
}

func logSafe(message string) slog.Value { return slog.StringValue(message) }

// InvalidReferenceError reports malformed reference text without retaining
// that text.
type InvalidReferenceError struct{ reason reason }

func NewInvalidReferenceError(value string) *InvalidReferenceError {
	return &InvalidReferenceError{reason: normalizeReason(value)}
}

func (e *InvalidReferenceError) Error() string {
	if e == nil {
		return ErrInvalidReference.Error()
	}
	return "credentials: invalid reference (" + reasonString(e.reason) + ")"
}

func (e *InvalidReferenceError) Unwrap() error { return ErrInvalidReference }
func (e *InvalidReferenceError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *InvalidReferenceError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *InvalidReferenceError) GoString() string               { return e.Error() }
func (e *InvalidReferenceError) LogValue() slog.Value           { return logSafe(e.Error()) }

// InvalidDescriptorError reports an invalid binding descriptor.
type InvalidDescriptorError struct{ reason reason }

func NewInvalidDescriptorError(value string) *InvalidDescriptorError {
	return &InvalidDescriptorError{reason: normalizeReason(value)}
}

func (e *InvalidDescriptorError) Error() string {
	if e == nil {
		return ErrInvalidDescriptor.Error()
	}
	return "credentials: invalid descriptor (" + reasonString(e.reason) + ")"
}

func (e *InvalidDescriptorError) Unwrap() []error {
	if e == nil {
		return []error{ErrInvalidDescriptor}
	}
	if e.reason == reasonScheme {
		return []error{ErrInvalidDescriptor, ErrInvalidScheme}
	}
	if e.reason == reasonUsage {
		return []error{ErrInvalidDescriptor, ErrInvalidUsage}
	}
	return []error{ErrInvalidDescriptor}
}
func (e *InvalidDescriptorError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *InvalidDescriptorError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *InvalidDescriptorError) GoString() string               { return e.Error() }
func (e *InvalidDescriptorError) LogValue() slog.Value           { return logSafe(e.Error()) }

// InvalidGenerationError reports an invalid source generation.
type InvalidGenerationError struct{ reason reason }

func NewInvalidGenerationError(value string) *InvalidGenerationError {
	return &InvalidGenerationError{reason: normalizeReason(value)}
}

func (e *InvalidGenerationError) Error() string {
	if e == nil {
		return ErrInvalidGeneration.Error()
	}
	return "credentials: invalid generation (" + reasonString(e.reason) + ")"
}
func (e *InvalidGenerationError) Unwrap() error { return ErrInvalidGeneration }
func (e *InvalidGenerationError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *InvalidGenerationError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *InvalidGenerationError) GoString() string               { return e.Error() }
func (e *InvalidGenerationError) LogValue() slog.Value           { return logSafe(e.Error()) }

// InvalidFailureError reports an unknown or empty failure classification.
type InvalidFailureError struct{ reason reason }

func NewInvalidFailureError(value string) *InvalidFailureError {
	return &InvalidFailureError{reason: normalizeReason(value)}
}

func (e *InvalidFailureError) Error() string {
	if e == nil {
		return ErrInvalidFailure.Error()
	}
	return "credentials: invalid failure (" + reasonString(e.reason) + ")"
}
func (e *InvalidFailureError) Unwrap() error { return ErrInvalidFailure }
func (e *InvalidFailureError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *InvalidFailureError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *InvalidFailureError) GoString() string               { return e.Error() }
func (e *InvalidFailureError) LogValue() slog.Value           { return logSafe(e.Error()) }

// InvalidRecordError reports an invalid safe catalog record.
type InvalidRecordError struct{ reason reason }

func NewInvalidRecordError(value string) *InvalidRecordError {
	return &InvalidRecordError{reason: normalizeReason(value)}
}

func (e *InvalidRecordError) Error() string {
	if e == nil {
		return ErrInvalidRecord.Error()
	}
	return "credentials: invalid record (" + reasonString(e.reason) + ")"
}
func (e *InvalidRecordError) Unwrap() error { return ErrInvalidRecord }
func (e *InvalidRecordError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return reasonString(e.reason)
}
func (e *InvalidRecordError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *InvalidRecordError) GoString() string               { return e.Error() }
func (e *InvalidRecordError) LogValue() slog.Value           { return logSafe(e.Error()) }

// SourceClosedError reports an operation that began after Close linearized.
type SourceClosedError struct{}

func (e *SourceClosedError) Error() string                  { return ErrSourceClosed.Error() }
func (e *SourceClosedError) Unwrap() error                  { return ErrSourceClosed }
func (e *SourceClosedError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *SourceClosedError) GoString() string               { return e.Error() }
func (e *SourceClosedError) LogValue() slog.Value           { return logSafe(e.Error()) }

// CanceledError reports context cancellation without retaining the context or
// its implementation details.
type CanceledError struct{ cause error }

func (e *CanceledError) Error() string { return ErrCanceled.Error() }

func (e *CanceledError) Unwrap() error {
	if e == nil {
		return context.Canceled
	}
	cause := e.cause
	if cause == nil {
		cause = context.Canceled
	}
	return cause
}
func (e *CanceledError) Is(target error) bool {
	if target == ErrCanceled {
		return true
	}
	return errors.Is(e.Unwrap(), target)
}
func (e *CanceledError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *CanceledError) GoString() string               { return e.Error() }
func (e *CanceledError) LogValue() slog.Value           { return logSafe(e.Error()) }

// NewCanceledError returns a closed cancellation classification retaining
// only whether the operation was canceled or exceeded its deadline.
func NewCanceledError(cause error) *CanceledError {
	return &CanceledError{cause: cancellationCause(cause)}
}

// canceledBoundaryError adds a package-owned classification to a context
// cancellation without retaining an arbitrary dependency error.
type canceledBoundaryError struct {
	base  error
	cause error
}

func (e *canceledBoundaryError) Error() string {
	if e == nil || e.base == nil {
		return ErrCanceled.Error()
	}
	return e.base.Error()
}

func (e *canceledBoundaryError) Unwrap() error {
	if e == nil {
		return ErrCanceled
	}
	base := e.base
	if base == nil {
		base = ErrCanceled
	}
	return base
}

func (e *canceledBoundaryError) Is(target error) bool {
	if target == ErrCanceled {
		return true
	}
	if e == nil {
		return target == context.Canceled
	}
	return errors.Is(e.cause, target) || errors.Is(e.base, target)
}

func (e *canceledBoundaryError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *canceledBoundaryError) GoString() string               { return e.Error() }
func (e *canceledBoundaryError) LogValue() slog.Value           { return logSafe(e.Error()) }

func newCanceledBoundaryError(base, cause error) error {
	return &canceledBoundaryError{base: base, cause: cancellationCause(cause)}
}

func cancellationCause(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

func isCancellation(err error) bool {
	return errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// NilContextError reports a nil context passed to a public operation.
type NilContextError struct{}

func (e *NilContextError) Error() string                  { return ErrNilContext.Error() }
func (e *NilContextError) Unwrap() error                  { return ErrNilContext }
func (e *NilContextError) Format(state fmt.State, _ rune) { formatSafe(state, e.Error()) }
func (e *NilContextError) GoString() string               { return e.Error() }
func (e *NilContextError) LogValue() slog.Value           { return logSafe(e.Error()) }

func contextError(ctx context.Context) error {
	if ctx == nil {
		return &NilContextError{}
	}
	if err := ctx.Err(); err != nil {
		return NewCanceledError(err)
	}
	return nil
}
