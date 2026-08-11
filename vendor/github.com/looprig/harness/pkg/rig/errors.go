package rig

type DefinitionErrorKind string

const (
	DefinitionNilOption                       DefinitionErrorKind = "nil_option"
	DefinitionMissingLoop                     DefinitionErrorKind = "missing_loop"
	DefinitionInvalidLoop                     DefinitionErrorKind = "invalid_loop"
	DefinitionDuplicateLoop                   DefinitionErrorKind = "duplicate_loop"
	DefinitionMissingPrimer                   DefinitionErrorKind = "missing_primer"
	DefinitionInvalidPrimer                   DefinitionErrorKind = "invalid_primer"
	DefinitionInvalidActivePrimer             DefinitionErrorKind = "invalid_active_primer"
	DefinitionMissingSessionStore             DefinitionErrorKind = "missing_session_store"
	DefinitionInvalidSessionStore             DefinitionErrorKind = "invalid_session_store"
	DefinitionInvalidDelegationLimits         DefinitionErrorKind = "invalid_delegation_limits"
	DefinitionInvalidForeignBuilders          DefinitionErrorKind = "invalid_foreign_builders"
	DefinitionInvalidGateCaps                 DefinitionErrorKind = "invalid_gate_caps"
	DefinitionInvalidRestoreDecider           DefinitionErrorKind = "invalid_restore_decider"
	DefinitionDuplicateOption                 DefinitionErrorKind = "duplicate_option"
	DefinitionInvalidHustle                   DefinitionErrorKind = "invalid_hustle"
	DefinitionDuplicateHustle                 DefinitionErrorKind = "duplicate_hustle"
	DefinitionMissingHustleLimits             DefinitionErrorKind = "missing_hustle_limits"
	DefinitionUnusedHustleLimits              DefinitionErrorKind = "unused_hustle_limits"
	DefinitionInvalidHustleLimits             DefinitionErrorKind = "invalid_hustle_limits"
	DefinitionInvalidHooks                    DefinitionErrorKind = "invalid_hooks"
	DefinitionMissingResourceStorage          DefinitionErrorKind = "missing_resource_storage"
	DefinitionInvalidResourceStorage          DefinitionErrorKind = "invalid_resource_storage"
	DefinitionMissingCompactionHustle         DefinitionErrorKind = "missing_compaction_hustle"
	DefinitionIncompatibleCompactionHustle    DefinitionErrorKind = "incompatible_compaction_hustle"
	DefinitionInvalidPermissionClassifiers    DefinitionErrorKind = "invalid_permission_classifiers"
	DefinitionInvalidPermissionReviewPolicy   DefinitionErrorKind = "invalid_permission_review_policy"
	DefinitionIncompletePermissionReview      DefinitionErrorKind = "incomplete_permission_review"
	DefinitionUnusedPermissionReviewLimits    DefinitionErrorKind = "unused_permission_review_limits"
	DefinitionInvalidPermissionReviewEvidence DefinitionErrorKind = "invalid_permission_review_evidence"
	DefinitionMissingPermissionReviewEvidence DefinitionErrorKind = "missing_permission_review_evidence"
	DefinitionUnusedPermissionReviewEvidence  DefinitionErrorKind = "unused_permission_review_evidence"

	// DefinitionInvalidPermissionReviewSecurityCeiling: WithPermissionReviewSecurityCeiling
	// was called with an empty (or all-whitespace) ceiling string. Rejected at
	// Define()-time rather than deferred to a later, harder-to-diagnose
	// review-context-capture failure (gate.ReviewContext's own non-empty
	// SecurityCeiling validation rule).
	DefinitionInvalidPermissionReviewSecurityCeiling DefinitionErrorKind = "invalid_permission_review_security_ceiling"
	// DefinitionMissingPermissionReviewSecurityCeiling: at least one permission
	// classifier is configured (WithPermissionClassifiers) but
	// WithPermissionReviewSecurityCeiling was never called. SecurityCeiling is a
	// consumer-owned value Harness cannot originate (Finding 2, Phase 6
	// spec-compliance review): a classifier-registered session with no ceiling
	// would otherwise fail every real evidence-tool containment check closed,
	// silently, at runtime.
	DefinitionMissingPermissionReviewSecurityCeiling DefinitionErrorKind = "missing_permission_review_security_ceiling"
	// DefinitionUnusedPermissionReviewSecurityCeiling: WithPermissionReviewSecurityCeiling
	// was called but no permission classifier is configured, mirroring
	// DefinitionUnusedPermissionReviewEvidence's "config X requires config Y"
	// symmetric check.
	DefinitionUnusedPermissionReviewSecurityCeiling DefinitionErrorKind = "unused_permission_review_security_ceiling"

	// DefinitionInvalidPermissionReviewObservations: WithPermissionReviewObservations
	// was called with a nil verifier.
	DefinitionInvalidPermissionReviewObservations DefinitionErrorKind = "invalid_permission_review_observations"
	// DefinitionUnusedPermissionReviewObservations: WithPermissionReviewObservations
	// was called but either no permission classifier is configured at all, or
	// no WithPermissionReviewEvidence was configured (design §13.4's
	// observation-recheck mechanism lives entirely inside the evidence
	// runtime — a verifier with no evidence runtime to ever record an
	// observation into is dead configuration). There is deliberately no
	// symmetric "missing" error the way DefinitionMissingPermissionReviewEvidence
	// pairs with WithPermissionReviewEvidence: see WithPermissionReviewObservations'
	// own doc comment for why "a classifier's evidence tools need this"
	// cannot be determined at Define()-time today, and how runtime instead
	// fails closed (internal/sessionruntime/gates.go's
	// verifyPermissionReviewObservations) if that ever turns out to matter
	// for a given session.
	DefinitionUnusedPermissionReviewObservations DefinitionErrorKind = "unused_permission_review_observations"
)

type DefinitionError struct {
	Kind  DefinitionErrorKind
	Name  string
	Cause error
}

func (e *DefinitionError) Error() string {
	msg := "rig: invalid definition (" + string(e.Kind) + ")"
	if e.Name != "" {
		msg += ": " + e.Name
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *DefinitionError) Unwrap() error { return e.Cause }

type LifecycleErrorKind string

const (
	LifecycleContextDone                     LifecycleErrorKind = "context_done"
	LifecycleIDGenerationFailed              LifecycleErrorKind = "id_generation_failed"
	LifecycleLeaseFailed                     LifecycleErrorKind = "lease_failed"
	LifecycleJournalFailed                   LifecycleErrorKind = "journal_failed"
	LifecycleAppenderFailed                  LifecycleErrorKind = "appender_failed"
	LifecycleSessionFailed                   LifecycleErrorKind = "session_failed"
	LifecycleProcessNotificationsUnsupported LifecycleErrorKind = "process_notifications_unsupported"
)

type LifecycleError struct {
	Kind  LifecycleErrorKind
	Cause error
}

func (e *LifecycleError) Error() string {
	msg := "rig: session lifecycle failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *LifecycleError) Unwrap() error { return e.Cause }
