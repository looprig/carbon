package commandsafety

import "github.com/looprig/classifiers/internal/evidence"

// RequiredEvidenceKinds returns the exact set of tool.Requirement.Kind
// values this classifier's evidence tools declare — the allowlist a
// consumer must configure (e.g. via Harness's rig.WithPermissionReviewEvidence)
// for evidence gathering to actually run. The returned slice is a fresh
// copy; callers may not observe or cause mutation of internal state.
//
// The set is derived from internal/evidence.RequirementKinds, the same
// single source of truth internal/evidence's own evidence tools declare
// their Requirement.Kind values from (see internal/evidence/catalog.go).
// This package never re-declares or hand-copies those kind strings.
func RequiredEvidenceKinds() []string {
	return evidence.RequirementKinds()
}
