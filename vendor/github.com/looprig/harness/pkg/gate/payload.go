package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type payloadKind string

const (
	payloadKindOpen        payloadKind = "open"
	payloadKindPermission  payloadKind = "permission"
	payloadKindAskUser     payloadKind = "ask_user"
	payloadKindResumeInput payloadKind = "resume_input"
	payloadKindForm        payloadKind = "form"
	payloadKindOpenURL     payloadKind = "open_url"
)

var (
	errMissingPayloadData = errors.New("missing payload data")
	errNullPayloadData    = errors.New("null payload data")
)

// Payload is the sealed union of durable gate payload records.
type Payload interface {
	payload()
}

// OpenPayload records a gate opening and its nested scenario payload.
type OpenPayload struct {
	GateID  ID      `json:"gate_id,omitzero"`
	Payload Payload `json:"payload,omitempty"`
}

// PermissionPayload carries the typed prepared access request a permission
// gate decides — the request narrowed to what the approval prompt displayed.
// It is validated at BOTH codec boundaries (tool.ValidateRequest on marshal,
// the strict DecodeRequest on unmarshal), so a malformed or token-bearing
// record can neither be journaled nor restored. It never carries grant tokens:
// tool.Request has no token field, and unknown wire keys are rejected.
type PermissionPayload struct {
	Request tool.Request `json:"request,omitzero"`
}

// AskUserPayload carries an explicit question and optional fixed choices.
type AskUserPayload struct {
	Question string   `json:"question,omitempty"`
	Choices  []string `json:"choices,omitempty"`
}

// ResumeInputPayload records input made available when a gate resumes work.
type ResumeInputPayload struct {
	InputID uuid.UUID `json:"input_id,omitzero"`
	Preview string    `json:"preview,omitempty"`
}

// FormPayload carries a bounded, structured human-input request.
//
// It is the AUTHORITATIVE record of what was asked, mirroring how
// AskUserPayload carries the question that Gate.Prompt merely renders: the
// Prompt is a presentation projection an opener derives from this payload,
// while the payload is what a response is validated against (ParseFormAnswers)
// and what the journal durably records. The two are deliberately not the same
// field — Prompt is public envelope, the payload is private.
//
// Schema is bounded and restricted to the answerable field kinds; see
// ValidateFormSchema. It is validated at BOTH codec boundaries, so a malformed
// schema can neither be journaled nor restored.
type FormPayload struct {
	Title  string       `json:"title,omitempty"`
	Body   string       `json:"body,omitempty"`
	Schema PromptSchema `json:"schema,omitzero"`
}

// OpenURLPayload asks a human to open an action URL out-of-band.
//
// URL is the EPHEMERAL action target. An authorization URL carries secrets —
// OAuth `state`, a PKCE challenge, one-time codes — so it MUST NOT reach a
// journal, an event, or an audit record. That exclusion is STRUCTURAL, not
// remembered: the field is `json:"-"`, and the codec marshals
// openURLPayloadData, a type that HAS NO URL FIELD at all. Both boundaries have
// to be deleted for a URL to leak. A decoded OpenURLPayload therefore ALWAYS has
// an empty URL, which is exactly why an open-url gate may not be Restorable
// (enforced by ValidateGate) — the action target cannot survive a restore, and a
// reconnecting integration must mint a fresh one.
//
// DisplayOrigin is the durable, journal-safe origin shown to the human, e.g.
// "https://github.com". It is validated as a BARE origin (scheme + host only) at
// both codec boundaries: without that check a caller could defeat the whole
// design by passing the full action URL as the "origin".
type OpenURLPayload struct {
	DisplayOrigin string `json:"display_origin,omitempty"`
	// URL is never serialized. See the type doc.
	URL                string `json:"-"`
	RequiresCompletion bool   `json:"requires_completion,omitzero"`
}

func (OpenPayload) payload()        {}
func (PermissionPayload) payload()  {}
func (AskUserPayload) payload()     {}
func (ResumeInputPayload) payload() {}
func (FormPayload) payload()        {}
func (OpenURLPayload) payload()     {}

// UnknownPayloadKindError is returned when a payload wrapper names no known kind.
type UnknownPayloadKindError struct {
	Kind string
}

func (e *UnknownPayloadKindError) Error() string {
	return fmt.Sprintf("gate: unknown payload kind %q", e.Kind)
}

// NilPayloadError is returned when MarshalPayload receives a nil payload.
type NilPayloadError struct{}

func (e *NilPayloadError) Error() string {
	return "gate: nil payload"
}

// PayloadEncodeError wraps failures while encoding a payload wrapper or data.
type PayloadEncodeError struct {
	Kind  string
	Cause error
}

func (e *PayloadEncodeError) Error() string {
	return fmt.Sprintf("gate: encode payload %q: %v", e.Kind, e.Cause)
}
func (e *PayloadEncodeError) Unwrap() error { return e.Cause }

// PayloadDecodeError wraps malformed payload JSON or malformed payload data.
type PayloadDecodeError struct {
	Kind  string
	Cause error
}

func (e *PayloadDecodeError) Error() string {
	if e.Kind == "" {
		return "gate: decode payload: " + e.Cause.Error()
	}
	return fmt.Sprintf("gate: decode payload %q: %v", e.Kind, e.Cause)
}
func (e *PayloadDecodeError) Unwrap() error { return e.Cause }

// RequestDecodeError wraps malformed request JSON or a decoded request that
// violates the prepared request invariants.
type RequestDecodeError struct{ Cause error }

func (e *RequestDecodeError) Error() string { return "gate: decode request: " + e.Cause.Error() }
func (e *RequestDecodeError) Unwrap() error { return e.Cause }

type payloadWrapper struct {
	Kind payloadKind     `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type openPayloadData struct {
	GateID  ID              `json:"gate_id,omitzero"`
	Payload json.RawMessage `json:"payload"`
}

// DecodeRequest strictly decodes and validates an untrusted prepared request.
// Unknown fields, duplicate object keys, trailing JSON, null, and invariant
// violations all fail closed with RequestDecodeError.
func DecodeRequest(data []byte) (tool.Request, error) {
	if isExplicitJSONNull(data) {
		return tool.Request{}, &RequestDecodeError{Cause: errNullPayloadData}
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return tool.Request{}, &RequestDecodeError{Cause: err}
	}
	var request tool.Request
	if err := decodeStrict(data, &request); err != nil {
		return tool.Request{}, &RequestDecodeError{Cause: err}
	}
	if err := tool.ValidateRequest(request); err != nil {
		return tool.Request{}, &RequestDecodeError{Cause: err}
	}
	return request, nil
}

// maxScanJSONDepth bounds the duplicate-field scanner's recursion on untrusted
// input. It matches the nesting limit encoding/json enforces during Decode, so
// the scanner can never be driven deeper than the decode that follows it.
const maxScanJSONDepth = 10000

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxScanJSONDepth {
		return errors.New("JSON nesting exceeds depth limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// openURLPayloadData is the DURABLE form of an OpenURLPayload. It deliberately
// has no URL field — that omission is the mechanism by which the ephemeral
// action target (and the secrets in its query string) is kept out of journals,
// events, and audit records. Do not add one.
type openURLPayloadData struct {
	DisplayOrigin      string `json:"display_origin,omitempty"`
	RequiresCompletion bool   `json:"requires_completion,omitzero"`
}

// MarshalPayload encodes a sealed payload as a {kind,data} discriminator wrapper.
func MarshalPayload(payload Payload) ([]byte, error) {
	kind, err := payloadTag(payload)
	if err != nil {
		return nil, err
	}
	data, err := marshalPayloadData(kind, payload)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(payloadWrapper{Kind: kind, Data: data})
	if err != nil {
		return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
	}
	return out, nil
}

// UnmarshalPayload decodes a {kind,data} payload wrapper and fails closed on unknown kinds.
func UnmarshalPayload(data []byte) (Payload, error) {
	var wrapper payloadWrapper
	if err := decodeStrict(data, &wrapper); err != nil {
		return nil, &PayloadDecodeError{Cause: err}
	}
	if wrapper.Kind == "" {
		return nil, &UnknownPayloadKindError{Kind: ""}
	}
	if len(wrapper.Data) == 0 {
		return nil, &PayloadDecodeError{Kind: string(wrapper.Kind), Cause: errMissingPayloadData}
	}
	if isExplicitJSONNull(wrapper.Data) {
		return nil, &PayloadDecodeError{Kind: string(wrapper.Kind), Cause: errNullPayloadData}
	}
	return unmarshalPayloadData(wrapper.Kind, wrapper.Data)
}

func payloadTag(payload Payload) (payloadKind, error) {
	switch v := payload.(type) {
	case nil:
		return "", &NilPayloadError{}
	case OpenPayload:
		return payloadKindOpen, nil
	case *OpenPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindOpen, nil
	case PermissionPayload:
		return payloadKindPermission, nil
	case *PermissionPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindPermission, nil
	case AskUserPayload:
		return payloadKindAskUser, nil
	case *AskUserPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindAskUser, nil
	case ResumeInputPayload:
		return payloadKindResumeInput, nil
	case *ResumeInputPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindResumeInput, nil
	case FormPayload:
		return payloadKindForm, nil
	case *FormPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindForm, nil
	case OpenURLPayload:
		return payloadKindOpenURL, nil
	case *OpenURLPayload:
		if v == nil {
			return "", &NilPayloadError{}
		}
		return payloadKindOpenURL, nil
	default:
		return "", &UnknownPayloadKindError{Kind: fmt.Sprintf("%T", payload)}
	}
}

func marshalPayloadData(kind payloadKind, payload Payload) (json.RawMessage, error) {
	switch kind {
	case payloadKindOpen:
		v := openPayloadValue(payload)
		if v.Payload == nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: &NilPayloadError{}}
		}
		nested, err := MarshalPayload(v.Payload)
		if err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		return marshalPayloadJSON(kind, openPayloadData{GateID: v.GateID, Payload: nested})
	case payloadKindPermission:
		v := permissionPayloadValue(payload)
		if err := tool.ValidateRequest(v.Request); err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		return marshalPayloadJSON(kind, v.Request)
	case payloadKindAskUser:
		return marshalPayloadJSON(kind, askUserPayloadValue(payload))
	case payloadKindResumeInput:
		return marshalPayloadJSON(kind, resumeInputPayloadValue(payload))
	case payloadKindForm:
		v := formPayloadValue(payload)
		if err := ValidateFormSchema(v.Schema); err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		return marshalPayloadJSON(kind, v)
	case payloadKindOpenURL:
		v := openURLPayloadValue(payload)
		if err := validateDisplayOrigin(v.DisplayOrigin); err != nil {
			return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
		}
		// openURLPayloadData has no URL field: the ephemeral action target is
		// dropped here by construction, not by remembering to clear it.
		return marshalPayloadJSON(kind, openURLPayloadData{
			DisplayOrigin:      v.DisplayOrigin,
			RequiresCompletion: v.RequiresCompletion,
		})
	default:
		return nil, &UnknownPayloadKindError{Kind: string(kind)}
	}
}

func unmarshalPayloadData(kind payloadKind, data json.RawMessage) (Payload, error) {
	switch kind {
	case payloadKindOpen:
		var raw openPayloadData
		if err := decodeStrict(data, &raw); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		if len(raw.Payload) == 0 {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: errMissingPayloadData}
		}
		nested, err := UnmarshalPayload(raw.Payload)
		if err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return OpenPayload{GateID: raw.GateID, Payload: nested}, nil
	case payloadKindPermission:
		// Restore is an untrusted boundary: the strict request decoder rejects
		// unknown fields, duplicate keys, and invariant violations fail-closed.
		request, err := DecodeRequest(data)
		if err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return PermissionPayload{Request: request}, nil
	case payloadKindAskUser:
		var payload AskUserPayload
		if err := decodeStrict(data, &payload); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return payload, nil
	case payloadKindResumeInput:
		var payload ResumeInputPayload
		if err := decodeStrict(data, &payload); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return payload, nil
	case payloadKindForm:
		var payload FormPayload
		if err := decodeStrict(data, &payload); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		// Restore is an untrusted boundary: re-validate rather than trust that
		// whatever wrote the record enforced the bounds.
		if err := ValidateFormSchema(payload.Schema); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		return payload, nil
	case payloadKindOpenURL:
		// decodeStrict + a data type with no URL field means a record carrying a
		// "url" key is REJECTED, not silently accepted and dropped.
		var raw openURLPayloadData
		if err := decodeStrict(data, &raw); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		if err := validateDisplayOrigin(raw.DisplayOrigin); err != nil {
			return nil, &PayloadDecodeError{Kind: string(kind), Cause: err}
		}
		// URL is intentionally left zero: it was never journaled.
		return OpenURLPayload{
			DisplayOrigin:      raw.DisplayOrigin,
			RequiresCompletion: raw.RequiresCompletion,
		}, nil
	default:
		return nil, &UnknownPayloadKindError{Kind: string(kind)}
	}
}

func marshalPayloadJSON(kind payloadKind, value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, &PayloadEncodeError{Kind: string(kind), Cause: err}
	}
	return data, nil
}

func openPayloadValue(payload Payload) OpenPayload {
	switch v := payload.(type) {
	case OpenPayload:
		return v
	case *OpenPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func permissionPayloadValue(payload Payload) PermissionPayload {
	switch v := payload.(type) {
	case PermissionPayload:
		return v
	case *PermissionPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func askUserPayloadValue(payload Payload) AskUserPayload {
	switch v := payload.(type) {
	case AskUserPayload:
		return v
	case *AskUserPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func resumeInputPayloadValue(payload Payload) ResumeInputPayload {
	switch v := payload.(type) {
	case ResumeInputPayload:
		return v
	case *ResumeInputPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func formPayloadValue(payload Payload) FormPayload {
	switch v := payload.(type) {
	case FormPayload:
		return v
	case *FormPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func openURLPayloadValue(payload Payload) OpenURLPayload {
	switch v := payload.(type) {
	case OpenURLPayload:
		return v
	case *OpenURLPayload:
		return *v
	default:
		panic("gate: internal payload type mismatch")
	}
}

func decodeStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isExplicitJSONNull(data json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
