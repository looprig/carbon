// This file defines the errors this package reports.
//
// The taxonomy here is deliberately tiny, and it does not import pkg/client:
// like internal/lifecycle and internal/limits, this package states what went
// wrong in its own terms and leaves classification to the client, which is the
// only layer that owns a public error contract. The client maps a *DefectError
// onto its catalog-invalid class, and a *limits.OverLimitError (which discovery
// returns unchanged) onto its catalog-over-limit class.

package catalog

import "fmt"

// DefectError reports catalog data that cannot be made into a usable
// generation: a name that is not an identifier, an ambiguous duplicate, a
// server that broke a protocol invariant.
//
// It is distinct from a limits.OverLimitError, which reports a catalog that is
// well-formed but larger than this binding will accept. The difference matters
// to a caller: an over-limit catalog may become acceptable with a raised bound,
// while a defective one is broken whatever the bounds are.
type DefectError struct {
	// Family is the catalog family the defect was found in. It is the zero
	// value for a defect that belongs to no family, such as an empty binding.
	Family Family
	// Reason is a bounded description of the defect. It may quote a
	// server-supplied identifier, which validateRawName has already checked for
	// control characters, so it is safe to render.
	Reason string
}

// Error renders "catalog: <family>: <reason>", omitting the family when there
// is none.
func (e *DefectError) Error() string {
	if e.Family < FamilyTools || e.Family >= familySentinel {
		return fmt.Sprintf("catalog: %s", e.Reason)
	}
	return fmt.Sprintf("catalog: %s: %s", e.Family, e.Reason)
}
