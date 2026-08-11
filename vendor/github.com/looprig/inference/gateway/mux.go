package gateway

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"unicode/utf8"

	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

// RouteKey identifies one exact route: the ingress API dialect and the
// harness-requested model alias, matched verbatim (case-sensitive, no
// normalization). An empty Model is an ordinary, unreserved alias like any
// other: it matches only a request that supplies an empty requested-model
// string for that ingress format. It carries no implicit "format default"
// meaning -- use FormatDefaults for that, and note that both may be
// configured for the same ingress format at once without conflict (an exact
// "" route and a FormatDefaults entry answer different requests: literally
// empty vs. anything else unmatched).
type RouteKey struct {
	Ingress model.APIFormat
	Model   string
}

// Mux is an immutable Resolver keyed by ingress API format and the
// harness-requested model alias, with a per-format default and a single
// ultimate default target. The zero value is not usable directly as a
// Resolver in production; construct with NewMux, which validates and
// defensively copies its input.
//
// Resolution precedence, applied by Resolve:
//
//  1. an exact match in Routes for (ingress, requestedModel);
//  2. FormatDefaults[ingress], if present;
//  3. Default, if non-nil;
//  4. otherwise, a *RouteNotFoundError.
//
// There is deliberately no fuzzy, prefix, glob, or provider-name matching.
type Mux struct {
	Routes         map[RouteKey]Target
	FormatDefaults map[model.APIFormat]Target
	Default        *Target
}

// NewMux validates and defensively copies cfg's Routes, FormatDefaults, and
// Default into a new, independent *Mux. After NewMux returns, mutating
// cfg.Routes, cfg.FormatDefaults, cfg.Default (the pointer, or the Target it
// points to), or any Target.Model reachable from those inputs has no effect
// on the returned Mux's resolution behavior. Every Target returned by the
// resulting Mux's Resolve is likewise independent of the Mux's internal
// state: mutating a returned Target (including its Model.Sampling
// pointer/slice fields) never corrupts a subsequent Resolve call.
//
// NewMux rejects, returning a *ConfigError:
//
//   - any Target (in Routes, FormatDefaults, or Default) with a nil Client;
//   - any Target whose Model fails model.Model.Validate (the *ConfigError
//     wraps the underlying *model.ValidationError, reachable via
//     errors.As/errors.Is);
//   - a duplicate logical route. Because Routes and FormatDefaults are Go
//     maps, a literal duplicate key cannot occur -- there is no way to
//     construct one. The real hazard at this API shape is Target.ID, a
//     stable diagnostic identity: if the same non-empty ID is bound, across
//     Routes/FormatDefaults/Default, to two Targets whose Model values are
//     not equal (compared by value, including Sampling's pointer/slice
//     fields), the ID would no longer identify one thing and NewMux rejects
//     the configuration. Reusing one ID for the literal same Model -- e.g.
//     one target reached through two different route keys -- is not a
//     conflict and is allowed. An empty ID asserts no diagnostic identity
//     and never participates in this check: two Targets with an empty ID
//     never collide with each other on that basis alone.
func NewMux(cfg Mux) (*Mux, error) {
	seenIDs := make(map[string]model.Model, len(cfg.Routes)+len(cfg.FormatDefaults)+1)

	checkAndClone := func(location string, t Target) (Target, error) {
		if t.Client == nil {
			return Target{}, &ConfigError{Location: location, Reason: "Client must not be nil"}
		}
		if err := t.Model.Validate(); err != nil {
			return Target{}, &ConfigError{Location: location, Reason: "invalid Model", Err: err}
		}
		if t.ID != "" {
			if existing, ok := seenIDs[t.ID]; ok {
				if !reflect.DeepEqual(existing, t.Model) {
					return Target{}, &ConfigError{
						Location: location,
						Reason:   fmt.Sprintf("duplicate Target.ID %q bound to conflicting Model values", t.ID),
					}
				}
			} else {
				seenIDs[t.ID] = t.Model
			}
		}
		t.Model = t.Model.Clone()
		return t, nil
	}

	routes := make(map[RouteKey]Target, len(cfg.Routes))
	for key, t := range cfg.Routes {
		stored, err := checkAndClone(fmt.Sprintf("Routes[%s/%s]", key.Ingress, key.Model), t)
		if err != nil {
			return nil, err
		}
		routes[key] = stored
	}

	formatDefaults := make(map[model.APIFormat]Target, len(cfg.FormatDefaults))
	for format, t := range cfg.FormatDefaults {
		stored, err := checkAndClone(fmt.Sprintf("FormatDefaults[%s]", format), t)
		if err != nil {
			return nil, err
		}
		formatDefaults[format] = stored
	}

	var def *Target
	if cfg.Default != nil {
		stored, err := checkAndClone("Default", *cfg.Default)
		if err != nil {
			return nil, err
		}
		def = &stored
	}

	return &Mux{Routes: routes, FormatDefaults: formatDefaults, Default: def}, nil
}

// Resolve implements Resolver, applying Mux's resolution precedence (exact
// route, then format default, then global default, then *RouteNotFoundError).
// The returned Target is independent of Mux's internal state: it is safe for
// the caller to mutate, including its Model.Sampling pointer/slice fields.
func (m *Mux) Resolve(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error) {
	if t, ok := m.resolveExact(ctx, ingress, requestedModel); ok {
		return t, nil
	}
	if t, ok := m.FormatDefaults[ingress]; ok {
		return cloneTarget(t), nil
	}
	if m.Default != nil {
		return cloneTarget(*m.Default), nil
	}
	return Target{}, &RouteNotFoundError{Ingress: ingress, Model: requestedModel}
}

// resolveExact returns only an exact Routes hit. It deliberately does not
// consult either default tier so Strict can reject unknown aliases.
func (m *Mux) resolveExact(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, bool) {
	if t, ok := m.Routes[RouteKey{Ingress: ingress, Model: requestedModel}]; ok {
		return cloneTarget(t), true
	}
	return Target{}, false
}

// ResolveExact implements ExactResolver. It consults only Mux.Routes and
// never falls back to FormatDefaults or Default.
func (m *Mux) ResolveExact(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error) {
	if t, ok := m.resolveExact(ctx, ingress, requestedModel); ok {
		return t, nil
	}
	return Target{}, &UnknownRouteError{Ingress: ingress, Alias: requestedModel}
}

// UnknownRouteError reports a strict-resolution miss. It includes only the
// untrusted request key; resolved targets and their provider/endpoint details
// are intentionally absent.
type UnknownRouteError struct {
	Ingress model.APIFormat
	Alias   string
}

func (e *UnknownRouteError) Error() string {
	return fmt.Sprintf("gateway: no route for ingress %s alias %s",
		boundedUnknownRouteErrorField(string(e.Ingress)),
		boundedUnknownRouteErrorField(e.Alias))
}

const maxUnknownRouteErrorFieldBytes = 96

// boundedUnknownRouteErrorField returns a quoted, bounded rendering for one
// untrusted UnknownRouteError field. It never changes the field stored on the
// error, which remains available through errors.As.
func boundedUnknownRouteErrorField(value string) string {
	if !utf8.ValidString(value) {
		return strconv.Quote("invalid-utf8")
	}

	const suffix = "..."
	end := len(value)
	truncated := false
	if end > maxUnknownRouteErrorFieldBytes {
		end = maxUnknownRouteErrorFieldBytes
		truncated = true
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
	}

	for end >= 0 {
		rendered := value[:end]
		if truncated {
			rendered += suffix
		}
		quoted := strconv.Quote(rendered)
		if len(quoted) <= maxUnknownRouteErrorFieldBytes {
			return quoted
		}
		if end == 0 {
			return strconv.Quote(suffix)
		}
		end--
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
		truncated = true
	}

	return strconv.Quote(suffix)
}

// Is lets callers match any UnknownRouteError with errors.Is while errors.As
// still exposes the request key fields.
func (e *UnknownRouteError) Is(target error) bool {
	_, ok := target.(*UnknownRouteError)
	return ok
}

type strictResolver struct {
	inner Resolver
}

// Strict wraps inner with exact-only resolution. Resolvers without an
// ExactResolver registration view cannot prove a request is registered, so
// strict resolution rejects them rather than permitting wildcard or fallback
// output.
func Strict(inner Resolver) Resolver {
	return &strictResolver{inner: inner}
}

func (s *strictResolver) Resolve(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error) {
	exact, ok := s.inner.(ExactResolver)
	if !ok {
		return Target{}, &UnknownRouteError{Ingress: ingress, Alias: requestedModel}
	}
	return exact.ResolveExact(ctx, ingress, requestedModel)
}

// cloneTarget returns a Target independent of t's Model.Sampling
// pointer/slice fields, so a caller holding the returned value cannot
// mutate the Mux's stored copy.
func cloneTarget(t Target) Target {
	t.Model = t.Model.Clone()
	return t
}

// FixedResolver is a Resolver that always resolves to the same Target,
// ignoring the requested ingress format and model alias entirely. Construct
// with Fixed. FixedFor additionally records an exact harness route for use by
// Strict; direct Resolve remains wildcard-compatible for both constructors.
type FixedResolver struct {
	target Target
	exact  *RouteKey
}

// Resolve implements Resolver. It ignores ingress and requestedModel and
// always succeeds with the target Fixed was constructed with.
func (f *FixedResolver) Resolve(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error) {
	return cloneTarget(f.target), nil
}

// ResolveExact implements ExactResolver for an explicitly registered FixedFor
// route. A plain Fixed has no harness route metadata and therefore fails closed
// under Strict instead of inferring one from the upstream Model.
func (f *FixedResolver) ResolveExact(ctx context.Context, ingress model.APIFormat, requestedModel string) (Target, error) {
	if f.exact == nil || f.exact.Ingress != ingress || f.exact.Model != requestedModel {
		return Target{}, &UnknownRouteError{Ingress: ingress, Alias: requestedModel}
	}
	return cloneTarget(f.target), nil
}

// Fixed returns a Resolver that ignores the requested model alias -- and the
// ingress format -- entirely and always routes to one fixed target built
// from client and m. Routing being trivial does not skip validation: client
// and m are validated exactly as NewMux validates every Target, returning a
// *ConfigError on a nil client or a Model that fails model.Model.Validate.
// Because Fixed has no harness registration metadata, Strict(Fixed(...))
// fails closed; use FixedFor when strict exact routing is required.
func Fixed(client inference.Client, m model.Model) (*FixedResolver, error) {
	if client == nil {
		return nil, &ConfigError{Location: "Fixed", Reason: "Client must not be nil"}
	}
	if err := m.Validate(); err != nil {
		return nil, &ConfigError{Location: "Fixed", Reason: "invalid Model", Err: err}
	}
	return &FixedResolver{target: Target{Client: client, Model: m.Clone()}}, nil
}

// FixedFor constructs a wildcard-compatible FixedResolver with one explicit
// harness registration for (ingress, alias). Strict uses that registration
// metadata; it does not infer the harness route from the target Model's
// provider-facing APIFormat or Name.
func FixedFor(client inference.Client, m model.Model, ingress model.APIFormat, alias string) (*FixedResolver, error) {
	resolver, err := Fixed(client, m)
	if err != nil {
		return nil, err
	}
	resolver.exact = &RouteKey{Ingress: ingress, Model: alias}
	return resolver, nil
}
