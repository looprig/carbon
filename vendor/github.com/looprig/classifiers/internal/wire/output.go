package wire

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/inference"
)

// schema.json's "risk" and "categories" properties carry a substantive
// description defining each enum value in prose (mirroring
// internal/prompt/command_safety.md's "### Risk Taxonomy" section), NOT a
// per-enum-value description: the bounded portable JSON Schema subset
// inference.ValidateOutputSchema accepts has no mechanism for one
// (github.com/looprig/inference's schemaNode supports only a single
// type-level "description", not per-enum-item metadata such as a
// const/description pair — an object element inside "enum" would fail
// enumValueMatches's string-type check for a string-typed node). A prose
// property-level description is therefore the richest definition this
// wire's schema can carry without breaking schema validity.
//
//go:embed schema.json
var outputSchemaJSON []byte

const (
	// OutputSchemaName is the model-facing structured-output schema name
	// handed to inference.OutputSchema.
	OutputSchemaName = "command_safety_assessment"
	// OutputRevision independently versions this wire's shape and strict
	// decode discipline, per design §19.2 ("prompt, policy, schema, wire,
	// and corpus revisions are independently explicit and included in
	// classifier identity").
	OutputRevision = "command-safety-output/v1"

	outputSchemaDescription = "The structured command-safety review verdict for exactly one prepared tool call."
	outputWireVersion       = "command_safety_output.v1"

	// MaxOutputWireBytes bounds the raw structured-output JSON before it is
	// parsed, independent of any hustle-level payload limit.
	MaxOutputWireBytes = 64 * 1024
)

// OutputSchema returns the provider-neutral structured-output policy handed
// to the model. It returns a fresh clone on every call so callers cannot
// mutate shared state.
func OutputSchema() inference.OutputSchema {
	return inference.OutputSchema{
		Name:        OutputSchemaName,
		Description: outputSchemaDescription,
		Schema:      append(json.RawMessage(nil), outputSchemaJSON...),
		Strict:      true,
	}
}

type outputWireV1 struct {
	Version        string            `json:"version"`
	Basis          outputBasisWireV1 `json:"basis"`
	Risk           string            `json:"risk"`
	Authorization  string            `json:"authorization"`
	Categories     []string          `json:"categories"`
	Recommendation string            `json:"recommendation"`
	Rationale      string            `json:"rationale"`
}

type outputBasisWireV1 struct {
	GateID             string `json:"gate_id"`
	ToolExecutionID    string `json:"tool_execution_id"`
	SubjectDigest      string `json:"subject_digest"`
	ContextRevision    string `json:"context_revision"`
	GatePolicyRevision string `json:"gate_policy_revision"`
	ClassifierRevision string `json:"classifier_revision"`
	SecurityCeiling    string `json:"security_ceiling"`
}

// OutputValidationField identifies the bounded part of a model's structured
// output that failed validation.
type OutputValidationField string

const (
	OutputFieldWire           OutputValidationField = "wire"
	OutputFieldBasis          OutputValidationField = "basis"
	OutputFieldRisk           OutputValidationField = "risk"
	OutputFieldAuthorization  OutputValidationField = "authorization"
	OutputFieldCategories     OutputValidationField = "categories"
	OutputFieldRecommendation OutputValidationField = "recommendation"
	OutputFieldRationale      OutputValidationField = "rationale"
)

// OutputValidationReason classifies an output validation failure.
type OutputValidationReason string

const (
	OutputReasonRequired    OutputValidationReason = "required"
	OutputReasonInvalid     OutputValidationReason = "invalid"
	OutputReasonUnsupported OutputValidationReason = "unsupported"
	OutputReasonOutOfBounds OutputValidationReason = "out_of_bounds"
	OutputReasonMismatch    OutputValidationReason = "mismatch"
)

// OutputValidationError reports a rejected model structured-output payload.
// It deliberately carries no rejected value — in particular never the
// model's rationale text — so untrusted or oversized model output can never
// leak through an error message, log line, or audit record.
type OutputValidationError struct {
	Field  OutputValidationField
	Reason OutputValidationReason
}

func (e *OutputValidationError) Error() string {
	return "wire: invalid command-safety output (" + string(e.Field) + ": " + string(e.Reason) + ")"
}

// Retryable reports whether this output-validation failure is a pure
// syntax/shape signal, safe for a caller to treat as
// hustle.NewRecoverableTerminalValidationError() under
// hustle.RetryPolicyClassifiedOnce (design §12.6). Every DecodeOutput
// failure is a shape problem — duplicate/unknown/missing/null wire fields,
// an oversized or non-UTF-8 payload, or a malformed/non-canonical enum
// string — EXCEPT a basis mismatch, which is a semantic identity check
// (the model echoed back a basis that does not match the trusted subject)
// and design §12.6 explicitly excludes: "A basis mismatch ... must not use
// it."
func (e *OutputValidationError) Retryable() bool {
	return !(e.Field == OutputFieldBasis && e.Reason == OutputReasonMismatch)
}

func outputError(field OutputValidationField, reason OutputValidationReason) error {
	return &OutputValidationError{Field: field, Reason: reason}
}

// DecodeOutput strictly decodes and validates one model structured-output
// payload against subject, and returns the resulting PermissionAssessment.
// Unknown fields, duplicate JSON keys, JSON null in any required position,
// trailing JSON content, an oversized payload, an oversized or (for
// non-low risk) empty rationale, an unknown or duplicate category, an
// unrecognized enum value, and a basis that does not exactly match subject
// are all rejected. The returned assessment's Basis is always subject.Basis
// verbatim — the model's echoed basis is used only to confirm consistency,
// never to populate the trusted result.
func DecodeOutput(subject gate.PermissionReviewSubject, raw json.RawMessage) (gate.PermissionAssessment, error) {
	if len(raw) == 0 || len(raw) > MaxOutputWireBytes || isExplicitJSONNull(raw) || !utf8.Valid(raw) {
		return gate.PermissionAssessment{}, outputError(OutputFieldWire, OutputReasonOutOfBounds)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return gate.PermissionAssessment{}, outputError(OutputFieldWire, OutputReasonInvalid)
	}
	var decoded outputWireV1
	if err := decodeStrict(raw, &decoded); err != nil {
		return gate.PermissionAssessment{}, outputError(OutputFieldWire, OutputReasonInvalid)
	}
	if err := validateOutputWireShape(raw); err != nil {
		return gate.PermissionAssessment{}, err
	}
	if decoded.Version != outputWireVersion {
		return gate.PermissionAssessment{}, outputError(OutputFieldWire, OutputReasonUnsupported)
	}
	if err := validateOutputBasisMatches(subject.Basis, decoded.Basis); err != nil {
		return gate.PermissionAssessment{}, err
	}

	risk, ok := gate.ParseReviewRisk(decoded.Risk)
	if !ok {
		return gate.PermissionAssessment{}, outputError(OutputFieldRisk, OutputReasonUnsupported)
	}
	authorization, ok := gate.ParseReviewAuthorization(decoded.Authorization)
	if !ok {
		return gate.PermissionAssessment{}, outputError(OutputFieldAuthorization, OutputReasonUnsupported)
	}
	recommendation, ok := gate.ParseReviewRecommendation(decoded.Recommendation)
	if !ok {
		return gate.PermissionAssessment{}, outputError(OutputFieldRecommendation, OutputReasonUnsupported)
	}
	categories := make([]gate.ReviewRiskCategory, len(decoded.Categories))
	for i, value := range decoded.Categories {
		categories[i] = gate.ReviewRiskCategory(value)
	}
	if err := gate.ValidateReviewCategories(categories); err != nil {
		return gate.PermissionAssessment{}, outputError(OutputFieldCategories, OutputReasonInvalid)
	}
	if !utf8.ValidString(decoded.Rationale) || len(decoded.Rationale) > gate.MaxPermissionReviewRationaleBytes {
		return gate.PermissionAssessment{}, outputError(OutputFieldRationale, OutputReasonOutOfBounds)
	}
	if risk != gate.ReviewRiskLow && strings.TrimSpace(decoded.Rationale) == "" {
		return gate.PermissionAssessment{}, outputError(OutputFieldRationale, OutputReasonRequired)
	}

	return gate.PermissionAssessment{
		Basis:          subject.Basis,
		Risk:           risk,
		Authorization:  authorization,
		Categories:     categories,
		Recommendation: recommendation,
		Rationale:      decoded.Rationale,
	}, nil
}

// validateOutputWireShape re-walks the raw JSON structurally so a `null`
// cannot silently stand in for an omitted field anywhere in the payload,
// including inside the nested basis object and each categories element.
// decodeStrict already rejects unknown top-level and nested-struct fields
// via DisallowUnknownFields; this pass additionally confirms every object
// has exactly its required member set and every required string or array
// value is not JSON null.
func validateOutputWireShape(data []byte) error {
	root, err := requireWireObject(data, []string{
		"version", "basis", "risk", "authorization", "categories", "recommendation", "rationale",
	})
	if err != nil {
		return outputError(OutputFieldWire, OutputReasonInvalid)
	}
	if err := requireWireStrings(root, "version", "risk", "authorization", "recommendation", "rationale"); err != nil {
		return outputError(OutputFieldWire, OutputReasonInvalid)
	}
	basis, err := requireWireObject(root["basis"], []string{
		"gate_id", "tool_execution_id", "subject_digest", "context_revision",
		"gate_policy_revision", "classifier_revision", "security_ceiling",
	})
	if err != nil {
		return outputError(OutputFieldBasis, OutputReasonInvalid)
	}
	if err := requireWireStrings(
		basis,
		"gate_id",
		"tool_execution_id",
		"subject_digest",
		"context_revision",
		"gate_policy_revision",
		"classifier_revision",
		"security_ceiling",
	); err != nil {
		return outputError(OutputFieldBasis, OutputReasonInvalid)
	}
	categories, err := requireWireArray(root["categories"])
	if err != nil {
		return outputError(OutputFieldCategories, OutputReasonInvalid)
	}
	for _, element := range categories {
		if err := requireWireString(element); err != nil {
			return outputError(OutputFieldCategories, OutputReasonInvalid)
		}
	}
	return nil
}

func validateOutputBasisMatches(basis gate.ReviewBasis, wire outputBasisWireV1) error {
	digestHex := hex.EncodeToString(basis.SubjectDigest[:])
	if wire.GateID != basis.GateID.String() ||
		wire.ToolExecutionID != basis.ToolExecutionID.String() ||
		wire.SubjectDigest != digestHex ||
		wire.ContextRevision != basis.ContextRevision ||
		wire.GatePolicyRevision != basis.GatePolicyRevision ||
		wire.ClassifierRevision != basis.ClassifierRevision ||
		wire.SecurityCeiling != basis.SecurityCeiling {
		return outputError(OutputFieldBasis, OutputReasonMismatch)
	}
	return nil
}
