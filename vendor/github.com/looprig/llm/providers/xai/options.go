package xai

// ReasoningOptions controls xAI reasoning. It is encoded as the Responses
// reasoning object or Chat Completions reasoning_effort field, as selected by
// the model APIFormat.
type ReasoningOptions struct {
	Effort string `json:"effort,omitempty"`
}

// ServiceTier selects xAI Responses processing priority.
type ServiceTier string

const (
	ServiceTierDefault  ServiceTier = "default"
	ServiceTierPriority ServiceTier = "priority"
)

type config struct {
	reasoning      *ReasoningOptions
	serviceTier    ServiceTier
	promptCacheKey string
}

// Option customizes an xAI Responses client at construction time.
type Option func(*config)

// WithReasoning sets xAI's reasoning effort.
func WithReasoning(options ReasoningOptions) Option {
	return func(c *config) {
		copy := options
		c.reasoning = &copy
	}
}

// WithServiceTier opts into xAI's default or priority processing tier.
func WithServiceTier(tier ServiceTier) Option {
	return func(c *config) { c.serviceTier = tier }
}

// WithPromptCacheKey sets the stable Responses prompt-cache routing key.
func WithPromptCacheKey(key string) Option {
	return func(c *config) { c.promptCacheKey = key }
}

func (c config) hasBodyOptions() bool {
	return c.reasoning != nil || c.serviceTier != "" || c.promptCacheKey != ""
}

func (c config) clone() config {
	clone := c
	if c.reasoning != nil {
		reasoning := *c.reasoning
		clone.reasoning = &reasoning
	}
	return clone
}
