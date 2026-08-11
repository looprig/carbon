package gate

// ReviewRisk is the classifier's closed risk level for a permission request.
type ReviewRisk string

const (
	ReviewRiskLow      ReviewRisk = "low"
	ReviewRiskMedium   ReviewRisk = "medium"
	ReviewRiskHigh     ReviewRisk = "high"
	ReviewRiskCritical ReviewRisk = "critical"
)

// ParseReviewRisk parses a closed review risk value.
func ParseReviewRisk(value string) (ReviewRisk, bool) {
	switch ReviewRisk(value) {
	case ReviewRiskLow, ReviewRiskMedium, ReviewRiskHigh, ReviewRiskCritical:
		return ReviewRisk(value), true
	default:
		return "", false
	}
}

// ReviewAuthorization is the closed strength of authorization evidenced by the
// live review context.
type ReviewAuthorization string

const (
	ReviewAuthorizationUnknown ReviewAuthorization = "unknown"
	ReviewAuthorizationLow     ReviewAuthorization = "low"
	ReviewAuthorizationMedium  ReviewAuthorization = "medium"
	ReviewAuthorizationHigh    ReviewAuthorization = "high"
)

// ParseReviewAuthorization parses a closed review authorization value.
func ParseReviewAuthorization(value string) (ReviewAuthorization, bool) {
	switch ReviewAuthorization(value) {
	case ReviewAuthorizationUnknown,
		ReviewAuthorizationLow,
		ReviewAuthorizationMedium,
		ReviewAuthorizationHigh:
		return ReviewAuthorization(value), true
	default:
		return "", false
	}
}

// ReviewRecommendation is the classifier's closed recommendation. A
// classifier can recommend a one-shot allow or defer to the already-open human
// gate; it cannot deny or create a durable approval.
type ReviewRecommendation string

const (
	ReviewAllow      ReviewRecommendation = "allow"
	ReviewNeedsHuman ReviewRecommendation = "needs_human"
)

// ParseReviewRecommendation parses a closed review recommendation value.
func ParseReviewRecommendation(value string) (ReviewRecommendation, bool) {
	switch ReviewRecommendation(value) {
	case ReviewAllow, ReviewNeedsHuman:
		return ReviewRecommendation(value), true
	default:
		return "", false
	}
}

// ReviewStatus is the closed terminal status recorded for a permission review.
type ReviewStatus string

const (
	ReviewStatusAllowed       ReviewStatus = "allowed"
	ReviewStatusNeedsHuman    ReviewStatus = "needs_human"
	ReviewStatusNotApplicable ReviewStatus = "not_applicable"
	ReviewStatusTimedOut      ReviewStatus = "timed_out"
	ReviewStatusFailed        ReviewStatus = "failed"
	ReviewStatusCancelled     ReviewStatus = "cancelled"
	ReviewStatusStale         ReviewStatus = "stale"
)

// ParseReviewStatus parses a closed review status value.
func ParseReviewStatus(value string) (ReviewStatus, bool) {
	switch ReviewStatus(value) {
	case ReviewStatusAllowed,
		ReviewStatusNeedsHuman,
		ReviewStatusNotApplicable,
		ReviewStatusTimedOut,
		ReviewStatusFailed,
		ReviewStatusCancelled,
		ReviewStatusStale:
		return ReviewStatus(value), true
	default:
		return "", false
	}
}

// ReviewRiskCategory is a closed, policy-relevant reason for a review risk.
type ReviewRiskCategory string

const (
	ReviewCategoryDataExfiltration            ReviewRiskCategory = "data_exfiltration"
	ReviewCategoryCredentialAccess            ReviewRiskCategory = "credential_access"  // #nosec G101 -- Closed taxonomy label, not a credential.
	ReviewCategoryCredentialProbing           ReviewRiskCategory = "credential_probing" // #nosec G101 -- Closed taxonomy label, not a credential.
	ReviewCategoryDestructiveLocal            ReviewRiskCategory = "destructive_local"
	ReviewCategoryDestructiveShared           ReviewRiskCategory = "destructive_shared"
	ReviewCategoryPersistentSecurityWeakening ReviewRiskCategory = "persistent_security_weakening"
	ReviewCategoryProductionMutation          ReviewRiskCategory = "production_mutation"
	ReviewCategoryProtectedSourceControl      ReviewRiskCategory = "protected_source_control"
	ReviewCategoryUntrustedCodeExecution      ReviewRiskCategory = "untrusted_code_execution"
	ReviewCategoryMutableNetwork              ReviewRiskCategory = "mutable_network"
	ReviewCategoryPromptInjection             ReviewRiskCategory = "prompt_injection"
	ReviewCategoryAuthorizationConflict       ReviewRiskCategory = "authorization_conflict"
	ReviewCategoryTargetAmbiguity             ReviewRiskCategory = "target_ambiguity"
	ReviewCategoryInsufficientEvidence        ReviewRiskCategory = "insufficient_evidence"
)

// MaxReviewCategories is the maximum number of distinct categories a review
// may carry. It equals the complete initial closed category domain.
const MaxReviewCategories int = 14

// ParseReviewRiskCategory parses a closed review risk category.
func ParseReviewRiskCategory(value string) (ReviewRiskCategory, bool) {
	switch ReviewRiskCategory(value) {
	case ReviewCategoryDataExfiltration,
		ReviewCategoryCredentialAccess,
		ReviewCategoryCredentialProbing,
		ReviewCategoryDestructiveLocal,
		ReviewCategoryDestructiveShared,
		ReviewCategoryPersistentSecurityWeakening,
		ReviewCategoryProductionMutation,
		ReviewCategoryProtectedSourceControl,
		ReviewCategoryUntrustedCodeExecution,
		ReviewCategoryMutableNetwork,
		ReviewCategoryPromptInjection,
		ReviewCategoryAuthorizationConflict,
		ReviewCategoryTargetAmbiguity,
		ReviewCategoryInsufficientEvidence:
		return ReviewRiskCategory(value), true
	default:
		return "", false
	}
}

// ReviewValidationField identifies the bounded part of a review value that
// failed validation.
type ReviewValidationField string

const (
	ReviewValidationFieldCategories ReviewValidationField = "categories"
)

// ReviewValidationReason classifies a review validation failure.
type ReviewValidationReason string

const (
	ReviewValidationUnsupported ReviewValidationReason = "unsupported"
	ReviewValidationDuplicate   ReviewValidationReason = "duplicate"
	ReviewValidationTooMany     ReviewValidationReason = "too_many"
)

// ReviewValidationError reports a bounded review validation failure. It
// deliberately carries no rejected value so untrusted classifier output cannot
// leak through logs or audit messages.
type ReviewValidationError struct {
	Field  ReviewValidationField
	Reason ReviewValidationReason
}

func (e *ReviewValidationError) Error() string {
	return "gate: invalid permission review"
}

// ValidateReviewCategories validates that categories contains only distinct,
// known values from the bounded category domain.
func ValidateReviewCategories(categories []ReviewRiskCategory) error {
	if len(categories) > MaxReviewCategories {
		return &ReviewValidationError{
			Field:  ReviewValidationFieldCategories,
			Reason: ReviewValidationTooMany,
		}
	}
	seen := make(map[ReviewRiskCategory]struct{}, len(categories))
	for _, category := range categories {
		if _, ok := ParseReviewRiskCategory(string(category)); !ok {
			return &ReviewValidationError{
				Field:  ReviewValidationFieldCategories,
				Reason: ReviewValidationUnsupported,
			}
		}
		if _, duplicate := seen[category]; duplicate {
			return &ReviewValidationError{
				Field:  ReviewValidationFieldCategories,
				Reason: ReviewValidationDuplicate,
			}
		}
		seen[category] = struct{}{}
	}
	return nil
}
