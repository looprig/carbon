// Package openai provides OpenAI Chat Completions and Responses API clients.
// The selected model's APIFormat chooses the wire dialect while both codecs
// remain available to callers that need to target either endpoint.
package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	codec "github.com/looprig/inference/codec"
	chat "github.com/looprig/inference/codec/openaiapi"
	responses "github.com/looprig/inference/codec/openairesponses"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

const defaultBaseURL = "https://api.openai.com/v1"

// New constructs an OpenAI Chat Completions or Responses API client. The
// selected model's APIFormat chooses the bundled wire codec and route; an
// empty model base URL uses OpenAI's canonical v1 root.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.Provider != model.ProviderName(llm.ProviderOpenAI) {
		return nil, &model.ValidationError{
			Field:  "Provider",
			Reason: fmt.Sprintf("OpenAI constructor requires provider %q", llm.ProviderOpenAI),
		}
	}
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderOpenAI, Kind: auth.AuthAPIKey}
	}

	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	cfg = cfg.clone()

	baseURL := selected.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	transportOptions := []transport.Option{}
	if cfg.tlsRootCAs != nil {
		transportOptions = append(transportOptions, transport.WithTLSRootCAs(cfg.tlsRootCAs))
	}
	if cfg.roundTripper != nil {
		transportOptions = append(transportOptions, transport.WithRoundTripper(cfg.roundTripper))
	}
	return transport.New(
		transport.Endpoint{
			BaseURL:   baseURL,
			Provider:  selected.Provider,
			APIFormat: selected.APIFormat,
		},
		apiRouter{},
		requestCodec{config: cfg, apiFormat: selected.APIFormat},
		auth.Key(key), transportOptions...,
	), nil
}

type apiRouter struct{}

func (apiRouter) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	path := "/chat/completions"
	if req.Model.APIFormat == model.APIFormatOpenAIResponses {
		path = "/responses"
	}
	return route.StaticChat(path).BuildRoute(baseURL, req, mode)
}

type requestCodec struct {
	config    config
	apiFormat model.APIFormat
}

var _ codec.StreamingCodec = requestCodec{}

func (c requestCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	base := codecFor(c.apiFormat)
	encoded, err := base.EncodeRequest(req, mode)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	if !c.config.hasBodyOptions() {
		return encoded, nil
	}

	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openai: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openai: decode encoded request: %w", err)
	}
	if c.config.reasoning != nil {
		if c.apiFormat == model.APIFormatOpenAI {
			if c.config.reasoning.Effort != "" {
				body["reasoning_effort"], err = json.Marshal(c.config.reasoning.Effort)
				if err != nil {
					return codec.EncodedRequest{}, fmt.Errorf("openai: encode chat reasoning option: %w", err)
				}
			}
			delete(body, "reasoning")
		} else {
			body["reasoning"], err = json.Marshal(c.config.reasoning)
			if err != nil {
				return codec.EncodedRequest{}, fmt.Errorf("openai: encode reasoning option: %w", err)
			}
			delete(body, "reasoning_effort")
		}
	}
	if c.config.serviceTier != "" {
		body["service_tier"], err = json.Marshal(c.config.serviceTier)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openai: encode service tier option: %w", err)
		}
	}
	if c.config.metadata != nil {
		body["metadata"], err = json.Marshal(c.config.metadata)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openai: encode metadata option: %w", err)
		}
	}
	if c.config.promptCacheKey != "" {
		body["prompt_cache_key"], err = json.Marshal(c.config.promptCacheKey)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openai: encode prompt cache key: %w", err)
		}
	}

	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openai: encode extended request: %w", err)
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

func (c requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	return codecFor(c.apiFormat).DecodeResponse(body)
}

func (c requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	return codecFor(c.apiFormat).DecodeStream(resp)
}

func codecFor(apiFormat model.APIFormat) codec.StreamingCodec {
	if apiFormat == model.APIFormatOpenAI {
		return chat.Codec{}
	}
	return responses.Codec{}
}
