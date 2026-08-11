package gate

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPermissionReviewRationaleBytes bounds live classifier diagnostics.
	MaxPermissionReviewRationaleBytes = 2048
	// MaxPermissionReviewPolicyRevisionBytes bounds the consumer policy label.
	MaxPermissionReviewPolicyRevisionBytes = 128
	// MaxPermissionClassifierRevisionBytes bounds classifier implementation labels.
	MaxPermissionClassifierRevisionBytes = 128
)

// PermissionAssessment is a classifier's authority-free recommendation for
// one exact permission review subject.
type PermissionAssessment struct {
	Basis          ReviewBasis
	Risk           ReviewRisk
	Authorization  ReviewAuthorization
	Categories     []ReviewRiskCategory
	Recommendation ReviewRecommendation
	Rationale      string
}

// PermissionReviewPolicy is the local, consumer-owned ceiling applied after a
// classifier result has been validated.
type PermissionReviewPolicy struct {
	Revision             string
	MaximumAutoRisk      ReviewRisk
	MinimumAuthorization map[ReviewRisk]ReviewAuthorization
	AbsoluteHuman        []ReviewRiskCategory
	MaterialTruncation   ReviewTruncationMask
	seal                 [sha256.Size]byte
}

type permissionReviewPolicyProjection struct {
	Revision           string               `json:"revision"`
	MaximumAutoRisk    ReviewRisk           `json:"maximum_auto_risk"`
	MinimumLow         ReviewAuthorization  `json:"minimum_low"`
	MinimumMedium      ReviewAuthorization  `json:"minimum_medium"`
	MinimumHigh        ReviewAuthorization  `json:"minimum_high"`
	AbsoluteHuman      []ReviewRiskCategory `json:"absolute_human"`
	MaterialTruncation ReviewTruncationMask `json:"material_truncation"`
}

// ReviewDecisionReason is the closed, non-sensitive explanation for a local
// review eligibility decision.
type ReviewDecisionReason string

const (
	ReviewDecisionEligible               ReviewDecisionReason = "eligible"
	ReviewDecisionInvalidPolicy          ReviewDecisionReason = "invalid_policy"
	ReviewDecisionInvalidAssessment      ReviewDecisionReason = "invalid_assessment"
	ReviewDecisionBasisMismatch          ReviewDecisionReason = "basis_mismatch"
	ReviewDecisionRecommendation         ReviewDecisionReason = "recommendation"
	ReviewDecisionRiskCeiling            ReviewDecisionReason = "risk_ceiling"
	ReviewDecisionAuthorization          ReviewDecisionReason = "authorization"
	ReviewDecisionAbsoluteHuman          ReviewDecisionReason = "absolute_human"
	ReviewDecisionMaterialTruncation     ReviewDecisionReason = "material_truncation"
	ReviewDecisionNoApplicableClassifier ReviewDecisionReason = "no_applicable_classifier"
	ReviewDecisionClassifierStatus       ReviewDecisionReason = "classifier_status"
)

// Valid reports whether the reason belongs to the closed decision domain.
func (r ReviewDecisionReason) Valid() bool {
	_, ok := ParseReviewDecisionReason(string(r))
	return ok
}

// ParseReviewDecisionReason parses an exact closed decision reason.
func ParseReviewDecisionReason(value string) (ReviewDecisionReason, bool) {
	switch ReviewDecisionReason(value) {
	case ReviewDecisionEligible,
		ReviewDecisionInvalidPolicy,
		ReviewDecisionInvalidAssessment,
		ReviewDecisionBasisMismatch,
		ReviewDecisionRecommendation,
		ReviewDecisionRiskCeiling,
		ReviewDecisionAuthorization,
		ReviewDecisionAbsoluteHuman,
		ReviewDecisionMaterialTruncation,
		ReviewDecisionNoApplicableClassifier,
		ReviewDecisionClassifierStatus:
		return ReviewDecisionReason(value), true
	default:
		return "", false
	}
}

// ReviewDecision reports only local one-shot eligibility. It deliberately
// carries neither a gate action nor classifier-provided text.
type ReviewDecision struct {
	Eligible bool
	Reason   ReviewDecisionReason
}

// NewPermissionReviewPolicy validates and owns a policy that is at least as
// restrictive as Harness's hard review ceiling.
func NewPermissionReviewPolicy(
	revision string,
	maximum ReviewRisk,
	minimum map[ReviewRisk]ReviewAuthorization,
	absoluteHuman []ReviewRiskCategory,
	material ReviewTruncationMask,
) (PermissionReviewPolicy, error) {
	policy := PermissionReviewPolicy{
		Revision:             revision,
		MaximumAutoRisk:      maximum,
		MinimumAuthorization: cloneMinimumAuthorization(minimum),
		AbsoluteHuman:        append([]ReviewRiskCategory(nil), absoluteHuman...),
		MaterialTruncation:   material,
	}
	if !validPermissionReviewPolicyShape(policy) {
		return PermissionReviewPolicy{}, reviewSubjectError(
			ReviewValidationFieldBasis,
			ReviewValidationInvalid,
		)
	}
	seal, ok := permissionReviewPolicySeal(policy)
	if !ok {
		return PermissionReviewPolicy{}, reviewSubjectError(
			ReviewValidationFieldBasis,
			ReviewValidationInvalid,
		)
	}
	policy.seal = seal
	return policy, nil
}

// DefaultPermissionReviewPolicy constructs the Codex-compatible default:
// low and medium need no authorization evidence, while high requires medium.
func DefaultPermissionReviewPolicy(revision string) (PermissionReviewPolicy, error) {
	return NewPermissionReviewPolicy(
		revision,
		ReviewRiskHigh,
		map[ReviewRisk]ReviewAuthorization{
			ReviewRiskLow:    ReviewAuthorizationUnknown,
			ReviewRiskMedium: ReviewAuthorizationUnknown,
			ReviewRiskHigh:   ReviewAuthorizationMedium,
		},
		nil,
		0,
	)
}

// Sealed reports whether policy was constructed through NewPermissionReviewPolicy
// or DefaultPermissionReviewPolicy, as opposed to a hand-built literal
// PermissionReviewPolicy{} that never went through either constructor. It is
// a convenience for a caller that wants to fail fast at its own
// configuration boundary rather than discovering the same zero seal later:
// EvaluatePermissionAssessment already fails closed on an unsealed policy
// regardless of whether a caller checks Sealed() first, so this method adds
// no new security enforcement — it only reports the existing invariant
// earlier and with a clearer symptom.
func (p PermissionReviewPolicy) Sealed() bool {
	return p.seal != ([sha256.Size]byte{})
}

// EvaluatePermissionAssessment validates all public inputs again and applies
// the local hard ceiling without mutating them.
func EvaluatePermissionAssessment(
	policy PermissionReviewPolicy,
	subject PermissionReviewSubject,
	assessment PermissionAssessment,
) ReviewDecision {
	if !validStoredPermissionReviewSubject(subject) {
		return reviewDecision(ReviewDecisionInvalidAssessment)
	}
	if !validPermissionReviewPolicy(policy) ||
		policy.Revision != subject.Basis.GatePolicyRevision {
		return reviewDecision(ReviewDecisionInvalidPolicy)
	}
	if assessment.Basis != subject.Basis {
		return reviewDecision(ReviewDecisionBasisMismatch)
	}
	if !validPermissionAssessment(assessment) {
		return reviewDecision(ReviewDecisionInvalidAssessment)
	}
	if assessment.Recommendation != ReviewAllow {
		return reviewDecision(ReviewDecisionRecommendation)
	}
	if subject.Context.Truncation.Material != 0 ||
		subject.Context.Truncation.Applied&policy.MaterialTruncation != 0 {
		return reviewDecision(ReviewDecisionMaterialTruncation)
	}
	if assessment.Risk == ReviewRiskCritical ||
		reviewRiskRank(assessment.Risk) > reviewRiskRank(policy.MaximumAutoRisk) {
		return reviewDecision(ReviewDecisionRiskCeiling)
	}
	if containsReviewCategory(policy.AbsoluteHuman, assessment.Categories) {
		return reviewDecision(ReviewDecisionAbsoluteHuman)
	}
	minimum := policy.MinimumAuthorization[assessment.Risk]
	if reviewAuthorizationRank(assessment.Authorization) < reviewAuthorizationRank(minimum) {
		return reviewDecision(ReviewDecisionAuthorization)
	}
	return ReviewDecision{Eligible: true, Reason: ReviewDecisionEligible}
}

func validPermissionReviewPolicy(policy PermissionReviewPolicy) bool {
	if !validPermissionReviewPolicyShape(policy) ||
		policy.seal == ([sha256.Size]byte{}) {
		return false
	}
	seal, ok := permissionReviewPolicySeal(policy)
	return ok && seal == policy.seal
}

func validPermissionReviewPolicyShape(policy PermissionReviewPolicy) bool {
	if strings.TrimSpace(policy.Revision) == "" ||
		!utf8.ValidString(policy.Revision) ||
		len(policy.Revision) > MaxPermissionReviewPolicyRevisionBytes {
		return false
	}
	if policy.MaximumAutoRisk != ReviewRiskLow &&
		policy.MaximumAutoRisk != ReviewRiskMedium &&
		policy.MaximumAutoRisk != ReviewRiskHigh {
		return false
	}
	if len(policy.MinimumAuthorization) != 3 {
		return false
	}
	for _, risk := range []ReviewRisk{ReviewRiskLow, ReviewRiskMedium, ReviewRiskHigh} {
		authorization, ok := policy.MinimumAuthorization[risk]
		if !ok || !validReviewAuthorization(authorization) {
			return false
		}
	}
	if policy.MinimumAuthorization[ReviewRiskHigh] == ReviewAuthorizationUnknown ||
		policy.MinimumAuthorization[ReviewRiskHigh] == ReviewAuthorizationLow ||
		reviewAuthorizationRank(policy.MinimumAuthorization[ReviewRiskHigh]) <
			reviewAuthorizationRank(policy.MinimumAuthorization[ReviewRiskMedium]) {
		return false
	}
	// The same monotonic-ceiling requirement applies one step down: a policy
	// that demands stronger authorization for Low than for Medium is
	// internally inverted (nonsensical, even though it is trusted-consumer
	// configuration only, not attacker-reachable) and must not validate.
	if reviewAuthorizationRank(policy.MinimumAuthorization[ReviewRiskMedium]) <
		reviewAuthorizationRank(policy.MinimumAuthorization[ReviewRiskLow]) {
		return false
	}
	for risk := range policy.MinimumAuthorization {
		if risk != ReviewRiskLow && risk != ReviewRiskMedium && risk != ReviewRiskHigh {
			return false
		}
	}
	if ValidateReviewCategories(policy.AbsoluteHuman) != nil ||
		policy.MaterialTruncation&^SupportedReviewTruncationMask != 0 {
		return false
	}
	return true
}

func permissionReviewPolicySeal(
	policy PermissionReviewPolicy,
) ([sha256.Size]byte, bool) {
	projection := permissionReviewPolicyProjection{
		Revision:           policy.Revision,
		MaximumAutoRisk:    policy.MaximumAutoRisk,
		MinimumLow:         policy.MinimumAuthorization[ReviewRiskLow],
		MinimumMedium:      policy.MinimumAuthorization[ReviewRiskMedium],
		MinimumHigh:        policy.MinimumAuthorization[ReviewRiskHigh],
		AbsoluteHuman:      append([]ReviewRiskCategory(nil), policy.AbsoluteHuman...),
		MaterialTruncation: policy.MaterialTruncation,
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(data), true
}

func validPermissionAssessment(assessment PermissionAssessment) bool {
	if _, ok := ParseReviewRisk(string(assessment.Risk)); !ok {
		return false
	}
	if !validReviewAuthorization(assessment.Authorization) {
		return false
	}
	if _, ok := ParseReviewRecommendation(string(assessment.Recommendation)); !ok {
		return false
	}
	if ValidateReviewCategories(assessment.Categories) != nil {
		return false
	}
	if !utf8.ValidString(assessment.Rationale) ||
		len(assessment.Rationale) > MaxPermissionReviewRationaleBytes ||
		assessment.Risk != ReviewRiskLow && strings.TrimSpace(assessment.Rationale) == "" {
		return false
	}
	return true
}

func validStoredPermissionReviewSubject(subject PermissionReviewSubject) bool {
	digest, err := SubjectDigest(subject)
	return err == nil &&
		subject.Basis.SubjectDigest != ([32]byte{}) &&
		subject.Basis.SubjectDigest == digest
}

func validReviewAuthorization(authorization ReviewAuthorization) bool {
	_, ok := ParseReviewAuthorization(string(authorization))
	return ok
}

func cloneMinimumAuthorization(
	minimum map[ReviewRisk]ReviewAuthorization,
) map[ReviewRisk]ReviewAuthorization {
	if minimum == nil {
		return nil
	}
	clone := make(map[ReviewRisk]ReviewAuthorization, len(minimum))
	for risk, authorization := range minimum {
		clone[risk] = authorization
	}
	return clone
}

func reviewDecision(reason ReviewDecisionReason) ReviewDecision {
	return ReviewDecision{Reason: reason}
}

func reviewRiskRank(risk ReviewRisk) int {
	switch risk {
	case ReviewRiskLow:
		return 1
	case ReviewRiskMedium:
		return 2
	case ReviewRiskHigh:
		return 3
	case ReviewRiskCritical:
		return 4
	default:
		return 0
	}
}

func reviewAuthorizationRank(authorization ReviewAuthorization) int {
	switch authorization {
	case ReviewAuthorizationUnknown:
		return 1
	case ReviewAuthorizationLow:
		return 2
	case ReviewAuthorizationMedium:
		return 3
	case ReviewAuthorizationHigh:
		return 4
	default:
		return 0
	}
}

func containsReviewCategory(
	absolute []ReviewRiskCategory,
	assessment []ReviewRiskCategory,
) bool {
	for _, category := range assessment {
		for _, blocked := range absolute {
			if category == blocked {
				return true
			}
		}
	}
	return false
}
