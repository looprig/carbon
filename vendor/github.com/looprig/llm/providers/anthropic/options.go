package anthropic

// ThinkingOptions controls Anthropic's native thinking request fields. Type is
// normally "adaptive" for current Claude models; BudgetTokens is retained for
// providers/models that still expose the manual thinking mode.
type ThinkingOptions struct {
	Type         string `json:"type"`
	Effort       string `json:"effort,omitempty"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

// CacheControlOptions selects Anthropic's prompt-cache boundary.
type CacheControlOptions struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type config struct {
	thinking       *ThinkingOptions
	betaHeaders    []string
	betaHeadersSet bool
	metadataUserID string
	cacheControl   *CacheControlOptions
}

// Option customizes an Anthropic Messages client at construction time.
type Option func(*config)

// WithThinking sets Anthropic's native thinking configuration. It is encoded
// as `thinking` and, when Effort is set, `output_config.effort`.
func WithThinking(options ThinkingOptions) Option {
	return func(c *config) {
		copy := options
		c.thinking = &copy
	}
}

// WithBetaHeaders replaces the Anthropic beta feature header values. The
// default matches the currently documented OpenCode Anthropic integration.
func WithBetaHeaders(values ...string) Option {
	return func(c *config) {
		c.betaHeaders = append([]string(nil), values...)
		c.betaHeadersSet = true
	}
}

// WithMetadataUserID sets Anthropic's supported metadata.user_id field.
func WithMetadataUserID(userID string) Option {
	return func(c *config) { c.metadataUserID = userID }
}

// WithPromptCacheControl marks the system prompt's cache boundary. The
// request codec turns Anthropic's string system prompt into its native text
// content-block form so cache_control remains a first-class block field.
func WithPromptCacheControl(options CacheControlOptions) Option {
	return func(c *config) {
		copy := options
		if copy.Type == "" {
			copy.Type = "ephemeral"
		}
		c.cacheControl = &copy
	}
}

func (c config) clone() config {
	clone := c
	if c.thinking != nil {
		thinking := *c.thinking
		if c.thinking.BudgetTokens != nil {
			budget := *c.thinking.BudgetTokens
			thinking.BudgetTokens = &budget
		}
		clone.thinking = &thinking
	}
	clone.betaHeaders = append([]string(nil), c.betaHeaders...)
	if c.cacheControl != nil {
		cache := *c.cacheControl
		clone.cacheControl = &cache
	}
	return clone
}

func (c config) headers() []string {
	if c.betaHeadersSet {
		return append([]string(nil), c.betaHeaders...)
	}
	return []string{
		"interleaved-thinking-2025-05-14",
		"fine-grained-tool-streaming-2025-05-14",
	}
}
