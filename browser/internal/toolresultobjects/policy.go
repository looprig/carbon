// Package toolresultobjects is Carbon's COMPOSITION GLUE for Factory's object
// route over a Host session's retained tool output: a production
// factory.ObjectPolicy built on committed journal evidence, the
// factory.SessionObjectStoreResolver that addresses the runtime's own store,
// the D7 ceiling check that ties the runtime's capture ceiling to Factory's
// verification ceiling, and a limiter in front of the evidence scan.
//
// It is copied from the looprig/tests integration lane's
// internal/toolresultobjects (the I2.2 reference shape), with one addition:
// FactoryOptions always composes the policy behind a RateLimitedPolicy (I2.2
// tests gate A4, see limiter.go).
//
// It lives in a composition because it must: Factory has no harness edge, so it
// cannot read a StepDone, and harness has no Factory edge, so it cannot answer
// a Factory seam.
//
// # The evidence rule (I2.2 decision D1)
//
// An object is served only when a COMMITTED StepDone in that session's runtime
// journal names its reference -- harness's
// (*sessionstore.Store).LookupToolResultCapture. SessionStore's metadata index
// is NOT that evidence: an upload whose StepDone never committed is an orphan
// with a perfectly good metadata row, and Factory's ObjectPolicy contract says
// so ("metadata index existence is not that evidence").
//
// # What a refusal is
//
// Every "you may not read this" wraps identity.ErrUnauthorized, which Factory
// (>= v0.11.0) answers with the SAME 404 body as an absent object. A store
// FAULT is not a refusal: it stays an ordinary error (500), because answering
// "absent" during an outage would tell a client its capture is gone. A
// throttled read (ErrRateLimited) is not a refusal either, for the same reason.
//
// # A known false absence (I2.2 tests gate A3)
//
// The evidence scan reads the runtime journal newest-first and stops after
// harness's ToolResultCaptureScanBudget (65,536) records. A capture with more
// than that many records committed after it, read by a Factory whose evidence
// cache does not already hold it (for example after a restart), is denied as
// absent (404) although it exists. The model's own read_tool_result is not
// affected: it resolves captures through the runtime's capture index.
package toolresultobjects

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sessionstore"
)

// Refusal reasons. Each denial wraps identity.ErrUnauthorized AND one of these,
// so an operator's log can say why while the client sees only absence.
var (
	// ErrForeignTenant: the principal's tenant is not the catalog entry's.
	ErrForeignTenant = errors.New("toolresultobjects: the principal's tenant does not own the session")
	// ErrUnknownBinding: the session's storage binding (or its tenant) is not
	// one this deployment composed, including a zero (legacy) binding.
	ErrUnknownBinding = errors.New("toolresultobjects: the session's storage binding is not composed here")
	// ErrNoRuntimeSession: the binding names no canonical runtime session.
	ErrNoRuntimeSession = errors.New("toolresultobjects: the binding names no runtime session")
	// ErrInvalidReference: the reference is not a well-formed object id.
	ErrInvalidReference = errors.New("toolresultobjects: the object reference is malformed")
	// ErrNoEvidence: no committed capture in this session's journal names the
	// reference -- an orphan, another session's object, or a forgery.
	ErrNoEvidence = errors.New("toolresultobjects: no committed capture names the reference")
	// ErrEvidenceBudget: the bounded journal scan ended before the journal's
	// start. The evidence may exist below it, so this is a refusal, never a
	// conclusion that the reference is foreign -- and never a grant.
	ErrEvidenceBudget = errors.New("toolresultobjects: the evidence scan budget was exhausted")
	// ErrCaptureCeiling: the runtime's capture ceiling exceeds Factory's
	// whole-object verification ceiling (I2.2 D7), or either is invalid.
	ErrCaptureCeiling = errors.New("toolresultobjects: the capture ceiling does not fit the object verification ceiling")
	// ErrIncompleteConfig: a constructor was handed a missing dependency.
	ErrIncompleteConfig = errors.New("toolresultobjects: incomplete configuration")
)

// Evidence is the committed-journal evidence a policy consults. The harness
// runtime store (*harness sessionstore.Store) implements it.
type Evidence interface {
	LookupToolResultCapture(ctx context.Context, runtimeSession uuid.UUID, ref sessionwire.ObjectReference) (event.ToolResultCapture, bool, error)
}

var _ Evidence = (*harnessstore.Store)(nil)

// Binding is the ONE storage binding a deployment composes: the same
// (StorageBindingID, BindingVersion) it passes factory.WithSessionBinding.
type Binding struct {
	StorageBindingID string
	BindingVersion   string
}

func (b Binding) complete() bool { return b.StorageBindingID != "" && b.BindingVersion != "" }

func (b Binding) matches(binding sessionstore.SessionBinding) bool {
	return binding.StorageBindingID == b.StorageBindingID && binding.BindingVersion == b.BindingVersion
}

// Policy is the production factory.ObjectPolicy. It grants
// sessionstore.ObjectKindToolResult for exactly the references a committed
// StepDone in the addressed session's RUNTIME journal names.
type Policy struct {
	binding  Binding
	evidence func(sessionwire.TenantID) (Evidence, bool)
}

var _ factory.ObjectPolicy = (*Policy)(nil)

// NewPolicy builds the policy for one composed binding. evidence answers the
// tenant's harness runtime store -- the SAME store its rig journals into and
// wires rig.WithToolResultObjects over -- and false for a tenant this
// deployment does not serve.
func NewPolicy(binding Binding, evidence func(sessionwire.TenantID) (Evidence, bool)) (*Policy, error) {
	if !binding.complete() || evidence == nil {
		return nil, fmt.Errorf("%w: NewPolicy needs a complete binding and an evidence lookup", ErrIncompleteConfig)
	}
	return &Policy{binding: binding, evidence: evidence}, nil
}

func deny(reason error, detail string) error {
	return fmt.Errorf("%w: %w: %s", identity.ErrUnauthorized, reason, detail)
}

// AuthorizeReference satisfies factory.ObjectPolicy.
func (p *Policy) AuthorizeReference(ctx context.Context, principal identity.Principal, entry sessionstore.CatalogEntry, ref sessionwire.ObjectReference) (sessionstore.ObjectKind, error) {
	record := entry.Record
	// Factory checks this too; the policy does not rely on it, because the
	// evidence it is about to read is tenant-scoped only by the lookup below.
	if record.TenantID != principal.Tenant() {
		return "", deny(ErrForeignTenant, string(record.SessionID))
	}
	binding := record.Binding
	if !p.binding.matches(binding) {
		return "", deny(ErrUnknownBinding, string(record.SessionID))
	}
	runtime, err := uuid.Parse(binding.RuntimeSessionID)
	if err != nil || runtime.IsZero() {
		return "", deny(ErrNoRuntimeSession, string(record.SessionID))
	}
	// Validated here, not left to the lookup: harness reports a malformed
	// reference as an ordinary error, which would surface as a 500 rather than
	// the absence a forged id deserves.
	if err := ref.Validate(); err != nil {
		return "", deny(ErrInvalidReference, string(record.SessionID))
	}
	evidence, ok := p.evidence(record.TenantID)
	if !ok || isNil(evidence) {
		return "", deny(ErrUnknownBinding, string(record.TenantID))
	}
	capture, found, err := evidence.LookupToolResultCapture(ctx, runtime, ref)
	if err != nil {
		var budget *harnessstore.ToolResultCaptureScanBudgetError
		if errors.As(err, &budget) {
			return "", fmt.Errorf("%w: %w: %w", identity.ErrUnauthorized, ErrEvidenceBudget, err)
		}
		return "", fmt.Errorf("toolresultobjects: reading capture evidence for %s: %w", record.SessionID, err)
	}
	if !found || capture.Reference == nil || capture.Reference.ObjectID != ref.ObjectID {
		return "", deny(ErrNoEvidence, string(record.SessionID))
	}
	return sessionstore.ObjectKindToolResult, nil
}

// NewResolver builds the factory.SessionObjectStoreResolver for one composed
// binding. objects answers the tenant's runtime store as an object reader --
// sessionstore.Open(ctx, runtimeBackend(tenant),
// sessionstore.WithLegacySingleTenant(tenant)), the same read the journal
// resolver answers. Factory rewrites every request to the binding's runtime
// session before calling it.
//
// An unknown binding or tenant is refused with ErrUnknownBinding (Factory
// answers 503): a resolver that answered a default would read one
// deployment's bytes under another's configuration.
func NewResolver(binding Binding, objects func(sessionwire.TenantID) (factory.ObjectReader, bool)) (factory.SessionObjectStoreResolver, error) {
	if !binding.complete() || objects == nil {
		return nil, fmt.Errorf("%w: NewResolver needs a complete binding and a store lookup", ErrIncompleteConfig)
	}
	return func(_ context.Context, tenant sessionwire.TenantID, _ sessionwire.SessionID, b sessionstore.SessionBinding) (factory.ObjectReader, error) {
		if !binding.matches(b) {
			return nil, fmt.Errorf("%w: %q/%q", ErrUnknownBinding, b.StorageBindingID, b.BindingVersion)
		}
		reader, ok := objects(tenant)
		if !ok || isNil(reader) {
			return nil, fmt.Errorf("%w: no runtime store for tenant %q", ErrUnknownBinding, tenant)
		}
		return reader, nil
	}, nil
}

// EffectiveCaptureBytes is the capture ceiling a loop actually applies for a
// declared loop.ToolLimits.CaptureBytes: zero means
// loop.DefaultToolResultCaptureBytes.
func EffectiveCaptureBytes(declared int) int {
	if declared == 0 {
		return loop.DefaultToolResultCaptureBytes
	}
	return declared
}

// CheckCaptureCeiling is I2.2 D7: the runtime's capture ceiling (a declared
// loop.ToolLimits.CaptureBytes, zero meaning harness's default) must not
// exceed Factory's whole-object verification ceiling, or every capture above
// it is retained and then answered 413 however it is paged. Neither module can
// see the other's number, so the composition asserts it.
func CheckCaptureCeiling(captureBytes int, limits factory.ObjectLimits) error {
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrCaptureCeiling, err)
	}
	effective := EffectiveCaptureBytes(captureBytes)
	if effective < 0 {
		return fmt.Errorf("%w: negative capture ceiling %d", ErrCaptureCeiling, captureBytes)
	}
	if uint64(effective) > limits.MaxVerificationBytes {
		return fmt.Errorf("%w: capture ceiling %d > verification ceiling %d", ErrCaptureCeiling, effective, limits.MaxVerificationBytes)
	}
	return nil
}

// Config is everything FactoryOptions composes.
type Config struct {
	Binding  Binding
	Evidence func(sessionwire.TenantID) (Evidence, bool)
	Objects  func(sessionwire.TenantID) (factory.ObjectReader, bool)
	// CaptureBytes is the runtime's declared loop.ToolLimits.CaptureBytes
	// (zero: harness's default). It is checked against Limits (D7).
	CaptureBytes int
	// Limits are the Factory object limits the deployment serves with.
	Limits factory.ObjectLimits
	// RateLimit bounds evidence scans per principal and per session. It is
	// required: the object route is never composed without it.
	RateLimit RateLimit
	// Now is the limiter's clock; nil means time.Now.
	Now func() time.Time
}

// FactoryOptions is the whole object-route recipe: the D7 check, then
// WithObjectPolicy (the evidence policy behind its limiter),
// WithSessionObjectStoreResolver and WithObjectLimits.
func FactoryOptions(cfg Config) ([]factory.Option, error) {
	if err := CheckCaptureCeiling(cfg.CaptureBytes, cfg.Limits); err != nil {
		return nil, err
	}
	policy, err := composedPolicy(cfg)
	if err != nil {
		return nil, err
	}
	resolver, err := NewResolver(cfg.Binding, cfg.Objects)
	if err != nil {
		return nil, err
	}
	return []factory.Option{
		factory.WithObjectPolicy(policy),
		factory.WithSessionObjectStoreResolver(resolver),
		factory.WithObjectLimits(cfg.Limits),
	}, nil
}

// composedPolicy is the evidence policy behind its limiter: a throttled call
// never reaches the journal scan.
func composedPolicy(cfg Config) (factory.ObjectPolicy, error) {
	policy, err := NewPolicy(cfg.Binding, cfg.Evidence)
	if err != nil {
		return nil, err
	}
	return NewRateLimitedPolicy(policy, cfg.RateLimit, cfg.Now)
}

// isNil reports a nil interface or a typed nil pointer inside one.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return value.IsNil()
	}
	return false
}
