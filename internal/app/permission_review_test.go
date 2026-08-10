package app

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/classifiers/pkg/commandsafety"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// permission_review_test.go exercises Task 23's own scope: the CONFIG SURFACE
// of classifier-based permission review (enable/disable, model requirement,
// policy selection, duplicate rejection, ceiling inheritance, headless
// composition safety, and fingerprint sensitivity). It deliberately never
// exercises a live classifier review round trip. Task 23 stopped at
// rig.Define/construction because internal/sessionruntime did not yet wire
// the evidence-tool runtime a registered classifier needs to actually
// execute; Task 24 (permission_review_evidence.go +
// rig.WithPermissionReviewEvidence, wired in by newPermissionReviewRegistration
// above) closes that specific gap, and permission_review_integration_test.go
// now exercises session construction/start and the end-to-end scenarios this
// file intentionally left for later.

// permissionReviewTestModel is a minimal model.Model satisfying
// commandsafety.New's capability requirements (Tools, StructuredOutput, and
// StructuredOutputWithTools) — testModel() (fake_test.go) deliberately omits
// these, since ordinary Loop tests do not need them.
func permissionReviewTestModel() model.Model {
	return model.CustomModel(
		model.ProviderName("test"), model.APIFormatOpenAI,
		"http://localhost:1234/v1", "test-classifier-model",
		model.WithTools(), model.WithStructuredOutput(), model.WithStructuredOutputWithTools(),
	)
}

// permissionReviewSubjectFixture builds one publicly-constructible, valid
// gate.PermissionReviewSubject for a command-execute Bash request, with its
// GatePolicyRevision bound to the given local decision policy revision (the
// revision EvaluatePermissionAssessment matches against). It mirrors the
// shape harness's own gate package tests use, built entirely from harness's
// public API.
func permissionReviewSubjectFixture(t *testing.T, gatePolicyRevision string) gate.PermissionReviewSubject {
	t.Helper()
	toolExecutionID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174110")
	context := gate.ReviewContext{
		Coordinates: identity.Coordinates{
			SessionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174101"),
			LoopID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174102"),
			TurnID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174103"),
			StepID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174104"),
		},
		ContextRevision:    "context-v1",
		WorkspaceRoot:      "/workspace",
		WorkingDirectory:   "/workspace/repo",
		SecurityCeiling:    "workspace-write",
		GatePolicyRevision: gatePolicyRevision,
		Entries: []gate.ReviewContextEntry{
			{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "inspect the repository"},
			{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: `{"command":"git status"}`},
		},
	}
	request := tool.Request{
		ToolName:           "Bash",
		Summary:            "run git status",
		ExecutionID:        toolExecutionID.String(),
		Command:            "git status",
		WorkingDirectory:   "/workspace/repo",
		ExpiresAtUnixMilli: 1900000000000,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       "git status",
			Description: "run git status",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: "git status",
			Candidates: []tool.RuleCandidate{{
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(git status)",
				Description: "Bash(git status)",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: "git status",
			}},
		}},
	}
	basis := gate.ReviewBasis{
		GateID:             uuid.MustParse("123e4567-e89b-12d3-a456-426614174109"),
		ToolExecutionID:    toolExecutionID,
		ContextRevision:    context.ContextRevision,
		GatePolicyRevision: context.GatePolicyRevision,
		ClassifierRevision: "command-safety-v1",
		SecurityCeiling:    context.SecurityCeiling,
	}
	subject, err := gate.NewPermissionReviewSubject(basis, request, context)
	if err != nil {
		t.Fatalf("gate.NewPermissionReviewSubject() error = %v", err)
	}
	return subject
}

// TestPermissionReviewOffByDefault proves requirement 1: a zero Config never
// enables permission review, and constructing a registration over it yields
// the disabled zero value with no rig options — a zero Config never
// auto-approves anything.
func TestPermissionReviewOffByDefault(t *testing.T) {
	t.Parallel()

	var cfg Config
	if cfg.PermissionReviewEnabled {
		t.Fatal("zero Config.PermissionReviewEnabled = true, want false")
	}

	registration, err := newPermissionReviewRegistration(cfg, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	if registration.enabled {
		t.Error("registration.enabled = true, want false for a zero Config")
	}
	if options := registration.options(); len(options) != 0 {
		t.Errorf("options() = %d options, want 0 for a disabled registration", len(options))
	}
}

// TestPermissionReviewExplicitEnable proves requirement 2: an explicit
// Config opt-in with a named model builds an enabled registration that
// installs both the classifier set and the review policy rig options, and
// that rig.Define (buildRigForDelegationCaps) accepts the resulting
// composition.
//
// It deliberately stops at rig.Define/construction and never calls
// assembly.NewSession: internal/sessionruntime does not yet wire
// hustleruntime.RuntimeConfig.Evidence, so starting a session whose hustle
// set includes a classifier's evidence tools currently fails closed with
// *hustleruntime.ConfigError{Reason: ConfigMissingCollaborator, Field:
// "runtime.evidence"} — a known, explicitly out-of-scope cross-cutting
// Harness gap (see the plan's Task 24 addendum), not a Carbon config-surface
// defect. Task 23's own scope is proving the config surface composes
// correctly, which construction success already demonstrates.
func TestPermissionReviewExplicitEnable(t *testing.T) {
	t.Parallel()

	cfg := Config{PermissionReviewEnabled: true, PermissionReviewModel: permissionReviewTestModel(), AccessProfile: AccessTrusted}
	registration, err := newPermissionReviewRegistration(cfg, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	if !registration.enabled {
		t.Fatal("registration.enabled = false, want true")
	}
	if options := registration.options(); len(options) != 5 {
		t.Fatalf("options() = %d options, want exactly 5 (classifiers + policy + evidence + observations + security ceiling)", len(options))
	}

	root := t.TempDir()
	access, sessionCfg := headlessTestAccess(t, cfg, root)
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), sessionCfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	if _, err := buildRigForDelegationCaps(
		definition, stores, root, sessionCfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration,
	); err != nil {
		t.Fatalf("buildRigForDelegationCaps() error = %v", err)
	}
}

// TestPermissionReviewSecurityCeilingOptionInstalled proves the Phase 6
// spec-compliance review's Finding 2 fix (Harness commit bed51463): an
// enabled registration installs rig.WithPermissionReviewSecurityCeiling
// alongside the classifier set. Before this fix, options() omitted it
// entirely, and rig.Define — which now REQUIRES a security ceiling whenever
// any permission classifier is registered — rejected every classifier
// registration outright with
// DefinitionMissingPermissionReviewSecurityCeiling (verified as the RED
// state this test's fix resolves: TestPermissionReviewExplicitEnable and
// TestPermissionReviewHeadlessComposesSafely both failed with exactly that
// error until this option was wired). Successful construction here is
// therefore direct proof the option is present and non-empty, not just that
// registration.options() grew by one.
func TestPermissionReviewSecurityCeilingOptionInstalled(t *testing.T) {
	t.Parallel()

	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
		AccessProfile:           AccessTrusted,
	}, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	if registration.securityCeiling == "" {
		t.Fatal("registration.securityCeiling is empty, want a non-empty consumer-supplied ceiling")
	}

	root := t.TempDir()
	access, sessionCfg := headlessTestAccess(t, Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
		AccessProfile:           AccessTrusted,
	}, root)
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), sessionCfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	if _, err := buildRigForDelegationCaps(
		definition, stores, root, sessionCfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration,
	); err != nil {
		t.Fatalf("buildRigForDelegationCaps() error = %v, want success now that WithPermissionReviewSecurityCeiling is wired", err)
	}
}

// TestPermissionReviewSecurityCeilingMatchesEvidenceContainment proves the
// task brief's single-source-of-truth requirement directly: the ceiling value
// installed via rig.WithPermissionReviewSecurityCeiling (registration.securityCeiling)
// is byte-for-byte the SAME value permissionReviewEvidenceContainment
// independently compares every evidence-tool containment check against
// (registration.evidenceContainment.ceiling — the unexported field is
// directly readable here since this test lives in the same package). Both
// are derived from the one evidenceCeilingFor(cfg.AccessProfile) call in
// newPermissionReviewRegistration, so they can never silently drift apart —
// but that call, and therefore this whole proof, only happens for
// AccessTrusted: newPermissionReviewRegistration's trusted-profile gate
// (settled 2026-08-05) returns the disabled zero registration for every other
// named profile before evidenceCeilingFor is ever consulted, no matter how
// PermissionReviewEnabled became true. Only the AccessTrusted subtest
// exercises the original single-source-of-truth assertions; every other
// profile asserts silent disablement instead.
func TestPermissionReviewSecurityCeilingMatchesEvidenceContainment(t *testing.T) {
	t.Parallel()

	for _, profile := range []AccessProfile{AccessReadOnly, AccessTrusted, AccessUnconfined, ""} {
		t.Run(string(profile)+"/empty-means-default", func(t *testing.T) {
			t.Parallel()
			registration, err := newPermissionReviewRegistration(Config{
				PermissionReviewEnabled: true,
				PermissionReviewModel:   permissionReviewTestModel(),
				AccessProfile:           profile,
			}, &fakeLLM{})
			if err != nil {
				t.Fatalf("newPermissionReviewRegistration() error = %v", err)
			}

			if profile != AccessTrusted {
				if registration.enabled {
					t.Fatalf("registration.enabled = true for profile %q, want false (silently disabled outside AccessTrusted)", profile)
				}
				if registration.securityCeiling != "" {
					t.Fatalf("registration.securityCeiling = %q, want empty (zero value) for a non-trusted profile", registration.securityCeiling)
				}
				if options := registration.options(); len(options) != 0 {
					t.Fatalf("options() = %d options, want 0 for a gate-disabled registration", len(options))
				}
				return
			}

			if !registration.enabled {
				t.Fatal("registration.enabled = false for AccessTrusted, want true")
			}
			containment, ok := registration.evidenceContainment.(permissionReviewEvidenceContainment)
			if !ok {
				t.Fatalf("registration.evidenceContainment = %T, want permissionReviewEvidenceContainment", registration.evidenceContainment)
			}
			if registration.securityCeiling != containment.ceiling {
				t.Fatalf("registration.securityCeiling = %q, evidenceContainment.ceiling = %q, want identical (single source of truth)",
					registration.securityCeiling, containment.ceiling)
			}
			if registration.securityCeiling != evidenceCeilingFor(profile) {
				t.Fatalf("registration.securityCeiling = %q, want evidenceCeilingFor(%q) = %q",
					registration.securityCeiling, profile, evidenceCeilingFor(profile))
			}
		})
	}
}

// TestPermissionReviewTrustedProfileGate proves the trusted-profile gate
// itself (settled with the user 2026-08-05): permission review only ever
// takes effect when Config.AccessProfile == AccessTrusted, no matter how
// PermissionReviewEnabled became true. Every non-trusted profile silently
// disables review — the identical zero registration PermissionReviewEnabled
// == false already produces, with no error and no log.
func TestPermissionReviewTrustedProfileGate(t *testing.T) {
	t.Parallel()

	for _, profile := range []AccessProfile{AccessReadOnly, AccessUnconfined, ""} {
		t.Run(string(profile), func(t *testing.T) {
			t.Parallel()
			registration, err := newPermissionReviewRegistration(Config{
				PermissionReviewEnabled: true,
				PermissionReviewModel:   permissionReviewTestModel(),
				AccessProfile:           profile,
			}, &fakeLLM{})
			if err != nil {
				t.Fatalf("newPermissionReviewRegistration() error = %v", err)
			}
			if registration.enabled {
				t.Fatalf("registration.enabled = true for profile %q, want false", profile)
			}
			if registration.securityCeiling != "" {
				t.Fatalf("registration.securityCeiling = %q, want empty (zero value)", registration.securityCeiling)
			}
			if options := registration.options(); len(options) != 0 {
				t.Fatalf("options() = %d options, want 0 for a gate-disabled registration", len(options))
			}
		})
	}

	t.Run(string(AccessTrusted), func(t *testing.T) {
		t.Parallel()
		registration, err := newPermissionReviewRegistration(Config{
			PermissionReviewEnabled: true,
			PermissionReviewModel:   permissionReviewTestModel(),
			AccessProfile:           AccessTrusted,
		}, &fakeLLM{})
		if err != nil {
			t.Fatalf("newPermissionReviewRegistration() error = %v", err)
		}
		if !registration.enabled {
			t.Fatal("registration.enabled = false for AccessTrusted, want true")
		}
	})
}

// TestPermissionReviewNamedModelRequired proves requirement 3: enabling
// permission review without a named PermissionReviewModel fails closed with a
// typed *PermissionReviewConfigError, rather than silently reusing an
// Carbon Loop's model.
func TestPermissionReviewNamedModelRequired(t *testing.T) {
	t.Parallel()

	cfg := Config{PermissionReviewEnabled: true, AccessProfile: AccessTrusted}
	_, err := newPermissionReviewRegistration(cfg, &fakeLLM{})
	var target *PermissionReviewConfigError
	if !errors.As(err, &target) {
		t.Fatalf("newPermissionReviewRegistration() error = %T %v, want *PermissionReviewConfigError", err, err)
	}
}

// TestPermissionReviewSelectableStrictDefaultPolicy proves requirement 4: the
// default and strict local decision policies are both well-formed and
// distinct, and the strict policy is BEHAVIORALLY stricter — never looser —
// than the default: the identical medium-risk assessment that the default
// policy finds eligible, the strict policy rejects on its lowered risk
// ceiling.
func TestPermissionReviewSelectableStrictDefaultPolicy(t *testing.T) {
	t.Parallel()

	defaultPolicy, err := permissionReviewPolicyFor(false)
	if err != nil {
		t.Fatalf("permissionReviewPolicyFor(false) error = %v", err)
	}
	strictPolicy, err := permissionReviewPolicyFor(true)
	if err != nil {
		t.Fatalf("permissionReviewPolicyFor(true) error = %v", err)
	}
	if !defaultPolicy.Sealed() || !strictPolicy.Sealed() {
		t.Fatal("both policies must be sealed (constructed through the gate constructors)")
	}
	if defaultPolicy.Revision == strictPolicy.Revision {
		t.Fatal("default and strict policies share a Revision, want distinct")
	}
	if defaultPolicy.MaximumAutoRisk != gate.ReviewRiskHigh {
		t.Errorf("default MaximumAutoRisk = %q, want %q", defaultPolicy.MaximumAutoRisk, gate.ReviewRiskHigh)
	}
	if strictPolicy.MaximumAutoRisk != gate.ReviewRiskLow {
		t.Errorf("strict MaximumAutoRisk = %q, want %q (stricter than default)", strictPolicy.MaximumAutoRisk, gate.ReviewRiskLow)
	}

	assessment := gate.PermissionAssessment{
		Risk:           gate.ReviewRiskMedium,
		Authorization:  gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow,
		Rationale:      "read-only diagnostic command",
	}

	defaultSubject := permissionReviewSubjectFixture(t, defaultPolicy.Revision)
	defaultAssessment := assessment
	defaultAssessment.Basis = defaultSubject.Basis
	defaultDecision := gate.EvaluatePermissionAssessment(defaultPolicy, defaultSubject, defaultAssessment)
	if !defaultDecision.Eligible {
		t.Fatalf("default policy decision = %+v, want Eligible for a medium-risk allow with unknown authorization", defaultDecision)
	}

	strictSubject := permissionReviewSubjectFixture(t, strictPolicy.Revision)
	strictAssessment := assessment
	strictAssessment.Basis = strictSubject.Basis
	strictDecision := gate.EvaluatePermissionAssessment(strictPolicy, strictSubject, strictAssessment)
	if strictDecision.Eligible {
		t.Fatalf("strict policy decision = %+v, want NOT Eligible (risk ceiling) for the SAME assessment the default policy allows", strictDecision)
	}
	if strictDecision.Reason != gate.ReviewDecisionRiskCeiling {
		t.Errorf("strict policy decision reason = %q, want %q", strictDecision.Reason, gate.ReviewDecisionRiskCeiling)
	}
}

// TestPermissionReviewDuplicateClassifierRegistrationRejected proves
// requirement 5 at the exact registry boundary newPermissionReviewRegistration
// calls (gate.NewPermissionClassifierSet): two classifiers sharing the same
// registration Name — the only way a duplicate could ever arise, since
// commandsafety.Classifier.Name() is a fixed constant — are rejected with a
// typed PermissionClassifierDuplicate reason, so a future bug that registered
// more than one command-safety classifier could never silently succeed.
func TestPermissionReviewDuplicateClassifierRegistrationRejected(t *testing.T) {
	t.Parallel()

	newClassifier := func(t *testing.T) gate.PermissionClassifier {
		t.Helper()
		classifier, err := commandsafety.New(commandsafety.Options{
			Inference: &fakeLLM{},
			Model:     permissionReviewTestModel(),
			Policy:    commandsafety.DefaultPolicy(),
			Evidence:  commandsafety.StandardEvidence(commandsafety.ReadEvidencePolicy{}),
		})
		if err != nil {
			t.Fatalf("commandsafety.New() error = %v", err)
		}
		return classifier
	}

	first := newClassifier(t)
	second := newClassifier(t)
	_, err := gate.NewPermissionClassifierSet(first, second)
	var target *gate.PermissionClassifierValidationError
	if !errors.As(err, &target) || target.Reason != gate.PermissionClassifierDuplicate {
		t.Fatalf("gate.NewPermissionClassifierSet() error = %T %v, want PermissionClassifierValidationError{Reason: PermissionClassifierDuplicate}", err, err)
	}
}

// TestPermissionReviewRigOptionDoubleRegistrationRejected proves requirement
// 5 at Carbon's own composition boundary: appending the SAME registration's
// rig options twice into one rig.Define call — the shape a future wiring bug
// could produce — fails closed with rig's singleton-option protection, rather
// than silently accepting a duplicate classifier registration.
func TestPermissionReviewRigOptionDoubleRegistrationRejected(t *testing.T) {
	t.Parallel()

	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
		AccessProfile:           AccessTrusted,
	}, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}

	probe, err := loop.Define(loop.WithName(identity.AgentName("permission-review-probe")), loop.WithInference(&fakeLLM{}, testModel()))
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)

	options := []rig.Option{
		rig.WithLoops(probe),
		rig.WithPrimers(string(probe.Name())),
		rig.WithActivePrimer(string(probe.Name())),
		rig.WithSessionStore(stores.session),
	}
	options = append(options, registration.options()...)
	options = append(options, registration.options()...) // deliberate double registration

	_, err = rig.Define(options...)
	var target *rig.DefinitionError
	if !errors.As(err, &target) || target.Kind != rig.DefinitionDuplicateOption {
		t.Fatalf("rig.Define() error = %T %v, want DefinitionError{Kind: DefinitionDuplicateOption}", err, err)
	}
}

// TestPermissionReviewDoesNotWidenAccessCeiling proves requirement 6: for
// every named access profile, enabling permission review (any model, either
// policy) leaves the durable access-config digest — which folds the
// selected profile and the complete normalized Carbon sandbox profile
// (accessConfigDigest, access.go) — byte-for-byte IDENTICAL to the
// same profile with permission review disabled. Carbon exposes no
// independent "security ceiling" knob for the classifier: whatever ceiling a
// request's own access-gate binding already grants under the selected
// profile is the only ceiling permission review can ever operate within.
func TestPermissionReviewDoesNotWidenAccessCeiling(t *testing.T) {
	t.Parallel()

	for _, profile := range []AccessProfile{AccessReadOnly, AccessTrusted, AccessUnconfined} {
		t.Run(string(profile), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()

			baseline, baseCfg := headlessTestAccess(t, Config{AccessProfile: profile}, root)
			withReview, reviewCfg := headlessTestAccess(t, Config{
				AccessProfile:                profile,
				PermissionReviewEnabled:      true,
				PermissionReviewModel:        permissionReviewTestModel(),
				PermissionReviewStrictPolicy: true,
			}, root)

			if baseline.configRev != withReview.configRev {
				t.Errorf("accessConfigDigest changed when permission review was enabled:\n disabled=%q\n  enabled=%q", baseline.configRev, withReview.configRev)
			}
			if baseCfg.AccessConfigRev != reviewCfg.AccessConfigRev {
				t.Errorf("Config.AccessConfigRev changed when permission review was enabled:\n disabled=%q\n  enabled=%q", baseCfg.AccessConfigRev, reviewCfg.AccessConfigRev)
			}
		})
	}
}

// TestPermissionReviewHeadlessComposesSafely proves requirement 7: enabling
// permission review in a headless (unattended) session changes nothing about
// headless mode's existing fail-closed gate wiring — all access gates stay
// non-interactive with no rule writer (gate.NewHeadlessEvaluator, exactly as
// buildHeadlessAccess already selects for every headless session) — and the
// resulting composition still passes rig.Define, so permission review composes
// safely alongside headless mode rather than silently changing its posture.
// (It stops at construction; see TestPermissionReviewExplicitEnable's comment
// for why starting the session is out of scope here.)
func TestPermissionReviewHeadlessComposesSafely(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := Config{PermissionReviewEnabled: true, PermissionReviewModel: permissionReviewTestModel(), AccessProfile: AccessTrusted}
	access, sessionCfg := headlessTestAccess(t, cfg, root)

	gateImpl, ok := access.gate.(*accessGate)
	if !ok || gateImpl.interactive || gateImpl.writer != nil {
		t.Fatalf("Carbon gate = %+v, want a non-interactive headless gate with no rule writer", access.gate)
	}

	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), sessionCfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	permissionReview, err := newPermissionReviewRegistration(sessionCfg, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	// Construction-only, like TestPermissionReviewExplicitEnable: starting the
	// session (assembly.NewSession) is blocked by the same known, out-of-scope
	// evidence-collaborator gap documented there.
	if _, err := buildRigForDelegationCaps(
		definition, stores, root, sessionCfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReview,
	); err != nil {
		t.Fatalf("buildRigForDelegationCaps() error = %v", err)
	}
}

// permissionReviewBadClassifierModel is deliberately classifier-INCAPABLE: it
// has Tools but neither StructuredOutput nor StructuredOutputWithTools, so
// commandsafety.New (newPermissionReviewRegistration's own construction step)
// rejects it outright. The Task 4 assembly-level tests below use successful
// vs. failed construction as the observable proxy for "which
// PermissionReviewModel actually reached newPermissionReviewRegistration,"
// since a *RuntimeAgent exposes no field revealing its permission-review
// registration directly.
func permissionReviewBadClassifierModel() model.Model {
	return model.CustomModel(
		model.ProviderName("test"), model.APIFormatOpenAI,
		"http://localhost:1234/v1", "bad-classifier-model",
		model.WithTools(),
	)
}

// permissionReviewFixtureLoader returns a productionModelsLoader with a valid
// primer and the given permission-review fields, mirroring the shape a real
// models.json load would produce after Layers 1-2. Every call in one test
// keeps ConfigRev and the primer identical so a restore's ONLY observable
// difference is the permission-review section — an unrelated ConfigRev drift
// would independently reject the restore and confound the assertion.
func permissionReviewFixtureLoader(enabled bool, reviewModel model.Model, strict bool) productionModelsLoader {
	return func(string) (productionModels, error) {
		return productionModels{
			PrimerClient: &fakeLLM{}, PrimerModel: testModel(), PrimerAlias: "fixture-primer",
			PrimerEfforts: []model.Effort{model.EffortNone}, ConfigRev: "fixture-config-rev",
			PermissionReviewEnabled: enabled, PermissionReviewModel: reviewModel, PermissionReviewStrict: strict,
		}, nil
	}
}

// TestPermissionReviewModelsJSONEnablesThroughFullCompositionWhenTrusted is
// Task 4's assembly-level proof that a models.json-shaped permission_review
// section (here, a productionModels value shaped exactly as
// compileProductionModels would produce one) flows all the way through
// persistence.go's SessionStoreFactory.Open — the real Layer 3 copy-over —
// into an actually ENABLED newPermissionReviewRegistration under
// AccessTrusted. It reuses TestPermissionReviewConfigFingerprintChanges's own
// technique: restoring a session under a widened permission-review
// configuration is a REJECTED drift, so a rejection here is only possible if
// the file's permission_review section genuinely registered a classifier
// through the production Open path, not a hand-built Config.
func TestPermissionReviewModelsJSONEnablesThroughFullCompositionWhenTrusted(t *testing.T) {
	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	cfg := Config{AccessProfile: AccessTrusted, HomeDir: t.TempDir()}

	factory := &SessionStoreFactory{stores: stores, loadModels: permissionReviewFixtureLoader(false, model.Model{}, false)}
	baseline, err := factory.Open(ctx, SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("Open() baseline error = %v", err)
	}
	sid := baseline.SessionID()
	if err := baseline.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// The SAME session store now "sees" a models.json with a permission_review
	// section (via the injected loader) on restore.
	factory.loadModels = permissionReviewFixtureLoader(true, permissionReviewTestModel(), false)

	_, err = factory.Open(ctx, SessionSelector{Resume: sid}, cfg)
	var rejected *session.RestoreRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Open(restore) after models.json enabled permission review under AccessTrusted error = %T %v, want *session.RestoreRejectedError", err, err)
	}
	foundPermissionWarn := false
	for _, change := range rejected.Assessment.Changes {
		if change.Category == event.DriftPermission && change.Severity == event.DriftWarn {
			foundPermissionWarn = true
		}
	}
	if !foundPermissionWarn {
		t.Fatalf("rejected.Assessment.Changes = %+v, want a DriftWarn change in category %q", rejected.Assessment.Changes, event.DriftPermission)
	}
}

// TestPermissionReviewModelsJSONSilentlyDisabledOutsideTrustedProfileThroughFullComposition
// is Task 4's assembly-level proof of the trusted-profile gate at the SAME
// composition boundary: with AccessProfile held constant (readonly or
// unconfined) across open and restore, a models.json permission_review
// section that would enable review under AccessTrusted instead changes
// NOTHING observable — the restore succeeds with no override, because
// newPermissionReviewRegistration's gate silently disabled it on both sides.
func TestPermissionReviewModelsJSONSilentlyDisabledOutsideTrustedProfileThroughFullComposition(t *testing.T) {
	for _, profile := range []AccessProfile{AccessReadOnly, AccessUnconfined} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := context.Background()
			stores := mustHeadlessTestStores(t)
			cfg := Config{AccessProfile: profile, HomeDir: t.TempDir()}

			factory := &SessionStoreFactory{stores: stores, loadModels: permissionReviewFixtureLoader(false, model.Model{}, false)}
			baseline, err := factory.Open(ctx, SessionSelector{}, cfg)
			if err != nil {
				t.Fatalf("Open() baseline error = %v", err)
			}
			sid := baseline.SessionID()
			if err := baseline.Close(ctx); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			factory.loadModels = permissionReviewFixtureLoader(true, permissionReviewTestModel(), false)

			restored, err := factory.Open(ctx, SessionSelector{Resume: sid}, cfg)
			if err != nil {
				t.Fatalf("Open(restore) after models.json enabled permission review under %q error = %v, want success (silently disabled outside AccessTrusted, no drift)", profile, err)
			}
			_ = restored.Close(ctx)
		})
	}
}

// TestPermissionReviewProgrammaticModelWinsOverModelsJSONThroughFullComposition
// is Task 4's assembly-level proof of the "programmatic enable wins" rule
// (Config.PermissionReviewEnabled's doc comment): when the caller has ALREADY
// set cfg.PermissionReviewEnabled/PermissionReviewModel before Open/
// newWithProductionModelsLoader runs, a models.json permission_review section
// naming a DIFFERENT model must never override it. It exercises BOTH Layer 3
// call sites — persistence.go's SessionStoreFactory.Open and assembly.go's
// newWithProductionModelsLoader — since the task requires both to compose
// identically. Each subtest pairs the real assertion with a negative control
// (nothing programmatic set) proving the harness genuinely distinguishes
// which model reached the classifier, via permissionReviewBadClassifierModel.
func TestPermissionReviewProgrammaticModelWinsOverModelsJSONThroughFullComposition(t *testing.T) {
	ctx := context.Background()
	fileHasBadModel := permissionReviewFixtureLoader(true, permissionReviewBadClassifierModel(), true)

	t.Run("persistence.go path (SessionStoreFactory.Open)", func(t *testing.T) {
		programmaticCfg := Config{
			AccessProfile: AccessTrusted, HomeDir: t.TempDir(),
			PermissionReviewEnabled: true, PermissionReviewModel: permissionReviewTestModel(),
		}
		factory := &SessionStoreFactory{stores: mustHeadlessTestStores(t), loadModels: fileHasBadModel}
		agent, err := factory.Open(ctx, SessionSelector{}, programmaticCfg)
		if err != nil {
			t.Fatalf("Open() error = %v, want success: the programmatic model, not the file's classifier-incapable one, must back the classifier", err)
		}
		_ = agent.Close(ctx)

		nothingProgrammaticCfg := Config{AccessProfile: AccessTrusted, HomeDir: t.TempDir()}
		controlFactory := &SessionStoreFactory{stores: mustHeadlessTestStores(t), loadModels: fileHasBadModel}
		if _, err := controlFactory.Open(ctx, SessionSelector{}, nothingProgrammaticCfg); err == nil {
			t.Fatal("negative control: Open() error = nil, want failure (the file's classifier-incapable model should have been adopted and rejected by commandsafety.New)")
		}
	})

	t.Run("assembly.go path (newWithProductionModelsLoader)", func(t *testing.T) {
		programmaticCfg := Config{
			AccessProfile:           AccessTrusted,
			PermissionReviewEnabled: true, PermissionReviewModel: permissionReviewTestModel(),
		}
		agent, err := newWithProductionModelsLoader(ctx, programmaticCfg, fileHasBadModel, func() (*sessionStores, error) {
			return openTestStores(t)
		})
		if err != nil {
			t.Fatalf("newWithProductionModelsLoader() error = %v, want success: the programmatic model, not the file's classifier-incapable one, must back the classifier", err)
		}
		_ = agent.Close(ctx)

		nothingProgrammaticCfg := Config{AccessProfile: AccessTrusted}
		if _, err := newWithProductionModelsLoader(ctx, nothingProgrammaticCfg, fileHasBadModel, func() (*sessionStores, error) {
			return openTestStores(t)
		}); err == nil {
			t.Fatal("negative control: newWithProductionModelsLoader() error = nil, want failure (the file's classifier-incapable model should have been adopted and rejected by commandsafety.New)")
		}
	})
}

// TestPermissionReviewConfigFingerprintChanges proves requirement 8, UPDATED
// for the Phase 6 spec-compliance review's Finding 3 fix (Harness commit
// 0186a2df): restoring a session under a DIFFERENT permission-review
// configuration than the one it was opened with is REJECTED, closed, exactly
// when that change WIDENS review coverage (disabled -> enabled, either
// policy) — never silently re-adopted — unless the caller explicitly opts in
// via Carbon's OWN existing config-mismatch-acceptance mechanism
// (SessionSelector.AllowConfigMismatch -> rig.WithAllowConfigMismatch(),
// persistence.go). Narrowing (a hypothetical enabled -> disabled restore) and
// the no-change control still restore cleanly with no override needed.
//
// This SUPERSEDES this test's own prior assumption (a Task 24 correction
// that a topology-only permission-review change always classified
// event.DriftInfo and so always auto-accepted under
// event.DefaultPolicyDecider). Harness's pkg/event/drift.go now carries a
// dedicated ConfigManifest.PermissionReviewConfigured signal, compared
// DIRECTIONALLY: disabled -> enabled is event.DriftWarn ("design §21: never
// silently resumes with a different permission-review configuration" — exactly
// the bug this fix closes), enabled -> disabled stays event.DriftInfo (strictly narrower, more
// human control). event.DefaultPolicyDecider (pkg/session/decider.go, the
// Harness default Carbon never overrides with a custom RestoreDecider)
// rejects on ANY Warn change, so RestoreSession now returns a typed
// *session.RestoreRejectedError for the widening case.
//
// Reasoning for why Carbon does NOT auto-accept this drift on the caller's
// behalf: per CLAUDE.md's "fail closed when access, permission, identity, or
// durable policy state is uncertain," and because TestRestoreRejectsAccessProfileDrift
// already establishes Carbon's existing convention for every other kind of
// rejected restore drift (access-profile change) — surface the plain
// rejection error to the caller and require the SAME explicit,
// caller-supplied SessionSelector.AllowConfigMismatch a caller would use
// for any other deliberate config change, rather than inventing a
// permission-review-specific auto-accept path. This test proves both halves:
// the default (no override) rejects, and the existing override still works
// unchanged.
func TestPermissionReviewConfigFingerprintChanges(t *testing.T) {
	t.Parallel()

	// newDisabledBaseline opens and shuts down a FRESH session (its own store
	// and workspace) with permission review disabled, so each scenario below
	// restores against a clean, unmutated "disabled" baseline: a successful
	// restore durably re-adopts its configuration, so reusing one baseline
	// session across an accepted-override scenario and a later reject-by-default
	// scenario would silently change what the second scenario is actually
	// comparing against.
	newDisabledBaseline := func(t *testing.T) (stores *sessionStores, root string, sid uuid.UUID) {
		t.Helper()
		stores, err := openTestStores(t)
		if err != nil {
			t.Fatalf("openTestStores() error = %v", err)
		}
		root = t.TempDir()
		access, cfg := headlessTestAccess(t, Config{}, root)
		definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), cfg, access)
		if err != nil {
			t.Fatalf("carbonTestDefinition() error = %v", err)
		}
		assembly, err := buildRig(definition, stores, root, cfg, false)
		if err != nil {
			t.Fatalf("buildRig() error = %v", err)
		}
		controller, err := assembly.NewSession(context.Background())
		if err != nil {
			t.Fatalf("NewSession() error = %v", err)
		}
		sid = controller.SessionID()
		if err := controller.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
		return stores, root, sid
	}

	restoreWith := func(t *testing.T, stores *sessionStores, root string, sid uuid.UUID, permissionReview permissionReviewRegistration, allowMismatch bool) error {
		t.Helper()
		racc, rcfg := headlessTestAccess(t, Config{}, root)
		rdef, err := carbonTestDefinition(&fakeLLM{}, testModel(), rcfg, racc)
		if err != nil {
			t.Fatalf("carbonTestDefinition() error = %v", err)
		}
		rasm, err := buildRigForDelegationCaps(
			rdef, stores, root, rcfg, allowMismatch,
			rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReview,
		)
		if err != nil {
			t.Fatalf("buildRigForDelegationCaps() error = %v", err)
		}
		rctrl, err := rasm.RestoreSession(context.Background(), sid)
		if err == nil {
			_ = rctrl.Shutdown(context.Background())
		}
		return err
	}

	enabled, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
		AccessProfile:           AccessTrusted,
	}, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	strict, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled:      true,
		PermissionReviewModel:        permissionReviewTestModel(),
		PermissionReviewStrictPolicy: true,
		AccessProfile:                AccessTrusted,
	}, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}

	t.Run("disabled to enabled/default rejected by default", func(t *testing.T) {
		t.Parallel()
		stores, root, sid := newDisabledBaseline(t)
		err := restoreWith(t, stores, root, sid, enabled, false)
		var rejected *session.RestoreRejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("restore under a DIFFERENT permission-review configuration (disabled -> enabled/default) error = %T %v, want *session.RestoreRejectedError", err, err)
		}
		if !rejected.Assessment.AnyWarn() {
			t.Fatalf("rejected.Assessment = %+v, want at least one Warn change", rejected.Assessment)
		}
		foundPermissionWarn := false
		for _, change := range rejected.Assessment.Changes {
			if change.Category == event.DriftPermission && change.Severity == event.DriftWarn {
				foundPermissionWarn = true
			}
		}
		if !foundPermissionWarn {
			t.Fatalf("rejected.Assessment.Changes = %+v, want a DriftWarn change in category %q", rejected.Assessment.Changes, event.DriftPermission)
		}
	})

	t.Run("disabled to enabled/default accepted with explicit override", func(t *testing.T) {
		t.Parallel()
		stores, root, sid := newDisabledBaseline(t)
		// The existing, pre-established mechanism (SessionSelector.AllowConfigMismatch
		// -> rig.WithAllowConfigMismatch): the SAME widening restore now succeeds
		// when the caller opts in exactly as TestRestoreRejectsAccessProfileDrift's
		// own access-profile case would.
		if err := restoreWith(t, stores, root, sid, enabled, true); err != nil {
			t.Fatalf("restore under a DIFFERENT permission-review configuration WITH AllowConfigMismatch error = %v, want success", err)
		}
	})

	t.Run("disabled to enabled/strict rejected by default", func(t *testing.T) {
		t.Parallel()
		stores, root, sid := newDisabledBaseline(t)
		err := restoreWith(t, stores, root, sid, strict, false)
		var rejected *session.RestoreRejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("restore under a DIFFERENT permission-review configuration (disabled -> enabled/strict) error = %T %v, want *session.RestoreRejectedError", err, err)
		}
	})

	t.Run("disabled to enabled/strict accepted with explicit override", func(t *testing.T) {
		t.Parallel()
		stores, root, sid := newDisabledBaseline(t)
		if err := restoreWith(t, stores, root, sid, strict, true); err != nil {
			t.Fatalf("restore under a DIFFERENT permission-review configuration (disabled -> enabled/strict) WITH AllowConfigMismatch error = %v, want success", err)
		}
	})

	t.Run("disabled to disabled needs no override", func(t *testing.T) {
		t.Parallel()
		stores, root, sid := newDisabledBaseline(t)
		if err := restoreWith(t, stores, root, sid, permissionReviewRegistration{}, false); err != nil {
			t.Fatalf("restore under the SAME (disabled) permission-review configuration failed: %v", err)
		}
	})
}
