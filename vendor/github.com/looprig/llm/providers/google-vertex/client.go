// Package vertex provides Google Vertex AI's documented Gemini generateContent
// route and Anthropic Claude rawPredict route. It reuses the shared Gemini and
// Anthropic codecs; only Vertex routing, bearer authentication, and the
// Vertex-required anthropic_version field are provider-specific.
package vertex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/codec/anthropicapi"
	"github.com/looprig/inference/codec/geminiapi"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

const (
	projectEnvironment      = "GOOGLE_CLOUD_PROJECT"
	locationEnvironment     = "GOOGLE_CLOUD_LOCATION"
	defaultAnthropicVersion = "vertex-2023-10-16"
)

// Option configures Vertex project/region routing or adds a documented request
// header. The access token passed to New must already be an OAuth/ADC bearer
// token; this package does not perform an interactive gcloud login.
type Option func(*config)

type config struct {
	project          string
	location         string
	headers          http.Header
	anthropicVersion string
}

func WithProject(project string) Option {
	return func(c *config) { c.project = strings.TrimSpace(project) }
}

func WithLocation(location string) Option {
	return func(c *config) { c.location = strings.TrimSpace(location) }
}

func WithHeader(name, value string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		c.headers.Set(name, value)
	}
}

// WithAnthropicVersion overrides Vertex's native Claude request version. The
// default is the version documented by Vertex's Claude rawPredict examples.
func WithAnthropicVersion(version string) Option {
	return func(c *config) { c.anthropicVersion = strings.TrimSpace(version) }
}

// New constructs a Vertex client using a caller-supplied OAuth bearer token.
// ProviderGoogleVertex selects Gemini or Anthropic from Model.APIFormat;
// ProviderGoogleVertexAnthropic is restricted to Anthropic by llm.ValidateModel.
func New(selected model.Model, accessToken auth.APIKey, options ...Option) (inference.Client, error) {
	if accessToken == "" {
		accessToken = auth.APIKey(strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN")))
	}
	if accessToken == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.Provider(selected.Provider), Kind: llm.AuthGCP}
	}
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	if cfg.project == "" {
		cfg.project = strings.TrimSpace(os.Getenv(projectEnvironment))
	}
	if cfg.location == "" {
		cfg.location = strings.TrimSpace(os.Getenv(locationEnvironment))
	}
	if cfg.project == "" || cfg.location == "" {
		return nil, &ConfigurationError{Reason: ProjectOrLocationMissing}
	}
	if !validProject(cfg.project) || !validLocation(cfg.location) {
		return nil, &ConfigurationError{Reason: ProjectOrLocationInvalid}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	if baseURL == "" {
		baseURL = vertexBaseURL(cfg.location)
	}
	selected.BaseURL = baseURL

	var base codec.StreamingCodec
	var router route.Router
	patch := func(body map[string]json.RawMessage) error { return nil }
	switch selected.APIFormat {
	case model.APIFormatGemini:
		base = geminiapi.Codec{}
		router = vertexGeminiRoute{project: cfg.project, location: cfg.location}
	case model.APIFormatAnthropic:
		base = anthropicapi.Codec{}
		router = vertexAnthropicRoute{project: cfg.project, location: cfg.location}
		version := defaultAnthropicVersion
		if cfg.anthropicVersion != "" {
			version = cfg.anthropicVersion
		}
		patch = func(body map[string]json.RawMessage) error {
			value, err := json.Marshal(version)
			if err != nil {
				return err
			}
			body["anthropic_version"] = value
			delete(body, "X-Vertex-Anthropic-Version")
			return nil
		}
	default:
		return nil, &model.ValidationError{Field: "APIFormat", Reason: "Vertex client supports Gemini or Anthropic formats"}
	}

	return transport.New(
		transport.Endpoint{BaseURL: baseURL, Provider: selected.Provider, APIFormat: selected.APIFormat},
		headerRoute{router: router, headers: cfg.headers},
		requestCodec{base: base, patch: patch},
		auth.Key(accessToken),
	), nil
}

func vertexBaseURL(location string) string {
	if strings.EqualFold(location, "global") {
		return (&url.URL{Scheme: "https", Host: "aiplatform.googleapis.com"}).String()
	}
	return (&url.URL{Scheme: "https", Host: location + "-aiplatform.googleapis.com"}).String()
}

func validProject(project string) bool {
	if len(project) < 6 || len(project) > 30 || project[0] < 'a' || project[0] > 'z' {
		return false
	}
	last := project[len(project)-1]
	if !isLowerAlphaNumeric(last) {
		return false
	}
	for _, ch := range project[1 : len(project)-1] {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' {
			return false
		}
	}
	return true
}

func validLocation(location string) bool {
	if location == "global" || len(location) < 2 || len(location) > 63 {
		return location == "global"
	}
	if !isLowerAlphaNumeric(location[0]) || !isLowerAlphaNumeric(location[len(location)-1]) {
		return false
	}
	for _, ch := range location[1 : len(location)-1] {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
}

type headerRoute struct {
	router  route.Router
	headers http.Header
}

func (r headerRoute) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	built, err := r.router.BuildRoute(baseURL, req, mode)
	if err != nil {
		return route.Route{}, err
	}
	built.Header = r.headers.Clone()
	return built, nil
}

type requestCodec struct {
	base  codec.StreamingCodec
	patch func(map[string]json.RawMessage) error
}

var _ codec.StreamingCodec = requestCodec{}

func (c requestCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	encoded, err := c.base.EncodeRequest(req, mode)
	if err != nil || c.patch == nil {
		return encoded, err
	}
	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("vertex: read encoded request: %w", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("vertex: decode encoded request: %w", err)
	}
	if err := c.patch(body); err != nil {
		return codec.EncodedRequest{}, err
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, fmt.Errorf("vertex: encode patched request: %w", err)
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

func (c requestCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	return c.base.DecodeResponse(body)
}

func (c requestCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	return c.base.DecodeStream(resp)
}

type vertexGeminiRoute struct {
	project  string
	location string
}

func (r vertexGeminiRoute) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	if req.Model.Name == "" {
		return route.Route{}, &ConfigurationError{Reason: ModelMissing}
	}
	method := "generateContent"
	if mode == codec.RequestModeStream {
		method = "streamGenerateContent?alt=sse"
	}
	return route.Route{
		Method: http.MethodPost,
		URL: fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			strings.TrimRight(baseURL, "/"), url.PathEscape(r.project), url.PathEscape(r.location), url.PathEscape(req.Model.Name), method),
	}, nil
}

type vertexAnthropicRoute struct {
	project  string
	location string
}

func (r vertexAnthropicRoute) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (route.Route, error) {
	if req.Model.Name == "" {
		return route.Route{}, &ConfigurationError{Reason: ModelMissing}
	}
	method := "rawPredict"
	if mode == codec.RequestModeStream {
		method = "streamRawPredict"
	}
	return route.Route{
		Method: http.MethodPost,
		URL: fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
			strings.TrimRight(baseURL, "/"), url.PathEscape(r.project), url.PathEscape(r.location), url.PathEscape(req.Model.Name), method),
	}, nil
}
