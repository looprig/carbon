package gate

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Bounds on one ObservationRequirement's fields and on how many may travel
// with a single classifier's PermissionAssessmentOutcome. These are sized
// like the other bounded string/collection fields in this package (see
// review_subject.go's MaxPermissionReview* constants) — generous for any
// real canonical identity or token, small enough that a misbehaving
// evidence tool cannot use this new channel to smuggle unbounded data
// through a review.
const (
	MaxObservationRequirementTargetBytes    = 4 << 10
	MaxObservationRequirementTokenBytes     = 4 << 10
	MaxObservationRequirementsPerAssessment = 256
)

// ObservationRequirement is one canonical-identity/token pair recorded when a
// target-sensitive evidence tool observed one target during evidence
// gathering (design §13.4, TOCTOU). Target is the verifier-defined canonical
// identity of the observed target (for example a canonicalized absolute path
// or a resolved git ref) — Harness never computes, interprets, or
// canonicalizes it, for the same reason EvidenceContainmentVerifier's own doc
// comment gives for why Harness does not own path canonicalization. Token is
// an opaque, verifier-defined proof of that target's observed state at
// capture time (for example a hash of stable metadata) — again never
// computed or interpreted by Harness.
//
// A zero ObservationRequirement is never valid on its own (see Valid); the
// zero value exists only so the type is usable as an ordinary Go value
// (comparable, safe to append to a nil slice).
type ObservationRequirement struct {
	Target string
	Token  string
}

// Valid reports whether both fields are non-empty, valid UTF-8, free of NUL
// bytes, and within MaxObservationRequirementTargetBytes/TokenBytes. It is
// the one shape check every producer and consumer of this type shares —
// evidence_runner.go applies it before ever recording a requirement (a
// malformed report from a target-sensitive tool is dropped, not retained),
// and CombinePermissionAssessments applies it again at the outcome boundary
// (defense in depth: never trust a single call site to have validated
// untrusted-shaped data).
func (o ObservationRequirement) Valid() bool {
	return validObservationRequirementField(o.Target, MaxObservationRequirementTargetBytes) &&
		validObservationRequirementField(o.Token, MaxObservationRequirementTokenBytes)
}

func validObservationRequirementField(value string, maxBytes int) bool {
	return value != "" &&
		utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') &&
		len(value) <= maxBytes
}

// EvidenceObservationVerifier is the consumer-supplied, read-only seam that
// rechecks every previously recorded ObservationRequirement immediately
// before a classifier-originated auto-approval claims the gate (design
// §13.4, TOCTOU). It mirrors EvidenceContainmentVerifier's shape exactly —
// same trusted-caller narrowness, same reused EvidenceContainmentPolicy (the
// canonical read root plus the review's own non-widenable security ceiling
// are the identical security context both checks need: containment
// independently re-resolves ONE prepared request at evidence-gathering time,
// this independently re-resolves EVERY previously recorded target at
// pre-approval time — the same "resolve fresh, trust nothing captured
// earlier" contract, just at a different point in the review's lifecycle).
// A new EvidenceObservationPolicy type was deliberately NOT introduced: the
// concerns are identical, and duplicating the type would only invite the two
// to drift.
//
// It receives no session, gate, mutation, grant, rule, or loop-control
// capability — only the policy and a defensive copy of the requirements to
// recheck. Implementations must independently re-derive each requirement's
// current token from its Target and fail closed (return a non-nil error) on
// any mismatch OR on any target that can no longer be unambiguously
// resolved/verified — exactly EvidenceContainmentVerifier's own "fail closed
// when a ... Requirement cannot be mapped unambiguously" requirement,
// applied to a recheck instead of a first check. This is the narrow,
// trusted-caller seam a consumer implements and installs via
// rig.WithPermissionReviewObservations.
type EvidenceObservationVerifier interface {
	VerifyEvidenceObservations(ctx context.Context, policy EvidenceContainmentPolicy, requirements []ObservationRequirement) error
}
