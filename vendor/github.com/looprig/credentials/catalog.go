package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Catalog is the safe, secret-free index of configured credential identities.
// Implementations must validate the complete on-disk/in-memory collection
// before returning a result: a duplicate reference or unknown schema is a
// fail-closed error, never a best-effort partial result.
type Catalog interface {
	Get(context.Context, Reference) (Record, error)
	List(context.Context) ([]Record, error)
	Create(context.Context, Record) error
	Delete(context.Context, Reference) error
}

// CatalogCAS is an optional capability for explicit reauthentication of an
// existing reference. Implementations must compare the complete expected
// record before replacing it; callers must never use it as an unconditional
// update primitive.
type CatalogCAS interface {
	Catalog
	Update(context.Context, Record, Record) error
}

var (
	ErrCatalogCorrupt           = errors.New("credentials: corrupt catalog")
	ErrCatalogConflict          = errors.New("credentials: catalog conflict")
	ErrCatalogNotFound          = errors.New("credentials: catalog record not found")
	ErrCatalogUnavailable       = errors.New("credentials: catalog unavailable")
	ErrCatalogCanceled          = errors.New("credentials: catalog operation canceled")
	ErrCatalogDurabilityUnknown = errors.New("credentials: catalog visible commit durability unknown")
	ErrCatalogUnsupported       = errors.New("credentials: catalog unsupported platform")
	ErrCatalogInvalidRecord     = errors.New("credentials: invalid catalog record")
	ErrCatalogInvalidDependency = errors.New("credentials: invalid catalog dependency")
	// InsecurePath is an alias of corruption for callers that distinguish the
	// filesystem boundary while retaining one fail-closed category.
	ErrCatalogInsecurePath             = ErrCatalogCorrupt
	ErrCatalogUnsupportedPlatform      = ErrCatalogUnsupported
	ErrCatalogVisibleDurabilityUnknown = ErrCatalogDurabilityUnknown
	ErrCatalogUnknownSchema            = ErrCatalogCorrupt
	ErrCatalogDuplicate                = ErrCatalogConflict
)

// CatalogError contains only package-owned classification and, where
// applicable, the already-validated safe credential reference.
type CatalogError struct {
	kind      catalogErrorKind
	reference Reference
	cause     error
}

type catalogErrorKind uint8

const (
	catalogErrorInvalid catalogErrorKind = iota
	catalogErrorCorrupt
	catalogErrorConflict
	catalogErrorNotFound
	catalogErrorUnavailable
	catalogErrorCanceled
	catalogErrorDurability
	catalogErrorUnsupported
	catalogErrorRecord
)

func newCatalogError(kind catalogErrorKind, ref Reference) *CatalogError {
	return &CatalogError{kind: kind, reference: ref}
}

func newCatalogCanceledError(cause error) *CatalogError {
	return &CatalogError{kind: catalogErrorCanceled, cause: cancellationCause(cause)}
}

func (e *CatalogError) Error() string {
	if e == nil {
		return ErrCatalogUnavailable.Error()
	}
	var base error
	switch e.kind {
	case catalogErrorCorrupt:
		base = ErrCatalogCorrupt
	case catalogErrorConflict:
		base = ErrCatalogConflict
	case catalogErrorNotFound:
		base = ErrCatalogNotFound
	case catalogErrorCanceled:
		base = ErrCatalogCanceled
	case catalogErrorDurability:
		base = ErrCatalogDurabilityUnknown
	case catalogErrorUnsupported:
		base = ErrCatalogUnsupported
	case catalogErrorRecord:
		base = ErrCatalogInvalidRecord
	default:
		base = ErrCatalogUnavailable
	}
	if e.reference.IsZero() || (e.kind != catalogErrorNotFound && e.kind != catalogErrorConflict) {
		return base.Error()
	}
	return base.Error() + ": " + e.reference.String()
}

func (e *CatalogError) Unwrap() error {
	if e == nil {
		return ErrCatalogUnavailable
	}
	var base error
	switch e.kind {
	case catalogErrorCorrupt:
		base = ErrCatalogCorrupt
	case catalogErrorConflict:
		base = ErrCatalogConflict
	case catalogErrorNotFound:
		base = ErrCatalogNotFound
	case catalogErrorCanceled:
		return ErrCatalogCanceled
	case catalogErrorDurability:
		base = ErrCatalogDurabilityUnknown
	case catalogErrorUnsupported:
		base = ErrCatalogUnsupported
	case catalogErrorRecord:
		base = ErrCatalogInvalidRecord
	default:
		base = ErrCatalogUnavailable
	}
	return base
}

func (e *CatalogError) Is(target error) bool {
	if errors.Is(e.Unwrap(), target) {
		return true
	}
	if e != nil && e.kind == catalogErrorCanceled {
		if target == ErrCanceled {
			return true
		}
		cause := e.cause
		if cause == nil {
			cause = context.Canceled
		}
		if errors.Is(cause, target) {
			return true
		}
	}
	return false
}

func (e *CatalogError) Reference() Reference {
	if e == nil {
		return Reference{}
	}
	return e.reference
}

func (e *CatalogError) Visible() bool { return e != nil && e.kind == catalogErrorDurability }

func (e *CatalogError) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(e.Error())) }
func (e *CatalogError) GoString() string               { return e.Error() }
func (e *CatalogError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

// CatalogDurabilityUnknownError reports a visible catalog mutation whose
// directory durability could not be confirmed. Callers must reread and adopt
// the visible result; they must not assume the previous catalog survived.
type CatalogDurabilityUnknownError = CatalogError

func catalogContextError(ctx context.Context) error {
	if ctx == nil {
		return newCatalogError(catalogErrorCanceled, Reference{})
	}
	if ctx.Err() != nil {
		return newCatalogError(catalogErrorCanceled, Reference{})
	}
	return nil
}

func validateCatalogRecord(record Record) error {
	if err := record.Validate(); err != nil {
		return newCatalogError(catalogErrorRecord, record.Reference)
	}
	return nil
}

// ValidateRecord is the public safe-record validation seam used by catalog
// implementations. Record validation never inspects or resolves secret state.
func ValidateRecord(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.CreatedAt.Location() == nil || record.UpdatedAt.Location() == nil {
		return ErrCatalogInvalidRecord
	}
	if record.UpdatedAt.Before(record.CreatedAt) {
		return ErrCatalogInvalidRecord
	}
	return nil
}

// CatalogTimeValid is kept small and explicit for callers constructing
// records without depending on a particular clock implementation.
func CatalogTimeValid(value time.Time) bool { return !value.IsZero() }

// NewCatalogNotFoundError returns a bounded not-found error for a validated
// reference. It is primarily useful to catalog implementations in subpackages.
func NewCatalogNotFoundError(ref Reference) error {
	return newCatalogError(catalogErrorNotFound, ref)
}

// NewCatalogConflictError returns a bounded compare-and-swap/duplicate error.
func NewCatalogConflictError(ref Reference) error {
	return newCatalogError(catalogErrorConflict, ref)
}

// NewCatalogDurabilityUnknownError returns a visible-commit durability error.
func NewCatalogDurabilityUnknownError(ref Reference) error {
	return newCatalogError(catalogErrorDurability, ref)
}

func NewCatalogCorruptError() error     { return newCatalogError(catalogErrorCorrupt, Reference{}) }
func NewCatalogUnavailableError() error { return newCatalogError(catalogErrorUnavailable, Reference{}) }
func NewCatalogCanceledError(cause ...error) error {
	if len(cause) > 0 {
		return newCatalogCanceledError(cause[0])
	}
	return newCatalogCanceledError(context.Canceled)
}
func NewCatalogUnsupportedError() error { return newCatalogError(catalogErrorUnsupported, Reference{}) }
