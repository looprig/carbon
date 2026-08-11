package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	failure "github.com/looprig/inference/failure"

	geminicodec "github.com/looprig/inference/codec/geminiapi"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
	usage "github.com/looprig/inference/usage"
	"github.com/looprig/llm"
)

const (
	// counterTokenizerRevision pins both the provider count method and the API
	// revision whose request encoding is used.
	counterTokenizerRevision contextcount.TokenizerRevision = "google-gemini-countTokens-v1beta"
	// googleSecurityPolicyRevision identifies the provider endpoint's
	// secret-free transport/auth policy. It deliberately excludes the RPC method,
	// tokenizer, credential, and retention metadata so inference and counting on
	// the same endpoint can share one security identity.
	googleSecurityPolicyRevision = "google-generative-language-api-key-tls-v1"
	maxCountResponseBodyBytes    = 64 << 10
	defaultCounterTimeout        = 60 * time.Second
)

// Counter is a separately constructed Gemini countTokens client. It is not
// embedded in Client so an inference client can never acquire the optional
// ContextCounter capability accidentally.
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

// NewCounter constructs an exact provider counter authenticated with key.
func NewCounter(key auth.APIKey) (contextcount.ContextCounter, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderGoogle, Kind: auth.AuthAPIKey}
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
		auth:        auth.Header(key, apiKeyHeader),
		hc:          newHTTPClient(),
		timeout:     defaultCounterTimeout,
	}
}

// CountContext sends the complete encoded inference request to countTokens.
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
	httpReq, err := buildRequest(counterCtx, c.endpoint, req.Model.Name, methodCountTokens, "", body)
	if err != nil {
		return contextcount.ContextCount{}, err
	}
	httpReq.Header.Set("Accept", contentTypeJSON)
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
	if llm.Provider(req.Model.Provider) != llm.ProviderGoogle {
		return nil, &failure.ModelMismatchError{
			BoundProvider: model.ProviderName(llm.ProviderGoogle), RequestProvider: req.Model.Provider,
			BoundEndpoint: c.endpoint, RequestEndpoint: req.Model.BaseURL,
		}
	}
	if err := llm.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if req.Model.APIFormat != model.APIFormatGemini {
		return nil, &UnsupportedAPIFormatError{APIFormat: req.Model.APIFormat}
	}
	generateBody, err := geminicodec.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	return wrapGenerateContentRequest(req.Model.Name, generateBody)
}

func wrapGenerateContentRequest(modelName string, generateBody []byte) ([]byte, error) {
	object := bytes.TrimSpace(generateBody)
	if !json.Valid(object) || len(object) < 2 || object[0] != '{' || object[len(object)-1] != '}' {
		return nil, &CounterRequestError{Reason: CounterRequestGenerateBodyInvalid}
	}
	if err := rejectModelField(object); err != nil {
		return nil, err
	}
	model, err := json.Marshal("models/" + modelName)
	if err != nil {
		return nil, &CounterRequestError{Reason: CounterRequestModelEncodingFailed, Err: err}
	}
	const prefix = `{"generateContentRequest":{"model":`
	body := make([]byte, 0, len(prefix)+len(model)+len(object)+2)
	body = append(body, prefix...)
	body = append(body, model...)
	if len(bytes.TrimSpace(object[1:len(object)-1])) > 0 {
		body = append(body, ',')
	}
	body = append(body, object[1:]...)
	body = append(body, '}')
	return body, nil
}

func rejectModelField(object []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(object))
	if _, err := decoder.Token(); err != nil {
		return &CounterRequestError{Reason: CounterRequestGenerateBodyInvalid, Err: err}
	}
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return &CounterRequestError{Reason: CounterRequestGenerateBodyInvalid, Err: err}
		}
		name, ok := nameToken.(string)
		if !ok {
			return &CounterRequestError{Reason: CounterRequestGenerateBodyInvalid}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return &CounterRequestError{Reason: CounterRequestGenerateBodyInvalid, Err: err}
		}
		if strings.EqualFold(name, "model") {
			return &CounterRequestError{Reason: CounterRequestModelCollision}
		}
	}
	return nil
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
	if len(body) > maxCountResponseBodyBytes {
		return nil, true, nil
	}
	return body, false, nil
}

type countTokensResponse struct {
	TotalTokens countScalar `json:"totalTokens"`
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
	if !response.TotalTokens.present {
		return 0, &CounterResponseError{Reason: CounterResponseMissingCount}
	}
	count, err := response.TotalTokens.tokenCount()
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
	return rejectDuplicateCountFields(decoder)
}

func rejectDuplicateCountFields(decoder *json.Decoder) error {
	seen := false
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil
		}
		var value json.RawMessage
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return nil
		}
		if !strings.EqualFold(name, string(CounterResponseFieldTotalTokens)) {
			continue
		}
		if seen {
			return &CounterResponseFieldError{Field: CounterResponseFieldTotalTokens, Reason: CounterResponseFieldDuplicate}
		}
		seen = true
	}
	return nil
}

func (c countScalar) tokenCount() (content.TokenCount, error) {
	raw := bytes.TrimSpace(c.raw)
	if bytes.Equal(raw, []byte("null")) {
		return 0, countNormalizationError(usage.UsageNormalizationReasonNull)
	}
	if !countNumber(raw) {
		return 0, countNormalizationError(usage.UsageNormalizationReasonInvalidType)
	}
	if bytes.ContainsAny(raw, ".eE") {
		return 0, countNormalizationError(usage.UsageNormalizationReasonFractional)
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, countNormalizationError(usage.UsageNormalizationReasonOutOfRange)
		}
		return 0, countNormalizationError(usage.UsageNormalizationReasonInvalidType)
	}
	if value < 0 {
		return 0, &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: usage.UsageNormalizationReasonNegative, Value: value}
	}
	return content.TokenCount(value), nil
}

func countNumber(raw []byte) bool {
	return len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9')
}

func countNormalizationError(reason usage.UsageNormalizationReason) error {
	return &usage.UsageNormalizationError{Field: usage.UsageNormalizationFieldInputTokens, Reason: reason}
}

// CounterCapability declares countTokens as exact provider counting over the
// same Google API endpoint, conservatively allowing provider logging.
func (c *Counter) CounterCapability() contextcount.CounterCapability {
	if c == nil || c.validateConfiguration() != nil {
		return contextcount.CounterCapability{}
	}
	return contextcount.CounterCapability{
		Provider:         contextcount.ProviderID(llm.ProviderGoogle),
		Transport:        contextcount.CounterTransportSameEndpoint,
		SecurityIdentity: counterSecurityIdentity(c.endpoint),
		Retention:        contextcount.RetentionLogged,
		TokenizerRev:     counterTokenizerRevision,
		Quality:          contextcount.CountQualityExactProvider,
	}
}

func counterSecurityIdentity(endpoint string) contextcount.SecurityIdentity {
	material := "provider=google\nendpoint=" + endpoint + "\nauth=x-goog-api-key\ntransport=tls\npolicy=" + googleSecurityPolicyRevision
	return contextcount.SecurityIdentity(sha256.Sum256([]byte(material)))
}

func canonicalCounterEndpoint(endpoint string) (string, *CounterEndpointError) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", counterEndpointError(CounterEndpointMalformed)
	}
	host, endpointErr := validateCounterEndpoint(parsed)
	if endpointErr != nil {
		return "", endpointErr
	}
	normalizeCounterEndpoint(parsed, host)
	return parsed.String(), nil
}

func validateCounterEndpoint(parsed *url.URL) (string, *CounterEndpointError) {
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme == "" {
		return "", counterEndpointError(CounterEndpointMalformed)
	}
	if parsed.User != nil {
		return "", counterEndpointError(CounterEndpointCredentials)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", counterEndpointError(CounterEndpointMissingHost)
	}
	if parsed.RawPath != "" {
		return "", counterEndpointError(CounterEndpointAmbiguousPath)
	}
	host, hostErr := canonicalEndpointHost(parsed.Hostname())
	if hostErr != nil {
		return "", hostErr
	}
	if transportErr := validateCounterTransport(parsed.Scheme, host); transportErr != nil {
		return "", transportErr
	}
	if !validEndpointPort(parsed.Port()) {
		return "", counterEndpointError(CounterEndpointMalformed)
	}
	return host, nil
}

func validateCounterTransport(scheme, host string) *CounterEndpointError {
	switch scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return counterEndpointError(CounterEndpointInsecureTransport)
		}
	default:
		return counterEndpointError(CounterEndpointUnsupportedScheme)
	}
	return nil
}

func normalizeCounterEndpoint(parsed *url.URL, host string) {
	parsed.Host = canonicalURLHost(host, parsed.Scheme, parsed.Port())
	parsed.User = nil
	parsed.Path = canonicalEndpointPath(parsed.Path)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
}

func canonicalURLHost(host, scheme, port string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func canonicalEndpointHost(host string) (string, *CounterEndpointError) {
	for _, char := range host {
		if char > 127 {
			return "", counterEndpointError(CounterEndpointNonASCIIHost)
		}
	}
	if strings.Contains(host, "%") {
		return "", counterEndpointError(CounterEndpointInvalidHost)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if strings.Contains(host, ":") {
		return "", counterEndpointError(CounterEndpointInvalidHost)
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	if !validDNSHost(host) {
		return "", counterEndpointError(CounterEndpointInvalidHost)
	}
	return host, nil
}

func validDNSHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func canonicalEndpointPath(value string) string {
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return cleaned
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validEndpointPort(port string) bool {
	if port == "" {
		return true
	}
	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value != 0
}

func counterEndpointError(reason CounterEndpointReason) *CounterEndpointError {
	return &CounterEndpointError{Reason: reason}
}
