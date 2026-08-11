package rig

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
)

type Option func(*definitionState) error

// singletonKey identifies an at-most-once rig option in definitionState.seen. The keys are
// the single source of truth shared by the option setters below and the auto-active-primer
// selection in definition.go, so a rename cannot silently desynchronize the two (a mismatch
// would defeat duplicate-option detection or the single-primer auto-active default).
type singletonKey string

const (
	keyActivePrimer                    singletonKey = "active_primer"
	keyDelegationLimits                singletonKey = "delegation_limits"
	keyConfigFingerprint               singletonKey = "config_fingerprint"
	keyForeignBuilder                  singletonKey = "foreign_builders"
	keyForeignServicesBuilder          singletonKey = "foreign_services_builders"
	keyRuntimeCatalog                  singletonKey = "runtime_catalog"
	keyGateCaps                        singletonKey = "gate_caps"
	keyAllowConfigMismatch             singletonKey = "allow_config_mismatch"
	keyRestoreDecider                  singletonKey = "restore_decider"
	keySnapshots                       singletonKey = "snapshots"
	keyOffloadGC                       singletonKey = "offload_gc"
	keyHustleLimits                    singletonKey = "hustle_limits"
	keyHooks                           singletonKey = "hooks"
	keyPermissionClassifiers           singletonKey = "permission_classifiers"
	keyPermissionReviewPolicy          singletonKey = "permission_review_policy"
	keyPermissionReviewLimits          singletonKey = "permission_review_limits"
	keyPermissionReviewEvidence        singletonKey = "permission_review_evidence"
	keyPermissionReviewSecurityCeiling singletonKey = "permission_review_security_ceiling"
	keyPermissionReviewObservations    singletonKey = "permission_review_observations"
	keySessionResourceStorage          singletonKey = "session_resource_storage"
)

// WithPermissionClassifiers installs the already-validated, ordered permission
// classifier registry. Registration order is behavioral and therefore remains
// significant in the rig fingerprint.
func WithPermissionClassifiers(classifiers gate.PermissionClassifierSet) Option {
	return func(state *definitionState) error {
		if state.seen[keyPermissionClassifiers] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyPermissionClassifiers)}
		}
		// Re-register the exported view at the rig boundary. This rejects the
		// zero value and freezes a new, independently owned registry view.
		frozen, err := gate.NewPermissionClassifierSet(classifiers.Classifiers()...)
		if err != nil {
			return &DefinitionError{Kind: DefinitionInvalidPermissionClassifiers, Cause: err}
		}
		state.seen[keyPermissionClassifiers] = true
		state.permissionClassifiers = frozen
		for _, classifier := range frozen.Classifiers() {
			state.hustles = append(state.hustles, classifier.Definition())
		}
		return nil
	}
}

// WithPermissionReviewPolicy installs the immutable local decision policy
// (design §20) every session forwards into the review runtime. Only the
// policy's Revision feeds rig identity (permissionReviewFingerprintFrom in
// fingerprint.go); the full value is what sessionruntime actually applies.
//
// A policy that was never built through gate.NewPermissionReviewPolicy or
// gate.DefaultPermissionReviewPolicy (a hand-built literal
// gate.PermissionReviewPolicy{}, whose zero seal makes it fail closed at
// EvaluatePermissionAssessment regardless) is rejected here immediately,
// rather than silently accepted and left to fail closed later with a
// confusing runtime symptom. This is a developer-experience improvement:
// the security property already holds without it.
func WithPermissionReviewPolicy(policy gate.PermissionReviewPolicy) Option {
	return func(state *definitionState) error {
		if state.seen[keyPermissionReviewPolicy] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyPermissionReviewPolicy)}
		}
		if !policy.Sealed() {
			return &DefinitionError{Kind: DefinitionInvalidPermissionReviewPolicy}
		}
		state.seen[keyPermissionReviewPolicy] = true
		state.permissionReviewPolicy = policy
		return nil
	}
}

// validPermissionReviewPolicyRevision is the defensive, belt-and-suspenders
// shape check permissionReviewFingerprintFrom (fingerprint.go) applies to a
// sealed policy's Revision before folding it into rig identity.
func validPermissionReviewPolicyRevision(revision string) bool {
	return utf8.ValidString(revision) &&
		revision != "" &&
		strings.TrimSpace(revision) == revision &&
		!strings.ContainsRune(revision, '\x00') &&
		len(revision) <= gate.MaxPermissionReviewPolicyRevisionBytes
}

// DefaultPermissionReviewBreakerThreshold is the default numeric
// circuit-breaker threshold Define() resolves for every turn-scoped and
// session-scoped counter (resolvePermissionReviewLimits in definition.go)
// when classifiers are configured but WithPermissionReviewLimits was never
// explicitly called.
const DefaultPermissionReviewBreakerThreshold = 20

// PermissionReviewLimits are the consumer-configurable bounded per-turn and
// per-session circuit-breaker thresholds (design §18) — the rig-level
// mirror of sessionruntime.PermissionReviewBreakerLimits. They are
// deliberately EXCLUDED from the rig fingerprint (fingerprint.go): these are
// operational tuning knobs, not behavioral identity, so two rigs that agree
// on classifiers and policy but differ only in these thresholds compare
// equal.
type PermissionReviewLimits struct {
	MaxConsecutiveNeedsHuman int
	MaxInvalidOrFailed       int
	MaxIdenticalSubjects     int
	MaxStaleResponses        int
	InterruptOnTrip          bool
	Session                  PermissionReviewSessionLimits
}

// PermissionReviewSessionLimits is PermissionReviewLimits' session-scoped
// counterpart (design §18: "per-turn AND per-session").
type PermissionReviewSessionLimits struct {
	MaxConsecutiveNeedsHuman int
	MaxInvalidOrFailed       int
	MaxIdenticalSubjects     int
	MaxStaleResponses        int
}

// WithPermissionReviewLimits installs the circuit-breaker thresholds
// (design §18) applied to automatic permission review. It is only
// meaningful paired with WithPermissionClassifiers; Define() enforces that
// pairing (DefinitionUnusedPermissionReviewLimits) rather than this Option,
// because option application order is not guaranteed (classifiers may be
// registered before or after this call in the Define() argument list).
//
// Omitting this option while classifiers ARE configured resolves a default
// of DefaultPermissionReviewBreakerThreshold on every one of the 8 numeric
// thresholds (resolvePermissionReviewLimits, definition.go); an explicit
// call always replaces that default wholesale — all 8 fields at once, never
// merged per-field.
func WithPermissionReviewLimits(limits PermissionReviewLimits) Option {
	return func(state *definitionState) error {
		if state.seen[keyPermissionReviewLimits] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyPermissionReviewLimits)}
		}
		state.seen[keyPermissionReviewLimits] = true
		state.permissionReviewLimits = limits
		return nil
	}
}

// WithPermissionReviewEvidence installs the consumer-supplied read-only
// evidence-tool access boundary every registered permission classifier's
// evidence tools run under (design §13.1). access answers the configured
// access state for one prepared evidence Requirement; containment
// independently performs the trusted-caller containment check (resolving
// symlinks, rejecting ambiguous scopes, enforcing the review's own security
// ceiling); allowedKinds is the explicit consumer-consent allowlist of
// Requirement.Kind values evidence tools may declare. Both access and
// containment are read-only, headless, trusted-caller seams — neither
// receives session, gate, mutation, grant, rule, or loop-control capability
// (design §13.1's Access/Containment split; Access alone was omitted from
// the design sketch of this option's signature, but the same fail-closed
// requirement applies to it: hustleruntime refuses to bind an evidence
// catalog with a nil Access evaluator exactly as it refuses a nil
// Containment verifier).
//
// Required whenever any registered classifier's definition needs evidence
// tools; Define fails closed (DefinitionMissingPermissionReviewEvidence) if
// omitted in that case, and rejects it (DefinitionUnusedPermissionReviewEvidence)
// when supplied but no registered classifier needs it — mirroring the
// existing MissingHustleLimits/UnusedHustleLimits "config X requires config
// Y" pairing already used elsewhere in this file.
func WithPermissionReviewEvidence(access gate.EvidenceAccessEvaluator, containment gate.EvidenceContainmentVerifier, allowedKinds []string) Option {
	return func(state *definitionState) error {
		if access == nil || containment == nil || len(allowedKinds) == 0 {
			return &DefinitionError{Kind: DefinitionInvalidPermissionReviewEvidence}
		}
		frozenKinds := append([]string(nil), allowedKinds...)
		return singletonCompile(keyPermissionReviewEvidence, sessionruntime.WithLifecyclePermissionReviewEvidence(access, containment, frozenKinds))(state)
	}
}

// WithPermissionReviewSecurityCeiling installs the consumer-supplied,
// effective security posture (design §13.1/§21) every registered permission
// classifier's ReviewContext/ReviewBasis carries as SecurityCeiling, and
// every evidence-tool containment check (WithPermissionReviewEvidence's
// Containment collaborator) is run against.
//
// SecurityCeiling is architecturally the SAME KIND of value as the
// Containment/AllowedKinds collaborators WithPermissionReviewEvidence
// installs — a consumer-owned concept Harness structurally cannot and
// should not originate (this module has no first-class "effective access
// posture" notion; the product composition root binds its own AccessProfile
// name here). It is
// NOT like a workspace root, which Harness genuinely owns and auto-derives.
// A plain string, not a provider func: a consumer's ceiling is fixed for the
// session's lifetime by design (YAGNI — see the design consult this option
// was written from).
//
// Required whenever any permission classifier is registered
// (WithPermissionClassifiers); Define fails closed
// (DefinitionMissingPermissionReviewSecurityCeiling) if omitted in that
// case — before this option existed, every session instead stamped a fixed
// Harness-side sentinel that could never equal a real consumer's own
// ceiling, so every real evidence-tool containment check failed closed
// unconditionally (Finding 2, Phase 6 spec-compliance review). It is
// rejected (DefinitionUnusedPermissionReviewSecurityCeiling) when supplied
// but no classifiers are configured, mirroring
// WithPermissionReviewEvidence's own "config X requires config Y" pairing.
// An empty (or all-whitespace) ceiling is rejected here, immediately, at
// Define() time — never deferred to a later, harder-to-diagnose
// review-context-capture failure (gate.ReviewContext's own non-empty
// SecurityCeiling validation rule already fails closed on an empty value,
// but silently and much later).
func WithPermissionReviewSecurityCeiling(ceiling string) Option {
	return func(state *definitionState) error {
		if strings.TrimSpace(ceiling) == "" {
			return &DefinitionError{Kind: DefinitionInvalidPermissionReviewSecurityCeiling}
		}
		return singletonCompile(keyPermissionReviewSecurityCeiling, sessionruntime.WithLifecyclePermissionReviewSecurityCeiling(ceiling))(state)
	}
}

// WithPermissionReviewObservations installs the consumer-supplied,
// read-only TOCTOU-recheck seam (design §13.4) every classifier-originated
// auto-approval's recorded observations are verified against immediately
// before the gate is claimed. verifier independently re-derives each
// previously recorded gate.ObservationRequirement's current token and fails
// closed (leaving the human gate open) on any mismatch or unverifiable
// target — see gate.EvidenceObservationVerifier's own doc comment for the
// full trusted-caller contract, which deliberately reuses
// gate.EvidenceContainmentPolicy rather than introducing a parallel policy
// type (the security context — canonical read root plus the review's own
// non-widenable ceiling — is identical to WithPermissionReviewEvidence's own
// Containment collaborator).
//
// A separate option, deliberately NOT folded into WithPermissionReviewEvidence's
// signature: the two are independent concerns (an evidence-tool access
// boundary a session needs whenever ANY registered classifier declares
// evidence tools, versus a TOCTOU recheck a session needs only when at least
// one of those evidence tools is target-sensitive). Folding Observations in
// as a fourth WithPermissionReviewEvidence parameter would force every
// existing and future caller with no target-sensitive evidence tools — the
// common case today, since no evidence tool in this codebase declares
// itself target-sensitive yet — to pass an explicit nil at that call site
// for a concern it will never use, which is exactly the Interface
// Segregation violation this package's own CLAUDE.md guidance warns
// against.
//
// Required (rejected as DefinitionUnusedPermissionReviewObservations)
// unless BOTH at least one permission classifier is registered AND
// WithPermissionReviewEvidence is also configured — mirroring the "config X
// requires config Y" pairing precedent, but ONLY in the unused direction.
// There is deliberately NO symmetric "missing" pairing error the way
// WithPermissionReviewEvidence itself is required whenever
// anyClassifierNeedsEvidence(state.permissionClassifiers) reports true:
// that check is possible because hustle.Definition.EvidenceToolPolicy()
// already gives Define() a real, static, per-classifier "does this need an
// evidence runtime at all" signal. Nothing analogous exists for "is any of
// THIS classifier's evidence tools target-sensitive" — no evidence tool
// definition in this codebase declares that today (it is a per-concrete-tool
// capability, tool.EvidenceObservation, probed only at runtime after a call
// executes, not a static Definition-level property Define() could inspect
// ahead of time the way it inspects EvidenceToolPolicy). Manufacturing a
// parallel static declarer purely to unlock a Define()-time "missing"
// check, before any real tool exists that would ever set it, would be
// speculative generality with no consumer to validate it against. Instead,
// runtime fails closed: internal/sessionruntime/gates.go's
// verifyPermissionReviewObservations treats "an observation WAS recorded
// but no verifier is configured" identically to a genuine mismatch — stale,
// human gate stays open, never a silent pass — so a consumer who adds a
// target-sensitive evidence tool later without also wiring this option
// gets an always-safe (if initially confusing, and always correctable)
// runtime outcome rather than a false sense of protection. If a future
// evidence-tool capability gives Define() a real static signal, tightening
// this to a "missing" pairing error too is a natural, non-breaking
// follow-up — see this addendum's plan section for the explicit flag.
func WithPermissionReviewObservations(verifier gate.EvidenceObservationVerifier) Option {
	return func(state *definitionState) error {
		if verifier == nil {
			return &DefinitionError{Kind: DefinitionInvalidPermissionReviewObservations}
		}
		return singletonCompile(keyPermissionReviewObservations, sessionruntime.WithLifecyclePermissionReviewObservations(verifier))(state)
	}
}

// MaxHustleQueued is the largest configured waiting capacity for either hustle
// lane. The execution controller may allocate no queue larger than this bound.
const MaxHustleQueued = 10_000

type DelegationLimits struct {
	Depth int
	Quota int
}

type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

// HustleLimits bounds the two independent execution lanes and their audit,
// finalization, and worker-drain operations.
type HustleLimits struct {
	BlockingConcurrent   int
	BlockingQueued       int
	BackgroundConcurrent int
	BackgroundQueued     int
	AuditTimeout         time.Duration
	FinalizationTimeout  time.Duration
	WorkerDrainTimeout   time.Duration
}

func WithLoops(definitions ...loop.Definition) Option {
	copyOf := append([]loop.Definition(nil), definitions...)
	return func(state *definitionState) error {
		state.loops = append(state.loops, copyOf...)
		return nil
	}
}

// WithHustles adds immutable hustle definitions to the rig.
func WithHustles(definitions ...hustle.Definition) Option {
	copyOf := append([]hustle.Definition(nil), definitions...)
	return func(state *definitionState) error {
		state.hustles = append(state.hustles, copyOf...)
		return nil
	}
}

// WithHustleLimits configures the required singleton lane bounds.
func WithHustleLimits(limits HustleLimits) Option {
	return func(state *definitionState) error {
		if state.seen[keyHustleLimits] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyHustleLimits)}
		}
		if invalidHustleLimits(limits) {
			return &DefinitionError{Kind: DefinitionInvalidHustleLimits}
		}
		return singleton(keyHustleLimits, func(state *definitionState) { state.hustleLimits = limits })(state)
	}
}

func invalidHustleLimits(limits HustleLimits) bool {
	return limits.BlockingConcurrent <= 0 ||
		limits.BlockingQueued < 0 || limits.BlockingQueued > MaxHustleQueued ||
		limits.BackgroundConcurrent <= 0 ||
		limits.BackgroundQueued < 0 || limits.BackgroundQueued > MaxHustleQueued ||
		limits.AuditTimeout <= 0 || limits.FinalizationTimeout <= 0 || limits.WorkerDrainTimeout <= 0
}

func WithPrimers(names ...string) Option {
	copyOf := append([]string(nil), names...)
	return func(state *definitionState) error {
		state.primers = append(state.primers, copyOf...)
		return nil
	}
}

func WithActivePrimer(name string) Option {
	return singleton(keyActivePrimer, func(state *definitionState) { state.activePrimer = name })
}

func WithSessionStore(store *sessionstore.Store) Option {
	return func(state *definitionState) error {
		if store == nil {
			return &DefinitionError{Kind: DefinitionInvalidSessionStore}
		}
		if state.storeSet {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: "session_store"}
		}
		state.storeSet = true
		state.store = store
		return nil
	}
}

func WithDelegationLimits(limits DelegationLimits) Option {
	return func(state *definitionState) error {
		if limits.Depth < 0 || limits.Quota < 0 {
			return &DefinitionError{Kind: DefinitionInvalidDelegationLimits}
		}
		return singletonCompile(keyDelegationLimits, sessionruntime.WithLifecycleLimits(sessionruntime.Limits{Depth: limits.Depth, Quota: limits.Quota}))(state)
	}
}

func WithFingerprintFields(fields ConfigFingerprintFields) Option {
	return singleton(keyConfigFingerprint, func(state *definitionState) { state.fingerprintFields = fields })
}

// WithHooks installs one immutable operation-hook set. Define validates and
// compiles the captured set after every option has resolved.
func WithHooks(set hook.Set) Option {
	captured := cloneHookSet(set)
	return singleton(keyHooks, func(state *definitionState) {
		state.hooks = cloneHookSet(captured)
	})
}

func cloneHookSet(set hook.Set) hook.Set {
	set.Guards = append([]hook.Guard(nil), set.Guards...)
	set.Around = append([]hook.Around(nil), set.Around...)
	return set
}

func WithForeignBuilders(builder foreign.Builder, restored foreign.RestoredBuilder) Option {
	return func(state *definitionState) error {
		if builder == nil || restored == nil {
			return &DefinitionError{Kind: DefinitionInvalidForeignBuilders}
		}
		return singletonCompile(keyForeignBuilder, sessionruntime.WithLifecycleForeignBuilders(builder, restored))(state)
	}
}

// WithForeignServicesBuilders opts a rig into the additive foreign-engine
// seam. Each live/restored foreign origin receives a fresh loop-scoped broker
// descriptor and delivery hook at session construction; no capability is part
// of the immutable rig configuration. The legacy WithForeignBuilders option
// remains supported and receives zero services.
func WithForeignServicesBuilders(builder foreign.ServicesBuilder, restored foreign.ServicesRestoredBuilder) Option {
	return func(state *definitionState) error {
		if builder == nil || restored == nil {
			return &DefinitionError{Kind: DefinitionInvalidForeignBuilders}
		}
		return singletonCompile(keyForeignServicesBuilder,
			sessionruntime.WithLifecycleForeignServicesBuilders(builder, restored))(state)
	}
}

// WithRuntimeCatalog installs the immutable parent-scoped runtime catalog
// forwarded to every new and restored session.
func WithRuntimeCatalog(catalog loop.RuntimeCatalog) Option {
	return singletonCompile(keyRuntimeCatalog, sessionruntime.WithLifecycleRuntimeCatalog(catalog))
}

func WithGateCaps(caps GateCaps) Option {
	return func(state *definitionState) error {
		if caps.MaxOpen < 0 || caps.MaxTimeout < 0 {
			return &DefinitionError{Kind: DefinitionInvalidGateCaps}
		}
		return singletonCompile(keyGateCaps, sessionruntime.WithLifecycleGateCaps(sessionruntime.GateCaps{MaxOpen: caps.MaxOpen, MaxTimeout: caps.MaxTimeout}))(state)
	}
}

func WithAllowConfigMismatch() Option {
	return singletonCompile(keyAllowConfigMismatch, sessionruntime.WithLifecycleAllowConfigMismatch())
}

// WithRestoreDecider installs the application policy that decides whether a
// configuration-drifted restore proceeds. It is the successor to
// WithAllowConfigMismatch: rather than a blanket override, the decider inspects the
// typed drift assessment and accepts or rejects. Omitting it leaves restore on the
// fail-secure session.DefaultPolicyDecider (reject on any Warn). A nil decider is
// rejected at definition time so the option cannot silently disarm the default.
func WithRestoreDecider(decider session.RestoreDecider) Option {
	return func(state *definitionState) error {
		if decider == nil {
			return &DefinitionError{Kind: DefinitionInvalidRestoreDecider}
		}
		return singletonCompile(keyRestoreDecider, sessionruntime.WithLifecycleRestoreDecider(decider))(state)
	}
}

func singleton(name singletonKey, apply func(*definitionState)) Option {
	return func(state *definitionState) error {
		if state.seen[name] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(name)}
		}
		state.seen[name] = true
		apply(state)
		return nil
	}
}

func singletonCompile(name singletonKey, option sessionruntime.LifecycleOption) Option {
	return singleton(name, func(state *definitionState) { state.lifecycleOptions = append(state.lifecycleOptions, option) })
}
