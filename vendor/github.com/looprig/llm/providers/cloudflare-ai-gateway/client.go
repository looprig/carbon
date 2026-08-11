// Package cloudflaregateway provides Cloudflare AI Gateway's documented
// OpenAI, Responses, and Anthropic proxy endpoints.
package cloudflaregateway

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	accountEnvironment       = "CLOUDFLARE_ACCOUNT_ID"
	gatewayEnvironment       = "CLOUDFLARE_GATEWAY_ID"
	legacyGatewayEnvironment = "CLOUDFLARE_AI_GATEWAY_ID"
	tokenEnvironment         = "CLOUDFLARE_API_TOKEN" // #nosec G101 -- environment variable name, not a credential value
)

// Option customizes Cloudflare account routing, gateway headers, and documented
// per-request cache/log/metadata controls.
type Option func(*config)

type config struct {
	account string
	options []simple.Option
}

// WithAccountID sets the Cloudflare account used when Model.BaseURL is empty.
func WithAccountID(account string) Option {
	return func(c *config) { c.account = strings.TrimSpace(account) }
}

// WithGatewayID selects the configured AI Gateway. It takes precedence over
// CLOUDFLARE_AI_GATEWAY_ID.
func WithGatewayID(gateway string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader("cf-aig-gateway-id", gateway)) }
}

// WithHeader adds a provider-supported request header.
func WithHeader(name, value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader(name, value)) }
}

// WithMetadata attaches Cloudflare AI Gateway metadata as the documented JSON
// request header. The input map is serialized at option application time.
func WithMetadata(metadata map[string]string) Option {
	return func(c *config) {
		if metadata == nil {
			return
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			return
		}
		c.options = append(c.options, simple.WithHeader("cf-aig-metadata", string(raw)))
	}
}

// WithSkipCache controls whether the gateway bypasses its cache.
func WithSkipCache(skip bool) Option {
	return func(c *config) {
		c.options = append(c.options, simple.WithHeader("cf-aig-skip-cache", strconv.FormatBool(skip)))
	}
}

// WithCacheTTL sets the gateway cache TTL in seconds.
func WithCacheTTL(seconds int) Option {
	return func(c *config) {
		c.options = append(c.options, simple.WithHeader("cf-aig-cache-ttl", strconv.Itoa(seconds)))
	}
}

// WithCacheKey sets the stable cache key used by the gateway.
func WithCacheKey(key string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader("cf-aig-cache-key", key)) }
}

// WithCollectLog enables or disables request log collection.
func WithCollectLog(collect bool) Option {
	return func(c *config) {
		c.options = append(c.options, simple.WithHeader("cf-aig-collect-log", strconv.FormatBool(collect)))
	}
}

func WithReasoningEffort(value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithReasoningEffort(value)) }
}

func WithThinkingBudget(budget int) Option {
	return func(c *config) { c.options = append(c.options, simple.WithThinkingBudget(budget)) }
}

// New constructs a Cloudflare AI Gateway client. A caller-supplied BaseURL is
// honored for private gateways and deterministic tests; otherwise the account
// ID is resolved from the option or CLOUDFLARE_ACCOUNT_ID.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv(tokenEnvironment)))
	}
	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	if baseURL == "" {
		account := cfg.account
		if account == "" {
			account = strings.TrimSpace(os.Getenv(accountEnvironment))
		}
		if account == "" {
			return nil, &ConfigurationError{Reason: AccountMissing}
		}
		baseURL = "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/v1"
	}
	gateway := strings.TrimSpace(os.Getenv(gatewayEnvironment))
	if gateway == "" {
		gateway = strings.TrimSpace(os.Getenv(legacyGatewayEnvironment))
	}
	if gateway != "" {
		cfg.options = append([]simple.Option{simple.WithHeader("cf-aig-gateway-id", gateway)}, cfg.options...)
	}
	selected.BaseURL = baseURL
	definition := simple.Definition{
		Provider:       llm.ProviderCloudflareAIGateway,
		DefaultBaseURL: baseURL,
		Authentication: auth.AuthAPIKey,
	}
	switch selected.APIFormat {
	case model.APIFormatOpenAIResponses:
		definition.DefaultPath = "/responses"
	case model.APIFormatAnthropic:
		definition.DefaultPath = "/messages"
	default:
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, cfg.options...)
}
