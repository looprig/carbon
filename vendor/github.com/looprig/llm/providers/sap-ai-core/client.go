// Package sap provides SAP AI Core's orchestration chat endpoint. SAP AI Core
// uses a service-key OAuth client-credentials flow, deployment discovery, and
// the deployment URL's /v2/chat route; it is not treated as a generic hosted
// API-key provider.
package sap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/codec/openaiapi"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/route"
	"github.com/looprig/inference/stream"
	"github.com/looprig/inference/transport"

	"github.com/looprig/llm"
)

const (
	serviceKeyEnvironment    = "AICORE_SERVICE_KEY"
	deploymentEnvironment    = "AICORE_DEPLOYMENT_URL"
	deploymentIDEnvironment  = "AICORE_DEPLOYMENT_ID"
	resourceGroupEnvironment = "AICORE_RESOURCE_GROUP"
)

// ServiceKey is the client-secret form of an SAP AI Core service key.
type ServiceKey struct {
	ClientID     string `json:"clientid"`
	ClientSecret string `json:"clientsecret"`
	TokenURL     string `json:"url"`
	ServiceURLs  struct {
		AIAPIURL string `json:"AI_API_URL"`
	} `json:"serviceurls"`
}

// ParseServiceKey parses the JSON value accepted by AICORE_SERVICE_KEY.
func ParseServiceKey(raw []byte) (ServiceKey, error) {
	var key ServiceKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return ServiceKey{}, &ConfigurationError{Reason: InvalidServiceKey}
	}
	if err := key.validate(); err != nil {
		return ServiceKey{}, err
	}
	return key, nil
}

func (k ServiceKey) validate() error {
	if strings.TrimSpace(k.ClientID) == "" || strings.TrimSpace(k.ClientSecret) == "" || strings.TrimSpace(k.TokenURL) == "" || strings.TrimSpace(k.ServiceURLs.AIAPIURL) == "" {
		return &ConfigurationError{Reason: InvalidServiceKey}
	}
	return nil
}

type Option func(*config)

type config struct {
	deploymentURL  string
	deploymentID   string
	resourceGroup  string
	headers        http.Header
	modelParams    map[string]json.RawMessage
	modelParamsErr error
}

// WithDeploymentURL bypasses deployment discovery and uses the already-known
// orchestration deployment URL. It is also useful for private SAP landscapes.
func WithDeploymentURL(value string) Option {
	return func(c *config) { c.deploymentURL = strings.TrimRight(strings.TrimSpace(value), "/") }
}

// WithDeploymentID selects a deployment from SAP's /v2/lm/deployments list.
func WithDeploymentID(value string) Option {
	return func(c *config) { c.deploymentID = strings.TrimSpace(value) }
}

// WithResourceGroup sets the AI-Resource-Group header used for discovery and
// inference. SAP defaults this value to "default".
func WithResourceGroup(value string) Option {
	return func(c *config) { c.resourceGroup = strings.TrimSpace(value) }
}

func WithHeader(name, value string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		c.headers.Set(name, value)
	}
}

// WithModelParams merges documented SAP Harmonized API model parameters into
// the /v2/chat request. Keys are forwarded verbatim in snake_case so SAP can
// apply model-specific parameters outside the shared inference fields.
func WithModelParams(params map[string]any) Option {
	return func(c *config) {
		if c.modelParams == nil {
			c.modelParams = make(map[string]json.RawMessage)
		}
		for name, value := range params {
			name = strings.TrimSpace(name)
			if name == "" {
				c.modelParamsErr = errors.New("model parameter name is empty")
				continue
			}
			raw, err := json.Marshal(value)
			if err != nil {
				c.modelParamsErr = err
				continue
			}
			c.modelParams[name] = raw
		}
	}
}

// WithModelParam adds one SAP Harmonized API model parameter.
func WithModelParam(name string, value any) Option {
	return WithModelParams(map[string]any{name: value})
}

// New constructs an SAP AI Core client. If Model.BaseURL or WithDeploymentURL
// is absent, the first request discovers a running orchestration deployment
// through the service key's AI_API_URL; construction itself performs no I/O.
func New(selected model.Model, serviceKey ServiceKey, options ...Option) (inference.Client, error) {
	if err := llm.ValidateModel(selected); err != nil {
		return nil, err
	}
	if selected.APIFormat != model.APIFormatOpenAI {
		return nil, &model.ValidationError{Field: "APIFormat", Reason: "SAP AI Core uses its OpenAI-compatible orchestration chat format"}
	}
	if err := serviceKey.validate(); err != nil {
		return nil, err
	}
	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	if cfg.modelParamsErr != nil {
		return nil, &ConfigurationError{Reason: InvalidModelParams, Err: cfg.modelParamsErr}
	}
	if cfg.deploymentID == "" {
		cfg.deploymentID = strings.TrimSpace(os.Getenv(deploymentIDEnvironment))
	}
	if cfg.resourceGroup == "" {
		cfg.resourceGroup = strings.TrimSpace(os.Getenv(resourceGroupEnvironment))
	}
	if cfg.resourceGroup == "" {
		cfg.resourceGroup = "default"
	}
	if cfg.deploymentURL == "" {
		cfg.deploymentURL = strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	}
	if cfg.deploymentURL == "" {
		cfg.deploymentURL = strings.TrimRight(strings.TrimSpace(os.Getenv(deploymentEnvironment)), "/")
	}

	client := &Client{
		selected:      selected,
		serviceKey:    serviceKey,
		deploymentURL: cfg.deploymentURL,
		deploymentID:  cfg.deploymentID,
		resourceGroup: cfg.resourceGroup,
		headers:       cfg.headers.Clone(),
		modelParams:   cloneRawFields(cfg.modelParams),
		auth:          newServiceKeyAuthenticator(serviceKey),
	}
	if client.deploymentURL != "" {
		client.inner = client.buildInner(client.deploymentURL)
	}
	return client, nil
}

// NewFromEnvironment reads AICORE_SERVICE_KEY and the documented deployment
// metadata environment variables.
func NewFromEnvironment(selected model.Model, options ...Option) (inference.Client, error) {
	raw := strings.TrimSpace(os.Getenv(serviceKeyEnvironment))
	if raw == "" {
		return nil, &ConfigurationError{Reason: ServiceKeyMissing}
	}
	key, err := ParseServiceKey([]byte(raw))
	if err != nil {
		return nil, err
	}
	return New(selected, key, options...)
}

// Client lazily resolves SAP's deployment URL and then delegates message
// encoding, decoding, SSE, usage, tools, and errors to the shared OpenAI codec
// and transport.
type Client struct {
	selected      model.Model
	serviceKey    ServiceKey
	deploymentURL string
	deploymentID  string
	resourceGroup string
	headers       http.Header
	modelParams   map[string]json.RawMessage
	auth          *serviceKeyAuthenticator
	mu            sync.Mutex
	inner         inference.Client
}

var _ inference.Client = (*Client)(nil)

func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	inner, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return inner.Invoke(ctx, req)
}

func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	inner, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	return inner.Stream(ctx, req)
}

func (c *Client) ensure(ctx context.Context) (inference.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inner != nil {
		return c.inner, nil
	}
	deploymentURL, err := c.resolveDeployment(ctx)
	if err != nil {
		return nil, err
	}
	c.deploymentURL = deploymentURL
	c.inner = c.buildInner(deploymentURL)
	return c.inner, nil
}

func (c *Client) buildInner(deploymentURL string) inference.Client {
	headers := c.headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("AI-Resource-Group", c.resourceGroup)
	return transport.New(
		transport.Endpoint{BaseURL: strings.TrimRight(deploymentURL, "/"), Provider: selectedProvider(c.selected), APIFormat: model.APIFormatOpenAI},
		headerRoute{headers: headers},
		modelParamsCodec{base: openaiapi.Codec{}, params: c.modelParams},
		c.auth,
	)
}

func cloneRawFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	clone := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		clone[name] = append(json.RawMessage(nil), value...)
	}
	return clone
}

type modelParamsCodec struct {
	base   codec.StreamingCodec
	params map[string]json.RawMessage
}

var _ codec.StreamingCodec = modelParamsCodec{}

func (c modelParamsCodec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	encoded, err := c.base.EncodeRequest(req, mode)
	if err != nil || len(c.params) == 0 {
		return encoded, err
	}
	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return codec.EncodedRequest{}, err
	}
	for name, value := range c.params {
		body[name] = append(json.RawMessage(nil), value...)
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	return codec.EncodedRequest{Header: encoded.Header.Clone(), Body: bytes.NewReader(patched)}, nil
}

func (c modelParamsCodec) DecodeResponse(body []byte) (*inference.Response, error) {
	return c.base.DecodeResponse(body)
}

func (c modelParamsCodec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	return c.base.DecodeStream(resp)
}

func selectedProvider(selected model.Model) model.ProviderName {
	return selected.Provider
}

type headerRoute struct{ headers http.Header }

func (r headerRoute) BuildRoute(baseURL string, _ inference.Request, _ codec.RequestMode) (route.Route, error) {
	return route.Route{Method: http.MethodPost, URL: strings.TrimRight(baseURL, "/") + "/v2/chat", Header: r.headers.Clone()}, nil
}

func (c *Client) resolveDeployment(ctx context.Context) (string, error) {
	token, err := c.auth.token(ctx)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(c.serviceKey.ServiceURLs.AIAPIURL, "/") + "/v2/lm/deployments"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", &RequestError{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("AI-Resource-Group", c.resourceGroup)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &RequestError{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &RequestError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		return "", &RequestError{Status: resp.StatusCode}
	}
	var result deploymentsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", &RequestError{Err: err}
	}
	for _, deployment := range append(result.Resources, result.Deployments...) {
		if deployment.DeploymentURL == "" || (c.deploymentID != "" && deployment.ID != c.deploymentID) {
			continue
		}
		if c.deploymentID == "" && deployment.ConfigurationName != "defaultOrchestrationConfig" && len(result.Resources) > 1 {
			continue
		}
		if deployment.Status != "" && !strings.EqualFold(deployment.Status, "RUNNING") {
			continue
		}
		return strings.TrimRight(deployment.DeploymentURL, "/"), nil
	}
	return "", &ConfigurationError{Reason: DeploymentMissing}
}

type deploymentsResponse struct {
	Resources   []deployment `json:"resources"`
	Deployments []deployment `json:"deployments"`
}

type deployment struct {
	ID                string `json:"id"`
	DeploymentURL     string `json:"deploymentUrl"`
	ConfigurationName string `json:"configurationName"`
	Status            string `json:"status"`
}

type serviceKeyAuthenticator struct {
	key         ServiceKey
	client      *http.Client
	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

func newServiceKeyAuthenticator(key ServiceKey) *serviceKeyAuthenticator {
	return &serviceKeyAuthenticator{key: key, client: http.DefaultClient}
}

func (a *serviceKeyAuthenticator) Authorize(ctx context.Context, req *http.Request) error {
	token, err := a.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (a *serviceKeyAuthenticator) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cachedToken != "" && time.Until(a.expiresAt) > 30*time.Second {
		return a.cachedToken, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.key.ClientID)
	form.Set("client_secret", a.key.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.key.TokenURL, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", &AuthError{Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// SAP accepts client credentials in the form body; Basic is also set for
	// OAuth servers configured to require RFC 6749 client authentication.
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.key.ClientID+":"+a.key.ClientSecret)))
	resp, err := a.client.Do(req)
	if err != nil {
		return "", &AuthError{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &AuthError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		return "", &AuthError{Status: resp.StatusCode}
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		return "", &AuthError{Err: errors.New("token response did not contain access_token")}
	}
	a.cachedToken = token.AccessToken
	if token.ExpiresIn <= 0 {
		token.ExpiresIn = 300
	}
	a.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return a.cachedToken, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}
