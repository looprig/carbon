package app

// This file is the CodeRig credential composition boundary.  A production
// model configuration names only a safe credential reference; this package is
// the only place that resolves that reference, reads the explicit local
// stores, and turns the resulting source into an inference client.  In
// particular, it never consults provider environment variables.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/looprig/credentials"
	credentialcatalog "github.com/looprig/credentials/catalog"
	"github.com/looprig/credentials/httpauth"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/retry"
	"github.com/looprig/llm"
	"github.com/looprig/llm/auto"
	anthropicsubscription "github.com/looprig/llm/providers/anthropic/subscription"
	openaisubscription "github.com/looprig/llm/providers/openai/subscription"
	"github.com/looprig/secrets"
	secretslocal "github.com/looprig/secrets/local"
)

var (
	// These errors are deliberately package-owned and bounded.  They are used
	// by the session and CLI boundaries without exposing filesystem paths,
	// provider responses, or secret material.
	ErrCredentialLifecycleClosed = errors.New("coderig: credential lifecycle closed")
	ErrCredentialLogoutActive    = errors.New("coderig: credential logout is waiting for active sessions")
	ErrCredentialLogoutBlocked   = errors.New("coderig: credential is unavailable during logout")
)

// CredentialCompositionError reports a fail-closed model/source binding
// failure.  Reference and provider are safe identifiers; Cause is only ever a
// package-owned safe classification from credentials/llm.
type CredentialCompositionError struct {
	Reference credentials.Reference
	Provider  string
	Reason    string
	Cause     error
}

func (e *CredentialCompositionError) Error() string {
	if e == nil {
		return "coderig: credential composition failed"
	}
	message := "coderig: credential composition failed"
	if e.Reference.Valid() {
		message += " for " + e.Reference.String()
	}
	if provider := safeCredentialText(e.Provider); provider != "" {
		message += " provider " + provider
	}
	if e.Reason != "" {
		message += ": " + e.Reason
	}
	return message
}

func (e *CredentialCompositionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *CredentialCompositionError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

// CredentialUnsupportedError is used for providers for which CodeRig has no
// sanctioned registration or source implementation.  Login callers receive
// the provider package's typed unsupported-registration error for the two
// currently gated subscription providers; this type covers all other names.
type CredentialUnsupportedError struct {
	Provider  string
	Operation string
}

func (e *CredentialUnsupportedError) Error() string {
	if e == nil {
		return "coderig: credential operation unsupported"
	}
	provider := safeCredentialText(e.Provider)
	if provider == "" {
		provider = "unknown"
	}
	operation := e.Operation
	if operation == "" {
		operation = "operation"
	}
	return "coderig: credential " + operation + " unsupported for provider " + provider
}

func safeCredentialText(value string) string {
	if value == "" || len(value) > credentials.MaxReferenceComponentLength {
		return ""
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return ""
	}
	return value
}

func (e *CredentialUnsupportedError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

// CredentialSummary is the safe catalog projection exposed by the CLI.  It
// intentionally excludes state references, account identifiers, and values.
type CredentialSummary struct {
	Reference string
	Provider  string
	Transport string
	Scheme    string
	Usage     string
	Status    string
}

// CredentialLogoutOutcome keeps local deletion and remote revocation as
// separate facts.  RemoteRevoked remains false unless a sanctioned remote
// revocation implementation explicitly reports success; the current API-key
// source has no such operation. If LocalCatalogDeleted is true while
// LocalStateDeleted is false, the catalog no longer points at the orphaned
// local state: operators should preserve the outcome, inspect the bounded
// state reference through the local store tooling, and reconcile that state
// explicitly before retrying or declaring logout complete.
type CredentialLogoutOutcome struct {
	Reference                 string
	Provider                  string
	RemoteRevocationAttempted bool
	RemoteRevoked             bool
	RemoteRevocationError     bool
	LocalCatalogDeleted       bool
	LocalStateDeleted         bool
	LocalDeleted              bool
}

// CredentialLogoutError reports one or more local lifecycle outcomes without
// retaining backend error strings.  The outcome is authoritative for what did
// and did not happen.
type CredentialLogoutError struct {
	Outcome  CredentialLogoutOutcome
	Catalog  bool
	State    bool
	Canceled bool
}

func (e *CredentialLogoutError) Error() string {
	if e == nil {
		return "coderig: credential logout failed"
	}
	if e.Canceled {
		return "coderig: credential logout canceled"
	}
	if e.Catalog && e.State {
		return "coderig: credential logout could not delete local catalog and state"
	}
	if e.Catalog {
		return "coderig: credential logout could not delete local catalog"
	}
	if e.State {
		return "coderig: credential logout could not delete local state"
	}
	return "coderig: credential logout failed"
}

func (e *CredentialLogoutError) Unwrap() error {
	if e != nil && e.Canceled {
		return context.Canceled
	}
	return nil
}

func (e *CredentialLogoutError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

// apiKeySource is an immutable source backed by one explicit local secret
// record.  The source stores only the already-constructed authorizer; callers
// cannot inspect its bytes through this package.
type apiKeySource struct {
	reference  credentials.Reference
	descriptor credentials.Descriptor
	generation credentials.Generation
	authorizer httpauth.Authorizer

	mu     sync.RWMutex
	closed bool
}

func (s *apiKeySource) Reference() credentials.Reference {
	if s == nil {
		return credentials.Reference{}
	}
	return s.reference
}

func (s *apiKeySource) Descriptor() credentials.Descriptor {
	if s == nil {
		return credentials.Descriptor{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.descriptor
}

func (s *apiKeySource) Acquire(ctx context.Context) (credentials.Lease, error) {
	if ctx == nil {
		return nil, credentials.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, credentials.NewCanceledError(err)
	}
	if s == nil {
		return nil, &credentials.SourceClosedError{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, &credentials.SourceClosedError{}
	}
	return apiKeyLease{generation: s.generation, descriptor: s.descriptor, authorizer: s.authorizer}, nil
}

func (s *apiKeySource) Invalidate(ctx context.Context, generation credentials.Generation, failure credentials.Failure) error {
	if ctx == nil {
		return credentials.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return credentials.NewCanceledError(err)
	}
	if err := generation.Validate(); err != nil {
		return err
	}
	if err := failure.Validate(); err != nil {
		return err
	}
	if s == nil {
		return &credentials.SourceClosedError{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return &credentials.SourceClosedError{}
	}
	// API keys have no in-process refresh path.  A stale invalidation is a
	// no-op, matching NoneSource's generation discipline.
	if generation != s.generation {
		return nil
	}
	return nil
}

func (s *apiKeySource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *apiKeySource) String() string {
	if s == nil {
		return "coderig: nil credential source"
	}
	return "coderig: api-key credential source"
}

func (s *apiKeySource) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, s.String())
}

type apiKeyLease struct {
	generation credentials.Generation
	descriptor credentials.Descriptor
	authorizer httpauth.Authorizer
}

func (l apiKeyLease) Generation() credentials.Generation { return l.generation }
func (l apiKeyLease) Descriptor() credentials.Descriptor { return l.descriptor }
func (l apiKeyLease) ExpiresAt() time.Time               { return time.Time{} }
func (l apiKeyLease) Authorizer() httpauth.Authorizer    { return l.authorizer }

func (l apiKeyLease) String() string { return "coderig: immutable api-key lease" }

func (l apiKeyLease) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, l.String())
}

// credentialRuntime owns all dependencies needed by one production model
// composition.  Its close order is source -> catalog -> secret store, the
// reverse of construction.  RuntimeAgent drains the client/session before
// invoking close.
type credentialRuntime struct {
	mu sync.Mutex

	store     *secretslocal.Store
	catalog   *credentialcatalog.Local
	namespace secrets.Namespace
	builder   credentials.Builder

	sources      map[credentials.Reference]credentials.Source
	refs         map[credentials.Reference]struct{}
	active       map[credentials.Reference]int
	blocked      map[credentials.Reference]bool
	closing      bool
	closed       bool
	closeDone    chan struct{}
	closeErr     error
	activeN      int
	activeDone   chan struct{}
	operations   int
	opDone       chan struct{}
	registryDone chan struct{}
}

// credentialRegistry is process-scoped. One canonical home identifies one
// local catalog/store pair, so model/session composition and lifecycle
// commands borrow the same runtime and source map. It deliberately does not
// coordinate across processes; cross-process drain remains an operator duty.
type credentialRegistry struct {
	mu      sync.Mutex
	entries map[string]*credentialRegistryEntry
}

type credentialRegistryEntry struct {
	home    string
	runtime *credentialRuntime
	borrows int
	done    chan struct{}
}

type credentialRegistryLease struct {
	registry *credentialRegistry
	key      string
	entry    *credentialRegistryEntry
	once     sync.Once
	err      error
}

var processCredentialRegistry = &credentialRegistry{entries: make(map[string]*credentialRegistryEntry)}

func canonicalCredentialHome(home string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return "", &CredentialCompositionError{Reason: "credential home is unavailable"}
	}
	return filepath.Clean(home), nil
}

func (r *credentialRuntime) lifecycleState() (closed, closing bool) {
	if r == nil {
		return true, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed, r.closing
}

func acquireCredentialRuntime(home string) (*credentialRegistryLease, *credentialRuntime, error) {
	key, err := canonicalCredentialHome(home)
	if err != nil {
		return nil, nil, err
	}
	for {
		processCredentialRegistry.mu.Lock()
		entry := processCredentialRegistry.entries[key]
		if entry == nil {
			runtime, err := newCredentialRuntime(key)
			if err != nil {
				processCredentialRegistry.mu.Unlock()
				return nil, nil, err
			}
			entry = &credentialRegistryEntry{home: key, runtime: runtime, borrows: 1, done: make(chan struct{})}
			runtime.registryDone = entry.done
			processCredentialRegistry.entries[key] = entry
			processCredentialRegistry.mu.Unlock()
			return &credentialRegistryLease{registry: processCredentialRegistry, key: key, entry: entry}, runtime, nil
		}
		closed, closing := entry.runtime.lifecycleState()
		if !closed && !closing {
			entry.borrows++
			processCredentialRegistry.mu.Unlock()
			return &credentialRegistryLease{registry: processCredentialRegistry, key: key, entry: entry}, entry.runtime, nil
		}
		if closing {
			done := entry.done
			processCredentialRegistry.mu.Unlock()
			<-done
			continue
		}
		delete(processCredentialRegistry.entries, key)
		processCredentialRegistry.mu.Unlock()
	}
}

func (l *credentialRegistryLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.registry.mu.Lock()
		if l.entry.borrows > 0 {
			l.entry.borrows--
		}
		last := l.entry.borrows == 0
		l.registry.mu.Unlock()
		if last {
			l.err = l.entry.runtime.Close()
			l.registry.mu.Lock()
			if l.registry.entries[l.key] == l.entry {
				delete(l.registry.entries, l.key)
			}
			l.registry.mu.Unlock()
		}
	})
	return l.err
}

// newCredentialRuntime is intentionally explicit: the caller supplies the
// already-resolved CodeRig home. No environment or user-home lookup occurs in
// this constructor.
func newCredentialRuntime(home string) (*credentialRuntime, error) {
	canonical, err := canonicalCredentialHome(home)
	if err != nil {
		return nil, err
	}
	secretStore, err := secretslocal.New(filepath.Join(canonical, "credentials", "secrets"))
	if err != nil {
		return nil, &CredentialCompositionError{Reason: "open credential secret store"}
	}
	catalog, err := credentialcatalog.New(filepath.Join(canonical, "credentials", "catalog"))
	if err != nil {
		_ = secretStore.Close()
		return nil, &CredentialCompositionError{Reason: "open credential catalog"}
	}
	namespace, err := secrets.NewNamespace("local", "credentials")
	if err != nil {
		_ = catalog.Close()
		_ = secretStore.Close()
		return nil, &CredentialCompositionError{Reason: "construct credential state namespace"}
	}

	runtime := &credentialRuntime{
		store:     secretStore,
		catalog:   catalog,
		namespace: namespace,
		sources:   make(map[credentials.Reference]credentials.Source),
		refs:      make(map[credentials.Reference]struct{}),
		active:    make(map[credentials.Reference]int),
		blocked:   make(map[credentials.Reference]bool),
		activeDone: func() chan struct{} {
			done := make(chan struct{})
			close(done)
			return done
		}(),
		opDone: func() chan struct{} {
			done := make(chan struct{})
			close(done)
			return done
		}(),
	}
	// Keep a fully explicit Builder on the runtime even though sourceFor adds
	// the one exact binding required for a particular model policy. The base
	// value makes all state/catalog dependencies inspectable and prevents a
	// future source factory from falling back to ambient discovery.
	runtime.builder = credentials.Builder{
		Catalog:        catalog,
		Store:          secretStore,
		StateNamespace: namespace,
		Providers:      credentials.NewProviderFactories(nil),
	}
	return runtime, nil
}

func (r *credentialRuntime) sourceFor(ctx context.Context, selected model.Model, ref credentials.Reference) (credentials.Source, error) {
	if r == nil {
		return nil, &CredentialCompositionError{Reference: ref, Provider: ref.Provider(), Reason: ErrCredentialLifecycleClosed.Error(), Cause: ErrCredentialLifecycleClosed}
	}
	if ctx == nil {
		return nil, &CredentialCompositionError{Reference: ref, Provider: ref.Provider(), Reason: "credential context is unavailable", Cause: credentials.ErrNilContext}
	}
	policy, err := llm.AuthPolicyForModel(selected)
	if err != nil || len(policy.Accepted) != 1 {
		return nil, &CredentialCompositionError{Reference: ref, Provider: string(selected.Provider), Reason: "provider auth policy is unavailable", Cause: err}
	}
	expected, err := policy.Accepted[0].Descriptor()
	if err != nil {
		return nil, &CredentialCompositionError{Reference: ref, Provider: string(selected.Provider), Reason: "provider auth descriptor is unavailable", Cause: err}
	}
	if ref.Provider() != expected.Provider {
		return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: "credential reference provider does not match model policy"}
	}

	// Serializing construction makes the one-source-per-reference guarantee
	// linearizable. Builder.Build performs catalog/state reads only; it never
	// opens a provider connection, so a missing or mismatched ref fails before
	// any inference network activity can occur.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: ErrCredentialLifecycleClosed.Error(), Cause: ErrCredentialLifecycleClosed}
	}
	if r.blocked[ref] {
		return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: ErrCredentialLogoutBlocked.Error(), Cause: ErrCredentialLogoutBlocked}
	}
	if source, ok := r.sources[ref]; ok {
		if !credentialDescriptorsMatchPolicy(source.Descriptor(), expected) {
			return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: "credential descriptor does not match model policy", Cause: &llm.AuthPolicyMismatchError{Reason: "credential reference is bound to a different transport"}}
		}
		return source, nil
	}

	builder := r.builder
	builder.Providers = credentials.NewProviderFactories(map[credentials.DescriptorBinding]credentials.SourceFactory{
		credentials.DescriptorBindingOf(expected): newAPIKeySource,
	})
	source, err := builder.Build(ctx, ref)
	if err != nil {
		return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: "resolve credential reference", Cause: err}
	}
	if source.Reference() != ref || !credentialDescriptorsMatchPolicy(source.Descriptor(), expected) {
		_ = source.Close()
		return nil, &CredentialCompositionError{Reference: ref, Provider: expected.Provider, Reason: "credential descriptor does not match model policy", Cause: &llm.AuthPolicyMismatchError{Reason: "credential source identity mismatch"}}
	}
	r.sources[ref] = source
	r.refs[ref] = struct{}{}
	// A source may be composed lazily by a caller after a session has already
	// been admitted. Charge every admitted session before publishing it so a
	// concurrent logout cannot close a source still borrowed by that session.
	if r.activeN > 0 {
		r.active[ref] = r.activeN
	}
	return source, nil
}

func credentialDescriptorsMatchPolicy(actual, expected credentials.Descriptor) bool {
	if !actual.Valid() || !expected.Valid() {
		return false
	}
	// Descriptor labels are presentation metadata. The complete authority
	// binding (provider, transport, scheme, usage, issuer, audience) is the
	// exact policy identity enforced by llm.AuthPolicyForModel.
	return actual.BindingCanonical() == expected.BindingCanonical()
}

// newAPIKeySource is the only API-key factory installed by CodeRig. It reads
// one explicit state reference from the injected resolver and immediately
// clears the mutable copy returned by Secret.Bytes.
func newAPIKeySource(ctx context.Context, input credentials.FactoryInput) (credentials.Source, error) {
	if ctx == nil || input.Resolver == nil {
		return nil, credentials.ErrFactoryConstruction
	}
	// CodeRig's current local factory owns only explicit API-key records. OAuth,
	// SigV4, and workload-identity records require their provider-specific
	// refresh/registration implementation; treating an opaque token as a
	// static API key would silently weaken that policy.
	if input.Descriptor.Scheme != credentials.SchemeAPIKey || input.Descriptor.Usage != credentials.UsageMeteredAPI {
		return nil, credentials.ErrFactoryUnsupported
	}
	record, err := input.Resolver.Resolve(ctx, input.State)
	if err != nil {
		return nil, err
	}
	bytes := record.Value.Bytes()
	defer clear(bytes)
	secret, err := secrets.New(bytes)
	if err != nil {
		return nil, err
	}
	authorizer, err := credentialAuthorizer(input.Descriptor, secret)
	if err != nil {
		return nil, err
	}
	generation, err := credentials.NewGeneration("local-api-key-v1")
	if err != nil {
		return nil, err
	}
	return &apiKeySource{reference: input.Reference, descriptor: input.Descriptor, generation: generation, authorizer: authorizer}, nil
}

func credentialAuthorizer(descriptor credentials.Descriptor, secret secrets.Secret) (httpauth.Authorizer, error) {
	provider := descriptor.Provider
	transport := descriptor.Transport
	switch provider {
	case "anthropic", "minimax":
		return httpauth.Header("x-api-key", secret)
	case "azure":
		return httpauth.Header("api-key", secret)
	case "azure-cognitive-services":
		if credentialTransportIsAnthropic(transport) {
			return httpauth.Header("x-api-key", secret)
		}
		return httpauth.Header("api-key", secret)
	case "google":
		return httpauth.Header("x-goog-api-key", secret)
	case "deepinfra", "zenmux", "opencode", "opencode-go":
		if credentialTransportIsAnthropic(transport) {
			return httpauth.Header("x-api-key", secret)
		}
	}
	return httpauth.Bearer(secret)
}

func credentialTransportIsAnthropic(transport string) bool {
	// Anthropic itself names the transport "messages"; providers that expose
	// the shared compatibility codec retain the API-format name.
	return transport == "messages" || transport == string(model.APIFormatAnthropic)
}

func (r *credentialRuntime) refsSnapshot() []credentials.Reference {
	r.mu.Lock()
	defer r.mu.Unlock()
	refs := make([]credentials.Reference, 0, len(r.refs))
	for ref := range r.refs {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs
}

func (r *credentialRuntime) beginSession() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return ErrCredentialLifecycleClosed
	}
	for _, blocked := range r.blocked {
		if blocked {
			return ErrCredentialLogoutBlocked
		}
	}
	if r.activeN == 0 {
		r.activeDone = make(chan struct{})
	}
	r.activeN++
	for ref := range r.refs {
		r.active[ref]++
	}
	return nil
}

func (r *credentialRuntime) endSession() {
	if r == nil {
		return
	}
	r.mu.Lock()
	wasActive := r.activeN > 0
	if wasActive {
		r.activeN--
	}
	if wasActive {
		for ref, count := range r.active {
			if count > 0 {
				r.active[ref] = count - 1
			}
		}
	}
	if wasActive && r.activeN == 0 && r.activeDone != nil {
		close(r.activeDone)
	}
	r.mu.Unlock()
}

// Close waits for all admitted sessions to drain, then closes sources and
// their borrowed local dependencies in reverse construction order. It is
// safe to race with endSession and is idempotent.
func (r *credentialRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.closing {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closing = true
	r.closeDone = make(chan struct{})
	done := r.closeDone
	for r.activeN > 0 || r.operations > 0 {
		activeDone := r.activeDone
		opDone := r.opDone
		active := r.activeN > 0
		operations := r.operations > 0
		r.mu.Unlock()
		// Session teardown calls endSession after the adapter/client has drained.
		// The one-shot channel avoids a polling loop in close/logout races.
		if active && activeDone != nil {
			<-activeDone
		}
		if operations && opDone != nil {
			<-opDone
		}
		r.mu.Lock()
	}
	refs := make([]credentials.Reference, 0, len(r.sources))
	for ref := range r.sources {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	sources := make([]credentials.Source, 0, len(refs))
	for _, ref := range refs {
		sources = append(sources, r.sources[ref])
	}
	catalog := r.catalog
	store := r.store
	r.closed = true
	r.mu.Unlock()

	var first error
	for _, source := range sources {
		if err := source.Close(); err != nil && first == nil {
			first = err
		}
	}
	if catalog != nil {
		if err := catalog.Close(); err != nil && first == nil {
			first = err
		}
	}
	if store != nil {
		if err := store.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.mu.Lock()
	r.closeErr = first
	if r.registryDone != nil {
		close(r.registryDone)
		r.registryDone = nil
	}
	close(done)
	r.mu.Unlock()
	return first
}

func (r *credentialRuntime) list(ctx context.Context) ([]CredentialSummary, error) {
	if r == nil || r.catalog == nil {
		return nil, nil
	}
	if ctx == nil {
		return nil, credentials.ErrNilContext
	}
	r.mu.Lock()
	closed := r.closed || r.closing
	r.mu.Unlock()
	if closed {
		return nil, ErrCredentialLifecycleClosed
	}
	records, err := r.catalog.List(ctx)
	if err != nil {
		return nil, &CredentialCompositionError{Reason: "list credential catalog", Cause: err}
	}
	summaries := make([]CredentialSummary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, CredentialSummary{
			Reference: record.Reference.String(),
			Provider:  record.Descriptor.Provider,
			Transport: record.Descriptor.Transport,
			Scheme:    record.Descriptor.Scheme.String(),
			Usage:     record.Descriptor.Usage.String(),
			Status:    "configured",
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Reference < summaries[j].Reference })
	return summaries, nil
}

// logout blocks new sessions before waiting for existing users to drain. It
// then closes the in-process source and performs catalog/state deletion as
// separate operations. No remote revocation is claimed for API-key sources.
func (r *credentialRuntime) logout(ctx context.Context, ref credentials.Reference) (CredentialLogoutOutcome, error) {
	outcome := CredentialLogoutOutcome{Reference: ref.String(), Provider: ref.Provider()}
	if r == nil || r.catalog == nil || r.store == nil {
		return outcome, ErrCredentialLifecycleClosed
	}
	if !ref.Valid() {
		return outcome, &CredentialCompositionError{Reason: "credential reference is invalid"}
	}
	if ctx == nil {
		return outcome, credentials.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return outcome, &CredentialLogoutError{Outcome: outcome, Canceled: true}
	}
	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()
		return outcome, ErrCredentialLifecycleClosed
	}
	if r.blocked[ref] {
		r.mu.Unlock()
		return outcome, ErrCredentialLogoutBlocked
	}
	r.blocked[ref] = true
	if r.operations == 0 {
		r.opDone = make(chan struct{})
	}
	r.operations++
	defer func() {
		r.mu.Lock()
		r.operations--
		if r.operations == 0 && r.opDone != nil {
			close(r.opDone)
		}
		r.mu.Unlock()
	}()
	for r.active[ref] > 0 {
		done := r.activeDone
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			// The block remains in force: a canceled logout must not reopen a
			// source while the caller believes it is being logged out.
			return outcome, &CredentialLogoutError{Outcome: outcome, Canceled: true}
		}
		r.mu.Lock()
	}
	// Publish the block before lookup/delete. This closes the race where a new
	// composition could borrow the source while logout was resolving its record.
	r.mu.Unlock()
	record, catalogErr := r.catalog.Get(ctx, ref)
	if catalogErr != nil {
		// A missing reference never existed in this runtime, so release its
		// transient block. Other failures remain fail-closed and keep the block.
		if errors.Is(catalogErr, credentials.ErrCatalogNotFound) {
			r.mu.Lock()
			delete(r.blocked, ref)
			r.mu.Unlock()
		}
		return outcome, &CredentialLogoutError{Outcome: outcome, Catalog: true}
	}
	r.mu.Lock()
	source := r.sources[ref]
	delete(r.sources, ref)
	delete(r.refs, ref)
	r.mu.Unlock()
	if source != nil {
		_ = source.Close()
	}

	if err := r.catalog.Delete(ctx, ref); err != nil && !errors.Is(err, credentials.ErrCatalogNotFound) {
		return outcome, &CredentialLogoutError{Outcome: outcome, Catalog: true}
	}
	outcome.LocalCatalogDeleted = true

	deleted, stateErr := r.store.Delete(ctx, record.State, secrets.UnconditionalDelete())
	if stateErr != nil {
		return outcome, &CredentialLogoutError{Outcome: outcome, State: true}
	}
	outcome.LocalStateDeleted = deleted.Status == secrets.DeleteStatusDeleted || deleted.Status == secrets.DeleteStatusAbsent
	outcome.LocalDeleted = outcome.LocalCatalogDeleted && outcome.LocalStateDeleted
	return outcome, nil
}

// ListCredentials borrows the process-shared explicit catalog composition. It
// is read-only with respect to provider authority and does not inspect values.
func ListCredentials(ctx context.Context, cfg Config) ([]CredentialSummary, error) {
	home, err := looprigHome(cfg)
	if err != nil {
		return nil, err
	}
	lease, runtime, err := acquireCredentialRuntime(home)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return runtime.list(ctx)
}

// LoginCredential is deliberately explicit and fail-closed. The current
// OpenAI and Anthropic subscription gates are checked before any URL/browser
// or network operation; both gates currently return their typed unsupported
// errors, so no browser is opened.
func LoginCredential(ctx context.Context, cfg Config, provider string) error {
	if ctx == nil {
		return credentials.ErrNilContext
	}
	home, err := looprigHome(cfg)
	if err != nil {
		return err
	}
	lease, _, err := acquireCredentialRuntime(home)
	if err != nil {
		return err
	}
	defer lease.Release()
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := ctx.Err(); err != nil {
		return credentials.NewCanceledError(err)
	}
	switch provider {
	case "openai":
		return openaisubscription.OpenAIRegistration().Require()
	case "anthropic":
		return anthropicsubscription.AnthropicRegistration().Require()
	default:
		return &CredentialUnsupportedError{Provider: provider, Operation: "login"}
	}
}

// LogoutCredential performs the explicit local lifecycle action. The command
// reports the returned outcome even when one local deletion operation fails.
func LogoutCredential(ctx context.Context, cfg Config, rawRef string) (CredentialLogoutOutcome, error) {
	ref, err := credentials.ParseReference(strings.TrimSpace(rawRef))
	if err != nil {
		return CredentialLogoutOutcome{}, &CredentialCompositionError{Reason: "credential reference is invalid"}
	}
	home, err := looprigHome(cfg)
	if err != nil {
		return CredentialLogoutOutcome{}, err
	}
	lease, runtime, err := acquireCredentialRuntime(home)
	if err != nil {
		return CredentialLogoutOutcome{Reference: ref.String(), Provider: ref.Provider()}, err
	}
	defer lease.Release()
	return runtime.logout(ctx, ref)
}

// credentialClientFor is the production model factory hook used by
// loadProductionModels. Static/legacy API keys retain auto.New's behavior;
// reference-backed models use the canonical llm.auto.NewWithAuth path.
func credentialClientFor(ctx context.Context, runtime *credentialRuntime, selected model.Model, input modelClientInput) (inference.Client, error) {
	if input.hasCredentialRef() {
		if runtime == nil {
			return nil, &CredentialCompositionError{Reference: input.CredentialRef, Provider: string(selected.Provider), Reason: "credential lifecycle is unavailable", Cause: ErrCredentialLifecycleClosed}
		}
		source, err := runtime.sourceFor(ctx, selected, input.CredentialRef)
		if err != nil {
			return nil, err
		}
		client, err := auto.NewWithAuth(selected, source)
		if err != nil {
			return nil, &CredentialCompositionError{Reference: input.CredentialRef, Provider: string(selected.Provider), Reason: "construct credential-backed inference client", Cause: err}
		}
		return retryClient(client)
	}
	client, err := auto.New(selected, auth.APIKey(input.APIKey))
	if err != nil {
		return nil, err
	}
	return retryClient(client)
}

func retryClient(client inference.Client) (inference.Client, error) {
	if client == nil {
		return nil, &CredentialCompositionError{Reason: "inference client is unavailable"}
	}
	return retry.New(client, defaultRetryPolicy)
}
