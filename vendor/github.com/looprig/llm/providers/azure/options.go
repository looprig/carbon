package azure

import (
	"os"
	"strings"
)

const resourceNameEnvironment = "AZURE_RESOURCE_NAME"

// ResourceConfigurationReason classifies Azure resource-name configuration
// failures without including provider-controlled values in the error text.
type ResourceConfigurationReason string

const (
	ResourceConfigurationMissing ResourceConfigurationReason = "resource name is missing"
	ResourceConfigurationInvalid ResourceConfigurationReason = "resource name is invalid"
)

// ResourceConfigurationError reports a missing or malformed Azure resource
// name. It is intentionally secret-free and inspectable with errors.As.
type ResourceConfigurationError struct {
	Reason ResourceConfigurationReason
}

func (e *ResourceConfigurationError) Error() string {
	return "azure: resource configuration: " + string(e.Reason)
}

type config struct {
	resourceName   string
	reasoning      *ReasoningOptions
	metadata       map[string]string
	promptCacheKey string
}

// Option customizes Azure OpenAI endpoint resolution and stable Responses
// request fields.
type Option func(*config)

// WithResourceName sets the Azure resource name used when Model.BaseURL is
// empty. It takes precedence over AZURE_RESOURCE_NAME.
func WithResourceName(name string) Option {
	return func(c *config) { c.resourceName = strings.TrimSpace(name) }
}

// ReasoningOptions controls the Azure Responses reasoning object. The fields
// are strings because Azure owns the accepted vocabulary and may add values
// without changing this package's neutral request model.
type ReasoningOptions struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// WithReasoning sets the Responses reasoning controls for every request. It
// replaces the reasoning controls inferred from model sampling.
func WithReasoning(options ReasoningOptions) Option {
	return func(c *config) {
		copy := options
		c.reasoning = &copy
	}
}

// WithMetadata attaches Azure Responses request metadata. The map is copied
// when the option is applied so later caller mutation cannot affect requests.
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

// WithPromptCacheKey sets the stable Azure Responses prompt-cache key.
func WithPromptCacheKey(key string) Option {
	return func(c *config) { c.promptCacheKey = key }
}

func (c config) clone() config {
	c.resourceName = strings.TrimSpace(c.resourceName)
	if c.reasoning != nil {
		reasoning := *c.reasoning
		c.reasoning = &reasoning
	}
	if c.metadata != nil {
		metadata := make(map[string]string, len(c.metadata))
		for key, value := range c.metadata {
			metadata[key] = value
		}
		c.metadata = metadata
	}
	return c
}

func (c config) hasBodyOptions() bool {
	return c.reasoning != nil || c.metadata != nil || c.promptCacheKey != ""
}

func resolveBaseURL(explicit, optionResource string) (string, error) {
	if baseURL := strings.TrimRight(strings.TrimSpace(explicit), "/"); baseURL != "" {
		return baseURL, nil
	}
	resource := strings.TrimSpace(optionResource)
	if resource == "" {
		resource = strings.TrimSpace(os.Getenv(resourceNameEnvironment))
	}
	if resource == "" {
		return "", &ResourceConfigurationError{Reason: ResourceConfigurationMissing}
	}
	if !validResourceName(resource) {
		return "", &ResourceConfigurationError{Reason: ResourceConfigurationInvalid}
	}
	return "https://" + resource + ".openai.azure.com/openai/v1", nil
}

func validResourceName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}
