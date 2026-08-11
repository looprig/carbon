package anthropic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	anthropicapi "github.com/looprig/inference/codec/anthropicapi"
	contextcount "github.com/looprig/inference/contextcount"
	failure "github.com/looprig/inference/failure"
	model "github.com/looprig/inference/model"
	usage "github.com/looprig/inference/usage"

	"github.com/looprig/llm"
)

const (
	counterTokenizerRevision        contextcount.TokenizerRevision = "anthropic-messages-count-tokens-v1" // #nosec G101 -- public tokenizer revision identifier, not a credential
	anthropicSecurityPolicyRevision                                = "anthropic-api-key-tls-v1"
	maxCountResponseBodyBytes                                      = 64 << 10
	maxAPIErrorBodyBytes                                           = 1 << 20
	defaultCounterTimeout                                          = 60 * time.Second
)

// Counter is a separately constructed exact Anthropic Messages count_tokens
// client. It is not embedded in the inference client.
type Counter struct {
	endpoint    string
	endpointErr *CounterEndpointError
	auth        auth.Authenticator
	hc          requestDoer
	timeout     time.Duration
}

var _ contextcount.ContextCounter = (*Counter)(nil)

type requestDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewCounter constructs an exact Anthropic Messages input-token counter.
func NewCounter(key auth.APIKey) (contextcount.ContextCounter, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderAnthropic, Kind: auth.AuthAPIKey}
	}
	counter := newCounter(key, defaultBaseURL)
	if counter.endpointErr != nil {
		return nil, counter.endpointErr
	}
	return counter, nil
}

func newCounter(key auth.APIKey, endpoint string) *Counter {
	canonical, endpointErr := canonicalCounterEndpoint(endpoint)
	return &Counter{
		endpoint:    canonical,
		endpointErr: endpointErr,
		auth:        auth.Header(key, "x-api-key"),
		hc:          newCounterHTTPClient(),
		timeout:     defaultCounterTimeout,
	}
}

func (c *Counter) CountContext(ctx context.Context, req inference.Request) (contextcount.ContextCount, error) {
	if err := c.validateState(ctx); err != nil {
		return contextcount.ContextCount{}, err
	}
	counterCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := c.preflight(req)
	if err != nil {
		return contextcount.ContextCount{}, err
	}
	httpReq, err := http.NewRequestWithContext(counterCtx, http.MethodPost, strings.TrimRight(c.endpoint, "/")+"/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return contextcount.ContextCount{}, &CounterRequestError{Reason: CounterRequestMalformed, Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if err := c.auth.Authorize(counterCtx, httpReq); err != nil {
		return contextcount.ContextCount{}, err
	}
	count, err := c.do(httpReq)
	if err != nil {
		return contextcount.ContextCount{}, err
	}
	return contextcount.ContextCount{Model: req.Model.Key(), InputTokens: count, Quality: contextcount.CountQualityExactProvider}, nil
}

func (c *Counter) validateState(ctx context.Context) error {
	if c == nil {
		return &CounterStateError{Reason: CounterStateNilReceiver}
	}
	if ctx == nil {
		return &CounterStateError{Reason: CounterStateNilContext}
	}
	return c.validateConfiguration()
}

func (c *Counter) validateConfiguration() error {
	if c.endpointErr != nil {
		return c.endpointErr
	}
	if c.endpoint == "" {
		return &CounterStateError{Reason: CounterStateMissingEndpoint}
	}
	if c.auth == nil {
		return &CounterStateError{Reason: CounterStateMissingAuthenticator}
	}
	if c.hc == nil {
		return &CounterStateError{Reason: CounterStateMissingHTTPDoer}
	}
	if c.timeout <= 0 {
		return &CounterStateError{Reason: CounterStateInvalidTimeout}
	}
	return nil
}

func (c *Counter) preflight(req inference.Request) ([]byte, error) {
	if llm.Provider(req.Model.Provider) != llm.ProviderAnthropic {
		return nil, &failure.ModelMismatchError{
			BoundProvider:   model.ProviderName(llm.ProviderAnthropic),
			RequestProvider: req.Model.Provider,
			BoundEndpoint:   c.endpoint,
			RequestEndpoint: req.Model.BaseURL,
		}
	}
	if err := llm.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if req.Model.APIFormat != model.APIFormatAnthropic {
		return nil, &model.ValidationError{Field: "APIFormat", Reason: "Anthropic counter requires anthropic"}
	}
	encoded, err := (anthropicapi.Codec{}).EncodeRequest(req, 0)
	if err != nil {
		return nil, &CounterRequestError{Reason: CounterRequestEncodeFailed, Err: err}
	}
	raw, err := io.ReadAll(encoded.Body)
	if err != nil {
		return nil, &CounterRequestError{Reason: CounterRequestEncodeFailed, Err: err}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, &CounterRequestError{Reason: CounterRequestMalformed, Err: err}
	}
	for field := range body {
		switch field {
		case "model", "system", "messages", "tools", "tool_choice":
		default:
			delete(body, field)
		}
	}
	patched, err := json.Marshal(body)
	if err != nil {
		return nil, &CounterRequestError{Reason: CounterRequestEncodeFailed, Err: err}
	}
	return patched, nil
}

func (c *Counter) do(req *http.Request) (content.TokenCount, error) {
	response, err := c.hc.Do(req)
	if err != nil {
		return 0, &failure.NetworkError{Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return 0, providerAPIError(response)
	}
	body, tooLarge, err := readCountResponseBody(response.Body)
	if err != nil {
		return 0, &failure.NetworkError{Err: err}
	}
	if tooLarge {
		return 0, &CounterResponseError{Reason: CounterResponseBodyTooLarge}
	}
	return decodeCountResponse(body)
}

func readCountResponseBody(reader io.Reader) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxCountResponseBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	return body, len(body) > maxCountResponseBodyBytes, nil
}

type countTokensResponse struct {
	InputTokens countScalar `json:"input_tokens"`
}

type countScalar struct {
	present bool
	raw     []byte
}

func (c *countScalar) UnmarshalJSON(data []byte) error {
	c.present = true
	c.raw = append(c.raw[:0], data...)
	return nil
}

func decodeCountResponse(body []byte) (content.TokenCount, error) {
	if err := rejectDuplicateCount(body); err != nil {
		return 0, &CounterResponseError{Reason: CounterResponseDuplicateField, Err: err}
	}
	var response countTokensResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, &CounterResponseError{Reason: CounterResponseMalformed, Err: err}
	}
	if !response.InputTokens.present {
		return 0, &CounterResponseError{Reason: CounterResponseMissingCount}
	}
	count, err := response.InputTokens.tokenCount()
	if err != nil {
		return 0, &CounterResponseError{Reason: CounterResponseInvalidCount, Err: err}
	}
	return count, nil
}

func rejectDuplicateCount(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil {
		return nil
	}
	delim, ok := start.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}
	seen := false
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
		if !strings.EqualFold(name, string(CounterResponseFieldInputTokens)) {
			continue
		}
		if seen {
			return &CounterResponseFieldError{Field: CounterResponseFieldInputTokens, Reason: CounterResponseFieldDuplicate}
		}
		seen = true
	}
	return nil
}

func (c countScalar) tokenCount() (content.TokenCount, error) {
	raw := bytes.TrimSpace(c.raw)
	if bytes.Equal(raw, []byte("null")) {
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: usage.UsageNormalizationReasonNull}
	}
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: usage.UsageNormalizationReasonInvalidType}
	}
	if bytes.ContainsAny(raw, ".eE") {
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: usage.UsageNormalizationReasonFractional}
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		reason := usage.UsageNormalizationReasonInvalidType
		if errors.Is(err, strconv.ErrRange) {
			reason = usage.UsageNormalizationReasonOutOfRange
		}
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: reason}
	}
	if value < 0 {
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: usage.UsageNormalizationReasonNegative, Value: value}
	}
	return content.TokenCount(value), nil
}

func (c *Counter) CounterCapability() contextcount.CounterCapability {
	if c == nil || c.validateConfiguration() != nil {
		return contextcount.CounterCapability{}
	}
	return contextcount.CounterCapability{
		Provider:         contextcount.ProviderID(llm.ProviderAnthropic),
		Transport:        contextcount.CounterTransportSeparateEndpoint,
		SecurityIdentity: counterSecurityIdentity(c.endpoint),
		Retention:        contextcount.RetentionLogged,
		TokenizerRev:     counterTokenizerRevision,
		Quality:          contextcount.CountQualityExactProvider,
	}
}

func counterSecurityIdentity(endpoint string) contextcount.SecurityIdentity {
	material := "provider=anthropic\nendpoint=" + endpoint + "\nauth=x-api-key\ntransport=tls\npolicy=" + anthropicSecurityPolicyRevision
	return contextcount.SecurityIdentity(sha256.Sum256([]byte(material)))
}

func providerAPIError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAPIErrorBodyBytes))
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	return failure.APIErrorFromResponse(response.StatusCode, body, response.Header, 0)
}

func canonicalCounterEndpoint(endpoint string) (string, *CounterEndpointError) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" {
		return "", &CounterEndpointError{Reason: CounterEndpointMalformed}
	}
	if parsed.User != nil {
		return "", &CounterEndpointError{Reason: CounterEndpointCredentials}
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", &CounterEndpointError{Reason: CounterEndpointMissingHost}
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	port := parsed.Port()
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return "", &CounterEndpointError{Reason: CounterEndpointInsecureTransport}
		}
	default:
		return "", &CounterEndpointError{Reason: CounterEndpointUnsupportedScheme}
	}
	parsed.Host = host
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newCounterHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultCounterTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}
