package gate

import (
	"encoding/json"
	"time"
)

// PolicyAction names the automatic behavior to apply when policy resolves a gate.
type PolicyAction string

const (
	// PolicyWait leaves the gate open for an explicit response.
	PolicyWait PolicyAction = "wait"
	// PolicyRespond resolves the gate with a configured response template.
	PolicyRespond PolicyAction = "respond"
	// PolicySuspendSession suspends session progress while the gate remains open.
	PolicySuspendSession PolicyAction = "suspend_session"
	// PolicyModelDecide asks a model decision policy to choose a response.
	PolicyModelDecide PolicyAction = "model_decide"
)

// ResponsePolicy describes automatic handling for an unresolved gate.
type ResponsePolicy struct {
	// Timeout marshals as a time.Duration integer in nanoseconds.
	Timeout time.Duration `json:"timeout,omitzero"`
	// OnTimeout is the action taken when Timeout elapses.
	OnTimeout PolicyAction `json:"on_timeout,omitempty"`
	// Response is the template submitted through RespondGate for PolicyRespond.
	Response ResponseTemplate `json:"response,omitzero"`
	// ModelDecision configures PolicyModelDecide when a responder exists.
	ModelDecision ModelDecisionPolicy `json:"model_decision,omitzero"`
}

// EffectiveAction returns the configured timeout action, defaulting to PolicyWait.
func (p ResponsePolicy) EffectiveAction() PolicyAction {
	if p.OnTimeout == "" {
		return PolicyWait
	}
	return p.OnTimeout
}

// ResponseTemplate is a reusable response payload for policy-driven resolution.
type ResponseTemplate struct {
	Action string                     `json:"action,omitempty"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

// ModelDecisionPolicy configures a model-assisted gate decision.
type ModelDecisionPolicy struct {
	Prompt         string           `json:"prompt,omitempty"`
	AllowedActions []string         `json:"allowed_actions,omitempty"`
	Default        ResponseTemplate `json:"default,omitzero"`
	Metadata       json.RawMessage  `json:"metadata,omitempty"`
}
