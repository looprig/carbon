package gate

import (
	"encoding/json"
	"errors"
	"fmt"
)

type responseAuditKind string

const (
	responseAuditKindPermission responseAuditKind = "permission"
	responseAuditKindAskUser    responseAuditKind = "ask_user"
	responseAuditKindForm       responseAuditKind = "form"
)

var (
	errMissingResponseAuditData = errors.New("missing response audit data")
	errNullResponseAuditData    = errors.New("null response audit data")
)

// ResponseAudit is the sealed union of durable, redacted gate response audit records.
type ResponseAudit interface {
	responseAudit()
}

// PermissionAudit is the durable, redacted record of a permission approval:
// the bounded display DESCRIPTIONS of the requirements the user approved and —
// for a workspace approval — of the reusable rule candidates that were
// displayed for persistence. It never carries grant tokens, token material, or
// raw tool arguments; descriptions are the only permitted content.
type PermissionAudit struct {
	RequirementDescriptions []string `json:"requirement_descriptions,omitempty"`
	CandidateDescriptions   []string `json:"candidate_descriptions,omitempty"`
}

// AskUserAudit stores a redacted preview of a user answer.
type AskUserAudit struct {
	AnswerPreview string `json:"answer_preview,omitempty"`
}

// FormAudit is the durable record of how a form gate was answered: the answers
// themselves, keyed by schema field name.
//
// It records USER-AUTHORED CONTENT verbatim, including free text, and that is
// deliberate. A journal is meant to be the durable truth of a session, and a
// human's form answer shaped that session as surely as anything they typed into
// chat — which command.UserInput already records verbatim, block for block. A
// form answer that reached a durable record only as "a field was answered" would
// make the journal an incomplete account of its own session. Recording the
// answer is the norm here; withholding it would be the outlier.
//
// This is NOT a licence to journal secrets, and two controls keep them out:
//
//   - Credential-soliciting fields are refused before a form is ever opened, so
//     the answer to one never exists. That rejection lives with the integration
//     that translates a third-party request into a schema (MCP design
//     §Elicitation); it is the load-bearing control, not this type.
//   - An authorization target is not user content and never becomes one. See
//     OpenURLPayload: a URL carrying a PKCE verifier or `state` is structurally
//     excluded from every durable type, and sensitive authorization goes through
//     an open-url gate rather than a form field.
//
// Unredacted is not unbounded. A form schema is authored by a third party, so a
// hostile or buggy integration must not be able to append an unbounded record to
// the journal. Values is bounded on BOTH codec boundaries by
// ValidateFormAuditBounds: at most maxFormFields entries, each name at most
// maxFormFieldNameBytes and each value at most maxFormValueBytes. Those are the
// same bounds ParseFormAnswers enforces on the way in, re-checked here so a
// record that was never parsed cannot be journaled or restored either.
type FormAudit struct {
	// Values holds the submitted answers keyed by field name, exactly as
	// ParseFormAnswers produced them (a confirm answer is "true"/"false"). Only
	// names declared by the form's schema appear. JSON object keys marshal in
	// sorted order, so the durable record is stable.
	Values map[string]string `json:"values,omitempty"`
}

func (PermissionAudit) responseAudit() {}
func (AskUserAudit) responseAudit()    {}
func (FormAudit) responseAudit()       {}

// UnknownResponseAuditKindError is returned when an audit wrapper names no known kind.
type UnknownResponseAuditKindError struct {
	Kind string
}

func (e *UnknownResponseAuditKindError) Error() string {
	return fmt.Sprintf("gate: unknown response audit kind %q", e.Kind)
}

// NilResponseAuditError is returned when MarshalResponseAudit receives nil audit data.
type NilResponseAuditError struct{}

func (e *NilResponseAuditError) Error() string {
	return "gate: nil response audit"
}

// ResponseAuditEncodeError wraps failures while encoding response audit data.
type ResponseAuditEncodeError struct {
	Kind  string
	Cause error
}

func (e *ResponseAuditEncodeError) Error() string {
	return fmt.Sprintf("gate: encode response audit %q: %v", e.Kind, e.Cause)
}
func (e *ResponseAuditEncodeError) Unwrap() error { return e.Cause }

// ResponseAuditDecodeError wraps malformed audit JSON or malformed audit data.
type ResponseAuditDecodeError struct {
	Kind  string
	Cause error
}

func (e *ResponseAuditDecodeError) Error() string {
	if e.Kind == "" {
		return "gate: decode response audit: " + e.Cause.Error()
	}
	return fmt.Sprintf("gate: decode response audit %q: %v", e.Kind, e.Cause)
}
func (e *ResponseAuditDecodeError) Unwrap() error { return e.Cause }

type responseAuditWrapper struct {
	Kind responseAuditKind `json:"kind"`
	Data json.RawMessage   `json:"data"`
}

// MarshalResponseAudit encodes a sealed response audit as a {kind,data} wrapper.
func MarshalResponseAudit(audit ResponseAudit) ([]byte, error) {
	kind, err := responseAuditTag(audit)
	if err != nil {
		return nil, err
	}
	data, err := marshalResponseAuditData(kind, audit)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(responseAuditWrapper{Kind: kind, Data: data})
	if err != nil {
		return nil, &ResponseAuditEncodeError{Kind: string(kind), Cause: err}
	}
	return out, nil
}

// UnmarshalResponseAudit decodes a {kind,data} wrapper and fails closed on unknown kinds.
func UnmarshalResponseAudit(data []byte) (ResponseAudit, error) {
	var wrapper responseAuditWrapper
	if err := decodeStrict(data, &wrapper); err != nil {
		return nil, &ResponseAuditDecodeError{Cause: err}
	}
	if wrapper.Kind == "" {
		return nil, &UnknownResponseAuditKindError{Kind: ""}
	}
	if len(wrapper.Data) == 0 {
		return nil, &ResponseAuditDecodeError{Kind: string(wrapper.Kind), Cause: errMissingResponseAuditData}
	}
	if isExplicitJSONNull(wrapper.Data) {
		return nil, &ResponseAuditDecodeError{Kind: string(wrapper.Kind), Cause: errNullResponseAuditData}
	}
	return unmarshalResponseAuditData(wrapper.Kind, wrapper.Data)
}

func responseAuditTag(audit ResponseAudit) (responseAuditKind, error) {
	switch v := audit.(type) {
	case nil:
		return "", &NilResponseAuditError{}
	case PermissionAudit:
		return responseAuditKindPermission, nil
	case *PermissionAudit:
		if v == nil {
			return "", &NilResponseAuditError{}
		}
		return responseAuditKindPermission, nil
	case AskUserAudit:
		return responseAuditKindAskUser, nil
	case *AskUserAudit:
		if v == nil {
			return "", &NilResponseAuditError{}
		}
		return responseAuditKindAskUser, nil
	case FormAudit:
		return responseAuditKindForm, nil
	case *FormAudit:
		if v == nil {
			return "", &NilResponseAuditError{}
		}
		return responseAuditKindForm, nil
	default:
		return "", &UnknownResponseAuditKindError{Kind: fmt.Sprintf("%T", audit)}
	}
}

func marshalResponseAuditData(kind responseAuditKind, audit ResponseAudit) (json.RawMessage, error) {
	switch kind {
	case responseAuditKindPermission:
		return marshalResponseAuditJSON(kind, permissionAuditValue(audit))
	case responseAuditKindAskUser:
		return marshalResponseAuditJSON(kind, askUserAuditValue(audit))
	case responseAuditKindForm:
		v := formAuditValue(audit)
		// A form audit carries third-party-solicited user content, so its bounds
		// are enforced here rather than trusted from the caller: an oversized
		// record must not reach the journal at all.
		if err := ValidateFormAuditBounds(v); err != nil {
			return nil, &ResponseAuditEncodeError{Kind: string(kind), Cause: err}
		}
		return marshalResponseAuditJSON(kind, v)
	default:
		return nil, &UnknownResponseAuditKindError{Kind: string(kind)}
	}
}

func unmarshalResponseAuditData(kind responseAuditKind, data json.RawMessage) (ResponseAudit, error) {
	switch kind {
	case responseAuditKindPermission:
		var audit PermissionAudit
		if err := decodeStrict(data, &audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		return audit, nil
	case responseAuditKindAskUser:
		var audit AskUserAudit
		if err := decodeStrict(data, &audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		return audit, nil
	case responseAuditKindForm:
		var audit FormAudit
		if err := decodeStrict(data, &audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		// Restore is an untrusted boundary: re-validate rather than trust that
		// whatever wrote the record enforced the bounds.
		if err := ValidateFormAuditBounds(audit); err != nil {
			return nil, &ResponseAuditDecodeError{Kind: string(kind), Cause: err}
		}
		return audit, nil
	default:
		return nil, &UnknownResponseAuditKindError{Kind: string(kind)}
	}
}

func marshalResponseAuditJSON(kind responseAuditKind, value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, &ResponseAuditEncodeError{Kind: string(kind), Cause: err}
	}
	return data, nil
}

func permissionAuditValue(audit ResponseAudit) PermissionAudit {
	switch v := audit.(type) {
	case PermissionAudit:
		return v
	case *PermissionAudit:
		return *v
	default:
		panic("gate: internal response audit type mismatch")
	}
}

func askUserAuditValue(audit ResponseAudit) AskUserAudit {
	switch v := audit.(type) {
	case AskUserAudit:
		return v
	case *AskUserAudit:
		return *v
	default:
		panic("gate: internal response audit type mismatch")
	}
}

func formAuditValue(audit ResponseAudit) FormAudit {
	switch v := audit.(type) {
	case FormAudit:
		return v
	case *FormAudit:
		return *v
	default:
		panic("gate: internal response audit type mismatch")
	}
}
