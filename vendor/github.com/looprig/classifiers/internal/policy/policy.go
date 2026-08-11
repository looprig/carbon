// Package policy holds the gate.command-safety classifier's own deterministic
// command-safety consistency policy.
//
// This is deliberately NOT Harness's PermissionReviewPolicy
// (github.com/looprig/harness/pkg/gate.PermissionReviewPolicy): that type is
// the consumer-owned, local decision ceiling Harness itself applies AFTER a
// classifier's assessment has been validated, and it independently re-derives
// eligibility from the assessment's Risk/Authorization/Categories using its
// own matrix. This package never touches that matrix and never grants,
// denies, or widens eligibility on its own.
//
// Instead, Policy encodes this classifier's own closed-taxonomy decision
// structure — which risk floor a reported category implies, which category
// can never resolve to an eligible allow, and which authorization threshold
// this classifier itself expects at each risk level — as deterministic Go
// data rather than leaving that structure purely to prompt-following.
// Reconcile consumes an already strictly-decoded model assessment (as
// internal/wire produces it) and, before that assessment ever crosses this
// module's public boundary, tightens an inconsistent allow recommendation to
// needs_human. It never lowers a reported risk, never raises a reported
// authorization, and never turns an existing needs_human back into allow: it
// only ever moves toward the more conservative outcome.
package policy

import "github.com/looprig/harness/pkg/gate"

// DefaultRevision is the initial command-safety policy revision. Per design
// §19.2 ("prompt, policy, schema, wire, and corpus revisions are
// independently explicit and included in classifier identity"), bump this
// whenever DefaultPolicy's taxonomy content changes meaning.
const DefaultRevision = "command-safety-policy/v1"

// Policy is the classifier's own deterministic command-safety consistency
// policy. Its zero value (no revision, nil tables) is a structurally valid
// "no additional tightening" policy; DefaultPolicy returns the populated,
// named default.
type Policy struct {
	Revision string

	// CategoryMinimumRisk floors the risk every listed category implies. A
	// decoded assessment that selects a category from this table yet
	// reports a risk below that category's floor is internally
	// inconsistent — the reported risk understates what the model's own
	// category selection already claims — and Reconcile treats that
	// inconsistency as unsafe to auto-approve.
	CategoryMinimumRisk map[gate.ReviewRiskCategory]gate.ReviewRisk

	// AbsoluteHumanCategories can never resolve to an eligible allow from
	// this classifier, at any authorization level, including a strong,
	// post-warning authorization: a confirmed disclosure to an untrusted
	// destination, for example, is not made safe by user approval.
	AbsoluteHumanCategories map[gate.ReviewRiskCategory]struct{}

	// MinimumAuthorization is this classifier's own default minimum
	// authorization required at each risk level before recommending allow,
	// independent of category. It mirrors, but does not replace or
	// substitute for, Harness's own local ceiling.
	MinimumAuthorization map[gate.ReviewRisk]gate.ReviewAuthorization
}

// DefaultPolicy returns the initial command-safety taxonomy policy. Tenant
// or product code may construct a stricter Policy value directly; this
// package places no floor on how conservative a caller-supplied Policy may
// be, only on how it is applied (Reconcile only ever tightens).
func DefaultPolicy() Policy {
	return Policy{
		Revision: DefaultRevision,
		CategoryMinimumRisk: map[gate.ReviewRiskCategory]gate.ReviewRisk{
			gate.ReviewCategoryDataExfiltration:            gate.ReviewRiskHigh,
			gate.ReviewCategoryCredentialProbing:           gate.ReviewRiskHigh,
			gate.ReviewCategoryPersistentSecurityWeakening: gate.ReviewRiskMedium,
			gate.ReviewCategoryDestructiveLocal:            gate.ReviewRiskMedium,
			gate.ReviewCategoryDestructiveShared:           gate.ReviewRiskHigh,
			gate.ReviewCategoryProtectedSourceControl:      gate.ReviewRiskMedium,
			gate.ReviewCategoryProductionMutation:          gate.ReviewRiskHigh,
			// credential_access deliberately has NO floor here: unlike
			// credential_probing (searching for or extracting a credential,
			// which floors at high because no flavor of probing is
			// routine), credential_access is routine, already-authorized
			// credential USE in the course of a requested task, and the
			// taxonomy's own split between the two categories exists
			// precisely to let that stay low risk. It is not a blind spot:
			// MinimumAuthorization below already applies uniformly across
			// every category, so credential_access reported at high risk
			// still requires at least medium authorization.
			gate.ReviewCategoryUntrustedCodeExecution: gate.ReviewRiskHigh,
			gate.ReviewCategoryMutableNetwork:         gate.ReviewRiskHigh,
		},
		AbsoluteHumanCategories: map[gate.ReviewRiskCategory]struct{}{
			gate.ReviewCategoryDataExfiltration: {},
			gate.ReviewCategoryPromptInjection:  {},
			// authorization_conflict, target_ambiguity, and
			// insufficient_evidence are all the classifier's OWN
			// self-reported signal that it lacks the confidence needed for
			// auto-approval (conflicting authorization evidence, an
			// unidentifiable target, or a fact it could not establish even
			// after investigation). Per the prompt's own "omissions are not
			// benign" guidance, that self-reported uncertainty must never
			// silently clear a risk/authorization bar; it always needs a
			// human.
			gate.ReviewCategoryAuthorizationConflict: {},
			gate.ReviewCategoryTargetAmbiguity:       {},
			gate.ReviewCategoryInsufficientEvidence:  {},
		},
		MinimumAuthorization: map[gate.ReviewRisk]gate.ReviewAuthorization{
			gate.ReviewRiskLow:    gate.ReviewAuthorizationUnknown,
			gate.ReviewRiskMedium: gate.ReviewAuthorizationUnknown,
			gate.ReviewRiskHigh:   gate.ReviewAuthorizationMedium,
			// Critical is intentionally absent: Reconcile always forces
			// needs_human at critical risk regardless of authorization,
			// matching Harness's own hard ceiling that critical risk never
			// auto-approves.
		},
	}
}

// Clone returns an independently owned copy sharing no backing map storage
// with the receiver.
func (p Policy) Clone() Policy {
	return Policy{
		Revision:                p.Revision,
		CategoryMinimumRisk:     cloneRiskByCategory(p.CategoryMinimumRisk),
		AbsoluteHumanCategories: cloneCategorySet(p.AbsoluteHumanCategories),
		MinimumAuthorization:    cloneAuthorizationByRisk(p.MinimumAuthorization),
	}
}

// Reconcile deterministically re-derives a fail-secure recommendation from
// an already strictly-decoded model assessment. An assessment that does not
// recommend allow is already the most conservative outcome and is returned
// unchanged. Otherwise, Reconcile checks, in order:
//
//  0. A risk value outside the closed four-value gate.ReviewRisk enum
//     (low/medium/high/critical) — including the zero value — always needs
//     a human. Reconcile is a fail-secure function and must not depend on
//     its caller having already enforced the closed enum (in production,
//     internal/wire's strict decoders do, before Reconcile ever runs, but
//     Reconcile itself must not rely on that): riskRank returns 0 for
//     exactly this case and for no valid enum value, so this check and the
//     critical-risk check below share one comparison.
//  1. Critical risk always needs a human, regardless of category or
//     authorization.
//  2. Any category in p.AbsoluteHumanCategories always needs a human, at any
//     authorization level.
//  3. Any category in p.CategoryMinimumRisk whose floor exceeds the
//     assessment's own reported risk is an internally inconsistent report
//     and needs a human.
//  4. The assessment's reported authorization must meet or exceed
//     p.MinimumAuthorization for its reported risk.
//
// Reconcile never lowers the reported risk, never raises the reported
// authorization, and never invents or drops a category: only Recommendation
// may change, and only from allow to needs_human.
func Reconcile(p Policy, assessment gate.PermissionAssessment) gate.PermissionAssessment {
	if assessment.Recommendation != gate.ReviewAllow {
		return assessment
	}
	if riskRank(assessment.Risk) == 0 || assessment.Risk == gate.ReviewRiskCritical {
		assessment.Recommendation = gate.ReviewNeedsHuman
		return assessment
	}
	for _, category := range assessment.Categories {
		if _, absolute := p.AbsoluteHumanCategories[category]; absolute {
			assessment.Recommendation = gate.ReviewNeedsHuman
			return assessment
		}
		if floor, ok := p.CategoryMinimumRisk[category]; ok && riskRank(assessment.Risk) < riskRank(floor) {
			assessment.Recommendation = gate.ReviewNeedsHuman
			return assessment
		}
	}
	if minimum, ok := p.MinimumAuthorization[assessment.Risk]; ok &&
		authorizationRank(assessment.Authorization) < authorizationRank(minimum) {
		assessment.Recommendation = gate.ReviewNeedsHuman
	}
	return assessment
}

func riskRank(risk gate.ReviewRisk) int {
	switch risk {
	case gate.ReviewRiskLow:
		return 1
	case gate.ReviewRiskMedium:
		return 2
	case gate.ReviewRiskHigh:
		return 3
	case gate.ReviewRiskCritical:
		return 4
	default:
		return 0
	}
}

func authorizationRank(authorization gate.ReviewAuthorization) int {
	switch authorization {
	case gate.ReviewAuthorizationUnknown:
		return 1
	case gate.ReviewAuthorizationLow:
		return 2
	case gate.ReviewAuthorizationMedium:
		return 3
	case gate.ReviewAuthorizationHigh:
		return 4
	default:
		return 0
	}
}

func cloneRiskByCategory(
	source map[gate.ReviewRiskCategory]gate.ReviewRisk,
) map[gate.ReviewRiskCategory]gate.ReviewRisk {
	if source == nil {
		return nil
	}
	clone := make(map[gate.ReviewRiskCategory]gate.ReviewRisk, len(source))
	for category, risk := range source {
		clone[category] = risk
	}
	return clone
}

func cloneCategorySet(
	source map[gate.ReviewRiskCategory]struct{},
) map[gate.ReviewRiskCategory]struct{} {
	if source == nil {
		return nil
	}
	clone := make(map[gate.ReviewRiskCategory]struct{}, len(source))
	for category := range source {
		clone[category] = struct{}{}
	}
	return clone
}

func cloneAuthorizationByRisk(
	source map[gate.ReviewRisk]gate.ReviewAuthorization,
) map[gate.ReviewRisk]gate.ReviewAuthorization {
	if source == nil {
		return nil
	}
	clone := make(map[gate.ReviewRisk]gate.ReviewAuthorization, len(source))
	for risk, authorization := range source {
		clone[risk] = authorization
	}
	return clone
}
