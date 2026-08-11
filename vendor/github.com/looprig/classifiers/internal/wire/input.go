// Package wire implements the command-safety classifier's strict, versioned
// JSON codecs for the model's input and output payloads: MarshalInput
// (input.go) projects a validated gate.PermissionReviewSubject into the
// bounded, versioned JSON the model sees as evidence, and DecodeOutput
// (output.go) strictly decodes and validates the model's structured-output
// verdict back into a gate.PermissionAssessment. Both directions follow the
// same strict decode discipline Harness's own pkg/gate uses for its wire
// codecs (see strict.go): no unknown or duplicate JSON field, no JSON null
// masquerading as an omitted field, and no trailing content.
package wire

import (
	"encoding/hex"
	"encoding/json"

	"github.com/looprig/harness/pkg/gate"
)

const (
	// InputWireVersion is the literal version tag stamped on every marshaled
	// input payload.
	InputWireVersion = "command_safety_input.v1"
	// InputRevision independently versions this wire's shape, per design
	// §19.2.
	InputRevision = "command-safety-input/v1"
	// MaxInputWireBytes bounds the marshaled input payload.
	MaxInputWireBytes = 1 << 20
)

type inputWireV1 struct {
	Version string             `json:"version"`
	Basis   inputBasisWireV1   `json:"basis"`
	Request inputRequestWireV1 `json:"request"`
	Context inputContextWireV1 `json:"context"`
}

type inputBasisWireV1 struct {
	GateID             string `json:"gate_id"`
	ToolExecutionID    string `json:"tool_execution_id"`
	SubjectDigest      string `json:"subject_digest"`
	ContextRevision    string `json:"context_revision"`
	GatePolicyRevision string `json:"gate_policy_revision"`
	ClassifierRevision string `json:"classifier_revision"`
	SecurityCeiling    string `json:"security_ceiling"`
}

type inputRequirementWireV1 struct {
	Kind        string `json:"kind"`
	Scope       string `json:"scope"`
	Match       string `json:"match"`
	Description string `json:"description"`
}

type inputRequestWireV1 struct {
	ToolName         string                   `json:"tool_name"`
	Summary          string                   `json:"summary"`
	Command          string                   `json:"command"`
	WorkingDirectory string                   `json:"working_directory"`
	Requirements     []inputRequirementWireV1 `json:"requirements"`
}

type inputContextEntryWireV1 struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

type inputContextWireV1 struct {
	WorkspaceRoot    string                    `json:"workspace_root"`
	WorkingDirectory string                    `json:"working_directory"`
	SecurityCeiling  string                    `json:"security_ceiling"`
	Entries          []inputContextEntryWireV1 `json:"entries"`
	OmittedEntries   int                       `json:"omitted_entries"`
	OmittedBytes     int                       `json:"omitted_bytes"`
}

// InputValidationField identifies the bounded part of a subject that failed
// input marshaling.
type InputValidationField string

const (
	InputFieldSubject InputValidationField = "subject"
	InputFieldWire    InputValidationField = "wire"
)

// InputValidationReason classifies an input marshaling failure.
type InputValidationReason string

const (
	InputReasonRequired    InputValidationReason = "required"
	InputReasonInvalid     InputValidationReason = "invalid"
	InputReasonMismatch    InputValidationReason = "mismatch"
	InputReasonOutOfBounds InputValidationReason = "out_of_bounds"
)

// InputValidationError reports a subject that cannot be marshaled into the
// model input wire. It deliberately carries no subject content.
type InputValidationError struct {
	Field  InputValidationField
	Reason InputValidationReason
}

func (e *InputValidationError) Error() string {
	return "wire: invalid command-safety input (" + string(e.Field) + ": " + string(e.Reason) + ")"
}

func inputError(field InputValidationField, reason InputValidationReason) error {
	return &InputValidationError{Field: field, Reason: reason}
}

// MarshalInput projects a validated PermissionReviewSubject into the
// versioned, bounded JSON view the model sees as this classifier's
// evidence. subject.Context's entries are carried verbatim: the prompt (not
// this codec) is responsible for labeling them as untrusted data.
func MarshalInput(subject gate.PermissionReviewSubject) (json.RawMessage, error) {
	digest, err := gate.SubjectDigest(subject)
	if err != nil {
		return nil, inputError(InputFieldSubject, InputReasonInvalid)
	}
	if subject.Basis.SubjectDigest == ([32]byte{}) {
		return nil, inputError(InputFieldSubject, InputReasonRequired)
	}
	if subject.Basis.SubjectDigest != digest {
		return nil, inputError(InputFieldSubject, InputReasonMismatch)
	}

	requirements := make([]inputRequirementWireV1, len(subject.Request.Requirements))
	for i, requirement := range subject.Request.Requirements {
		requirements[i] = inputRequirementWireV1{
			Kind:        requirement.Kind,
			Scope:       requirement.Scope,
			Match:       requirement.Match,
			Description: requirement.Description,
		}
	}
	entries := make([]inputContextEntryWireV1, len(subject.Context.Entries))
	for i, entry := range subject.Context.Entries {
		entries[i] = inputContextEntryWireV1{
			Origin:    string(entry.Origin),
			Kind:      string(entry.Kind),
			Content:   entry.Content,
			Truncated: entry.Truncated,
		}
	}

	wire := inputWireV1{
		Version: InputWireVersion,
		Basis: inputBasisWireV1{
			GateID:             subject.Basis.GateID.String(),
			ToolExecutionID:    subject.Basis.ToolExecutionID.String(),
			SubjectDigest:      hex.EncodeToString(subject.Basis.SubjectDigest[:]),
			ContextRevision:    subject.Basis.ContextRevision,
			GatePolicyRevision: subject.Basis.GatePolicyRevision,
			ClassifierRevision: subject.Basis.ClassifierRevision,
			SecurityCeiling:    subject.Basis.SecurityCeiling,
		},
		Request: inputRequestWireV1{
			ToolName:         subject.Request.ToolName,
			Summary:          subject.Request.Summary,
			Command:          subject.Request.Command,
			WorkingDirectory: subject.Request.WorkingDirectory,
			Requirements:     requirements,
		},
		Context: inputContextWireV1{
			WorkspaceRoot:    subject.Context.WorkspaceRoot,
			WorkingDirectory: subject.Context.WorkingDirectory,
			SecurityCeiling:  subject.Context.SecurityCeiling,
			Entries:          entries,
			OmittedEntries:   subject.Context.Truncation.OmittedEntries,
			OmittedBytes:     subject.Context.Truncation.OmittedBytes,
		},
	}

	data, err := json.Marshal(wire)
	if err != nil {
		return nil, inputError(InputFieldWire, InputReasonInvalid)
	}
	if len(data) > MaxInputWireBytes {
		return nil, inputError(InputFieldWire, InputReasonOutOfBounds)
	}
	return data, nil
}
