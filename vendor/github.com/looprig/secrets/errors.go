package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var (
	ErrInvalidVersion        = errors.New("secrets: invalid version")
	ErrInvalidOptions        = errors.New("secrets: invalid options")
	ErrInvalidPageToken      = errors.New("secrets: invalid page token")
	ErrNotFound              = errors.New("secrets: secret not found")
	ErrUnsupportedScheme     = errors.New("secrets: unsupported scheme")
	ErrUnsupportedCapability = errors.New("secrets: unsupported capability")
	ErrInsecurePath          = errors.New("secrets: insecure path")
	ErrCorruptRecord         = errors.New("secrets: corrupt record")
	ErrConflict              = errors.New("secrets: version conflict")
	ErrUnavailable           = errors.New("secrets: backend unavailable")
	ErrCanceled              = errors.New("secrets: operation canceled")
)

// errorReason is a closed vocabulary used by public errors. Keeping reason
// state as an enum prevents malformed input or a provider response from being
// retained in an error value and later exposed by %#v.
type errorReason uint8

const (
	errorReasonInvalid errorReason = iota
	errorReasonLength
	errorReasonEncoding
	errorReasonScheme
	errorReasonEndpointScheme
	errorReasonPath
	errorReasonPathEncoding
	errorReasonPathBoundary
	errorReasonPathSeparator
	errorReasonPathTraversal
	errorReasonPathCharacter
	errorReasonPrefix
	errorReasonZero
	errorReasonNilReceiver
	errorReasonRecordReference
	errorReasonMetadataReference
	errorReasonRecordVersion
	errorReasonMetadataVersion
	errorReasonReserved
	errorReasonVersion
	errorReasonToken
	errorReasonUnexpectedVersion
	errorReasonCompareAndSwapVersion
	errorReasonPrecondition
	errorReasonCharacter
	errorReasonPageSize
	errorReasonPageLimit
	errorReasonPageToken
)

func normalizeErrorReason(reason string) errorReason {
	switch reason {
	case "length":
		return errorReasonLength
	case "encoding":
		return errorReasonEncoding
	case "scheme":
		return errorReasonScheme
	case "endpoint scheme":
		return errorReasonEndpointScheme
	case "path":
		return errorReasonPath
	case "path encoding":
		return errorReasonPathEncoding
	case "path boundary":
		return errorReasonPathBoundary
	case "path separator":
		return errorReasonPathSeparator
	case "path traversal":
		return errorReasonPathTraversal
	case "path character":
		return errorReasonPathCharacter
	case "prefix":
		return errorReasonPrefix
	case "zero":
		return errorReasonZero
	case "nil receiver":
		return errorReasonNilReceiver
	case "record reference":
		return errorReasonRecordReference
	case "metadata reference":
		return errorReasonMetadataReference
	case "record version":
		return errorReasonRecordVersion
	case "metadata version":
		return errorReasonMetadataVersion
	case "reserved":
		return errorReasonReserved
	case "version":
		return errorReasonVersion
	case "token":
		return errorReasonToken
	case "unexpected version":
		return errorReasonUnexpectedVersion
	case "compare-and-swap version":
		return errorReasonCompareAndSwapVersion
	case "precondition":
		return errorReasonPrecondition
	case "character":
		return errorReasonCharacter
	case "page size":
		return errorReasonPageSize
	case "page limit":
		return errorReasonPageLimit
	case "page token":
		return errorReasonPageToken
	default:
		return errorReasonInvalid
	}
}

func errorReasonText(reason errorReason) string {
	switch reason {
	case errorReasonLength:
		return "length"
	case errorReasonEncoding:
		return "encoding"
	case errorReasonScheme:
		return "scheme"
	case errorReasonEndpointScheme:
		return "endpoint scheme"
	case errorReasonPath:
		return "path"
	case errorReasonPathEncoding:
		return "path encoding"
	case errorReasonPathBoundary:
		return "path boundary"
	case errorReasonPathSeparator:
		return "path separator"
	case errorReasonPathTraversal:
		return "path traversal"
	case errorReasonPathCharacter:
		return "path character"
	case errorReasonPrefix:
		return "prefix"
	case errorReasonZero:
		return "zero"
	case errorReasonNilReceiver:
		return "nil receiver"
	case errorReasonRecordReference:
		return "record reference"
	case errorReasonMetadataReference:
		return "metadata reference"
	case errorReasonRecordVersion:
		return "record version"
	case errorReasonMetadataVersion:
		return "metadata version"
	case errorReasonReserved:
		return "reserved"
	case errorReasonVersion:
		return "version"
	case errorReasonToken:
		return "token"
	case errorReasonUnexpectedVersion:
		return "unexpected version"
	case errorReasonCompareAndSwapVersion:
		return "compare-and-swap version"
	case errorReasonPrecondition:
		return "precondition"
	case errorReasonCharacter:
		return "character"
	case errorReasonPageSize:
		return "page size"
	case errorReasonPageLimit:
		return "page limit"
	case errorReasonPageToken:
		return "page token"
	default:
		return "invalid"
	}
}

// formatSafeError deliberately ignores formatting flags and writes only the
// already-normalized Error string. This keeps every public error's ordinary,
// Go-syntax, and %+v forms free of private payloads.
func formatSafeError(state fmt.State, message string) {
	_, _ = state.Write([]byte(message))
}

// InvalidVersionError reports an empty, oversized, or unsafe version without
// retaining the caller's raw value.
type InvalidVersionError struct{ reason errorReason }

// NewInvalidVersionError constructs a bounded invalid-version error.
func NewInvalidVersionError(reason string) *InvalidVersionError {
	return &InvalidVersionError{reason: normalizeErrorReason(reason)}
}

func (e *InvalidVersionError) Error() string {
	if e == nil {
		return ErrInvalidVersion.Error()
	}
	return "secrets: invalid version (" + errorReasonText(e.reason) + ")"
}

func (e *InvalidVersionError) Unwrap() error { return ErrInvalidVersion }

// Reason returns a normalized, secret-free reason label.
func (e *InvalidVersionError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InvalidVersionError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InvalidVersionError) GoString() string { return e.Error() }

func (e *InvalidVersionError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// InvalidPageTokenError reports malformed or oversized token text without
// retaining raw input.
type InvalidPageTokenError struct{ reason errorReason }

// NewInvalidPageTokenError constructs a bounded invalid-page-token error.
func NewInvalidPageTokenError(reason string) *InvalidPageTokenError {
	return &InvalidPageTokenError{reason: normalizeErrorReason(reason)}
}

func (e *InvalidPageTokenError) Error() string {
	if e == nil {
		return ErrInvalidPageToken.Error()
	}
	return "secrets: invalid page token (" + errorReasonText(e.reason) + ")"
}

func (e *InvalidPageTokenError) Unwrap() error { return ErrInvalidPageToken }

// Reason returns a normalized, secret-free reason label.
func (e *InvalidPageTokenError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InvalidPageTokenError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InvalidPageTokenError) GoString() string { return e.Error() }

func (e *InvalidPageTokenError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// InvalidOptionsError reports malformed mutation or pagination options.
type InvalidOptionsError struct{ reason errorReason }

// NewInvalidOptionsError constructs a bounded invalid-options error.
func NewInvalidOptionsError(reason string) *InvalidOptionsError {
	return &InvalidOptionsError{reason: normalizeErrorReason(reason)}
}

func (e *InvalidOptionsError) Error() string {
	if e == nil {
		return ErrInvalidOptions.Error()
	}
	return "secrets: invalid options (" + errorReasonText(e.reason) + ")"
}

func (e *InvalidOptionsError) Unwrap() error { return ErrInvalidOptions }

// Reason returns a normalized, secret-free reason label.
func (e *InvalidOptionsError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InvalidOptionsError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InvalidOptionsError) GoString() string { return e.Error() }

func (e *InvalidOptionsError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// NotFoundError reports an absent safe reference.
type NotFoundError struct{ reference Reference }

// NewNotFoundError constructs an error carrying only the already-validated
// reference.
func NewNotFoundError(reference Reference) *NotFoundError {
	return &NotFoundError{reference: reference}
}

func (e *NotFoundError) Error() string {
	if e == nil || e.reference.IsZero() {
		return ErrNotFound.Error()
	}
	return "secrets: secret not found: " + e.reference.String()
}

func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// Reference returns the safe reference associated with the error.
func (e *NotFoundError) Reference() Reference {
	if e == nil {
		return Reference{}
	}
	return e.reference
}

func (e *NotFoundError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *NotFoundError) GoString() string { return e.Error() }

func (e *NotFoundError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// UnsupportedSchemeError reports a backend scheme for which an implementation
// has no resolver. Scheme detail is deliberately not retained: even a
// syntactically valid scheme can be caller-controlled secret-bearing input.
type UnsupportedSchemeError struct{}

// NewUnsupportedSchemeError constructs a bounded unsupported-scheme error.
func NewUnsupportedSchemeError() *UnsupportedSchemeError {
	return &UnsupportedSchemeError{}
}

func (e *UnsupportedSchemeError) Error() string { return ErrUnsupportedScheme.Error() }

func (e *UnsupportedSchemeError) Unwrap() error { return ErrUnsupportedScheme }

func (e *UnsupportedSchemeError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *UnsupportedSchemeError) GoString() string { return e.Error() }

func (e *UnsupportedSchemeError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// UnsupportedCapabilityError reports a requested capability that the backend
// cannot provide safely. Capability detail is intentionally not retained.
type UnsupportedCapabilityError struct{}

// NewUnsupportedCapabilityError constructs a bounded unsupported-capability
// error.
func NewUnsupportedCapabilityError() *UnsupportedCapabilityError {
	return &UnsupportedCapabilityError{}
}

func (e *UnsupportedCapabilityError) Error() string { return ErrUnsupportedCapability.Error() }

func (e *UnsupportedCapabilityError) Unwrap() error { return ErrUnsupportedCapability }

func (e *UnsupportedCapabilityError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *UnsupportedCapabilityError) GoString() string { return e.Error() }

func (e *UnsupportedCapabilityError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// InsecurePathError reports an unsafe path or permission boundary.
type InsecurePathError struct{ reason errorReason }

// NewInsecurePathError constructs a bounded insecure-path error.
func NewInsecurePathError(reason string) *InsecurePathError {
	return &InsecurePathError{reason: normalizeErrorReason(reason)}
}

func (e *InsecurePathError) Error() string {
	if e == nil {
		return ErrInsecurePath.Error()
	}
	return "secrets: insecure path (" + errorReasonText(e.reason) + ")"
}

func (e *InsecurePathError) Unwrap() error { return ErrInsecurePath }

// Reason returns a normalized, secret-free reason label.
func (e *InsecurePathError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InsecurePathError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InsecurePathError) GoString() string { return e.Error() }

func (e *InsecurePathError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// CorruptRecordError reports a record that cannot be safely decoded.
type CorruptRecordError struct{ reference Reference }

// NewCorruptRecordError constructs an error carrying only the safe reference.
// Backend detail is intentionally not accepted or retained.
func NewCorruptRecordError(reference Reference) *CorruptRecordError {
	return &CorruptRecordError{reference: reference}
}

func (e *CorruptRecordError) Error() string {
	if e == nil || e.reference.IsZero() {
		return ErrCorruptRecord.Error()
	}
	return "secrets: corrupt record " + e.reference.String()
}

func (e *CorruptRecordError) Unwrap() error { return ErrCorruptRecord }

// Reference returns the safe reference associated with the error.
func (e *CorruptRecordError) Reference() Reference {
	if e == nil {
		return Reference{}
	}
	return e.reference
}

func (e *CorruptRecordError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *CorruptRecordError) GoString() string { return e.Error() }

func (e *CorruptRecordError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// ConflictError reports a failed compare-and-swap precondition. Version
// details are intentionally not retained in the public error; callers can
// resolve the record again if they need fresh coordination state.
type ConflictError struct{ reference Reference }

// NewConflictError constructs an error carrying only the safe reference.
func NewConflictError(reference Reference) *ConflictError {
	return &ConflictError{reference: reference}
}

func (e *ConflictError) Error() string {
	if e == nil || e.reference.IsZero() {
		return ErrConflict.Error()
	}
	return "secrets: version conflict for " + e.reference.String()
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Reference returns the safe reference associated with the error.
func (e *ConflictError) Reference() Reference {
	if e == nil {
		return Reference{}
	}
	return e.reference
}

func (e *ConflictError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *ConflictError) GoString() string { return e.Error() }

func (e *ConflictError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// VersionMismatchError is a descriptive alias for ConflictError.
type VersionMismatchError = ConflictError

// UnavailableError reports an unavailable backend. Operation is normalized to
// a closed vocabulary and arbitrary causes are intentionally not accepted or
// retained.
type UnavailableError struct {
	operation errorOperation
	reference Reference
}

// NewUnavailableError constructs a bounded unavailable-backend error.
func NewUnavailableError(operation string, reference Reference) *UnavailableError {
	return &UnavailableError{operation: normalizeErrorOperation(operation), reference: reference}
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return ErrUnavailable.Error()
	}
	operation := errorOperationText(e.operation)
	if e.reference.IsZero() {
		return "secrets: backend unavailable during " + operation
	}
	return "secrets: backend unavailable during " + operation + " for " + e.reference.String()
}

func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Reference returns the safe reference associated with the error.
func (e *UnavailableError) Reference() Reference {
	if e == nil {
		return Reference{}
	}
	return e.reference
}

func (e *UnavailableError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *UnavailableError) GoString() string { return e.Error() }

func (e *UnavailableError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

type errorOperation uint8

const (
	errorOperationUnknown errorOperation = iota
	errorOperationResolve
	errorOperationPut
	errorOperationDelete
	errorOperationList
	errorOperationRead
	errorOperationWrite
)

func normalizeErrorOperation(operation string) errorOperation {
	switch operation {
	case "resolve":
		return errorOperationResolve
	case "put":
		return errorOperationPut
	case "delete":
		return errorOperationDelete
	case "list":
		return errorOperationList
	case "read":
		return errorOperationRead
	case "write":
		return errorOperationWrite
	default:
		return errorOperationUnknown
	}
}

func errorOperationText(operation errorOperation) string {
	switch operation {
	case errorOperationResolve:
		return "resolve"
	case errorOperationPut:
		return "put"
	case errorOperationDelete:
		return "delete"
	case errorOperationList:
		return "list"
	case errorOperationRead:
		return "read"
	case errorOperationWrite:
		return "write"
	default:
		return "operation"
	}
}

type cancellationKind uint8

const (
	cancellationGeneric cancellationKind = iota
	cancellationCanceled
	cancellationDeadline
)

// CanceledError reports cancellation while preserving errors.Is only for the
// normalized context cancellation sentinels, without retaining arbitrary
// wrapped text.
type CanceledError struct {
	operation errorOperation
	kind      cancellationKind
}

// NewCanceledError constructs a bounded cancellation error. Only the two
// standard context sentinels are retained as a normalized kind.
func NewCanceledError(operation string, cause error) *CanceledError {
	kind := cancellationGeneric
	if errors.Is(cause, context.Canceled) {
		kind = cancellationCanceled
	} else if errors.Is(cause, context.DeadlineExceeded) {
		kind = cancellationDeadline
	}
	return &CanceledError{operation: normalizeErrorOperation(operation), kind: kind}
}

func (e *CanceledError) Error() string {
	if e == nil {
		return ErrCanceled.Error()
	}
	return "secrets: operation canceled during " + errorOperationText(e.operation)
}

func (e *CanceledError) Is(target error) bool {
	if target == ErrCanceled {
		return true
	}
	if e == nil {
		return false
	}
	switch target {
	case context.Canceled:
		return e.kind == cancellationCanceled
	case context.DeadlineExceeded:
		return e.kind == cancellationDeadline
	default:
		return false
	}
}

func (e *CanceledError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *CanceledError) GoString() string { return e.Error() }

func (e *CanceledError) LogValue() slog.Value { return slog.StringValue(e.Error()) }
