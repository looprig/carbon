// Package chutes is a Chutes end-to-end-encrypted, TEE-attested LLM client.
// It satisfies inference.Client and tunnels OpenAI chat completions through the
// Chutes /e2e/invoke API sealed with post-quantum ML-KEM-768 + ChaCha20-Poly1305.
package chutes

import (
	"bytes"
	"context"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/llm"
	"github.com/looprig/llm/e2e"
)

// compile-time assertion: *Client satisfies inference.Client.
var _ inference.Client = (*Client)(nil)

// Default attestation-service URLs (WIRE.md section 7). The NRAS path is /v3,
// not /v4. Tests override these via WithNRAS.
const (
	defaultAPIBase = "https://api.chutes.ai"
	defaultLLMBase = "https://llm.chutes.ai"
	defaultNRASURL = "https://nras.attestation.nvidia.com/v3/attest/gpu"
	defaultJWKSURL = "https://nras.attestation.nvidia.com/.well-known/jwks.json"
)

// sessionTTL caps how long an attested session (and its invoke nonces) is
// reused before re-attesting. The server advertises a ~60s nonce TTL; we stay
// comfortably under it so a request never races a server-side expiry.
const sessionTTL = 50 * time.Second

// Client is a Chutes end-to-end-encrypted, attested chat client. It resolves
// model names to chute IDs, discovers + attests TEE instances, caches the
// attested session, and tunnels OpenAI chat requests through POST /e2e/invoke
// sealed with post-quantum ML-KEM + ChaCha20-Poly1305.
//
// A Client is safe for concurrent use: the model->chute and session caches are
// guarded by mu.
//
// Connection-binding note (fail-safe asymmetry): unlike the generic
// transport.Client — which binds one Endpoint and rejects a request whose
// Model.Provider/BaseURL differs with a pre-I/O *failure.ModelMismatchError —
// this client binds its gateway endpoints at construction (New's apiBase/llmBase)
// and enforces model identity per request via NVIDIA TEE attestation. A
// provider/endpoint mismatch therefore surfaces as an attestation failure
// (attestation cannot bind to the wrong model/instance), not an
// *failure.ModelMismatchError. This is fail-safe: the request is never sent when
// the check fails; only the error type differs.
type Client struct {
	http    *http.Client
	apiBase string // https://api.chutes.ai — e2e + evidence endpoints
	llmBase string // https://llm.chutes.ai — /v1/models resolution
	apiKey  string
	nrasURL string
	jwksURL string

	mu           sync.Mutex
	sessions     map[string]*attestedSession // keyed by chute_id
	chuteByModel map[string]string           // model name -> chute_id cache

	// attestFn performs the evidence-fetch + verify step for one instance.
	// It defaults to the real attestation (defaultAttest); tests override it
	// because real attestation cannot bind to a test-generated ML-KEM key.
	attestFn func(ctx context.Context, inst instance, chuteID string) error

	// streamDone, if set, is called exactly once by the streaming reader
	// goroutine just before it returns. Test-only hook (see withStreamDone)
	// to prove the goroutine always exits; nil in production.
	streamDone func()
}

// Option configures a Client at construction. Only the knobs that production
// or tests actually use are exposed; nothing speculative.
type Option func(*Client)

// WithHTTPClient sets the HTTP client used for every request (e2e, evidence,
// model listing, NRAS, JWKS). Useful for timeouts, proxies, and httptest.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithLLMBase overrides the base URL used to resolve model names via
// /v1/models. Production uses https://llm.chutes.ai.
func WithLLMBase(base string) Option {
	return func(c *Client) { c.llmBase = base }
}

// WithNRAS overrides the NVIDIA attestation-service and JWKS URLs.
func WithNRAS(nrasURL, jwksURL string) Option {
	return func(c *Client) {
		c.nrasURL = nrasURL
		c.jwksURL = jwksURL
	}
}

// New builds a Client. apiBase is the e2e/evidence host (https://api.chutes.ai);
// apiKey is the Chutes bearer token. Defaults (llmBase, NRAS/JWKS URLs, an
// http.Client, the caches, and the real attestFn) are applied first, then
// options override.
func New(apiBase, apiKey string, opts ...Option) *Client {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	c := &Client{
		http:         &http.Client{},
		apiBase:      apiBase,
		llmBase:      defaultLLMBase,
		apiKey:       apiKey,
		nrasURL:      defaultNRASURL,
		jwksURL:      defaultJWKSURL,
		sessions:     make(map[string]*attestedSession),
		chuteByModel: make(map[string]string),
	}
	c.attestFn = c.defaultAttest
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// attestedSession is a verified, sealed channel to one TEE instance: the
// instance's ML-KEM public key we seal requests to, its id, the remaining
// single-use invoke nonces, and the time the session expires.
type attestedSession struct {
	key        []byte // instance ML-KEM-768 encapsulation key (raw bytes)
	instanceID string
	nonces     []string
	expiry     time.Time
}

// popNonce removes and returns the next single-use invoke nonce. ok is false
// when the session is exhausted.
func (s *attestedSession) popNonce() (string, bool) {
	if len(s.nonces) == 0 {
		return "", false
	}
	n := s.nonces[0]
	s.nonces = s.nonces[1:]
	return n, true
}

// Invoke sends a non-streaming chat completion through the attested e2e channel
// and returns the decrypted response as a provider-neutral *inference.Response.
// It validates the model and request features before any network I/O — fail
// closed.
func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	if err := llm.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return nil, err
	}
	chuteID, err := c.resolveChute(ctx, req.Model.Name)
	if err != nil {
		return nil, err
	}

	sess, err := c.session(ctx, chuteID)
	if err != nil {
		return nil, err
	}

	respDK, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, &e2e.Error{Op: "response keygen", Err: err}
	}
	plaintext, err := encodeRequest(req, false, respDK.EncapsulationKey().Bytes())
	if err != nil {
		return nil, err
	}

	// Try once; on a recoverable status refresh the session and retry once.
	respBody, status, errBody, err := c.invoke(ctx, chuteID, sess, plaintext)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		sess, err = c.recover(ctx, chuteID, status, errBody, respDK)
		if err != nil {
			return nil, err
		}
		respBody, status, errBody, err = c.invoke(ctx, chuteID, sess, plaintext)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, apiError(status, tryDecryptErrorBody(errBody, respDK))
		}
	}

	return decodeResponse(respBody, respDK)
}

// Stream sends a streaming e2e chat request and returns a
// *stream.StreamReader[content.Chunk] over the decrypted deltas.
// It validates the model and request features before any network I/O — fail
// closed.
// The returned reader MUST be Closed by the caller.
func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	if err := llm.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return nil, err
	}
	chuteID, err := c.resolveChute(ctx, req.Model.Name)
	if err != nil {
		return nil, err
	}

	sess, err := c.session(ctx, chuteID)
	if err != nil {
		return nil, err
	}

	respDK, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, &e2e.Error{Op: "response keygen", Err: err}
	}
	plaintext, err := encodeRequest(req, true, respDK.EncapsulationKey().Bytes())
	if err != nil {
		return nil, err
	}

	return c.chatStream(ctx, chuteID, sess, plaintext, respDK)
}

// invoke seals the plaintext to the session's instance key, POSTs it to
// /e2e/invoke with the exact wire headers, and returns the raw response body on
// 200 or the status+body otherwise. A transport failure returns
// *failure.NetworkError.
func (c *Client) invoke(ctx context.Context, chuteID string, sess *attestedSession, plaintext []byte) (respBody []byte, status int, errBody []byte, err error) {
	nonce, ok := sess.popNonce()
	if !ok {
		// Treat exhaustion like a nonce rejection so the caller refetches.
		return nil, http.StatusForbidden, []byte(`{"detail":"nonce exhausted"}`), nil
	}

	mlkemCT, blob, err := e2e.Seal(plaintext, sess.key, []byte("e2e-req-v1"), true)
	if err != nil {
		return nil, 0, nil, err
	}
	reqBody := append(mlkemCT, blob...)

	url := c.apiBase + "/e2e/invoke"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, nil, &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("X-Chute-Id", chuteID)
	httpReq.Header.Set("X-Instance-Id", sess.instanceID)
	httpReq.Header.Set("X-E2E-Nonce", nonce)
	httpReq.Header.Set("X-E2E-Stream", "false")
	httpReq.Header.Set("X-E2E-Path", "/v1/chat/completions")
	httpReq.Header.Set("Content-Type", "application/octet-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, nil, &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, 0, nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, httpResp.StatusCode, body, nil
	}
	return body, http.StatusOK, nil, nil
}

// recover decides how to refresh state after a non-200 invoke status. The
// retryable codes (403+"nonce", 410, 426) carry plaintext signal bodies the
// server picks BEFORE encrypting, so they are inspected as-is. The default
// branch passes the error body through tryDecryptErrorBody so the surfaced
// *failure.APIError carries the real upstream error rather than ciphertext.
func (c *Client) recover(ctx context.Context, chuteID string, status int, body []byte, respDK *mlkem.DecapsulationKey768) (*attestedSession, error) {
	switch {
	case status == http.StatusForbidden && bytes.Contains(body, []byte("nonce")):
		c.dropSession(chuteID)
		return c.session(ctx, chuteID)
	case status == http.StatusGone || status == http.StatusUpgradeRequired:
		c.dropSession(chuteID)
		return c.session(ctx, chuteID)
	default:
		return nil, apiError(status, tryDecryptErrorBody(body, respDK))
	}
}

// resolveChute maps a model name to its chute UUID, caching the result. On a
// miss it lists /v1/models from llmBase and finds the matching id. Transport
// failures return *failure.NetworkError; non-2xx returns *failure.APIError; an
// unknown model returns a plain error.
func (c *Client) resolveChute(ctx context.Context, model string) (string, error) {
	c.mu.Lock()
	if id, ok := c.chuteByModel[model]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	url := c.llmBase + "/v1/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return "", &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return "", apiError(httpResp.StatusCode, body)
	}

	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			ChuteID string `json:"chute_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("chutes resolve: decode /v1/models: %w", err)
	}
	for _, m := range doc.Data {
		if m.ID == model {
			if m.ChuteID == "" {
				return "", fmt.Errorf("chutes resolve: model %q has no chute_id", model)
			}
			c.mu.Lock()
			c.chuteByModel[model] = m.ChuteID
			c.mu.Unlock()
			return m.ChuteID, nil
		}
	}
	return "", fmt.Errorf("chutes resolve: model %q not found in /v1/models", model)
}

// defaultAttest is the production attestation step. It binds a fresh 64-hex
// nonce to the instance's e2e_pubkey via the TDX quote, then verifies the GPU
// evidence through NRAS. It fails closed: any check failure returns the
// AttestationError and the caller must not send a request.
func (c *Client) defaultAttest(ctx context.Context, inst instance, chuteID string) error {
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return attestErr(ReasonEvidenceMalformed, err)
	}
	nonceHex := hex.EncodeToString(nonceBytes) // 64 hex chars

	url := c.apiBase + "/instances/" + inst.id + "/evidence?nonce=" + nonceHex
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return apiError(httpResp.StatusCode, body)
	}

	var doc struct {
		Quote string `json:"quote"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return attestErr(ReasonEvidenceMalformed, err)
	}
	rawQuote, err := base64.StdEncoding.DecodeString(doc.Quote)
	if err != nil {
		return attestErr(ReasonEvidenceMalformed, err)
	}

	// (1) TDX quote signature + report_data binding to the exact pubkey string.
	if err := verifyTDXQuote(rawQuote, nonceHex, inst.pubKeyB64); err != nil {
		return err
	}
	// (2) NVIDIA GPU evidence via NRAS.
	gpu, err := parseGPUEvidence(body)
	if err != nil {
		return attestErr(ReasonEvidenceMalformed, err)
	}
	if err := verifyNvidiaEvidence(ctx, c.http, c.nrasURL, c.jwksURL, gpu); err != nil {
		return err
	}
	return nil
}

// session returns a cached, non-expired, non-exhausted attested session for the
// chute, or establishes a new one (discover -> attest -> cache). It fails
// closed: if attestation errors, nothing is cached and the error propagates so
// no request is ever sent against an unverified instance.
func (c *Client) session(ctx context.Context, chuteID string) (*attestedSession, error) {
	c.mu.Lock()
	if s, ok := c.sessions[chuteID]; ok && time.Now().Before(s.expiry) && len(s.nonces) > 0 {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()

	instances, err := discoverInstances(ctx, c.http, c.apiBase, c.apiKey, chuteID)
	if err != nil {
		return nil, err
	}
	inst := instances[0]

	if err := c.attestFn(ctx, inst, chuteID); err != nil {
		return nil, err // fail closed: do not cache, do not send.
	}

	s := &attestedSession{
		key:        inst.pubKey,
		instanceID: inst.id,
		nonces:     inst.nonces,
		expiry:     time.Now().Add(sessionTTL),
	}
	c.mu.Lock()
	c.sessions[chuteID] = s
	c.mu.Unlock()
	return s, nil
}

// dropSession evicts the cached session for a chute, forcing the next call to
// re-discover and re-attest.
func (c *Client) dropSession(chuteID string) {
	c.mu.Lock()
	delete(c.sessions, chuteID)
	c.mu.Unlock()
}
