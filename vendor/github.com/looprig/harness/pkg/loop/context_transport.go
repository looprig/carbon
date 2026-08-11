package loop

import (
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// ContextTransport is one admitted (wire transport -> trust posture) pair a
// loop definition allows a live model switch to move to.
type ContextTransport struct {
	Provider   model.ProviderName
	APIFormat  model.APIFormat
	BaseURL    string
	Capability contextcount.InferenceCapability
}

// contextTransportKey projects the transport-identifying fields of a model,
// ignoring fields (Name, Sampling, Caps, Limits, Origin) that do not change
// the wire transport or trust posture. This is a deliberate product decision,
// not an oversight: Sampling.Effort in particular is never part of transport
// or trust identity — it is a per-request sampling parameter, validated
// separately by each model's own declared effort set, and switching effort
// never crosses a security/retention boundary the way switching transport
// does. Name varies freely within one transport (many models, one endpoint).
type contextTransportKey struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
}

// transportKeyOf projects the transport identity of a model.
func transportKeyOf(m model.Model) contextTransportKey {
	return contextTransportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
}

// lookupTransport reports the admitted capability for m's transport identity
// within set, if any member matches.
func lookupTransport(set []ContextTransport, m model.Model) (contextcount.InferenceCapability, bool) {
	key := transportKeyOf(m)
	for _, t := range set {
		if (contextTransportKey{Provider: t.Provider, APIFormat: t.APIFormat, BaseURL: t.BaseURL}) == key {
			return t.Capability, true
		}
	}
	return contextcount.InferenceCapability{}, false
}

// ContextTransportNotDeclaredError reports a candidate model whose transport
// is not a member of a loop definition's declared ContextTransport set.
type ContextTransportNotDeclaredError struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
}

func (e *ContextTransportNotDeclaredError) Error() string {
	return "loop: context model transport is not a declared ContextTransport"
}

// validateContextTransportMembership reports whether m's transport identity is
// a member of transports. Define always populates transports (either the
// caller-declared set, or the synthesized single-member default derived from
// the base model) before its mode-binding loop calls this, so the empty-set
// branch below is defensive only — no production caller passes an empty set.
func validateContextTransportMembership(transports []ContextTransport, m model.Model) error {
	if len(transports) == 0 {
		return nil
	}
	if _, ok := lookupTransport(transports, m); !ok {
		return &ContextTransportNotDeclaredError{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
	}
	return nil
}
