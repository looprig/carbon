package compat

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
)

// Definition contains immutable wire and policy facts for one simple provider.
// It is intentionally assembled by a public provider package, never discovered
// from an arbitrary model string.
type Definition struct {
	Provider          llm.Provider
	DefaultBaseURL    string
	DefaultPath       string
	Authentication    auth.AuthKind
	KeyHeader         string
	Authenticator     func(auth.APIKey) (auth.Authenticator, error)
	PatchHeaders      func(inference.Request, http.Header)
	NormalizeResponse func([]byte) ([]byte, error)
	NormalizeStream   func(*http.Response) (*http.Response, error)
}

// ProviderOptions is the common option state accepted by simple provider
// packages. Provider-specific packages expose only the option functions that
// their official documentation supports.
type ProviderOptions struct {
	Headers      http.Header
	Path         string
	BodyFields   map[string]json.RawMessage
	PatchRequest func(map[string]json.RawMessage) error
	err          error
}

// Option customizes ProviderOptions.
type Option func(*ProviderOptions)

// WithHeader adds one documented static request header.
func WithHeader(name, value string) Option {
	return func(options *ProviderOptions) {
		if options.Headers == nil {
			options.Headers = make(http.Header)
		}
		options.Headers.Set(name, value)
	}
}

// WithPath overrides the documented provider route. It is intended for gateway
// packages whose upstream API has a stable proxy prefix.
func WithPath(path string) Option {
	return func(options *ProviderOptions) {
		options.Path = strings.TrimSpace(path)
	}
}

// WithBodyField adds one JSON body field. The value is marshaled when the option
// is applied; unsupported values are retained as an option error and returned by
// NewProvider before any client is built.
func WithBodyField(name string, value any) Option {
	return func(options *ProviderOptions) {
		raw, err := json.Marshal(value)
		if err != nil {
			options.err = err
			return
		}
		if options.BodyFields == nil {
			options.BodyFields = make(map[string]json.RawMessage)
		}
		options.BodyFields[name] = raw
	}
}

// WithBodyPatch applies a provider-local patch after common fields are merged.
func WithBodyPatch(patch func(map[string]json.RawMessage) error) Option {
	return func(options *ProviderOptions) { options.PatchRequest = patch }
}

func (o ProviderOptions) clone() ProviderOptions {
	clone := o
	clone.Headers = o.Headers.Clone()
	if o.BodyFields != nil {
		clone.BodyFields = make(map[string]json.RawMessage, len(o.BodyFields))
		for key, value := range o.BodyFields {
			clone.BodyFields[key] = append(json.RawMessage(nil), value...)
		}
	}
	return clone
}

// NewProvider resolves the selected model's default endpoint, builds the
// appropriate generic authenticator, and delegates semantic encoding/decoding
// to New. It supports only AuthNone and AuthAPIKey; special credential providers
// must use their own constructors.
func NewProvider(selected model.Model, key auth.APIKey, definition Definition, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.Provider != model.ProviderName(definition.Provider) {
		return nil, &model.ValidationError{Field: "Provider", Reason: "provider constructor received a different provider model"}
	}
	if selected.BaseURL == "" {
		selected.BaseURL = strings.TrimRight(definition.DefaultBaseURL, "/")
	}
	if strings.TrimSpace(selected.BaseURL) == "" {
		return nil, &model.ValidationError{Field: "BaseURL", Reason: "provider has no resolved default endpoint"}
	}

	var config ProviderOptions
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	config = config.clone()
	if config.err != nil {
		return nil, config.err
	}

	var authenticator auth.Authenticator
	if definition.Authenticator != nil {
		var err error
		authenticator, err = definition.Authenticator(key)
		if err != nil {
			return nil, err
		}
	} else {
		switch definition.Authentication {
		case auth.AuthNone:
			authenticator = auth.None()
		case auth.AuthAPIKey:
			if key == "" {
				return nil, &llm.AuthRequiredError{Provider: definition.Provider, Kind: auth.AuthAPIKey}
			}
			if definition.KeyHeader != "" {
				authenticator = auth.Header(key, definition.KeyHeader)
			} else {
				authenticator = auth.Key(key)
			}
		default:
			return nil, &UnsupportedAuthenticationError{Provider: definition.Provider, Kind: definition.Authentication}
		}
	}

	patch := config.PatchRequest
	if len(config.BodyFields) > 0 {
		previous := patch
		patch = func(body map[string]json.RawMessage) error {
			for key, value := range config.BodyFields {
				body[key] = append(json.RawMessage(nil), value...)
			}
			if previous != nil {
				return previous(body)
			}
			return nil
		}
	}

	return New(selected, Config{
		Authenticator:     authenticator,
		Headers:           config.Headers,
		Path:              firstNonEmpty(config.Path, definition.DefaultPath),
		PatchHeaders:      definition.PatchHeaders,
		PatchRequest:      patch,
		NormalizeResponse: definition.NormalizeResponse,
		NormalizeStream:   definition.NormalizeStream,
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// UnsupportedAuthenticationError is returned when a simple provider wrapper
// is accidentally used for an OAuth, GCP, service-key, or account-token flow.
type UnsupportedAuthenticationError struct {
	Provider llm.Provider
	Kind     auth.AuthKind
}

func (e *UnsupportedAuthenticationError) Error() string {
	return "compat: provider " + string(e.Provider) + " requires " + string(e.Kind) + " authentication"
}
