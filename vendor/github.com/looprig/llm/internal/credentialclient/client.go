// Package credentialclient adapts a credentials.Source to the call-scoped
// authorization seam exposed by inference transports. It owns provider policy
// checks, lease acquisition, provider auth-failure classification, and the
// bounded recovery exchange; the low-level transport only authorizes and sends.
package credentialclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/credentials"
	"github.com/looprig/credentials/httpauth"
	"github.com/looprig/inference"
	"github.com/looprig/inference/failure"
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
)

// InvokeWithAuth and StreamWithAuth are the additive call-scoped methods on
// inference transports. Keeping these small local interfaces lets provider
// clients retain the legacy inference.Client surface while the adapter supplies
// a fresh lease for each concrete wire attempt.
type invokeWithAuth interface {
	InvokeWithAuth(context.Context, inference.Request, httpauth.Authorizer) (*inference.Response, error)
}

type streamWithAuth interface {
	StreamWithAuth(context.Context, inference.Request, httpauth.Authorizer) (*stream.StreamReader[content.Chunk], error)
}

// Client is a provider-bound source adapter. It is safe for concurrent use:
// source and inner are immutable references, and all per-call state is local.
type Client struct {
	inner         inference.Client
	source        credentials.Source
	policy        llm.AuthPolicy
	sourceBinding credentials.Descriptor
	allowLegacy   bool
	recoveryMu    sync.Mutex
	recovery      map[credentials.Generation]*recoveryState
	observed      []credentials.Generation
}

type recoveryState struct {
	generation credentials.Generation
	done       chan struct{}
	err        error
}

// New binds source to inner under one exact policy. A nil source is never
// interpreted as unauthenticated; callers must pass credentials.NewNoneSource
// for an explicitly local/no-credential transport. A legacy-only inner client
// is rejected because using its default authenticator would bypass the source.
func New(inner inference.Client, source credentials.Source, policy llm.AuthPolicy) (inference.Client, error) {
	return newClient(inner, source, policy, false)
}

// ValidateSource performs the construction-time source and exact-policy
// checks without allocating a transport. Composition roots can call it before
// dispatch so a nil or mismatched source cannot even construct a provider
// network object.
func ValidateSource(source credentials.Source, policy llm.AuthPolicy) error {
	if isNil(source) {
		return &ConstructionError{Reason: "credential source is nil; pass an explicit NoneSource for local transport"}
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	return policy.Match(source.Descriptor())
}

// SupportsCallScoped reports whether an inference client exposes at least one
// additive call-scoped authorization method. It is used by composition roots
// to keep legacy concrete clients source-compatible while ensuring every
// transport that can consume leases is wrapped by Client.
func SupportsCallScoped(inner inference.Client) bool {
	if isNil(inner) {
		return false
	}
	_, invokeOK := inner.(invokeWithAuth)
	_, streamOK := inner.(streamWithAuth)
	return invokeOK || streamOK
}

// NewLegacyCompatible is the narrow compatibility escape hatch for old
// provider clients that predate call-scoped transport methods. It is permitted
// only for an immutable StaticSource created by auto.New; dynamic sources must
// use New and are rejected rather than silently bypassing lease authorization.
func NewLegacyCompatible(inner inference.Client, source credentials.Source, policy llm.AuthPolicy) (inference.Client, error) {
	if _, ok := source.(*StaticSource); !ok {
		return New(inner, source, policy)
	}
	return newClient(inner, source, policy, true)
}

func newClient(inner inference.Client, source credentials.Source, policy llm.AuthPolicy, allowLegacy bool) (inference.Client, error) {
	if isNil(inner) {
		return nil, &ConstructionError{Reason: "inner inference client is nil"}
	}
	if err := ValidateSource(source, policy); err != nil {
		return nil, err
	}
	sourceBinding := source.Descriptor()
	if !SupportsCallScoped(inner) && !allowLegacy {
		return nil, &ConstructionError{Reason: "inner client does not support call-scoped authorization"}
	}
	policy.Accepted = append([]llm.AuthBinding(nil), policy.Accepted...)
	return &Client{inner: inner, source: source, policy: policy, sourceBinding: sourceBinding, allowLegacy: allowLegacy, recovery: make(map[credentials.Generation]*recoveryState)}, nil
}

// Invoke acquires and verifies one lease before every initial/recovery wire
// attempt. Authentication recovery is deliberately bounded to one extra wire
// attempt and never implements or resets an outer inference retry budget.
func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	if c == nil {
		return nil, &ConstructionError{Reason: "credential client is nil"}
	}
	if ctx == nil {
		return nil, errors.New("llm: nil invocation context")
	}
	invoker, ok := c.inner.(invokeWithAuth)
	if !ok && !c.allowLegacy {
		return nil, &ConstructionError{Reason: "inner client does not support call-scoped invocation"}
	}
	lease, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	var response *inference.Response
	if ok {
		response, err = invoker.InvokeWithAuth(ctx, req, c.authorizer(lease))
	} else {
		response, err = c.inner.Invoke(ctx, req)
	}
	if !ok {
		return response, err
	}
	failureClass, recoverable := c.recoveryClass(err)
	if !recoverable {
		return response, err
	}
	if invalidateErr := c.invalidateOnce(ctx, lease.Generation(), failureClass); invalidateErr != nil {
		return nil, &RecoveryError{Stage: "invalidate", Err: invalidateErr}
	}
	// Exactly one recovery acquisition and one recovery wire exchange. No loop
	// here is intentional: ordinary retry policy belongs outside this adapter.
	recoveryLease, acquireErr := c.acquire(ctx)
	if acquireErr != nil {
		return nil, acquireErr
	}
	return invoker.InvokeWithAuth(ctx, req, c.authorizer(recoveryLease))
}

// Stream follows the same lease/recovery contract as Invoke. If a failed
// streaming call returned a reader alongside an error, close it before recovery
// so no body/connection is leaked.
func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	if c == nil {
		return nil, &ConstructionError{Reason: "credential client is nil"}
	}
	if ctx == nil {
		return nil, errors.New("llm: nil streaming context")
	}
	streamer, ok := c.inner.(streamWithAuth)
	if !ok && !c.allowLegacy {
		return nil, &ConstructionError{Reason: "inner client does not support call-scoped streaming"}
	}
	lease, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	var reader *stream.StreamReader[content.Chunk]
	if ok {
		reader, err = streamer.StreamWithAuth(ctx, req, c.authorizer(lease))
	} else {
		reader, err = c.inner.Stream(ctx, req)
	}
	if !ok {
		return reader, err
	}
	failureClass, recoverable := c.recoveryClass(err)
	if !recoverable {
		return reader, err
	}
	if reader != nil {
		_ = reader.Close()
	}
	if invalidateErr := c.invalidateOnce(ctx, lease.Generation(), failureClass); invalidateErr != nil {
		return nil, &RecoveryError{Stage: "invalidate", Err: invalidateErr}
	}
	recoveryLease, acquireErr := c.acquire(ctx)
	if acquireErr != nil {
		return nil, acquireErr
	}
	return streamer.StreamWithAuth(ctx, req, c.authorizer(recoveryLease))
}

// invalidateOnce serializes invalidation for one source generation. Many
// concurrent calls can observe the same expired lease, but only the first
// caller is allowed to mutate source state; followers wait for that result and
// perform their own single bounded recovery acquisition/wire attempt.
func (c *Client) invalidateOnce(ctx context.Context, generation credentials.Generation, failure credentials.Failure) error {
	c.recoveryMu.Lock()
	if c.recovery == nil {
		c.recovery = make(map[credentials.Generation]*recoveryState)
	}
	if current := c.recovery[generation]; current != nil {
		done := current.done
		c.recoveryMu.Unlock()
		select {
		case <-done:
			c.recoveryMu.Lock()
			err := current.err
			c.recoveryMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	state := &recoveryState{generation: generation, done: make(chan struct{})}
	c.recovery[generation] = state
	c.recoveryMu.Unlock()

	err := c.source.Invalidate(ctx, generation, failure)
	c.recoveryMu.Lock()
	state.err = err
	if err != nil {
		delete(c.recovery, generation)
	}
	close(state.done)
	c.recoveryMu.Unlock()
	return err
}

func (c *Client) authorizer(lease credentials.Lease) httpauth.Authorizer {
	if lease == nil {
		return &originAuthorizer{expected: ""}
	}
	if c.sourceBinding.Audience == "" {
		return lease.Authorizer()
	}
	return &originAuthorizer{inner: lease.Authorizer(), expected: c.sourceBinding.Audience}
}

type originAuthorizer struct {
	inner    httpauth.Authorizer
	expected string
}

func (a *originAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request == nil || request.URL == nil {
		return &LeaseError{Reason: "credential request has no URL"}
	}
	origin, err := requestOrigin(request.URL)
	if err != nil || origin != a.expected {
		return &LeaseError{Reason: "credential request origin does not match the exact policy audience"}
	}
	if a.inner == nil {
		return &LeaseError{Reason: "credential lease has no authorizer"}
	}
	return a.inner.Authorize(ctx, request)
}

func (a *originAuthorizer) String() string { return "llm origin-guarded authorizer" }

func requestOrigin(value *url.URL) (string, error) {
	if value == nil || value.Scheme == "" || value.Host == "" || value.User != nil {
		return "", errors.New("invalid request origin")
	}
	scheme := strings.ToLower(value.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", errors.New("unsupported request origin scheme")
	}
	host := strings.ToLower(strings.TrimSuffix(value.Hostname(), "."))
	if host == "" {
		return "", errors.New("request origin has no host")
	}
	port := value.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host, nil
}

func (c *Client) acquire(ctx context.Context) (credentials.Lease, error) {
	lease, err := c.source.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if isNil(lease) {
		return nil, &LeaseError{Reason: "credential source returned a nil lease"}
	}
	leaseDescriptor := lease.Descriptor()
	if err := c.policy.Match(leaseDescriptor); err != nil {
		return nil, err
	}
	if !sameBinding(c.sourceBinding, leaseDescriptor) {
		return nil, &LeaseError{Reason: "credential source lease identity changed after construction"}
	}
	if err := lease.Generation().Validate(); err != nil {
		return nil, &LeaseError{Reason: "credential source returned an invalid generation"}
	}
	c.observeGeneration(lease.Generation())
	if isNil(lease.Authorizer()) {
		return nil, &LeaseError{Reason: "credential source returned a nil authorizer"}
	}
	return lease, nil
}

// observeGeneration prunes completed successful invalidations from older
// generations when a newer lease is observed. In-flight entries remain until
// their waiters are notified; the table retains only a small recent generation
// window, plus active overlaps, never entries proportional to call count.
func (c *Client) observeGeneration(generation credentials.Generation) {
	c.recoveryMu.Lock()
	defer c.recoveryMu.Unlock()
	if len(c.observed) == 0 || c.observed[len(c.observed)-1] != generation {
		c.observed = append(c.observed, generation)
		if len(c.observed) > 3 {
			c.observed = c.observed[len(c.observed)-3:]
		}
	}
	for key, state := range c.recovery {
		if key == generation {
			continue
		}
		select {
		case <-state.done:
			if state.err == nil && !containsGeneration(c.observed, key) {
				delete(c.recovery, key)
			}
		default:
		}
	}
}

func containsGeneration(generations []credentials.Generation, wanted credentials.Generation) bool {
	for _, generation := range generations {
		if generation == wanted {
			return true
		}
	}
	return false
}

type renewableSource interface {
	CanRecover(credentials.Failure) bool
}

func (c *Client) recoveryClass(err error) (credentials.Failure, bool) {
	failureClass, ok := classifyAuthFailureForTransport(c.sourceBinding.Transport, err)
	if !ok {
		return "", false
	}
	renewable, ok := c.source.(renewableSource)
	if !ok || !renewable.CanRecover(failureClass) {
		return "", false
	}
	return failureClass, true
}

// ClassifyAuthFailure maps bounded provider errors into the closed credentials
// failure classes. Only explicit authentication codes can trigger a refresh;
// malformed requests, permission denials, quota, and rate limits cannot.
func ClassifyAuthFailure(err error) (credentials.Failure, bool) {
	return classifyAuthFailureForTransport("", err)
}

var authFailureCodesByTransport = map[string]map[string]struct{}{
	"":                 {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"openai":           {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"anthropic":        {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"gemini":           {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"responses":        {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"chat":             {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"openai-responses": {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"messages":         {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"generate-content": {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
	"bedrock-converse": {"unauthorized": {}, "unauthenticated": {}, "authentication_error": {}, "invalid_api_key": {}},
}

func classifyAuthFailureForTransport(transport string, err error) (credentials.Failure, bool) {
	if err == nil {
		return "", false
	}
	var apiErr *failure.APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return "", false
	}
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(apiErr.ProviderCode))
	}
	allowed, supported := authFailureCodesByTransport[strings.ToLower(strings.TrimSpace(transport))]
	if !supported {
		return "", false
	}
	if _, safe := allowed[code]; safe && (apiErr.Status == 401 || apiErr.Status == 403) {
		return classifyAuthFailure(err), true
	}
	return "", false
}

func sameBinding(left, right credentials.Descriptor) bool {
	return left.Provider == right.Provider && left.Transport == right.Transport &&
		left.Scheme == right.Scheme && left.Usage == right.Usage &&
		left.Issuer == right.Issuer && left.Audience == right.Audience
}

func classifyAuthFailure(err error) credentials.Failure {
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) && apiErr != nil && apiErr.Status == 401 {
		code := strings.ToLower(strings.TrimSpace(apiErr.Code))
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(apiErr.ProviderCode))
		}
		if code == "invalid_api_key" || code == "authentication_error" {
			return credentials.FailureAuthRejected
		}
		return credentials.FailureAuthExpired
	}
	return credentials.FailureAuthRejected
}

func isNil(value any) bool {
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

// ConstructionError reports fail-closed adapter construction failures.
type ConstructionError struct{ Reason string }

func (e *ConstructionError) Error() string {
	if e == nil || e.Reason == "" {
		return "llm: credential client construction failed"
	}
	return "llm: credential client construction failed: " + e.Reason
}

// LeaseError reports an invalid source lease before any authorization or I/O.
type LeaseError struct{ Reason string }

func (e *LeaseError) Error() string {
	if e == nil || e.Reason == "" {
		return "llm: invalid credential lease"
	}
	return "llm: invalid credential lease: " + e.Reason
}

// RecoveryError reports a safe source recovery boundary failure.
type RecoveryError struct {
	Stage string
	Err   error
}

func (e *RecoveryError) Error() string {
	if e == nil {
		return "llm: credential recovery failed"
	}
	if e.Err == nil {
		return "llm: credential recovery failed at " + e.Stage
	}
	return "llm: credential recovery failed at " + e.Stage + ": " + e.Err.Error()
}

func (e *RecoveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StaticSource is an immutable in-memory source used solely by the legacy
// API-key compatibility wrapper. It has a real descriptor and generation so it
// still passes the same exact policy and per-lease checks as a dynamic source.
type StaticSource struct {
	descriptor credentials.Descriptor
	reference  credentials.Reference
	generation credentials.Generation
	authorizer httpauth.Authorizer

	mu     sync.RWMutex
	closed bool
}

// NewStaticSource constructs a source around one already-built HTTP authorizer.
// The binding's scheme/usage/authority metadata is validated before the source
// can be used.
func NewStaticSource(binding llm.AuthBinding, authorizer httpauth.Authorizer) (*StaticSource, error) {
	if isNil(authorizer) {
		return nil, &ConstructionError{Reason: "static source authorizer is nil"}
	}
	descriptor, err := binding.Descriptor()
	if err != nil {
		return nil, err
	}
	if descriptor.Scheme != credentials.SchemeAPIKey {
		return nil, &ConstructionError{Reason: "static source requires api_key scheme"}
	}
	reference, err := credentials.NewReference(binding.Provider, "inline")
	if err != nil {
		return nil, err
	}
	generation, err := credentials.NewGeneration("static")
	if err != nil {
		return nil, err
	}
	return &StaticSource{descriptor: descriptor, reference: reference, generation: generation, authorizer: authorizer}, nil
}

func (s *StaticSource) Reference() credentials.Reference {
	if s == nil {
		return credentials.Reference{}
	}
	return s.reference
}

func (s *StaticSource) Descriptor() credentials.Descriptor {
	if s == nil {
		return credentials.Descriptor{}
	}
	return s.descriptor
}

func (s *StaticSource) Acquire(ctx context.Context) (credentials.Lease, error) {
	if ctx == nil {
		return nil, errors.New("llm: nil acquisition context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, &credentials.SourceClosedError{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, &credentials.SourceClosedError{}
	}
	return staticLease{generation: s.generation, descriptor: s.descriptor, authorizer: s.authorizer}, nil
}

func (s *StaticSource) Invalidate(ctx context.Context, generation credentials.Generation, failureClass credentials.Failure) error {
	if ctx == nil {
		return errors.New("llm: nil invalidation context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := generation.Validate(); err != nil {
		return err
	}
	if err := failureClass.Validate(); err != nil {
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
	// Static key values cannot refresh themselves. Matching and stale
	// generations are nevertheless accepted as a no-op so a late response never
	// mutates a future source generation.
	return nil
}

func (s *StaticSource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type staticLease struct {
	generation credentials.Generation
	descriptor credentials.Descriptor
	authorizer httpauth.Authorizer
}

func (l staticLease) Generation() credentials.Generation { return l.generation }
func (l staticLease) Descriptor() credentials.Descriptor { return l.descriptor }
func (l staticLease) ExpiresAt() time.Time               { return time.Time{} }
func (l staticLease) Authorizer() httpauth.Authorizer    { return l.authorizer }

// Compile-time assertions for the adapter's public contract.
var (
	_ inference.Client   = (*Client)(nil)
	_ credentials.Source = (*StaticSource)(nil)
)
