package gate

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/hustle"
)

// PermissionAssessmentOutcome is one classifier's applicability and terminal
// review state. Only applicable, allowed outcomes carry an assessment that can
// contribute to eligibility.
//
// Observations is the set of ObservationRequirement tokens (design §13.4,
// TOCTOU) this classifier's OWN evidence gathering recorded — one review may
// gather evidence about several distinct targets, so this is a slice, not a
// single value. It is meaningful only when Status is ReviewStatusAllowed
// (validPermissionAssessmentOutcome requires it empty otherwise, mirroring
// Assessment's own zero-value-unless-allowed rule): a classifier that never
// reached an allowed terminal never contributes evidence a response could be
// approved on, so its observations (if any were gathered before it failed)
// are simply discarded rather than carried forward.
type PermissionAssessmentOutcome struct {
	Subject      PermissionReviewSubject
	Applicable   bool
	Status       ReviewStatus
	Assessment   PermissionAssessment
	Observations []ObservationRequirement
}

// CombinePermissionAssessments applies ordered conjunctive review semantics
// against one immutable registered classifier set. Every registered classifier
// must contribute exactly one outcome in registration order. The first
// applicable failure wins; non-applicable outcomes are neutral only when their
// status is exactly not_applicable.
func CombinePermissionAssessments(
	policy PermissionReviewPolicy,
	classifiers PermissionClassifierSet,
	outcomes []PermissionAssessmentOutcome,
) ReviewDecision {
	if !validPermissionReviewPolicy(policy) {
		return reviewDecision(ReviewDecisionInvalidPolicy)
	}
	if len(classifiers.ordered) == 0 ||
		len(outcomes) != len(classifiers.ordered) {
		return reviewDecision(ReviewDecisionInvalidAssessment)
	}
	var commonDigest [32]byte
	revisions := make(map[string]struct{}, len(outcomes))
	for index, outcome := range outcomes {
		if outcome.Subject.Basis.ClassifierRevision !=
			classifiers.ordered[index].Revision() {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if !validPermissionAssessmentOutcomeStatus(outcome) {
			return reviewDecision(ReviewDecisionClassifierStatus)
		}
		if outcome.Status != ReviewStatusAllowed &&
			!zeroPermissionAssessment(outcome.Assessment) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if !validStoredPermissionReviewSubject(outcome.Subject) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if policy.Revision != outcome.Subject.Basis.GatePolicyRevision {
			return reviewDecision(ReviewDecisionInvalidPolicy)
		}
		revision := outcome.Subject.Basis.ClassifierRevision
		if _, duplicate := revisions[revision]; duplicate {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		revisions[revision] = struct{}{}
		digest, err := permissionReviewCommonSubjectDigest(outcome.Subject)
		if err != nil {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if index == 0 {
			commonDigest = digest
		} else if digest != commonDigest {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if !validPermissionAssessmentOutcome(outcome) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
	}
	applicable := false
	for _, outcome := range outcomes {
		if !outcome.Applicable {
			continue
		}
		applicable = true
		if outcome.Status != ReviewStatusAllowed {
			return reviewDecision(ReviewDecisionClassifierStatus)
		}
		decision := EvaluatePermissionAssessment(policy, outcome.Subject, outcome.Assessment)
		if !decision.Eligible {
			return decision
		}
	}
	if !applicable {
		return reviewDecision(ReviewDecisionNoApplicableClassifier)
	}
	return ReviewDecision{Eligible: true, Reason: ReviewDecisionEligible}
}

func validPermissionAssessmentOutcome(outcome PermissionAssessmentOutcome) bool {
	if outcome.Status != ReviewStatusAllowed {
		return zeroPermissionAssessment(outcome.Assessment) && len(outcome.Observations) == 0
	}
	if !validObservationRequirements(outcome.Observations) {
		return false
	}
	return outcome.Assessment.Basis == outcome.Subject.Basis &&
		validPermissionAssessment(outcome.Assessment)
}

// validObservationRequirements bounds and shape-checks one outcome's
// recorded observations. It never inspects Target/Token content beyond
// ObservationRequirement.Valid()'s own generic shape rules — meaning is
// entirely consumer-owned.
func validObservationRequirements(observations []ObservationRequirement) bool {
	if len(observations) > MaxObservationRequirementsPerAssessment {
		return false
	}
	for _, observation := range observations {
		if !observation.Valid() {
			return false
		}
	}
	return true
}

func validPermissionAssessmentOutcomeStatus(
	outcome PermissionAssessmentOutcome,
) bool {
	if !outcome.Applicable {
		return outcome.Status == ReviewStatusNotApplicable
	}
	status, valid := ParseReviewStatus(string(outcome.Status))
	return valid && status != ReviewStatusNotApplicable
}

func zeroPermissionAssessment(assessment PermissionAssessment) bool {
	return assessment.Basis == (ReviewBasis{}) &&
		assessment.Risk == "" &&
		assessment.Authorization == "" &&
		len(assessment.Categories) == 0 &&
		assessment.Recommendation == "" &&
		assessment.Rationale == ""
}

// PermissionClassifier is the deliberately narrow contract implemented by
// trusted classifier packages. It conveys data and immutable Hustle policy,
// never gate response or durable-grant authority.
type PermissionClassifier interface {
	Name() hustle.Name
	Revision() string
	Definition() hustle.Definition
	Applies(PermissionReviewSubject) bool
	MarshalInput(PermissionReviewSubject) (json.RawMessage, error)
	ValidateResult(PermissionReviewSubject, hustle.Result) (PermissionAssessment, error)
}

// PermissionClassifierSet is an immutable, ordered classifier registry.
type PermissionClassifierSet struct {
	ordered []PermissionClassifier
}

// MaxPermissionClassifierNameBytes bounds the stable audit identity stored in
// registry and event records.
const MaxPermissionClassifierNameBytes = 128

// PermissionClassifierValidationReason is the bounded registry rejection
// domain. Rejected classifier metadata is never included in the error.
type PermissionClassifierValidationReason string

const (
	PermissionClassifierInvalid   PermissionClassifierValidationReason = "invalid"
	PermissionClassifierDuplicate PermissionClassifierValidationReason = "duplicate"
)

// PermissionClassifierValidationError reports only a bounded reason and
// registration position.
type PermissionClassifierValidationError struct {
	Index  int
	Reason PermissionClassifierValidationReason
}

func (*PermissionClassifierValidationError) Error() string {
	return "gate: invalid permission classifier registration"
}

// PermissionClassifierNameValidationError reports a rejected classifier audit
// name without echoing the untrusted value.
type PermissionClassifierNameValidationError struct{}

func (*PermissionClassifierNameValidationError) Error() string {
	return "gate: invalid permission classifier name"
}

// ValidatePermissionClassifierName applies the stricter canonical name
// contract shared by permission-classifier registries and durable audit events.
func ValidatePermissionClassifierName(name hustle.Name) error {
	value := string(name)
	if !utf8.ValidString(value) ||
		value == "" ||
		strings.TrimSpace(value) != value ||
		strings.IndexByte(value, 0) >= 0 ||
		len(value) > MaxPermissionClassifierNameBytes ||
		name.Validate() != nil {
		return &PermissionClassifierNameValidationError{}
	}
	return nil
}

// PermissionClassifierPanicMethod identifies which trust-boundary method of a
// registered PermissionClassifier panicked. It exists only to route a
// recovered panic into a bounded, content-free error — never to convey the
// panic value itself.
type PermissionClassifierPanicMethod string

const (
	PermissionClassifierPanicMarshalInput   PermissionClassifierPanicMethod = "marshal_input"
	PermissionClassifierPanicValidateResult PermissionClassifierPanicMethod = "validate_result"
)

// PermissionClassifierPanicError is the redacted recovery product for a panic
// raised by a registered PermissionClassifier implementation. Classifiers are
// trusted but not infallible: a buggy implementation can still panic, and a
// panic on the review goroutine would otherwise crash the whole process
// rather than just fail the one review. This is the bounded internal failure
// design's error taxonomy calls "callback panic at a trusted boundary" —
// it deliberately retains no panic value, since the panic could be an error,
// a string, or anything else, and could itself carry raw classifier-
// controlled subject content.
type PermissionClassifierPanicError struct {
	Method PermissionClassifierPanicMethod
}

func (e *PermissionClassifierPanicError) Error() string {
	return "gate: permission classifier panicked (" + string(e.Method) + ")"
}

type frozenPermissionClassifier struct {
	source     PermissionClassifier
	name       hustle.Name
	revision   string
	definition hustle.Definition
}

func (c *frozenPermissionClassifier) Name() hustle.Name             { return c.name }
func (c *frozenPermissionClassifier) Revision() string              { return c.revision }
func (c *frozenPermissionClassifier) Definition() hustle.Definition { return c.definition }

// Applies is intentionally NOT recover-wrapped here: PermissionClassifier.Applies
// returns a bare bool, so this registry boundary has no way to signal "the
// call panicked" distinctly from an ordinary "not applicable" false without
// changing the interface every registered classifier implements (which would
// also let a panic in one classifier masquerade as an ordinary non-applicable
// result and let a DIFFERENT classifier's allow decide the whole gate — see
// design §11/§25.4). internal/sessionruntime's reviewOne, which needs and has
// that three-way distinction (applicable / not-applicable / failed) in the
// gate.PermissionAssessmentOutcome it builds, recovers this specific call
// itself instead.
func (c *frozenPermissionClassifier) Applies(subject PermissionReviewSubject) bool {
	return c.source.Applies(subject.Clone())
}

func (c *frozenPermissionClassifier) MarshalInput(
	subject PermissionReviewSubject,
) (raw json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			raw = nil
			err = &PermissionClassifierPanicError{Method: PermissionClassifierPanicMarshalInput}
		}
	}()
	return c.source.MarshalInput(subject.Clone())
}

// ValidateResult is recover-wrapped here for defense-in-depth consistency
// with MarshalInput, even though a panic here is already indirectly caught by
// internal/hustleruntime's own callValidator recover (this method is invoked
// from inside a validator closure hustleruntime wraps). Recovering at this
// registry boundary instead means every caller of a registered
// PermissionClassifier — not only today's one call site — gets the same
// bounded-error guarantee, and the resulting error is classified as an
// ordinary validation failure rather than needing a second typed-error case
// downstream.
func (c *frozenPermissionClassifier) ValidateResult(
	subject PermissionReviewSubject,
	result hustle.Result,
) (assessment PermissionAssessment, err error) {
	defer func() {
		if recover() != nil {
			assessment = PermissionAssessment{}
			err = &PermissionClassifierPanicError{Method: PermissionClassifierPanicValidateResult}
		}
	}()
	return c.source.ValidateResult(subject.Clone(), result)
}

// NewPermissionClassifierSet validates metadata without executing classifier
// applicability, serialization, or result parsing behavior.
func NewPermissionClassifierSet(
	classifiers ...PermissionClassifier,
) (PermissionClassifierSet, error) {
	if len(classifiers) == 0 {
		return PermissionClassifierSet{}, classifierSetError(0, PermissionClassifierInvalid)
	}
	ordered := make([]PermissionClassifier, 0, len(classifiers))
	names := make(map[hustle.Name]struct{}, len(classifiers))
	revisions := make(map[string]struct{}, len(classifiers))
	for index, classifier := range classifiers {
		if nilPermissionClassifier(classifier) {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		name := classifier.Name()
		revision := classifier.Revision()
		definition := classifier.Definition()
		if err := ValidatePermissionClassifierName(name); err != nil {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		if _, duplicate := names[name]; duplicate {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierDuplicate)
		}
		if strings.TrimSpace(revision) == "" ||
			!utf8.ValidString(revision) ||
			len(revision) > MaxPermissionClassifierRevisionBytes {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		if _, duplicate := revisions[revision]; duplicate {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierDuplicate)
		}
		descriptor := definition.Descriptor()
		if err := descriptor.Validate(); err != nil ||
			descriptor.Participation != hustle.ParticipationBlocking ||
			descriptor.ModelSource != hustle.ModelSourceNamed ||
			descriptor.OutputSchemaName == "" ||
			descriptor.OutputSchemaSHA256 == ([32]byte{}) ||
			descriptor.StructuredOutputRevision == "" ||
			descriptor.EvidenceToolPolicyRevision == "" ||
			descriptor.EvidenceToolDefinitionsSHA256 == ([32]byte{}) ||
			descriptor.EvidenceProducedToolNamesSHA256 == ([32]byte{}) ||
			!descriptor.StructuredOutputWithTools ||
			descriptor.Name != name ||
			descriptor.PolicyRevision != revision {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		names[name] = struct{}{}
		revisions[revision] = struct{}{}
		ordered = append(ordered, &frozenPermissionClassifier{
			source:     classifier,
			name:       name,
			revision:   revision,
			definition: definition,
		})
	}
	return PermissionClassifierSet{ordered: ordered}, nil
}

// Classifiers returns an independent ordered registry view.
func (s PermissionClassifierSet) Classifiers() []PermissionClassifier {
	return append([]PermissionClassifier(nil), s.ordered...)
}

func nilPermissionClassifier(classifier PermissionClassifier) bool {
	if classifier == nil {
		return true
	}
	value := reflect.ValueOf(classifier)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func classifierSetError(
	index int,
	reason PermissionClassifierValidationReason,
) error {
	return &PermissionClassifierValidationError{Index: index, Reason: reason}
}
