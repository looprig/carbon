// Package gitlab provides GitLab Duo's documented AI Gateway proxy endpoints.
// It exchanges the caller's GitLab PAT/OAuth access token for the short-lived
// direct-access token required by the proxy before forwarding inference calls.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	failure "github.com/looprig/inference/failure"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultOpenAIBaseURL    = "https://cloud.gitlab.com/ai/v1/proxy/openai/v1"
	DefaultAnthropicBaseURL = "https://cloud.gitlab.com/ai/v1/proxy/anthropic"
	defaultInstanceURL      = "https://gitlab.com"
	defaultGatewayURL       = "https://cloud.gitlab.com"
	instanceEnvironment     = "GITLAB_INSTANCE_URL"
	gatewayEnvironment      = "GITLAB_AI_GATEWAY_URL"
)

type Option func(*options)

type options struct {
	simple        []simple.Option
	gatewayURL    string
	instanceURL   string
	featureFlags  map[string]bool
	modelOverride *modelOverride
}

func WithHeader(name, value string) Option {
	return func(opts *options) { opts.simple = append(opts.simple, simple.WithHeader(name, value)) }
}

// WithAIGatewayURL overrides the GitLab AI Gateway proxy base URL. An instance
// URL is intentionally not sent as a made-up inference header: GitLab uses it
// for control-plane discovery, while this package owns only the inference
// proxy transport.
func WithAIGatewayURL(baseURL string) Option {
	return func(opts *options) { opts.gatewayURL = strings.TrimRight(strings.TrimSpace(baseURL), "/") }
}

// WithInstanceURL overrides the GitLab instance used for direct-access-token
// exchange. It is useful for self-managed GitLab and deterministic tests.
func WithInstanceURL(baseURL string) Option {
	return func(opts *options) { opts.instanceURL = strings.TrimRight(strings.TrimSpace(baseURL), "/") }
}

// WithFeatureFlag includes one documented GitLab AI feature flag in the
// direct-access-token exchange request.
func WithFeatureFlag(name string, enabled bool) Option {
	return func(opts *options) {
		if opts.featureFlags == nil {
			opts.featureFlags = make(map[string]bool)
		}
		opts.featureFlags[strings.TrimSpace(name)] = enabled
	}
}

// WithUpstreamModelID explicitly selects a documented upstream model ID for a
// custom or newer GitLab alias. The API format must match the selected model so
// a Chat/Responses/Anthropic request cannot be sent to the wrong upstream API.
func WithUpstreamModelID(id string, apiFormat model.APIFormat) Option {
	return func(opts *options) {
		opts.modelOverride = &modelOverride{ID: id, Format: apiFormat}
	}
}

func WithAIGatewayHeader(name, value string) Option { return WithHeader(name, value) }

func WithReasoningEffort(value string) Option {
	return func(opts *options) { opts.simple = append(opts.simple, simple.WithReasoningEffort(value)) }
}

func WithThinkingBudget(budget int) Option {
	return func(opts *options) { opts.simple = append(opts.simple, simple.WithThinkingBudget(budget)) }
}

func New(selected model.Model, key auth.APIKey, providerOptions ...Option) (inference.Client, error) {
	var configured options
	for _, option := range providerOptions {
		if option != nil {
			option(&configured)
		}
	}
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv("GITLAB_TOKEN")))
	}
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderGitLab, Kind: llm.AuthOAuth}
	}
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	upstreamModel, err := resolveModel(selected.Name, selected.APIFormat, configured.modelOverride)
	if err != nil {
		return nil, err
	}
	instanceURL := configured.instanceURL
	if instanceURL == "" {
		instanceURL = strings.TrimRight(strings.TrimSpace(os.Getenv(instanceEnvironment)), "/")
	}
	if instanceURL == "" {
		instanceURL = defaultInstanceURL
	}
	if err := validateInstanceURL(instanceURL); err != nil {
		return nil, err
	}
	gatewayURL := configured.gatewayURL
	if gatewayURL == "" {
		gatewayURL = strings.TrimRight(strings.TrimSpace(os.Getenv(gatewayEnvironment)), "/")
	}
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	if err := validateInstanceURL(gatewayURL); err != nil {
		return nil, err
	}
	directAuthenticator := newDirectAccessAuthenticator(key, instanceURL, configured.featureFlags)
	definition := simple.Definition{
		Provider:       llm.ProviderGitLab,
		Authentication: auth.AuthAPIKey,
		Authenticator: func(_ auth.APIKey) (auth.Authenticator, error) {
			return directAuthenticator, nil
		},
	}
	modelPatch := simple.WithBodyPatch(func(body map[string]json.RawMessage) error {
		encoded, err := json.Marshal(upstreamModel)
		if err != nil {
			return err
		}
		body["model"] = encoded
		return nil
	})
	if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultPath = "/messages"
		definition.DefaultBaseURL = gatewayURL + "/ai/v1/proxy/anthropic"
		if strings.TrimSpace(selected.BaseURL) != "" {
			definition.DefaultBaseURL = strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
		}
		defaults := []simple.Option{
			simple.WithHeader("User-Agent", "looprig-llm-gitlab"),
			simple.WithHeader("anthropic-version", "2023-06-01"),
			simple.WithHeader("anthropic-beta", "context-1m-2025-08-07"),
		}
		defaults = append(defaults, configured.simple...)
		defaults = append(defaults, modelPatch)
		client, err := simple.New(selected, key, definition, defaults...)
		if err != nil {
			return nil, err
		}
		return &authRetryClient{inner: client, authenticator: directAuthenticator}, nil
	}
	if selected.APIFormat == model.APIFormatOpenAIResponses {
		definition.DefaultPath = "/responses"
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	definition.DefaultBaseURL = gatewayURL + "/ai/v1/proxy/openai/v1"
	if strings.TrimSpace(selected.BaseURL) != "" {
		definition.DefaultBaseURL = strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	}
	defaults := []simple.Option{
		simple.WithHeader("User-Agent", "looprig-llm-gitlab"),
	}
	defaults = append(defaults, configured.simple...)
	defaults = append(defaults, modelPatch)
	client, err := simple.New(selected, key, definition, defaults...)
	if err != nil {
		return nil, err
	}
	return &authRetryClient{inner: client, authenticator: directAuthenticator}, nil
}

type authRetryClient struct {
	inner         inference.Client
	authenticator *directAccessAuthenticator
}

func (c *authRetryClient) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	response, err := c.inner.Invoke(ctx, req)
	if !isInferenceUnauthorized(err) {
		return response, err
	}
	c.authenticator.invalidate()
	return c.inner.Invoke(ctx, req)
}

func (c *authRetryClient) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	reader, err := c.inner.Stream(ctx, req)
	if !isInferenceUnauthorized(err) {
		return reader, err
	}
	c.authenticator.invalidate()
	return c.inner.Stream(ctx, req)
}

func isInferenceUnauthorized(err error) bool {
	var apiErr *failure.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}
