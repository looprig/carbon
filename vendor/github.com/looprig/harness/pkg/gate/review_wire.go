package gate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

const (
	permissionReviewSubjectWireVersion = "permission_review_subject.v1"
	permissionReviewSubjectWireKind    = "harness.permission"
	// The common projection must never grow a valid classifier-specific wire.
	permissionReviewCommonClassifierRevision = ""
	zeroPermissionReviewDigestHex            = "0000000000000000000000000000000000000000000000000000000000000000"
)

// MaxPermissionReviewSubjectWireBytes bounds strict subject decoding before
// JSON parsing.
const MaxPermissionReviewSubjectWireBytes = 1 << 20

type permissionReviewSubjectWireV1 struct {
	Version  string                    `json:"version"`
	GateKind string                    `json:"gate_kind"`
	Basis    permissionReviewBasisV1   `json:"basis"`
	Request  permissionReviewRequestV1 `json:"request"`
	Context  permissionReviewContextV1 `json:"context"`
}

type permissionReviewBasisV1 struct {
	GateID             string `json:"gate_id"`
	ToolExecutionID    string `json:"tool_execution_id"`
	SubjectDigest      string `json:"subject_digest"`
	ContextRevision    string `json:"context_revision"`
	GatePolicyRevision string `json:"gate_policy_revision"`
	ClassifierRevision string `json:"classifier_revision"`
	SecurityCeiling    string `json:"security_ceiling"`
}

type permissionReviewRequestV1 struct {
	ToolName           string                          `json:"tool_name"`
	Summary            string                          `json:"summary"`
	ExecutionID        string                          `json:"execution_id"`
	Command            string                          `json:"command"`
	WorkingDirectory   string                          `json:"working_directory"`
	ExpiresAtUnixMilli int64                           `json:"expires_at_unix_milli"`
	Requirements       []permissionReviewRequirementV1 `json:"requirements"`
}

type permissionReviewRequirementV1 struct {
	Kind        string                        `json:"kind"`
	Scope       string                        `json:"scope"`
	Match       string                        `json:"match"`
	Description string                        `json:"description"`
	GrantClass  string                        `json:"grant_class"`
	GrantTarget string                        `json:"grant_target"`
	Candidates  []permissionReviewCandidateV1 `json:"candidates"`
}

type permissionReviewCandidateV1 struct {
	Kind        string `json:"kind"`
	Match       string `json:"match"`
	Description string `json:"description"`
	GrantClass  string `json:"grant_class"`
	GrantTarget string `json:"grant_target"`
}

type permissionReviewContextV1 struct {
	Coordinates        permissionReviewCoordinatesV1 `json:"coordinates"`
	ContextRevision    string                        `json:"context_revision"`
	WorkspaceRoot      string                        `json:"workspace_root"`
	WorkingDirectory   string                        `json:"working_directory"`
	RetryReason        string                        `json:"retry_reason"`
	SecurityCeiling    string                        `json:"security_ceiling"`
	GatePolicyRevision string                        `json:"gate_policy_revision"`
	Entries            []permissionReviewEntryV1     `json:"entries"`
	Truncation         permissionReviewTruncationV1  `json:"truncation"`
}

type permissionReviewCoordinatesV1 struct {
	SessionID string `json:"session_id"`
	LoopID    string `json:"loop_id"`
	TurnID    string `json:"turn_id"`
	StepID    string `json:"step_id"`
}

type permissionReviewEntryV1 struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

type permissionReviewTruncationV1 struct {
	Applied        uint16 `json:"applied"`
	Material       uint16 `json:"material"`
	OmittedEntries int    `json:"omitted_entries"`
	OmittedBytes   int    `json:"omitted_bytes"`
}

func marshalPermissionReviewSubject(subject PermissionReviewSubject) ([]byte, error) {
	if err := validatePermissionReviewSubject(subject); err != nil {
		return nil, err
	}
	digest, err := permissionReviewSubjectDigest(subject)
	if err != nil {
		return nil, err
	}
	if subject.Basis.SubjectDigest == ([32]byte{}) ||
		subject.Basis.SubjectDigest != digest {
		return nil, reviewSubjectError(ReviewValidationFieldDigest, ReviewValidationMismatch)
	}
	wire := permissionReviewSubjectToWire(subject, hex.EncodeToString(digest[:]))
	data, err := json.Marshal(wire)
	if err != nil || len(data) > MaxPermissionReviewSubjectWireBytes {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationOutOfBounds)
	}
	return data, nil
}

func unmarshalPermissionReviewSubject(data []byte) (PermissionReviewSubject, error) {
	if len(data) == 0 ||
		len(data) > MaxPermissionReviewSubjectWireBytes ||
		isExplicitJSONNull(data) ||
		!utf8.Valid(data) {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldWire,
			ReviewValidationOutOfBounds,
		)
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldWire,
			ReviewValidationInvalid,
		)
	}
	var wire permissionReviewSubjectWireV1
	if err := decodeStrict(data, &wire); err != nil {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldWire,
			ReviewValidationInvalid,
		)
	}
	if wire.Version != permissionReviewSubjectWireVersion ||
		wire.GateKind != permissionReviewSubjectWireKind {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldWire,
			ReviewValidationUnsupported,
		)
	}
	if err := validatePermissionReviewWireShape(data, wire); err != nil {
		return PermissionReviewSubject{}, err
	}

	subject, storedDigest, err := permissionReviewSubjectFromWire(wire)
	if err != nil {
		return PermissionReviewSubject{}, err
	}
	basis := subject.Basis
	basis.SubjectDigest = [32]byte{}
	subject, err = NewPermissionReviewSubject(basis, subject.Request, subject.Context)
	if err != nil {
		return PermissionReviewSubject{}, err
	}
	if subject.Basis.SubjectDigest != storedDigest {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationMismatch,
		)
	}
	return subject, nil
}

func permissionReviewCommonSubjectDigest(
	subject PermissionReviewSubject,
) ([sha256.Size]byte, error) {
	if err := validatePermissionReviewSubject(subject); err != nil {
		return [sha256.Size]byte{}, err
	}
	digest, err := permissionReviewSubjectDigest(subject)
	if err != nil ||
		subject.Basis.SubjectDigest == ([sha256.Size]byte{}) ||
		subject.Basis.SubjectDigest != digest {
		return [sha256.Size]byte{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationMismatch,
		)
	}
	wire := permissionReviewSubjectToWire(subject, zeroPermissionReviewDigestHex)
	wire.Basis.ClassifierRevision = permissionReviewCommonClassifierRevision
	data, err := json.Marshal(wire)
	if err != nil || len(data) > MaxPermissionReviewSubjectWireBytes {
		return [sha256.Size]byte{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationOutOfBounds,
		)
	}
	return sha256.Sum256(data), nil
}

func permissionReviewSubjectDigest(subject PermissionReviewSubject) ([32]byte, error) {
	if reason := permissionReviewBasisPreflightReason(subject.Basis); reason != "" {
		return [32]byte{}, reviewSubjectError(ReviewValidationFieldBasis, reason)
	}
	if reason := permissionReviewRequestPreflightReason(subject.Request); reason != "" {
		return [32]byte{}, reviewSubjectError(ReviewValidationFieldRequest, reason)
	}
	if err := preflightPermissionReviewContextProjection(subject.Context); err != nil {
		return [32]byte{}, err
	}
	wire := permissionReviewSubjectToWire(subject, zeroPermissionReviewDigestHex)
	data, err := json.Marshal(wire)
	if err != nil || len(data) > MaxPermissionReviewSubjectWireBytes {
		return [32]byte{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationOutOfBounds,
		)
	}
	return sha256.Sum256(data), nil
}

func permissionReviewSubjectToWire(
	subject PermissionReviewSubject,
	digest string,
) permissionReviewSubjectWireV1 {
	requirements := make([]permissionReviewRequirementV1, len(subject.Request.Requirements))
	for i, requirement := range subject.Request.Requirements {
		candidates := make([]permissionReviewCandidateV1, len(requirement.Candidates))
		for j, candidate := range requirement.Candidates {
			candidates[j] = permissionReviewCandidateV1{
				Kind: candidate.Kind, Match: candidate.Match,
				Description: candidate.Description, GrantClass: candidate.GrantClass,
				GrantTarget: candidate.GrantTarget,
			}
		}
		requirements[i] = permissionReviewRequirementV1{
			Kind: requirement.Kind, Scope: requirement.Scope, Match: requirement.Match,
			Description: requirement.Description, GrantClass: requirement.GrantClass,
			GrantTarget: requirement.GrantTarget, Candidates: candidates,
		}
	}
	return permissionReviewSubjectWireV1{
		Version:  permissionReviewSubjectWireVersion,
		GateKind: permissionReviewSubjectWireKind,
		Basis: permissionReviewBasisV1{
			GateID: subject.Basis.GateID.String(), ToolExecutionID: subject.Basis.ToolExecutionID.String(),
			SubjectDigest: digest, ContextRevision: subject.Basis.ContextRevision,
			GatePolicyRevision: subject.Basis.GatePolicyRevision,
			ClassifierRevision: subject.Basis.ClassifierRevision,
			SecurityCeiling:    subject.Basis.SecurityCeiling,
		},
		Request: permissionReviewRequestV1{
			ToolName: subject.Request.ToolName, Summary: subject.Request.Summary,
			ExecutionID: subject.Request.ExecutionID, Command: subject.Request.Command,
			WorkingDirectory:   subject.Request.WorkingDirectory,
			ExpiresAtUnixMilli: subject.Request.ExpiresAtUnixMilli,
			Requirements:       requirements,
		},
		Context: permissionReviewContextToWire(subject.Context),
	}
}

func permissionReviewContextToWire(context ReviewContext) permissionReviewContextV1 {
	entries := make([]permissionReviewEntryV1, len(context.Entries))
	for i, entry := range context.Entries {
		entries[i] = permissionReviewEntryV1{
			Origin: string(entry.Origin), Kind: string(entry.Kind),
			Content: entry.Content, Truncated: entry.Truncated,
		}
	}
	return permissionReviewContextV1{
		Coordinates: permissionReviewCoordinatesV1{
			SessionID: context.Coordinates.SessionID.String(),
			LoopID:    context.Coordinates.LoopID.String(),
			TurnID:    context.Coordinates.TurnID.String(),
			StepID:    context.Coordinates.StepID.String(),
		},
		ContextRevision:    context.ContextRevision,
		WorkspaceRoot:      context.WorkspaceRoot,
		WorkingDirectory:   context.WorkingDirectory,
		RetryReason:        context.RetryReason,
		SecurityCeiling:    context.SecurityCeiling,
		GatePolicyRevision: context.GatePolicyRevision,
		Entries:            entries,
		Truncation: permissionReviewTruncationV1{
			Applied:        uint16(context.Truncation.Applied),
			Material:       uint16(context.Truncation.Material),
			OmittedEntries: context.Truncation.OmittedEntries,
			OmittedBytes:   context.Truncation.OmittedBytes,
		},
	}
}

func permissionReviewSubjectFromWire(
	wire permissionReviewSubjectWireV1,
) (PermissionReviewSubject, [32]byte, error) {
	gateID, err := parseCanonicalReviewUUID(wire.Basis.GateID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	toolExecutionID, err := parseCanonicalReviewUUID(wire.Basis.ToolExecutionID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	sessionID, err := parseCanonicalReviewUUID(wire.Context.Coordinates.SessionID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	loopID, err := parseCanonicalReviewUUID(wire.Context.Coordinates.LoopID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	turnID, err := parseCanonicalReviewUUID(wire.Context.Coordinates.TurnID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	stepID, err := parseCanonicalReviewUUID(wire.Context.Coordinates.StepID)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}
	storedDigest, err := parseCanonicalReviewDigest(wire.Basis.SubjectDigest)
	if err != nil {
		return PermissionReviewSubject{}, [32]byte{}, err
	}

	requirements := make([]tool.Requirement, len(wire.Request.Requirements))
	for i, requirement := range wire.Request.Requirements {
		candidates := make([]tool.RuleCandidate, len(requirement.Candidates))
		for j, candidate := range requirement.Candidates {
			candidates[j] = tool.RuleCandidate{
				Kind: candidate.Kind, Match: candidate.Match,
				Description: candidate.Description, GrantClass: candidate.GrantClass,
				GrantTarget: candidate.GrantTarget,
			}
		}
		requirements[i] = tool.Requirement{
			Kind: requirement.Kind, Scope: requirement.Scope, Match: requirement.Match,
			Description: requirement.Description, GrantClass: requirement.GrantClass,
			GrantTarget: requirement.GrantTarget, Candidates: candidates,
		}
	}
	entries := make([]ReviewContextEntry, len(wire.Context.Entries))
	for i, entry := range wire.Context.Entries {
		entries[i] = ReviewContextEntry{
			Origin: ReviewContextOrigin(entry.Origin), Kind: ReviewContextKind(entry.Kind),
			Content: entry.Content, Truncated: entry.Truncated,
		}
	}
	return PermissionReviewSubject{
		Basis: ReviewBasis{
			GateID: gateID, ToolExecutionID: toolExecutionID,
			SubjectDigest: storedDigest, ContextRevision: wire.Basis.ContextRevision,
			GatePolicyRevision: wire.Basis.GatePolicyRevision,
			ClassifierRevision: wire.Basis.ClassifierRevision,
			SecurityCeiling:    wire.Basis.SecurityCeiling,
		},
		Request: tool.Request{
			ToolName: wire.Request.ToolName, Summary: wire.Request.Summary,
			ExecutionID: wire.Request.ExecutionID, Command: wire.Request.Command,
			WorkingDirectory:   wire.Request.WorkingDirectory,
			ExpiresAtUnixMilli: wire.Request.ExpiresAtUnixMilli,
			Requirements:       requirements,
		},
		Context: ReviewContext{
			Coordinates: identity.Coordinates{
				SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: stepID,
			},
			ContextRevision:    wire.Context.ContextRevision,
			WorkspaceRoot:      wire.Context.WorkspaceRoot,
			WorkingDirectory:   wire.Context.WorkingDirectory,
			RetryReason:        wire.Context.RetryReason,
			SecurityCeiling:    wire.Context.SecurityCeiling,
			GatePolicyRevision: wire.Context.GatePolicyRevision,
			Entries:            entries,
			Truncation: ReviewTruncation{
				Applied:        ReviewTruncationMask(wire.Context.Truncation.Applied),
				Material:       ReviewTruncationMask(wire.Context.Truncation.Material),
				OmittedEntries: wire.Context.Truncation.OmittedEntries,
				OmittedBytes:   wire.Context.Truncation.OmittedBytes,
			},
		},
	}, storedDigest, nil
}

func parseCanonicalReviewUUID(value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.IsZero() || parsed.String() != value {
		return uuid.UUID{}, reviewSubjectError(
			ReviewValidationFieldWire,
			ReviewValidationInvalid,
		)
	}
	return parsed, nil
}

func parseCanonicalReviewDigest(value string) ([32]byte, error) {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return [32]byte{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationInvalid,
		)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return [32]byte{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationInvalid,
		)
	}
	var digest [32]byte
	copy(digest[:], decoded)
	return digest, nil
}

func validatePermissionReviewWireShape(
	data []byte,
	wire permissionReviewSubjectWireV1,
) error {
	// Scalars whose zero values can otherwise pass structural decoding are
	// explicitly required. Optional request/context strings are still emitted
	// by the canonical encoder, while their domain validation belongs to the
	// typed request/context boundary.
	if wire.Version == "" || wire.GateKind == "" ||
		wire.Basis.GateID == "" || wire.Basis.ToolExecutionID == "" ||
		wire.Basis.SubjectDigest == "" || wire.Basis.ContextRevision == "" ||
		wire.Basis.GatePolicyRevision == "" || wire.Basis.ClassifierRevision == "" ||
		wire.Basis.SecurityCeiling == "" ||
		wire.Context.Coordinates.SessionID == "" ||
		wire.Context.Coordinates.LoopID == "" ||
		wire.Context.Coordinates.TurnID == "" ||
		wire.Context.Coordinates.StepID == "" {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationRequired)
	}
	// Every canonical root member must be present even where its decoded zero
	// value would otherwise be indistinguishable from a missing JSON field.
	root, err := requireReviewWireObject(data, []string{
		"version", "gate_kind", "basis", "request", "context",
	})
	if err != nil {
		return err
	}
	if err := requireReviewWireStrings(root, "version", "gate_kind"); err != nil {
		return err
	}
	basis, err := requireReviewWireObject(root["basis"], []string{
		"gate_id", "tool_execution_id", "subject_digest", "context_revision",
		"gate_policy_revision", "classifier_revision", "security_ceiling",
	})
	if err != nil {
		return err
	}
	if err := requireReviewWireStrings(
		basis,
		"gate_id",
		"tool_execution_id",
		"subject_digest",
		"context_revision",
		"gate_policy_revision",
		"classifier_revision",
		"security_ceiling",
	); err != nil {
		return err
	}
	if reason := permissionReviewBasisIdentityPreflightReason(
		wire.Basis.ContextRevision,
		wire.Basis.GatePolicyRevision,
		wire.Basis.ClassifierRevision,
		wire.Basis.SecurityCeiling,
	); reason != "" {
		return reviewSubjectError(ReviewValidationFieldBasis, reason)
	}
	request, err := requireReviewWireObject(root["request"], []string{
		"tool_name", "summary", "execution_id", "command", "working_directory",
		"expires_at_unix_milli", "requirements",
	})
	if err != nil {
		return err
	}
	if err := requireReviewWireStrings(
		request,
		"tool_name",
		"summary",
		"execution_id",
		"command",
		"working_directory",
	); err != nil {
		return err
	}
	if err := requireReviewWireInteger(request["expires_at_unix_milli"]); err != nil {
		return err
	}
	requirements, err := requireReviewWireArray(request["requirements"])
	if err != nil {
		return err
	}
	for _, rawRequirement := range requirements {
		requirement, err := requireReviewWireObject(rawRequirement, []string{
			"kind", "scope", "match", "description", "grant_class",
			"grant_target", "candidates",
		})
		if err != nil {
			return err
		}
		if err := requireReviewWireStrings(
			requirement,
			"kind",
			"scope",
			"match",
			"description",
			"grant_class",
			"grant_target",
		); err != nil {
			return err
		}
		candidates, err := requireReviewWireArray(requirement["candidates"])
		if err != nil {
			return err
		}
		for _, rawCandidate := range candidates {
			candidate, err := requireReviewWireObject(rawCandidate, []string{
				"kind", "match", "description", "grant_class", "grant_target",
			})
			if err != nil {
				return err
			}
			if err := requireReviewWireStrings(
				candidate,
				"kind",
				"match",
				"description",
				"grant_class",
				"grant_target",
			); err != nil {
				return err
			}
		}
	}
	context, err := requireReviewWireObject(root["context"], []string{
		"coordinates", "context_revision", "workspace_root", "working_directory",
		"retry_reason", "security_ceiling", "gate_policy_revision", "entries",
		"truncation",
	})
	if err != nil {
		return err
	}
	if err := requireReviewWireStrings(
		context,
		"context_revision",
		"workspace_root",
		"working_directory",
		"retry_reason",
		"security_ceiling",
		"gate_policy_revision",
	); err != nil {
		return err
	}
	coordinates, err := requireReviewWireObject(context["coordinates"], []string{
		"session_id", "loop_id", "turn_id", "step_id",
	})
	if err != nil {
		return err
	}
	if err := requireReviewWireStrings(
		coordinates,
		"session_id",
		"loop_id",
		"turn_id",
		"step_id",
	); err != nil {
		return err
	}
	entries, err := requireReviewWireArray(context["entries"])
	if err != nil {
		return err
	}
	for _, rawEntry := range entries {
		entry, err := requireReviewWireObject(rawEntry, []string{
			"origin", "kind", "content", "truncated",
		})
		if err != nil {
			return err
		}
		if err := requireReviewWireStrings(entry, "origin", "kind", "content"); err != nil {
			return err
		}
		if err := requireReviewWireBool(entry["truncated"]); err != nil {
			return err
		}
	}
	truncation, err := requireReviewWireObject(context["truncation"], []string{
		"applied", "material", "omitted_entries", "omitted_bytes",
	})
	if err != nil {
		return err
	}
	for _, key := range []string{
		"applied",
		"material",
		"omitted_entries",
		"omitted_bytes",
	} {
		if err := requireReviewWireInteger(truncation[key]); err != nil {
			return err
		}
	}
	return nil
}

func requireReviewWireObject(
	data []byte,
	required []string,
) (map[string]json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	if len(object) != len(required) {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationRequired)
		}
	}
	return object, nil
}

func requireReviewWireArray(data []byte) ([]json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return nil, reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	return values, nil
}

func requireReviewWireStrings(
	object map[string]json.RawMessage,
	keys ...string,
) error {
	for _, key := range keys {
		if err := requireReviewWireString(object[key]); err != nil {
			return err
		}
	}
	return nil
}

func requireReviewWireString(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	return nil
}

func requireReviewWireBool(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if !bytes.Equal(trimmed, []byte("true")) &&
		!bytes.Equal(trimmed, []byte("false")) {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	return nil
}

func requireReviewWireInteger(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	if _, err := strconv.ParseInt(string(trimmed), 10, 64); err != nil {
		return reviewSubjectError(ReviewValidationFieldWire, ReviewValidationInvalid)
	}
	return nil
}
