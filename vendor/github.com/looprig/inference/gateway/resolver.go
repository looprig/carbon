package gateway

import (
	"context"

	"github.com/looprig/inference/model"
)

// Resolver maps an ingress API dialect and the harness-requested model
// alias -- untrusted, caller-supplied input -- to a fully bound Target.
//
// Implementations must not perform unbounded work: Resolve is called on the
// request path. The built-in implementation is Mux, which performs a bounded
// set of map lookups with no fuzzy, prefix, glob, or provider-name matching.
// A resolver that needs that kind of matching implements Resolver directly.
type Resolver interface {
	Resolve(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error)
}

// ExactResolver is the optional exact-registration contract consumed by
// Strict. ResolveExact must return a Target only for the exact (ingress,
// requestedModel) pair it registered, without applying wildcard or fallback
// behavior. It must return an *UnknownRouteError for an unregistered pair.
// Custom resolvers can implement this contract alongside Resolver to opt into
// strict resolution.
type ExactResolver interface {
	ResolveExact(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error)
}
