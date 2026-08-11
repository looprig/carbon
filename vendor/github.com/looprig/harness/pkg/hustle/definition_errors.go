package hustle

// DefinitionErrorKind identifies an invalid immutable hustle definition.
type DefinitionErrorKind string

const (
	DefinitionMissingName           DefinitionErrorKind = "missing_name"
	DefinitionReservedName          DefinitionErrorKind = "reserved_name"
	DefinitionNilOption             DefinitionErrorKind = "nil_option"
	DefinitionDuplicateOption       DefinitionErrorKind = "duplicate_option"
	DefinitionInvalidParticipation  DefinitionErrorKind = "invalid_participation"
	DefinitionInvalidModelSource    DefinitionErrorKind = "invalid_model_source"
	DefinitionMissingModelSource    DefinitionErrorKind = "missing_model_source"
	DefinitionInvalidClient         DefinitionErrorKind = "invalid_client"
	DefinitionInvalidModel          DefinitionErrorKind = "invalid_model"
	DefinitionInvalidTimeout        DefinitionErrorKind = "invalid_timeout"
	DefinitionInvalidLimits         DefinitionErrorKind = "invalid_limits"
	DefinitionInvalidSystemPrompt   DefinitionErrorKind = "invalid_system_prompt"
	DefinitionInvalidPromptRevision DefinitionErrorKind = "invalid_prompt_revision"
	DefinitionMissingPolicyRevision DefinitionErrorKind = "missing_policy_revision"
	DefinitionInvalidPolicyRevision DefinitionErrorKind = "invalid_policy_revision"
	DefinitionInvalidOutputSchema   DefinitionErrorKind = "invalid_output_schema"
	DefinitionInvalidEvidenceTools  DefinitionErrorKind = "invalid_evidence_tools"
	DefinitionInvalidRetryPolicy    DefinitionErrorKind = "invalid_retry_policy"
)

// DefinitionError reports a definition boundary failure without retaining raw
// prompts, model endpoints, or client identity in its message.
type DefinitionError struct {
	Kind  DefinitionErrorKind
	Field string
	Cause error
}

func (e *DefinitionError) Error() string {
	message := "hustle: invalid definition: " + string(e.Kind)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	return message
}

func (e *DefinitionError) Unwrap() error { return e.Cause }

// BindErrorKind identifies a definition binding failure.
type BindErrorKind string

const (
	BindInvalidDefinition    BindErrorKind = "invalid_definition"
	BindInvalidContext       BindErrorKind = "invalid_context"
	BindMissingModelResolver BindErrorKind = "missing_model_resolver"
	BindInvalidEvidenceTools BindErrorKind = "invalid_evidence_tools"
)

// BindError reports why an immutable definition could not be bound.
type BindError struct {
	Kind  BindErrorKind
	Cause error
}

func (e *BindError) Error() string { return "hustle: bind: " + string(e.Kind) }

func (e *BindError) Unwrap() error { return e.Cause }

// ResolveErrorKind identifies an inference binding resolution failure.
type ResolveErrorKind string

const (
	ResolveInvalidContext ResolveErrorKind = "invalid_context"
	ResolveInvalidLoopID  ResolveErrorKind = "invalid_loop_id"
	ResolveModelFailed    ResolveErrorKind = "model_failed"
	ResolveInvalidBinding ResolveErrorKind = "invalid_binding"
)

// ResolveError reports a model-resolution failure without exposing model or
// client details. Cause remains available to trusted callers through errors.Is.
type ResolveError struct {
	Kind  ResolveErrorKind
	Cause error
}

func (e *ResolveError) Error() string { return "hustle: resolve inference: " + string(e.Kind) }

func (e *ResolveError) Unwrap() error { return e.Cause }

// RevisionError reports the impossible failure to encode a closed, typed
// policy projection. It exists so even programmer failures retain type identity.
type RevisionError struct{ Cause error }

func (e *RevisionError) Error() string { return "hustle: policy revision encoding failed" }

func (e *RevisionError) Unwrap() error { return e.Cause }
