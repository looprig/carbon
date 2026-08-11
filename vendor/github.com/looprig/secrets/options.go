package secrets

// Precondition selects the mutation condition for Put and Delete.
type Precondition uint8

const (
	PreconditionUnconditional Precondition = iota
	PreconditionCreateOnly
	PreconditionCompareAndSwap
)

// PutOptions carries one explicit mutation precondition. The zero value is
// unconditional. ExpectedVersion is required only for compare-and-swap.
type PutOptions struct {
	Precondition    Precondition
	ExpectedVersion Version
}

// UnconditionalPut returns options for an unconditional mutation.
func UnconditionalPut() PutOptions { return PutOptions{} }

// CreateOnlyPut returns options that succeed only when the reference is
// absent.
func CreateOnlyPut() PutOptions { return PutOptions{Precondition: PreconditionCreateOnly} }

// CompareAndSwapPut returns options requiring the supplied existing version.
func CompareAndSwapPut(version Version) PutOptions {
	return PutOptions{Precondition: PreconditionCompareAndSwap, ExpectedVersion: version}
}

// Validate checks that PutOptions describe exactly one supported condition.
func (o PutOptions) Validate() error {
	switch o.Precondition {
	case PreconditionUnconditional, PreconditionCreateOnly:
		if !o.ExpectedVersion.IsZero() {
			return NewInvalidOptionsError("unexpected version")
		}
	case PreconditionCompareAndSwap:
		if o.ExpectedVersion.IsZero() || !o.ExpectedVersion.Valid() || o.ExpectedVersion.IsUnsupported() {
			return NewInvalidOptionsError("compare-and-swap version")
		}
	default:
		return NewInvalidOptionsError("precondition")
	}
	return nil
}

// DeleteOptions carries an unconditional or compare-and-swap condition. The
// zero value is unconditional and deletion of an absent reference is
// idempotent.
type DeleteOptions struct {
	Precondition    Precondition
	ExpectedVersion Version
}

// UnconditionalDelete returns options for an unconditional delete.
func UnconditionalDelete() DeleteOptions { return DeleteOptions{} }

// CompareAndSwapDelete returns options requiring the supplied existing
// version.
func CompareAndSwapDelete(version Version) DeleteOptions {
	return DeleteOptions{Precondition: PreconditionCompareAndSwap, ExpectedVersion: version}
}

// Validate checks DeleteOptions.
func (o DeleteOptions) Validate() error {
	switch o.Precondition {
	case PreconditionUnconditional:
		if !o.ExpectedVersion.IsZero() {
			return NewInvalidOptionsError("unexpected version")
		}
	case PreconditionCompareAndSwap:
		if o.ExpectedVersion.IsZero() || !o.ExpectedVersion.Valid() || o.ExpectedVersion.IsUnsupported() {
			return NewInvalidOptionsError("compare-and-swap version")
		}
	default:
		return NewInvalidOptionsError("precondition")
	}
	return nil
}
