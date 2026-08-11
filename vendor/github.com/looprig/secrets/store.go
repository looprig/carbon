package secrets

import (
	"context"
	"errors"
)

// Resolver is the read-only secret resolution contract.
type Resolver interface {
	Resolve(context.Context, Reference) (Record, error)
}

// Store is a mutable Resolver. Implementations must reject zero Secrets and
// enforce the requested precondition rather than silently weakening it.
type Store interface {
	Resolver
	Put(context.Context, Reference, Secret, PutOptions) (Record, error)
	Delete(context.Context, Reference, DeleteOptions) (DeleteResult, error)
}

// Lister is an optional metadata-only capability. Implementations must honor
// the caller's bounded limit and namespace exactly; values are never listed.
type Lister interface {
	List(context.Context, Namespace, PageToken, int) (Page[Metadata], error)
}

// PreconditionCapabilities reports affirmative support for conditional
// mutations. A Store that does not support a requested precondition must
// return an UnsupportedCapabilityError instead of treating it as
// unconditional.
type PreconditionCapabilities interface {
	SupportsCreateOnly() bool
	SupportsCompareAndSwap() bool
}

// VisibleCommitError classifies a mutation error whose linearization point
// has already passed. Implementations must keep any additional error detail
// bounded and safe; callers use the returned mutation result as authoritative
// and must not assume the previous state survived.
type VisibleCommitError interface {
	error
	Visible() bool
}

// IsVisibleCommit reports whether err contains a visible-commit classification
// without requiring consumers to import a backend-specific error package.
func IsVisibleCommit(err error) bool {
	if err == nil {
		return false
	}
	var visible VisibleCommitError
	return errors.As(err, &visible) && visible.Visible()
}
