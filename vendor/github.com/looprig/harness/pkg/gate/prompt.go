package gate

import "encoding/json"

// FieldKind identifies the expected shape of a prompt field value.
type FieldKind string

const (
	// FieldText accepts free-form text input.
	FieldText FieldKind = "text"
	// FieldSelect accepts one value from a fixed option list.
	FieldSelect FieldKind = "select"
	// FieldMultiSelect accepts multiple values from a fixed option list.
	FieldMultiSelect FieldKind = "multi_select"
	// FieldConfirm accepts a boolean confirmation. A schema whose only field is
	// a FieldConfirm is the confirmation-only request.
	FieldConfirm FieldKind = "confirm"
)

// Prompt is the user-facing content and schema for resolving a gate.
//
// It is the PUBLIC presentation projection: it travels on the gate envelope
// (and therefore on event.GateOpened) while the payload stays private to the
// opener and the session. A renderer sees only this.
//
// Every field a renderer must be able to TRUST — rather than treat as arbitrary
// text an integration supplied — is derived from the private payload by the
// session at open time (see the GateHost open path), never taken on the
// caller's word. Origin and Schema are those fields; Title and Body are prose.
type Prompt struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	// Origin is the validated bare origin an open-url gate asks a human to
	// authorize, e.g. "https://github.com". It is the security-load-bearing part
	// of such a prompt: it is the thing a human makes the trust decision on, so
	// a renderer must be able to display it AS a validated origin.
	//
	// For a KindOpenURL gate it is REQUIRED and ValidateGate enforces that it is
	// a bare origin — scheme and host, no path, query, fragment, or userinfo —
	// with the same check that guards the durable OpenURLPayload.DisplayOrigin.
	// That is what lets a renderer trust it structurally instead of by
	// convention: an opener cannot smuggle a full action URL (with its `state`
	// and PKCE parameters) into the place a human reads as "who am I trusting".
	//
	// It is NOT the action target. The ephemeral URL lives only on the private
	// OpenURLPayload, reaches no durable record and no renderer, and opening it
	// is the host's job.
	//
	// Other kinds leave it empty.
	Origin   string       `json:"origin,omitempty"`
	Schema   PromptSchema `json:"schema,omitzero"`
	Controls []Control    `json:"controls,omitempty"`
}

// Control describes an action the resolver may choose for a prompt.
type Control struct {
	Action string `json:"action,omitempty"`
	Label  string `json:"label,omitempty"`
}

// ApprovalControls returns the exact, complete control set of a combined
// access-approval prompt. An interactive gate offers exactly these three
// actions; there is no session scope, user-global scope, persistent-deny
// action, or second capability prompt.
func ApprovalControls() []Control {
	return []Control{
		{Action: string(ApprovalApprove), Label: string(ApprovalApprove)},
		{Action: string(ApprovalApproveAlwaysWorkspace), Label: string(ApprovalApproveAlwaysWorkspace)},
		{Action: string(ApprovalDeny), Label: string(ApprovalDeny)},
	}
}

// Field describes one structured input in a prompt schema.
type Field struct {
	Name     string          `json:"name,omitempty"`
	Label    string          `json:"label,omitempty"`
	Kind     FieldKind       `json:"kind,omitempty"`
	Required bool            `json:"required,omitzero"`
	Options  []Option        `json:"options,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// Option is a selectable value for select-style fields.
type Option struct {
	Value string `json:"value,omitempty"`
	Label string `json:"label,omitempty"`
}

// PromptSchema groups the structured fields requested by a prompt.
type PromptSchema struct {
	Fields []Field `json:"fields,omitempty"`
}
