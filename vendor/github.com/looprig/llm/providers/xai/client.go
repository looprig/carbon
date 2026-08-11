// Package xai provides xAI Chat Completions and Responses API clients. The
// selected model's APIFormat chooses the codec; this package owns xAI's
// endpoint, bearer authentication, options, and one native Responses
// reasoning stream-event alias.
package xai

import (
	"bufio"
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

const defaultBaseURL = "https://api.x.ai/v1"

// New constructs an xAI Chat Completions or Responses API client. The selected
// model's APIFormat chooses the bundled wire codec and route; an empty model
// base URL uses xAI's canonical API root.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.Provider != model.ProviderName(llm.ProviderXAI) {
		return nil, &model.ValidationError{
			Field:  "Provider",
			Reason: fmt.Sprintf("xAI constructor requires provider %q", llm.ProviderXAI),
		}
	}
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderXAI, Kind: auth.AuthAPIKey}
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
	return transport.New(
		transport.Endpoint{
			BaseURL:   baseURL,
			Provider:  selected.Provider,
			APIFormat: selected.APIFormat,
		},
		apiRouter{},
		requestCodec{config: cfg, apiFormat: selected.APIFormat},
		auth.Key(key),
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
		return codec.EncodedRequest{}, fmt.Errorf("xai: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("xai: decode encoded request: %w", err)
	}
	if c.config.reasoning != nil {
		if c.apiFormat == model.APIFormatOpenAI {
			if c.config.reasoning.Effort != "" {
				body["reasoning_effort"], err = json.Marshal(c.config.reasoning.Effort)
				if err != nil {
					return codec.EncodedRequest{}, fmt.Errorf("xai: encode chat reasoning option: %w", err)
				}
			}
			delete(body, "reasoning")
		} else {
			body["reasoning"], err = json.Marshal(c.config.reasoning)
			if err != nil {
				return codec.EncodedRequest{}, fmt.Errorf("xai: encode reasoning option: %w", err)
			}
			delete(body, "reasoning_effort")
		}
	}
	if c.config.serviceTier != "" {
		body["service_tier"], err = json.Marshal(c.config.serviceTier)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("xai: encode service tier option: %w", err)
		}
	}
	if c.config.promptCacheKey != "" {
		body["prompt_cache_key"], err = json.Marshal(c.config.promptCacheKey)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("xai: encode prompt cache key: %w", err)
		}
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("xai: encode extended request: %w", err)
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

func (c requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	return codecFor(c.apiFormat).DecodeResponse(body)
}

func (c requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	if c.apiFormat == model.APIFormatOpenAIResponses {
		resp.Body = &reasoningEventBody{source: resp.Body, reader: bufio.NewReader(resp.Body)}
	}
	return codecFor(c.apiFormat).DecodeStream(resp)
}

func codecFor(apiFormat model.APIFormat) codec.StreamingCodec {
	if apiFormat == model.APIFormatOpenAI {
		return chat.Codec{}
	}
	return responses.Codec{}
}

// reasoningEventBody translates xAI's documented reasoning_text delta event
// to the shared Responses codec's reasoning_summary_text delta vocabulary. It
// transforms complete SSE data lines only and preserves streaming/backpressure.
type reasoningEventBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	pending []byte
}

func (b *reasoningEventBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			b.pending = normalizeReasoningEventLine(line)
			continue
		}
		return 0, err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *reasoningEventBody) Close() error { return b.source.Close() }

func normalizeReasoningEventLine(line []byte) []byte {
	const dataPrefix = "data:"
	idx := bytes.Index(line, []byte(dataPrefix))
	if idx < 0 {
		return line
	}
	payload := bytes.TrimSpace(line[idx+len(dataPrefix):])
	if len(payload) == 0 {
		return line
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return line
	}
	var eventType string
	if err := json.Unmarshal(envelope["type"], &eventType); err != nil || eventType != "response.reasoning_text.delta" {
		return line
	}
	envelope["type"], _ = json.Marshal("response.reasoning_summary_text.delta")
	updated, err := json.Marshal(envelope)
	if err != nil {
		return line
	}
	newline := []byte("\n")
	if bytes.HasSuffix(line, []byte("\r\n")) {
		newline = []byte("\r\n")
	}
	result := make([]byte, 0, idx+len(dataPrefix)+1+len(updated)+len(newline))
	result = append(result, line[:idx]...)
	result = append(result, dataPrefix...)
	result = append(result, ' ')
	result = append(result, updated...)
	result = append(result, newline...)
	return result
}
