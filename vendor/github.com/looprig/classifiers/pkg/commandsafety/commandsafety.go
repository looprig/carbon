package commandsafety

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/looprig/classifiers/internal/evidence"
	"github.com/looprig/classifiers/internal/policy"
	"github.com/looprig/classifiers/internal/prompt"
	"github.com/looprig/classifiers/internal/wire"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

// Name is the stable registration name of the command-safety classifier.
const Name hustle.Name = "gate.command-safety"

const (
	// timeout bounds one invocation, including its evidence-tool loop.
	timeout = 90 * time.Second
	// inputBytes/outputBytes bound the serialized hustle request/response.
	// outputBytes intentionally matches wire.MaxOutputWireBytes: the model's
	// structured output can never legitimately exceed what the output codec
	// will accept.
	inputBytes  = 1 << 20
	outputBytes = wire.MaxOutputWireBytes

	// evidenceRevision changes whenever the command-safety evidence-tool
	// catalog's produced tool set, schemas, or bound limits change in a way
	// that should be visible in classifier/rig identity (see
	// hustle.DefinitionDescriptor's evidence-tool policy revision).
	evidenceRevision = "command-safety-evidence/v2"

	// evidenceMaxRounds/evidenceMaxCalls/evidenceMaxCallsPerRound bound the
	// tool-use loop shape; evidenceMaxResultBytes/evidenceMaxEvidenceBytes
	// bound one call's result and the whole loop's aggregate evidence,
	// independently of every internal/evidence tool's own source-level
	// truncation (design §12.2/§13.1 — these are the runtime's OWN bounds,
	// not a substitute for a tool bounding itself).
	evidenceMaxRounds        = 8
	evidenceMaxCalls         = 24
	evidenceMaxCallsPerRound = 6
	evidenceMaxResultBytes   = 256 << 10
	evidenceMaxEvidenceBytes = 2 << 20
)

// Policy is the consumer-tunable command-safety risk policy: the classifier's
// own deterministic taxonomy of category risk floors, absolute-human
// categories, and minimum authorization thresholds (internal/policy.Policy).
// Revision alone participates in classifier identity (see
// Definition().Descriptor().PolicyRevision); the taxonomy content it names
// determines what ValidateResult reconciles a decoded model assessment
// against before that assessment ever crosses this module's public boundary.
// Policy is a type alias rather than a wrapper so its Clone method (defensive
// deep copy of every map field) is inherited directly from internal/policy.
type Policy = policy.Policy

// DefaultPolicy returns the initial command-safety policy: a stable,
// non-empty revision label paired with internal/policy.DefaultPolicy's
// taxonomy content.
func DefaultPolicy() Policy {
	return policy.DefaultPolicy()
}

// ReadEvidencePolicy configures the read-only evidence collection used by
// StandardEvidence: the bounded output limits every internal/evidence
// filesystem and Git tool truncates at the source (Limits' zero value falls
// back to evidence.DefaultLimits()), and an optional injected
// VisibilityResolver for evidence_git_remotes (nil means every remote is
// reported evidence.VisibilityUnknown — see evidence.VisibilityResolver for
// why this package never performs its own network lookup).
type ReadEvidencePolicy struct {
	Limits             evidence.Limits
	VisibilityResolver evidence.VisibilityResolver
}

// StandardEvidence returns the command-safety classifier's evidence-tool
// policy: the complete filesystem and Git evidence pack from
// internal/evidence (design §13.2 — canonical path resolution, lstat and
// resolved-target metadata, bounded directory listing/file reading/glob/
// grep, Git repository/worktree state, status and diff metadata, remotes,
// branch/upstream/default-branch, and remote-visibility evidence), bound by
// policy.
func StandardEvidence(policy ReadEvidencePolicy) hustle.EvidenceToolPolicy {
	return hustle.EvidenceToolPolicy{
		Revision: evidenceRevision,
		Limits: hustle.ToolLoopLimits{
			MaxRounds:        evidenceMaxRounds,
			MaxCalls:         evidenceMaxCalls,
			MaxCallsPerRound: evidenceMaxCallsPerRound,
			MaxResultBytes:   evidenceMaxResultBytes,
			MaxEvidenceBytes: evidenceMaxEvidenceBytes,
		},
		Definitions: evidence.Definitions(policy.Limits, policy.VisibilityResolver),
	}
}

// Options configures New. Inference and Model together become the
// classifier's immutable named model binding; Policy and Evidence become
// its immutable local policy and evidence-tool catalog.
type Options struct {
	Inference inference.Client
	Model     model.Model
	Policy    Policy
	Evidence  hustle.EvidenceToolPolicy
}

// AbsoluteHumanCategoryFloor is the minimum set of gate.ReviewRiskCategory
// values every Policy passed to New must mark absolute-human (via
// Policy.AbsoluteHumanCategories), regardless of what else a caller
// configures. These are exactly the categories representing the classifier
// being uncertain about itself (gate.ReviewCategoryAuthorizationConflict,
// gate.ReviewCategoryTargetAmbiguity, gate.ReviewCategoryInsufficientEvidence
// — conflicting authorization evidence, an unidentifiable target, or a fact
// the classifier could not establish even after investigation) or a class of
// harm severe enough that no policy configuration should ever be able to
// permit auto-approval (gate.ReviewCategoryDataExfiltration,
// gate.ReviewCategoryPromptInjection). A caller-supplied Policy may always
// mark MORE categories absolute-human than this; New only ever floors the
// set, never widens what a caller can additionally restrict. This closes a
// real gap: a Policy with an empty or partial AbsoluteHumanCategories used to
// pass New unchanged, silently discarding this classifier's own safety
// taxonomy and letting internal/policy.Reconcile validate a model's
// assessment against nothing.
var AbsoluteHumanCategoryFloor = []gate.ReviewRiskCategory{
	gate.ReviewCategoryDataExfiltration,
	gate.ReviewCategoryPromptInjection,
	gate.ReviewCategoryAuthorizationConflict,
	gate.ReviewCategoryTargetAmbiguity,
	gate.ReviewCategoryInsufficientEvidence,
}

// ErrPolicyMissingAbsoluteHumanFloor is the sentinel New's *ConstructionError
// wraps as Cause when a caller-supplied Policy's AbsoluteHumanCategories does
// not cover every category in AbsoluteHumanCategoryFloor. Use errors.Is to
// detect this specific rejection reason.
var ErrPolicyMissingAbsoluteHumanFloor = errors.New(
	"commandsafety: policy AbsoluteHumanCategories must cover the classifier's own self-uncertainty/safety floor",
)

// missingAbsoluteHumanFloor reports every category in AbsoluteHumanCategoryFloor
// that is missing from have, in AbsoluteHumanCategoryFloor's own fixed order.
func missingAbsoluteHumanFloor(have map[gate.ReviewRiskCategory]struct{}) []gate.ReviewRiskCategory {
	var missing []gate.ReviewRiskCategory
	for _, category := range AbsoluteHumanCategoryFloor {
		if _, ok := have[category]; !ok {
			missing = append(missing, category)
		}
	}
	return missing
}

// ConstructionField identifies the Options field a construction error
// concerns.
type ConstructionField string

const (
	FieldInference         ConstructionField = "inference"
	FieldModel             ConstructionField = "model"
	FieldModelCapabilities ConstructionField = "model_capabilities"
	FieldPolicy            ConstructionField = "policy"
	FieldEvidence          ConstructionField = "evidence"
	FieldDefinition        ConstructionField = "definition"
)

// ConstructionError reports why New rejected its Options. Cause, when
// present, is either an already secret-free typed error from Harness's own
// construction machinery (hustle.DefinitionError, model.ValidationError) or
// one of this package's own construction-time validation sentinels (e.g.
// ErrPolicyMissingAbsoluteHumanFloor, optionally wrapping the fixed,
// non-secret enum values that failed validation): never raw request content.
type ConstructionError struct {
	Field ConstructionField
	Cause error
}

func (e *ConstructionError) Error() string {
	message := "commandsafety: invalid construction (" + string(e.Field) + ")"
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *ConstructionError) Unwrap() error { return e.Cause }

// Classifier is the command-safety gate.PermissionClassifier implementation.
// Its zero value is invalid; construct one with New.
type Classifier struct {
	definition hustle.Definition
	revision   string
	policy     Policy
}

// New validates options and returns an immutable command-safety classifier.
//
// It requires a non-nil (including non-typed-nil) inference client, a
// structurally valid model that additionally advertises Tools,
// StructuredOutput, and StructuredOutputWithTools (design §12.3: a
// tool-using structured Hustle requires all three), a non-empty policy
// revision, and an evidence-tool policy hustle.Define accepts. Every
// rejection is a typed *ConstructionError; no supplied value is echoed.
func New(options Options) (*Classifier, error) {
	if nilInferenceClient(options.Inference) {
		return nil, &ConstructionError{Field: FieldInference}
	}
	if err := options.Model.Validate(); err != nil {
		return nil, &ConstructionError{Field: FieldModel, Cause: err}
	}
	if !options.Model.Caps.Tools || !options.Model.Caps.StructuredOutput || !options.Model.Caps.StructuredOutputWithTools {
		return nil, &ConstructionError{Field: FieldModelCapabilities}
	}
	policyRevision := strings.TrimSpace(options.Policy.Revision)
	if policyRevision == "" {
		return nil, &ConstructionError{Field: FieldPolicy}
	}
	if missing := missingAbsoluteHumanFloor(options.Policy.AbsoluteHumanCategories); len(missing) > 0 {
		return nil, &ConstructionError{
			Field: FieldPolicy,
			Cause: fmt.Errorf("%w: missing %v", ErrPolicyMissingAbsoluteHumanFloor, missing),
		}
	}
	// A tool-using structured Hustle (design §12.3) requires at least one
	// evidence tool definition: hustle.EvidenceToolPolicy{} (the zero value)
	// is a structurally valid "evidence tools disabled" policy to
	// hustle.Define, but a disabled policy would silently build a
	// definition with StructuredOutputWithTools == false, which
	// gate.NewPermissionClassifierSet later rejects outright. Fail here,
	// at construction, with a typed and specific reason instead.
	if len(options.Evidence.Definitions) == 0 {
		return nil, &ConstructionError{Field: FieldEvidence}
	}

	definition, err := hustle.Define(
		hustle.WithName(Name),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(timeout),
		hustle.WithLimits(hustle.Limits{InputBytes: inputBytes, OutputBytes: outputBytes}),
		hustle.WithNamedInference(options.Inference, options.Model),
		hustle.WithSystemPrompt(prompt.CommandSafety(), prompt.CommandSafetyRevision),
		hustle.WithPolicyRevision(policyRevision),
		hustle.WithOutputSchema(wire.OutputSchema()),
		hustle.WithEvidenceTools(options.Evidence),
		hustle.WithRetryPolicy(hustle.RetryPolicyClassifiedOnce),
	)
	if err != nil {
		return nil, &ConstructionError{Field: FieldDefinition, Cause: err}
	}

	return &Classifier{definition: definition, revision: policyRevision, policy: options.Policy.Clone()}, nil
}

// Name returns the classifier's stable registration name.
func (c *Classifier) Name() hustle.Name { return Name }

// Revision returns the classifier's active policy revision.
func (c *Classifier) Revision() string { return c.revision }

// Definition returns the classifier's immutable hustle definition.
func (c *Classifier) Definition() hustle.Definition { return c.definition }

// Applies reports whether subject's prepared request contains a
// command-execution requirement. Applicability is based on the typed
// tool.CapabilityCommandExecute requirement kind, never a tool display
// name (design §19.1). A later task extends this to the fuller
// command-triggered filesystem/network combination.
func (c *Classifier) Applies(subject gate.PermissionReviewSubject) bool {
	for _, requirement := range subject.Request.Requirements {
		if requirement.Kind == tool.CapabilityCommandExecute {
			return true
		}
	}
	return false
}

// MarshalInput projects subject into the classifier's versioned model input.
func (c *Classifier) MarshalInput(subject gate.PermissionReviewSubject) (json.RawMessage, error) {
	return wire.MarshalInput(subject)
}

// ValidateResult strictly decodes and validates one hustle result against
// subject, then reconciles the decoded model assessment against this
// classifier's own deterministic command-safety policy (internal/policy)
// before returning it. Reconciliation only ever tightens: it can turn an
// internally inconsistent or taxonomically absolute-human allow
// recommendation into needs_human, but it never lowers a reported risk,
// never raises a reported authorization, and never turns an existing
// needs_human back into allow. It never returns an assessment whose Basis
// differs from subject.Basis.
func (c *Classifier) ValidateResult(
	subject gate.PermissionReviewSubject,
	result hustle.Result,
) (gate.PermissionAssessment, error) {
	assessment, err := wire.DecodeOutput(subject, result.Output)
	if err != nil {
		// Design §12.6: only a pure syntax/shape terminal-decoding failure
		// (duplicate, unknown, missing, or otherwise invalid wire fields) may
		// be classified retryable. A basis mismatch is a semantic identity
		// check, not a wire-shape problem, and must never use the marker —
		// see wire.OutputValidationError.Retryable for the exact boundary.
		var validationErr *wire.OutputValidationError
		if errors.As(err, &validationErr) && validationErr.Retryable() {
			return gate.PermissionAssessment{}, hustle.NewRecoverableTerminalValidationError()
		}
		return gate.PermissionAssessment{}, err
	}
	return policy.Reconcile(c.policy, assessment), nil
}

// nilInferenceClient reports whether client is nil, including a typed nil
// concrete value boxed in the interface (a plain `client == nil` check
// misses that case).
func nilInferenceClient(client inference.Client) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ gate.PermissionClassifier = (*Classifier)(nil)
