// Package azurecognitive provides Azure Cognitive Services' documented OpenAI
// and Anthropic-compatible model endpoints.
package azurecognitive

import (
	"os"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const resourceEnvironment = "AZURE_COGNITIVE_SERVICES_RESOURCE_NAME"

type config struct {
	resource string
	options  []simple.Option
}

// Option customizes Azure Cognitive Services endpoint resolution and documented
// request headers/body controls.
type Option func(*config)

// WithResourceName sets the resource used when Model.BaseURL is empty. It takes
// precedence over AZURE_COGNITIVE_SERVICES_RESOURCE_NAME.
func WithResourceName(resource string) Option {
	return func(c *config) { c.resource = strings.TrimSpace(resource) }
}

func WithHeader(name, value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader(name, value)) }
}

func WithThinkingBudget(budget int) Option {
	return func(c *config) { c.options = append(c.options, simple.WithThinkingBudget(budget)) }
}

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	if baseURL == "" {
		resource := cfg.resource
		if resource == "" {
			resource = strings.TrimSpace(os.Getenv(resourceEnvironment))
		}
		if resource == "" {
			return nil, &ResourceConfigurationError{Reason: ResourceMissing}
		}
		if !validResource(resource) {
			return nil, &ResourceConfigurationError{Reason: ResourceInvalid}
		}
		if selected.APIFormat == model.APIFormatAnthropic {
			baseURL = "https://" + resource + ".services.ai.azure.com/anthropic/v1"
		} else {
			baseURL = defaultOpenAIBaseURL(resource)
		}
	}
	selected.BaseURL = baseURL

	definition := simple.Definition{
		Provider:       llm.ProviderAzureCognitiveServices,
		DefaultBaseURL: baseURL,
		Authentication: auth.AuthAPIKey,
		KeyHeader:      "api-key",
	}
	if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultPath = "/messages"
		definition.KeyHeader = "x-api-key"
		defaults := []simple.Option{simple.WithHeader("anthropic-version", "2023-06-01")}
		defaults = append(defaults, cfg.options...)
		return simple.New(selected, key, definition, defaults...)
	}
	if selected.APIFormat == model.APIFormatOpenAIResponses {
		definition.DefaultPath = "/responses"
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, cfg.options...)
}

func defaultOpenAIBaseURL(resource string) string {
	return "https://" + resource + ".cognitiveservices.azure.com/openai/v1"
}
