// Package anthropic provides a native Anthropic Messages API client. It keeps
// Anthropic's top-level system prompt, typed content blocks, tool_result/tool_use
// turns, thinking controls, and SSE lifecycle intact by delegating wire semantics
// to inference/codec/anthropicapi.
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	codec "github.com/looprig/inference/codec"
	anthropicapi "github.com/looprig/inference/codec/anthropicapi"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

const (
	defaultBaseURL   = "https://api.anthropic.com/v1"
	anthropicVersion = "2023-06-01"
)

// New constructs an Anthropic native Messages API client. The selected model
// must identify Anthropic and model.APIFormatAnthropic. An empty model base URL
// uses Anthropic's canonical API root.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.Provider != model.ProviderName(llm.ProviderAnthropic) {
		return nil, &model.ValidationError{
			Field:  "Provider",
			Reason: fmt.Sprintf("Anthropic constructor requires provider %q", llm.ProviderAnthropic),
		}
	}
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderAnthropic, Kind: auth.AuthAPIKey}
	}

	cfg := config{}
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
	return transport.New(
		transport.Endpoint{
			BaseURL:   baseURL,
			Provider:  selected.Provider,
			APIFormat: selected.APIFormat,
		},
		messagesRouter{betaHeaders: cfg.headers()},
		requestCodec{config: cfg},
		auth.Header(key, "x-api-key"),
	), nil
}

type messagesRouter struct {
	betaHeaders []string
}

func (r messagesRouter) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	built, err := route.StaticChat("/messages").BuildRoute(baseURL, req, mode)
	if err != nil {
		return route.Route{}, err
	}
	built.Header = make(http.Header)
	built.Header.Set("anthropic-version", anthropicVersion)
	if len(r.betaHeaders) > 0 {
		built.Header.Set("anthropic-beta", strings.Join(r.betaHeaders, ","))
	}
	return built, nil
}

type requestCodec struct {
	config config
}

var _ codec.StreamingCodec = requestCodec{}

func (c requestCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	encoded, err := (anthropicapi.Codec{}).EncodeRequest(req, mode)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	if !c.hasBodyOptions() {
		return encoded, nil
	}
	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("anthropic: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("anthropic: decode encoded request: %w", err)
	}
	if c.config.thinking != nil {
		thinking := *c.config.thinking
		if thinking.Type == "" {
			thinking.Type = "adaptive"
		}
		body["thinking"], err = json.Marshal(makeThinkingRequest(thinking))
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("anthropic: encode thinking option: %w", err)
		}
		if thinking.Effort != "" {
			var outputConfig map[string]json.RawMessage
			if rawConfig, ok := body["output_config"]; ok {
				_ = json.Unmarshal(rawConfig, &outputConfig)
			}
			if outputConfig == nil {
				outputConfig = make(map[string]json.RawMessage)
			}
			outputConfig["effort"], err = json.Marshal(thinking.Effort)
			if err != nil {
				return codec.EncodedRequest{}, fmt.Errorf("anthropic: encode thinking effort: %w", err)
			}
			body["output_config"], err = json.Marshal(outputConfig)
			if err != nil {
				return codec.EncodedRequest{}, fmt.Errorf("anthropic: encode output config: %w", err)
			}
		}
	}
	if c.config.metadataUserID != "" {
		body["metadata"], err = json.Marshal(map[string]string{"user_id": c.config.metadataUserID})
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("anthropic: encode metadata: %w", err)
		}
	}
	if c.config.cacheControl != nil {
		if err := applySystemCacheControl(body, *c.config.cacheControl); err != nil {
			return codec.EncodedRequest{}, err
		}
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("anthropic: encode extended request: %w", err)
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

type thinkingRequest struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

func makeThinkingRequest(options ThinkingOptions) thinkingRequest {
	return thinkingRequest{Type: options.Type, BudgetTokens: options.BudgetTokens}
}

func (c requestCodec) hasBodyOptions() bool {
	return c.config.thinking != nil || c.config.metadataUserID != "" || c.config.cacheControl != nil
}

func applySystemCacheControl(body map[string]json.RawMessage, control CacheControlOptions) error {
	cacheRaw, err := json.Marshal(control)
	if err != nil {
		return fmt.Errorf("anthropic: encode cache control: %w", err)
	}
	if rawSystem, ok := body["system"]; ok {
		var text string
		if err := json.Unmarshal(rawSystem, &text); err == nil && text != "" {
			block := map[string]json.RawMessage{}
			block["type"], _ = json.Marshal("text")
			block["text"], _ = json.Marshal(text)
			block["cache_control"] = cacheRaw
			body["system"], err = json.Marshal([]map[string]json.RawMessage{block})
			if err != nil {
				return fmt.Errorf("anthropic: encode cached system prompt: %w", err)
			}
			return nil
		}
	}
	// If no system prompt exists, place the boundary on the final content block
	// (the native Anthropic cache-control location for a conversation prefix).
	if rawMessages, ok := body["messages"]; ok {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(rawMessages, &messages); err == nil {
			for index := len(messages) - 1; index >= 0; index-- {
				var blocks []map[string]json.RawMessage
				if err := json.Unmarshal(messages[index]["content"], &blocks); err != nil || len(blocks) == 0 {
					continue
				}
				blocks[len(blocks)-1]["cache_control"] = cacheRaw
				messages[index]["content"], err = json.Marshal(blocks)
				if err != nil {
					return fmt.Errorf("anthropic: encode cached message: %w", err)
				}
				body["messages"], err = json.Marshal(messages)
				if err != nil {
					return fmt.Errorf("anthropic: encode cached messages: %w", err)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("anthropic: prompt cache control requires a non-empty system prompt or message")
}

func (requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	return (anthropicapi.Codec{}).DecodeResponse(body)
}

func (requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	return (anthropicapi.Codec{}).DecodeStream(resp)
}
