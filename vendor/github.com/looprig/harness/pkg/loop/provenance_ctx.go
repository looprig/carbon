package loop

import "context"

// provenanceKey is the unexported ctx-key type for the current step's Provenance.
// A distinct zero-size struct so the value never collides across packages (the
// idiomatic Go ctx-key pattern) and cannot be constructed by an outside package.
type provenanceKey struct{}

// WithProvenance returns a child ctx carrying the current loop/turn/step
// coordinates, so a tool (e.g. an agent collaboration tool) can learn its OWN provenance and
// pass it as the `parent` when spawning a sub-loop. The loop injects it at the
// tool-batch boundary, where all three ids are unambiguously the running step's.
func WithProvenance(ctx context.Context, p Provenance) context.Context {
	return context.WithValue(ctx, provenanceKey{}, p)
}

// ProvenanceFrom returns the Provenance set by WithProvenance, and whether it was
// present. An absent key yields the zero Provenance and false — fail-safe: a tool
// run outside a turn (no provenance injected) treats it as root/unknown rather than
// panicking.
func ProvenanceFrom(ctx context.Context) (Provenance, bool) {
	p, ok := ctx.Value(provenanceKey{}).(Provenance)
	return p, ok
}
