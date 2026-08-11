// Package cloudflareworkers provides Cloudflare Workers AI's documented
// OpenAI-compatible AI Gateway route.
package cloudflareworkers

import (
	"os"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	accountEnvironment = "CLOUDFLARE_ACCOUNT_ID"
	keyEnvironment     = "CLOUDFLARE_API_KEY"
)

type Option func(*config)

type config struct {
	account string
	options []simple.Option
}

// WithAccountID sets the Cloudflare account used when Model.BaseURL is empty.
func WithAccountID(account string) Option {
	return func(c *config) { c.account = strings.TrimSpace(account) }
}

// WithGatewayID selects an AI Gateway when Workers AI is routed through one.
func WithGatewayID(gateway string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader("cf-aig-gateway-id", gateway)) }
}

func WithHeader(name, value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader(name, value)) }
}

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv(keyEnvironment)))
		if key == "" {
			key = auth.APIKey(strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN")))
		}
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
	selected.BaseURL = baseURL
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderCloudflareWorkersAI,
		DefaultBaseURL: baseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, cfg.options...)
}
