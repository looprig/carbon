// Package openrouter provides the OpenRouter-specific construction and request
// options for the OpenAI-compatible Chat Completions API.
package openrouter

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/codec/openaiapi"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

// ReasoningOptions controls OpenRouter's provider-specific reasoning object.
// Pointer fields preserve the difference between an omitted value and an
// explicitly supplied false or zero.
type ReasoningOptions struct {
	Effort    string `json:"effort,omitempty"`
	MaxTokens *int   `json:"max_tokens,omitempty"`
	Exclude   *bool  `json:"exclude,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	Context   string `json:"context,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// ProviderRoutingOptions controls OpenRouter's provider-selection policy.
// Pointer fields preserve the difference between an omitted value and an
// explicitly supplied false.
type ProviderRoutingOptions struct {
	Order             []string `json:"order,omitempty"`
	AllowFallbacks    *bool    `json:"allow_fallbacks,omitempty"`
	RequireParameters *bool    `json:"require_parameters,omitempty"`
	DataCollection    string   `json:"data_collection,omitempty"`
	ZDR               *bool    `json:"zdr,omitempty"`
}

type config struct {
	headers         http.Header
	usage           *bool
	reasoning       *ReasoningOptions
	promptCacheKey  string
	providerRouting *ProviderRoutingOptions
	tlsRootCAs      *x509.CertPool
}

// Option customizes an OpenRouter client at construction time.
type Option func(*config)

// WithTLSRootCAs installs a caller-owned verified certificate pool for tests
// and controlled clients; nil is rejected rather than silently using defaults.
func WithTLSRootCAs(roots *x509.CertPool) Option {
	if roots == nil {
		panic("openrouter: TLS roots must not be nil")
	}
	return func(c *config) { c.tlsRootCAs = roots }
}

// WithHTTPReferer adds OpenRouter's optional HTTP-Referer attribution header.
func WithHTTPReferer(value string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		if value == "" {
			c.headers.Del("HTTP-Referer")
			return
		}
		c.headers.Set("HTTP-Referer", value)
	}
}

// WithTitle adds OpenRouter's optional X-OpenRouter-Title attribution header.
func WithTitle(value string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		if value == "" {
			c.headers.Del("X-OpenRouter-Title")
			return
		}
		c.headers.Set("X-OpenRouter-Title", value)
	}
}

// WithUsage requests OpenRouter to include its usage metadata in the response.
func WithUsage(include bool) Option {
	return func(c *config) {
		c.usage = boolPtr(include)
	}
}

// WithReasoning adds OpenRouter's reasoning request object.
func WithReasoning(options ReasoningOptions) Option {
	return func(c *config) {
		c.reasoning = cloneReasoningOptions(options)
	}
}

// WithPromptCacheKey sets OpenRouter's stable prompt-cache key.
func WithPromptCacheKey(value string) Option {
	return func(c *config) {
		c.promptCacheKey = value
	}
}

// WithProviderRouting adds OpenRouter provider-routing preferences.
func WithProviderRouting(options ProviderRoutingOptions) Option {
	return func(c *config) {
		c.providerRouting = cloneProviderRoutingOptions(options)
	}
}

// New builds an OpenRouter inference client. The selected model must identify
// OpenRouter and the OpenAI API format; an empty API key is rejected before a
// client is constructed. An empty model base URL uses OpenRouter's canonical
// API root.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.Provider != model.ProviderName(llm.ProviderOpenRouter) {
		return nil, &model.ValidationError{
			Field:  "Provider",
			Reason: fmt.Sprintf("OpenRouter constructor requires provider %q", llm.ProviderOpenRouter),
		}
	}
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderOpenRouter, Kind: auth.AuthAPIKey}
	}

	cfg := config{}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	cfg = cloneConfig(cfg)

	baseURL := selected.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	transportOptions := []transport.Option{}
	if cfg.tlsRootCAs != nil {
		transportOptions = append(transportOptions, transport.WithTLSRootCAs(cfg.tlsRootCAs))
	}
	return transport.New(
		transport.Endpoint{
			BaseURL:   baseURL,
			Provider:  selected.Provider,
			APIFormat: selected.APIFormat,
		},
		chatRouter{headers: cfg.headers},
		requestCodec{config: cfg},
		auth.Key(key), transportOptions...,
	), nil
}

type chatRouter struct {
	headers http.Header
}

func (r chatRouter) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	built, err := route.StaticChat("/chat/completions").BuildRoute(baseURL, req, mode)
	if err != nil {
		return route.Route{}, err
	}
	built.Header = r.headers.Clone()
	return built, nil
}

type requestCodec struct {
	config config
}

var _ codec.StreamingCodec = requestCodec{}

func (c requestCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	encoded, err := (openaiapi.Codec{}).EncodeRequest(req, mode)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	if !c.config.hasBodyOptions() {
		return encoded, nil
	}

	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openrouter: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openrouter: decode encoded request: %w", err)
	}
	if c.config.usage != nil {
		body["usage"], err = json.Marshal(struct {
			Include bool `json:"include"`
		}{Include: *c.config.usage})
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openrouter: encode usage option: %w", err)
		}
	}
	if c.config.reasoning != nil {
		body["reasoning"], err = json.Marshal(c.config.reasoning)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openrouter: encode reasoning option: %w", err)
		}
		// The OpenRouter reasoning object is the explicit provider-specific
		// configuration. Do not send the legacy OpenAI reasoning_effort field
		// alongside it, since the two controls can disagree.
		delete(body, "reasoning_effort")
	}
	if c.config.promptCacheKey != "" {
		body["prompt_cache_key"], err = json.Marshal(c.config.promptCacheKey)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openrouter: encode prompt cache key: %w", err)
		}
	}
	if c.config.providerRouting != nil {
		body["provider"], err = json.Marshal(c.config.providerRouting)
		if err != nil {
			return codec.EncodedRequest{}, fmt.Errorf("openrouter: encode provider routing option: %w", err)
		}
	}

	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("openrouter: encode extended request: %w", err)
	}
	return codec.EncodedRequest{
		Header: encoded.Header.Clone(),
		Body:   bytes.NewReader(patched),
	}, nil
}

func (requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	if normalized, err := normalizeOpenRouterReasoning(body); err == nil {
		body = normalized
	}
	return (openaiapi.Codec{}).DecodeResponse(body)
}

func (requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	resp.Body = &reasoningResponseBody{source: resp.Body}
	return (openaiapi.Codec{}).DecodeStream(resp)
}

// normalizeOpenRouterReasoning translates OpenRouter's reasoning response
// aliases into the reasoning_content field understood by the shared OpenAI
// decoder. The neutral content model carries reasoning as text, so structured
// reasoning details contribute their text/summary fields when available.
func normalizeOpenRouterReasoning(body []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	rawChoices, ok := envelope["choices"]
	if !ok {
		return body, nil
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil {
		return body, nil
	}

	changed := false
	for _, choice := range choices {
		field := "message"
		rawMessage, ok := choice[field]
		if !ok {
			field = "delta"
			rawMessage, ok = choice[field]
		}
		if !ok {
			continue
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(rawMessage, &message); err != nil {
			continue
		}

		if rawReasoningContent, exists := message["reasoning_content"]; exists {
			var reasoningContent string
			if err := json.Unmarshal(rawReasoningContent, &reasoningContent); err == nil && reasoningContent != "" {
				continue
			}
		}

		reasoning := ""
		if rawReasoning, exists := message["reasoning"]; exists {
			_ = json.Unmarshal(rawReasoning, &reasoning)
		}
		if reasoning == "" {
			reasoning = reasoningDetailsText(message["reasoning_details"])
		}
		if reasoning == "" {
			continue
		}

		normalizedReasoning, err := json.Marshal(reasoning)
		if err != nil {
			return nil, err
		}
		message["reasoning_content"] = normalizedReasoning
		updatedMessage, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		choice[field] = updatedMessage
		changed = true
	}
	if !changed {
		return body, nil
	}

	updatedChoices, err := json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	envelope["choices"] = updatedChoices
	return json.Marshal(envelope)
}

func reasoningDetailsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var details []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return ""
	}
	var parts []string
	for _, detail := range details {
		for _, field := range []string{"text", "summary"} {
			var value string
			if err := json.Unmarshal(detail[field], &value); err == nil && value != "" {
				parts = append(parts, value)
				break
			}
		}
	}
	return strings.Join(parts, "\n")
}

// reasoningResponseBody rewrites complete SSE data lines while preserving the
// underlying response body's ownership and streaming behavior.
type reasoningResponseBody struct {
	source  io.ReadCloser
	pending []byte
	output  bytes.Buffer
	done    bool
	err     error
}

func (b *reasoningResponseBody) Read(p []byte) (int, error) {
	for b.output.Len() == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.done {
			return 0, io.EOF
		}

		buf := make([]byte, 32*1024)
		n, err := b.source.Read(buf)
		if n > 0 {
			b.pending = append(b.pending, buf[:n]...)
			b.processLines(false)
		}
		if err != nil {
			if err == io.EOF {
				b.done = true
				b.processLines(true)
				if b.output.Len() == 0 {
					return 0, io.EOF
				}
				b.err = io.EOF
			} else {
				b.err = err
			}
		}
	}
	return b.output.Read(p)
}

func (b *reasoningResponseBody) Close() error {
	return b.source.Close()
}

func (b *reasoningResponseBody) processLines(atEOF bool) {
	for {
		line, rest, ok := splitSSELine(b.pending, atEOF)
		if !ok {
			return
		}
		b.output.Write(transformSSELine(line))
		b.pending = rest
	}
}

func splitSSELine(data []byte, atEOF bool) (line, rest []byte, ok bool) {
	for i, value := range data {
		switch value {
		case '\n':
			return data[:i+1], data[i+1:], true
		case '\r':
			if i+1 == len(data) && !atEOF {
				return nil, data, false
			}
			end := i + 1
			if i+1 < len(data) && data[i+1] == '\n' {
				end++
			}
			return data[:end], data[end:], true
		}
	}
	if atEOF && len(data) > 0 {
		return data, nil, true
	}
	return nil, data, false
}

func transformSSELine(line []byte) []byte {
	coreEnd := len(line)
	if coreEnd > 0 && line[coreEnd-1] == '\n' {
		coreEnd--
		if coreEnd > 0 && line[coreEnd-1] == '\r' {
			coreEnd--
		}
	} else if coreEnd > 0 && line[coreEnd-1] == '\r' {
		coreEnd--
	}
	core := line[:coreEnd]
	if !bytes.HasPrefix(core, []byte("data:")) {
		return line
	}
	value := core[len("data:"):]
	space := len(value) > 0 && value[0] == ' '
	if space {
		value = value[1:]
	}
	normalized, err := normalizeOpenRouterReasoning(value)
	if err != nil || bytes.Equal(normalized, value) {
		return line
	}

	result := make([]byte, 0, len(line)+len(normalized)-len(value))
	result = append(result, core[:len("data:")]...)
	if space {
		result = append(result, ' ')
	}
	result = append(result, normalized...)
	result = append(result, line[coreEnd:]...)
	return result
}

func (c config) hasBodyOptions() bool {
	return c.usage != nil || c.reasoning != nil || c.promptCacheKey != "" || c.providerRouting != nil
}

func cloneConfig(in config) config {
	out := in
	out.headers = in.headers.Clone()
	if in.usage != nil {
		out.usage = boolPtr(*in.usage)
	}
	if in.reasoning != nil {
		out.reasoning = cloneReasoningOptions(*in.reasoning)
	}
	if in.providerRouting != nil {
		out.providerRouting = cloneProviderRoutingOptions(*in.providerRouting)
	}
	return out
}

func cloneReasoningOptions(in ReasoningOptions) *ReasoningOptions {
	out := in
	out.MaxTokens = intPtrValue(in.MaxTokens)
	out.Exclude = boolPtrValue(in.Exclude)
	out.Enabled = boolPtrValue(in.Enabled)
	return &out
}

func cloneProviderRoutingOptions(in ProviderRoutingOptions) *ProviderRoutingOptions {
	out := in
	out.Order = append([]string(nil), in.Order...)
	out.AllowFallbacks = boolPtrValue(in.AllowFallbacks)
	out.RequireParameters = boolPtrValue(in.RequireParameters)
	out.ZDR = boolPtrValue(in.ZDR)
	return &out
}

func boolPtr(value bool) *bool { return &value }

func boolPtrValue(value *bool) *bool {
	if value == nil {
		return nil
	}
	return boolPtr(*value)
}

func intPtrValue(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
