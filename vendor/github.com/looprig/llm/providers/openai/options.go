package openai

import (
	"crypto/x509"
	"net/http"
)

// ReasoningOptions controls OpenAI reasoning. Effort is encoded as the
// Responses reasoning object or Chat Completions reasoning_effort field,
// depending on the selected model APIFormat. Summary applies to Responses.
type ReasoningOptions struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ServiceTier selects the OpenAI Responses processing tier.
type ServiceTier string

const (
	ServiceTierAuto    ServiceTier = "auto"
	ServiceTierDefault ServiceTier = "default"
	ServiceTierFlex    ServiceTier = "flex"
	ServiceTierScale   ServiceTier = "scale"
)

type config struct {
	reasoning      *ReasoningOptions
	serviceTier    ServiceTier
	metadata       map[string]string
	promptCacheKey string
	tlsRootCAs     *x509.CertPool
	roundTripper   http.RoundTripper
}

// WithRoundTripper installs a caller-owned verified transport.
func WithRoundTripper(rt http.RoundTripper) Option {
	if rt == nil {
		panic("openai: round tripper must not be nil")
	}
	return func(c *config) { c.roundTripper = rt }
}

// Option customizes an OpenAI client at construction time.
type Option func(*config)

func WithTLSRootCAs(roots *x509.CertPool) Option {
	if roots == nil {
		panic("openai: TLS roots must not be nil")
	}
	return func(c *config) { c.tlsRootCAs = roots }
}

// WithReasoning sets the provider reasoning controls. It replaces any
// reasoning controls inferred from model sampling so the caller has one
// explicit provider-specific policy.
func WithReasoning(options ReasoningOptions) Option {
	return func(c *config) {
		copy := options
		c.reasoning = &copy
	}
}

// WithServiceTier selects the OpenAI service tier.
func WithServiceTier(tier ServiceTier) Option {
	return func(c *config) { c.serviceTier = tier }
}

// WithMetadata attaches OpenAI request metadata. The map is copied when the
// option is applied so later caller mutation cannot change a live client.
func WithMetadata(metadata map[string]string) Option {
	return func(c *config) {
		if metadata == nil {
			c.metadata = nil
			return
		}
		c.metadata = make(map[string]string, len(metadata))
		for key, value := range metadata {
			c.metadata[key] = value
		}
	}
}

// WithPromptCacheKey sets the stable OpenAI prompt-cache key for requests.
func WithPromptCacheKey(key string) Option {
	return func(c *config) { c.promptCacheKey = key }
}

func (c config) hasBodyOptions() bool {
	return c.reasoning != nil || c.serviceTier != "" || c.metadata != nil || c.promptCacheKey != ""
}

func (c config) clone() config {
	clone := c
	if c.reasoning != nil {
		reasoning := *c.reasoning
		clone.reasoning = &reasoning
	}
	if c.metadata != nil {
		clone.metadata = make(map[string]string, len(c.metadata))
		for key, value := range c.metadata {
			clone.metadata[key] = value
		}
	}
	return clone
}
