package gate

import (
	"encoding/json"
	"errors"
)

// ApprovalAction is one of the three exact user-facing permission decisions.
type ApprovalAction string

const (
	ApprovalApprove                ApprovalAction = "Approve"
	ApprovalApproveAlwaysWorkspace ApprovalAction = "Approve always for this workspace"
	ApprovalDeny                   ApprovalAction = "Deny"
)

// ApprovalActionDecodeError wraps malformed JSON or a non-exact action.
type ApprovalActionDecodeError struct{ Cause error }

func (e *ApprovalActionDecodeError) Error() string {
	return "gate: decode approval action: " + e.Cause.Error()
}

func (e *ApprovalActionDecodeError) Unwrap() error { return e.Cause }

type approvalActionData struct {
	Action ApprovalAction `json:"action"`
}

// DecodeApprovalAction strictly decodes one of the three exact approval
// actions. Unknown fields, duplicate keys, trailing JSON, and null fail closed.
func DecodeApprovalAction(data []byte) (ApprovalAction, error) {
	if isExplicitJSONNull(data) {
		return "", &ApprovalActionDecodeError{Cause: errNullPayloadData}
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return "", &ApprovalActionDecodeError{Cause: err}
	}
	var raw approvalActionData
	if err := decodeStrict(data, &raw); err != nil {
		return "", &ApprovalActionDecodeError{Cause: err}
	}
	action, ok := ParseApprovalAction(string(raw.Action))
	if !ok {
		return "", &ApprovalActionDecodeError{Cause: errors.New("unknown approval action")}
	}
	return action, nil
}

// ParseApprovalAction returns the exact approval action named by s. It is the
// single validation source shared by DecodeApprovalAction and the session gate
// route; anything but the three exact actions fails closed.
func ParseApprovalAction(s string) (ApprovalAction, bool) {
	switch ApprovalAction(s) {
	case ApprovalApprove, ApprovalApproveAlwaysWorkspace, ApprovalDeny:
		return ApprovalAction(s), true
	default:
		return "", false
	}
}

// ResponseSourceKind identifies who produced a gate response.
type ResponseSourceKind string

const (
	// ResponseFromUser records a response supplied by a human user.
	ResponseFromUser ResponseSourceKind = "user"
	// ResponseFromPolicy records a response supplied by gate policy.
	ResponseFromPolicy ResponseSourceKind = "policy"
	// ResponseFromModel records a response supplied by model decision policy.
	ResponseFromModel ResponseSourceKind = "model"
	// ResponseFromClassifier records a response the session itself
	// constructed from an eligible automated permission-classifier
	// assessment (design §16.3). Only a private session-runtime method may
	// stamp this source; a public caller's attempt to select it directly is
	// rejected rather than honored (see internal/sessionruntime.RespondGate).
	ResponseFromClassifier ResponseSourceKind = "classifier"
)

// ResponseRequest is the generic action and value payload used to answer a gate.
type ResponseRequest struct {
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

// GateResponse is the resolved response envelope for a gate.
type GateResponse struct {
	GateID ID                         `json:"gate_id,omitzero"`
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
	Source ResponseSource             `json:"source,omitzero"`
}

// Answer is the validated result of answering a HOST-OWNED gate, delivered live
// to the opener that is blocked on it.
//
// It is a LIVE delivery type, not a durable record, and has no JSON codec. A gate
// answered on behalf of a loop turns into a command the loop consumes in memory;
// this is the same thing for a gate whose opener is the host itself. What survives
// the process is the GateResolved event and its FormAudit, which records the same
// answers durably (see FormAudit) — so the two are separate because they travel
// differently, not because one hides something from the other.
type Answer struct {
	GateID ID
	Action string
	// Values holds form answers keyed by field name. It is nil for any action
	// other than an accepted form.
	Values map[string]string
	Source ResponseSource
}

// ResponseSource describes the origin and reason for a response.
type ResponseSource struct {
	Kind   ResponseSourceKind `json:"kind,omitempty"`
	Reason string             `json:"reason,omitempty"`
}
