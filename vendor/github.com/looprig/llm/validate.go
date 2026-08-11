package llm

import (
	"fmt"

	model "github.com/looprig/inference/model"
)

// ValidateModel is the fail-closed provider-policy preset that layers the
// known-provider truth table on top of model.Model's structural validation.
// model.Model.Validate is deliberately provider-policy-free (it only checks a
// non-empty Name and a syntactically safe non-empty BaseURL); this preset adds the
// checks inference dropped:
//
//   - Provider must be a known backend (RequiredAuth errors on an unclassified one).
//   - Provider must speak the model's APIFormat (fail-closed: an unsupported pair
//     is rejected).
//   - An empty BaseURL is only acceptable when the provider resolves a canonical or
//     region-routed endpoint (allowsEmptyBaseURL); otherwise an explicit base is
//     required.
//
// It reproduces the pre-split harness Model.Validate behavior, returning a
// *model.ValidationError on the first rule violated. OriginCustom models
// validate identically to catalog rows; the lower trust in a custom model's Caps is
// a downstream gating concern, not this preset's.
func ValidateModel(m model.Model) error {
	p := Provider(m.Provider)

	// RequiredAuth is the canonical provider registry: it errors on any provider not
	// yet classified there, which is exactly "unknown provider" here.
	if _, err := p.RequiredAuth(); err != nil {
		return &model.ValidationError{Field: "Provider", Reason: fmt.Sprintf("unknown provider %q", m.Provider)}
	}
	if !p.supportsAPIFormat(m.APIFormat) {
		return &model.ValidationError{
			Field:  "APIFormat",
			Reason: fmt.Sprintf("provider %q does not support API format %q", m.Provider, m.APIFormat),
		}
	}

	// Structural validation: non-empty Name, and a syntactically safe BaseURL when
	// non-empty (https, or http only for a loopback host, no userinfo). inference
	// treats an empty BaseURL as a wildcard and accepts it; the provider endpoint
	// policy below decides whether that wildcard is acceptable for this provider.
	if err := m.Validate(); err != nil {
		return err
	}

	// Provider endpoint policy: an empty BaseURL means "use the provider's canonical
	// endpoint", valid only for a provider that supplies one (or region-routed
	// Bedrock). Fail-closed for any provider with no default.
	if m.BaseURL == "" && !p.allowsEmptyBaseURL() {
		return &model.ValidationError{Field: "BaseURL", Reason: "must not be empty"}
	}
	return nil
}
