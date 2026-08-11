package credentials

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/looprig/secrets"
)

// SharingScope describes the widest visibility for state coordination. The
// values are ordered so a coordinator can be checked against the configured
// state store without relying on provider-specific knowledge.
type SharingScope uint8

const (
	SharingProcess SharingScope = iota + 1
	SharingHost
	SharingDistributed
)

// Descriptive aliases retain obvious names at call sites.
const (
	ScopeProcess            = SharingProcess
	ScopeHost               = SharingHost
	ScopeDistributed        = SharingDistributed
	SharingScopeProcess     = SharingProcess
	SharingScopeHost        = SharingHost
	SharingScopeDistributed = SharingDistributed
)

func (s SharingScope) Valid() bool { return s >= SharingProcess && s <= SharingDistributed }
func (s SharingScope) AtLeast(required SharingScope) bool {
	return s.Valid() && required.Valid() && s >= required
}
func (s SharingScope) String() string {
	switch s {
	case SharingProcess:
		return "process"
	case SharingHost:
		return "host"
	case SharingDistributed:
		return "distributed"
	default:
		return "invalid"
	}
}

// RefreshCoordinator provides context-aware exclusion around one refresh or
// state rotation. Implementations own any lock lifetime and must invoke fn
// while exclusion is held.
type RefreshCoordinator interface {
	Scope() SharingScope
	WithLock(context.Context, Reference, func(context.Context) error) error
}

// Clock is an injected time source for provider factories. Builder itself does
// not use ambient time; the interface exists to keep construction explicit.
type Clock interface{ Now() time.Time }

type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

// CallbackListener is an intentionally opaque seam owned by provider OAuth
// implementations. The base builder does not invoke it or infer a browser.
type CallbackListener interface{}

// DescriptorBinding is the complete authority binding used for factory
// selection. Label is deliberately omitted because it is presentation only.
type DescriptorBinding struct {
	Provider  string
	Transport string
	Scheme    Scheme
	Usage     UsageClass
	Issuer    string
	Audience  string
}

func DescriptorBindingOf(descriptor Descriptor) DescriptorBinding {
	return DescriptorBinding{Provider: descriptor.Provider, Transport: descriptor.Transport, Scheme: descriptor.Scheme, Usage: descriptor.Usage, Issuer: descriptor.Issuer, Audience: descriptor.Audience}
}

func (b DescriptorBinding) Valid() bool {
	descriptor, err := NewDescriptor(b.Provider, b.Transport, b.Scheme, b.Usage, b.Issuer, b.Audience, "")
	return err == nil && DescriptorBindingOf(descriptor) == b
}

func (b DescriptorBinding) Canonical() string {
	if !b.Valid() {
		return ""
	}
	return strings.Join([]string{b.Provider, b.Transport, string(b.Scheme), string(b.Usage), b.Issuer, b.Audience}, "\x1f")
}

// Binding returns the factory-selection tuple for d.
func (d Descriptor) Binding() DescriptorBinding { return DescriptorBindingOf(d) }

// BindingCanonical is stable and excludes presentation label.
func (d Descriptor) BindingCanonical() string { return DescriptorBindingOf(d).Canonical() }

// FactoryInput is the complete explicit construction context. It contains a
// safe catalog record and opaque state capabilities, never secret bytes.
type FactoryInput struct {
	Record             Record
	Reference          Reference
	Descriptor         Descriptor
	State              secrets.Reference
	Resolver           secrets.Resolver
	Store              secrets.Store
	Preconditions      secrets.PreconditionCapabilities
	StateIndex         secrets.Lister
	StateNamespace     secrets.Namespace
	RefreshCoordinator RefreshCoordinator
	StateSharing       SharingScope
	HTTPClient         *http.Client
	Clock              Clock
	Callback           CallbackListener
}

// SourceFactory constructs exactly one source for one already-validated
// catalog record.
type SourceFactory func(context.Context, FactoryInput) (Source, error)

// ProviderFactories is an immutable copied factory registry. It is keyed by
// the complete descriptor binding, not by provider or a presentation label.
// The backing map is private; callers can only obtain copied snapshots.
type ProviderFactories struct {
	factories map[DescriptorBinding]SourceFactory
}

// NewProviderFactories copies an input map so later caller mutation cannot
// affect an active Builder.
func NewProviderFactories(input map[DescriptorBinding]SourceFactory) ProviderFactories {
	copyMap := make(map[DescriptorBinding]SourceFactory, len(input))
	for key, value := range input {
		copyMap[key] = value
	}
	return ProviderFactories{factories: copyMap}
}

// Lookup returns the factory for one exact descriptor binding.
func (p ProviderFactories) Lookup(binding DescriptorBinding) (SourceFactory, bool) {
	factory, ok := p.factories[binding]
	return factory, ok
}

// List returns all registered bindings in deterministic canonical order.
func (p ProviderFactories) List() []DescriptorBinding {
	keys := make([]DescriptorBinding, 0, len(p.factories))
	for key := range p.factories {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Canonical() < keys[j].Canonical() })
	return keys
}

// Snapshot returns a mutable copy that cannot alter this registry.
func (p ProviderFactories) Snapshot() map[DescriptorBinding]SourceFactory {
	copyMap := make(map[DescriptorBinding]SourceFactory, len(p.factories))
	for key, value := range p.factories {
		copyMap[key] = value
	}
	return copyMap
}

var (
	ErrBuilderDependency   = errors.New("credentials: builder dependency unavailable")
	ErrFactoryUnsupported  = errors.New("credentials: no exact credential factory")
	ErrFactoryMismatch     = errors.New("credentials: credential factory identity mismatch")
	ErrFactoryConstruction = errors.New("credentials: credential factory construction failed")
	ErrRefreshScope        = errors.New("credentials: refresh coordinator scope too weak")
	ErrStateNamespace      = errors.New("credentials: invalid credential state namespace")
	ErrBuilderRecord       = errors.New("credentials: invalid builder record")
)

// Builder constructs one explicit source from one catalog reference. All
// dependencies are injected; no environment, home directory, browser, or
// mutable package registry is consulted.
type Builder struct {
	Catalog        Catalog
	Resolver       secrets.Resolver
	Store          secrets.Store
	Preconditions  secrets.PreconditionCapabilities
	StateIndex     secrets.Lister
	StateNamespace secrets.Namespace
	RefreshLocks   RefreshCoordinator
	StateSharing   SharingScope
	HTTPClient     *http.Client
	Clock          Clock
	Callback       CallbackListener
	Providers      ProviderFactories
}

// Build resolves exactly ref and invokes exactly one complete-binding factory.
func (b *Builder) Build(ctx context.Context, ref Reference) (Source, error) {
	if ctx == nil {
		return nil, ErrBuilderDependency
	}
	if err := ctx.Err(); err != nil {
		return nil, NewCanceledError(err)
	}
	if b == nil || isNilValue(b.Catalog) || !ref.Valid() {
		return nil, ErrBuilderDependency
	}
	if b.StateNamespace.IsZero() {
		return nil, ErrStateNamespace
	}
	record, err := b.Catalog.Get(ctx, ref)
	if err != nil {
		return nil, safeBuilderCatalogError(err)
	}
	if err := ValidateRecord(record); err != nil || record.Reference != ref {
		return nil, ErrBuilderRecord
	}
	// Keep this defense at the construction boundary even when Record.Validate
	// is strengthened: one credential reference cannot select another provider.
	if record.Reference.Provider() != record.Descriptor.Provider {
		return nil, ErrBuilderRecord
	}
	if !b.StateNamespace.Contains(record.State) {
		return nil, ErrStateNamespace
	}
	factory, ok := b.Providers.Lookup(DescriptorBindingOf(record.Descriptor))
	if !ok || isNilValue(factory) {
		return nil, ErrFactoryUnsupported
	}

	refreshable := descriptorRequiresRefresh(record.Descriptor)
	stateResolver, effectiveCaps, err := b.resolveDependencies(refreshable)
	if err != nil {
		return nil, err
	}
	input := FactoryInput{
		Record: record, Reference: record.Reference, Descriptor: record.Descriptor, State: record.State,
		Resolver: stateResolver, Store: b.Store,
		Preconditions: effectiveCaps, StateIndex: b.StateIndex,
		StateNamespace: b.StateNamespace, RefreshCoordinator: b.RefreshLocks,
		StateSharing: b.StateSharing, HTTPClient: b.HTTPClient,
		Clock: b.Clock, Callback: b.Callback,
	}
	source, err := factory(ctx, input)
	if err != nil {
		if isCancellation(err) {
			return nil, NewCanceledError(err)
		}
		if errors.Is(err, ErrFactoryUnsupported) {
			return nil, ErrFactoryUnsupported
		}
		return nil, ErrFactoryConstruction
	}
	if isNilValue(source) {
		return nil, ErrFactoryMismatch
	}
	if source.Reference() != ref || source.Descriptor() != record.Descriptor {
		_ = source.Close()
		return nil, ErrFactoryMismatch
	}
	return source, nil
}

func safeBuilderCatalogError(err error) error {
	switch {
	case errors.Is(err, ErrCatalogNotFound):
		return ErrCatalogNotFound
	case errors.Is(err, ErrCatalogCorrupt):
		return ErrCatalogCorrupt
	case errors.Is(err, ErrCatalogConflict):
		return ErrCatalogConflict
	case errors.Is(err, ErrCanceled), errors.Is(err, ErrCatalogCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return newCanceledBoundaryError(ErrCatalogCanceled, err)
	case errors.Is(err, ErrCatalogDurabilityUnknown):
		return ErrCatalogDurabilityUnknown
	default:
		return ErrCatalogUnavailable
	}
}

func (b *Builder) resolveDependencies(refreshable bool) (secrets.Resolver, secrets.PreconditionCapabilities, error) {
	if b == nil {
		return nil, nil, ErrBuilderDependency
	}
	storePresent := !isNilValue(b.Store)
	resolverPresent := !isNilValue(b.Resolver)
	if storePresent && resolverPresent {
		return nil, nil, ErrBuilderDependency
	}
	var resolver secrets.Resolver = b.Resolver
	if storePresent {
		resolver = b.Store
	}
	if !resolverPresent && !storePresent {
		return nil, nil, ErrBuilderDependency
	}
	// Resolve capabilities once and pass this exact effective object to the
	// factory. Explicit capabilities win; writable stores otherwise affirm
	// support themselves. Resolver-only read paths have nothing to infer.
	effectiveCaps := b.Preconditions
	if effectiveCaps == nil && storePresent {
		if inferred, ok := b.Store.(secrets.PreconditionCapabilities); ok {
			effectiveCaps = inferred
		}
	}
	if !refreshable {
		return resolver, effectiveCaps, nil
	}
	if !storePresent || isNilValue(b.RefreshLocks) || !b.StateSharing.Valid() {
		return nil, nil, ErrBuilderDependency
	}
	if isNilValue(effectiveCaps) || !effectiveCaps.SupportsCreateOnly() || !effectiveCaps.SupportsCompareAndSwap() {
		return nil, nil, ErrBuilderDependency
	}
	if !b.RefreshLocks.Scope().AtLeast(b.StateSharing) {
		return nil, nil, ErrRefreshScope
	}
	return resolver, effectiveCaps, nil
}

func descriptorRequiresRefresh(descriptor Descriptor) bool {
	switch descriptor.Scheme {
	case SchemeOAuth, SchemeSigV4, SchemeWorkloadIdentity:
		return true
	default:
		return false
	}
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// StableBindings returns the factory bindings in deterministic order for
// diagnostics and tests. It does not expose factories or mutable state.
func StableBindings(factories ProviderFactories) []DescriptorBinding {
	return factories.List()
}
