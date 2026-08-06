package app

import (
	"strings"

	"github.com/looprig/classifiers/pkg/commandsafety"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/inference"
)

// permission_review.go is CodeRig's composition-root wiring for classifier-based
// automatic permission review (design doc 2026-07-27 §19-20). It constructs
// exactly ONE command-safety classifier over CodeRig's shared inference client
// and the operator-selected named model, wraps it in Harness's
// gate.PermissionClassifierSet, and selects one of two supported local
// decision policies. It never adds classifier code to roleGate (toolsets.go)
// or duplicates Harness's ceiling-comparison/eligibility logic — that stays in
// harness/pkg/gate. Permission review is off by default: a zero Config never
// auto-approves anything.

const (
	// permissionReviewDefaultPolicyRevision is CodeRig's local decision policy
	// revision for the Codex-compatible default (gate.DefaultPermissionReviewPolicy):
	// low/medium risk need no authorization evidence, high requires medium.
	permissionReviewDefaultPolicyRevision = "coderig-permission-review-default-v1"
	// permissionReviewStrictPolicyRevision is CodeRig's local decision policy
	// revision for the strict alternative: automatic approval is capped at LOW
	// risk (medium and high always need a human), and the minimum authorization
	// floor at every risk tier is raised to at least the default's. It is
	// STRICTER than the default at every dimension it changes, never looser,
	// per design §10's hard-ceiling rule that a consumer may tighten but never
	// relax.
	permissionReviewStrictPolicyRevision = "coderig-permission-review-strict-v1"
)

// PermissionReviewConfigError reports an invalid permission-review Config
// combination discovered before any classifier or rig option is constructed
// (for example PermissionReviewEnabled without a named PermissionReviewModel).
// It is errors.As-recoverable.
type PermissionReviewConfigError struct {
	Reason string
	Cause  error
}

func (e *PermissionReviewConfigError) Error() string {
	if e.Cause != nil {
		return "coderig: invalid permission review configuration (" + e.Reason + "): " + e.Cause.Error()
	}
	return "coderig: invalid permission review configuration (" + e.Reason + ")"
}

func (e *PermissionReviewConfigError) Unwrap() error { return e.Cause }

// permissionReviewRegistration is the complete, optional permission-review rig
// registration. Its zero value is DISABLED (options() returns nil), matching
// Config's off-by-default contract. enabled is a concrete field, rather than a
// nil-checked pointer, so "not configured" is structural and cannot be
// confused with a partially-built classifier set.
type permissionReviewRegistration struct {
	enabled             bool
	classifiers         gate.PermissionClassifierSet
	policy              gate.PermissionReviewPolicy
	evidenceAccess      gate.EvidenceAccessEvaluator
	evidenceContainment gate.EvidenceContainmentVerifier
	// evidenceObservation is CodeRig's gate.EvidenceObservationVerifier
	// (design §13.4, TOCTOU — Addendum 4, permission_review_observation.go),
	// bound to the SAME ceiling value evidenceContainment uses. It independently
	// rechecks every observation a target-sensitive evidence tool recorded
	// during a review's evidence gathering, immediately before a
	// classifier-originated auto-approval claims the gate, and installs via
	// rig.WithPermissionReviewObservations alongside the other two evidence
	// seams.
	evidenceObservation gate.EvidenceObservationVerifier
	evidenceKinds       []string
	// securityCeiling is CodeRig's consumer-supplied effective security
	// posture (rig.WithPermissionReviewSecurityCeiling), installed into
	// every registered classifier's ReviewContext/ReviewBasis and every
	// evidence-tool containment check. It is derived from the EXACT SAME
	// evidenceCeilingFor call newPermissionReviewRegistration uses to build
	// evidenceContainment below, so the two can never independently drift
	// (permission_review_evidence.go's evidenceCeilingFor doc comment).
	securityCeiling string
}

// newPermissionReviewRegistration resolves cfg's permission-review selection
// into a registration. When cfg.PermissionReviewEnabled is false it returns
// the disabled zero value and no error — CodeRig's default. Permission review
// also only ever takes effect when cfg.AccessProfile == AccessTrusted: every
// other named profile (and the empty default) returns the SAME disabled zero
// value and no error, no matter how PermissionReviewEnabled became true —
// the programmatic seam or a models.json permission_review section. This
// gate is intentionally silent (no error, no log), matching the
// disabled-by-default case it is indistinguishable from to a caller. When
// enabled AND trusted, it requires a non-empty cfg.PermissionReviewModel
// (CodeRig never inherits an operator Loop's model for the classifier),
// constructs ONE command-safety classifier over client and that named model
// with the classifier's own default policy and standard read-only evidence
// tools, and selects the default or strict local decision policy per
// cfg.PermissionReviewStrictPolicy.
//
// It never derives, accepts, or threads any independent "security ceiling":
// the classifier's assessments are eligible only within whatever a request's
// OWN access-gate binding (access.go/toolsets.go, unchanged by this file)
// already permits, and CodeRig exposes no knob here that could widen it.
//
// It also builds CodeRig's evidence-tool authorization boundary
// (permission_review_evidence.go): permissionReviewEvidenceAccess,
// permissionReviewEvidenceContainment bound to this session's selected
// AccessProfile as the one trusted security ceiling, and the exact evidence
// Requirement.Kind allowlist commandsafety.RequiredEvidenceKinds() reports —
// never hand-copied. options() installs all three together via
// rig.WithPermissionReviewEvidence, which Define requires whenever a
// registered classifier's Definition needs evidence tools (every
// command-safety classifier does).
//
// Finally, it builds permissionReviewEvidenceObservation
// (permission_review_observation.go) bound to the EXACT SAME ceiling value
// evidenceContainment uses — the TOCTOU recheck seam design §13.4 adds
// (Addendum 4), installed via rig.WithPermissionReviewObservations.
func newPermissionReviewRegistration(cfg Config, client inference.Client) (permissionReviewRegistration, error) {
	if !cfg.PermissionReviewEnabled {
		return permissionReviewRegistration{}, nil
	}
	if cfg.AccessProfile != AccessTrusted {
		return permissionReviewRegistration{}, nil
	}
	if strings.TrimSpace(cfg.PermissionReviewModel.Name) == "" {
		return permissionReviewRegistration{}, &PermissionReviewConfigError{
			Reason: "PermissionReviewModel is required when PermissionReviewEnabled is true",
		}
	}

	classifier, err := commandsafety.New(commandsafety.Options{
		Inference: client,
		Model:     cfg.PermissionReviewModel,
		Policy:    commandsafety.DefaultPolicy(),
		Evidence:  commandsafety.StandardEvidence(commandsafety.ReadEvidencePolicy{}),
	})
	if err != nil {
		return permissionReviewRegistration{}, err
	}
	classifiers, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		return permissionReviewRegistration{}, err
	}
	policy, err := permissionReviewPolicyFor(cfg.PermissionReviewStrictPolicy)
	if err != nil {
		return permissionReviewRegistration{}, err
	}
	ceiling := evidenceCeilingFor(cfg.AccessProfile)
	return permissionReviewRegistration{
		enabled:             true,
		classifiers:         classifiers,
		policy:              policy,
		evidenceAccess:      newPermissionReviewEvidenceAccess(),
		evidenceContainment: newPermissionReviewEvidenceContainment(ceiling),
		evidenceObservation: newPermissionReviewEvidenceObservation(ceiling),
		evidenceKinds:       commandsafety.RequiredEvidenceKinds(),
		securityCeiling:     ceiling,
	}, nil
}

// permissionReviewPolicyFor selects CodeRig's default or strict local decision
// policy. The strict policy is stricter than the default at EVERY dimension it
// changes: MaximumAutoRisk drops from high to low (so any medium- or
// high-risk assessment always needs a human, no matter its reported
// authorization), and every risk tier's minimum authorization is at or above
// the default's — never below.
func permissionReviewPolicyFor(strict bool) (gate.PermissionReviewPolicy, error) {
	if !strict {
		return gate.DefaultPermissionReviewPolicy(permissionReviewDefaultPolicyRevision)
	}
	return gate.NewPermissionReviewPolicy(
		permissionReviewStrictPolicyRevision,
		gate.ReviewRiskLow,
		map[gate.ReviewRisk]gate.ReviewAuthorization{
			gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
			gate.ReviewRiskMedium: gate.ReviewAuthorizationMedium,
			gate.ReviewRiskHigh:   gate.ReviewAuthorizationHigh,
		},
		nil,
		0,
	)
}

// options returns the rig options that install this registration, or nil when
// disabled. A disabled registration therefore changes nothing about the
// assembled rig: no classifier hustle, no review policy, no breaker limits,
// no evidence-tool authorization boundary.
func (r permissionReviewRegistration) options() []rig.Option {
	if !r.enabled {
		return nil
	}
	return []rig.Option{
		rig.WithPermissionClassifiers(r.classifiers),
		rig.WithPermissionReviewPolicy(r.policy),
		rig.WithPermissionReviewEvidence(r.evidenceAccess, r.evidenceContainment, r.evidenceKinds),
		rig.WithPermissionReviewObservations(r.evidenceObservation),
		rig.WithPermissionReviewSecurityCeiling(r.securityCeiling),
	}
}
