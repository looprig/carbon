// Package compat contains the deliberately small shared core used by documented
// providers whose public endpoint is compatible with one of the bundled request
// codecs. Provider packages still own identity, defaults, authentication policy,
// options, and any provider-specific response normalization.
package compat

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
	"github.com/looprig/inference/codec/anthropicapi"
	"github.com/looprig/inference/codec/openaiapi"
	"github.com/looprig/inference/codec/openairesponses"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

// Config customizes one compatibility-backed provider client. Provider packages
// should construct this value from their public options rather than exposing it
// directly as their own public API.
type Config struct {
	Authenticator     auth.Authenticator
	Headers           http.Header
	Path              string
	PatchHeaders      func(inference.Request, http.Header)
	PatchRequest      func(map[string]json.RawMessage) error
	NormalizeResponse func([]byte) ([]byte, error)
	NormalizeStream   func(*http.Response) (*http.Response, error)
}

// Clone returns an independent configuration copy. Header and body-patch state
// are copied so a caller cannot mutate a live client through an option value.
func (c Config) Clone() Config {
	clone := c
	clone.Headers = c.Headers.Clone()
	return clone
}

// MissingAuthenticatorError reports a provider construction bug before any
// transport is created. Passing auth.None() is the explicit no-auth choice.
type MissingAuthenticatorError struct{}

func (e *MissingAuthenticatorError) Error() string {
	return "compat: authenticator is required; pass auth.None() for an unauthenticated endpoint"
}

// New constructs a transport-backed client for an already validated provider
// model. The model's APIFormat selects the bundled semantic codec; Path may
// override the codec's conventional route when a compatible provider uses a
// documented proxy path.
func New(selected model.Model, config Config) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if strings.TrimSpace(selected.BaseURL) == "" {
		return nil, &model.ValidationError{Field: "BaseURL", Reason: "compatibility client requires a resolved base URL"}
	}
	if config.Authenticator == nil {
		return nil, &MissingAuthenticatorError{}
	}
	config = config.Clone()

	baseCodec, err := codecFor(selected.APIFormat)
	if err != nil {
		return nil, err
	}
	path := config.Path
	if path == "" {
		path = defaultPath(selected.APIFormat)
	}
	if !strings.HasPrefix(path, "/") {
		return nil, &model.ValidationError{Field: "Path", Reason: "compatibility route must start with /"}
	}

	return transport.New(
		transport.Endpoint{
			BaseURL:   strings.TrimRight(selected.BaseURL, "/"),
			Provider:  selected.Provider,
			APIFormat: selected.APIFormat,
		},
		headerRoute{path: path, headers: config.Headers, patch: config.PatchHeaders},
		requestCodec{
			base:              baseCodec,
			patch:             config.PatchRequest,
			normalizeResponse: config.NormalizeResponse,
			normalizeStream:   config.NormalizeStream,
		},
		config.Authenticator,
	), nil
}

type headerRoute struct {
	path    string
	headers http.Header
	patch   func(inference.Request, http.Header)
}

func (r headerRoute) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	built, err := route.StaticChat(r.path).BuildRoute(baseURL, req, mode)
	if err != nil {
		return route.Route{}, err
	}
	built.Header = r.headers.Clone()
	if r.patch != nil {
		r.patch(req, built.Header)
	}
	return built, nil
}

type requestCodec struct {
	base              codec.StreamingCodec
	patch             func(map[string]json.RawMessage) error
	normalizeResponse func([]byte) ([]byte, error)
	normalizeStream   func(*http.Response) (*http.Response, error)
}

var _ codec.StreamingCodec = requestCodec{}

func (c requestCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	encoded, err := c.base.EncodeRequest(req, mode)
	if err != nil || c.patch == nil {
		return encoded, err
	}
	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("compat: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("compat: decode encoded request: %w", err)
	}
	if body == nil {
		return codec.EncodedRequest{}, fmt.Errorf("compat: encoded request is not a JSON object")
	}
	if err := c.patch(body); err != nil {
		return codec.EncodedRequest{}, err
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("compat: encode patched request: %w", err)
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

func (c requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	if c.normalizeResponse != nil {
		normalized, err := c.normalizeResponse(body)
		if err != nil {
			return nil, err
		}
		body = normalized
	}
	return c.base.DecodeResponse(body)
}

func (c requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	if c.normalizeStream != nil {
		normalized, err := c.normalizeStream(resp)
		if err != nil {
			return nil, err
		}
		resp = normalized
	}
	return c.base.DecodeStream(resp)
}

func codecFor(apiFormat model.APIFormat) (codec.StreamingCodec, error) {
	switch apiFormat {
	case model.APIFormatOpenAI:
		return openaiapi.Codec{}, nil
	case model.APIFormatOpenAIResponses:
		return openairesponses.Codec{}, nil
	case model.APIFormatAnthropic:
		return anthropicapi.Codec{}, nil
	default:
		return nil, &model.ValidationError{Field: "APIFormat", Reason: "compatibility client has no bundled codec for this API format"}
	}
}

func defaultPath(apiFormat model.APIFormat) string {
	switch apiFormat {
	case model.APIFormatOpenAIResponses:
		return "/responses"
	case model.APIFormatAnthropic:
		return "/messages"
	default:
		return "/chat/completions"
	}
}
