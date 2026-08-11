package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/looprig/inference/auth"
)

const directAccessTTL = 25 * time.Minute

type directAccessResponse struct {
	Headers map[string]string `json:"headers"`
	Token   string            `json:"token"`
}

type directAccessAuthenticator struct {
	key          auth.APIKey
	instanceURL  string
	featureFlags map[string]bool
	client       *http.Client

	mu        sync.Mutex
	token     directAccessResponse
	expiresAt time.Time
}

func newDirectAccessAuthenticator(key auth.APIKey, instanceURL string, featureFlags map[string]bool) *directAccessAuthenticator {
	flags := make(map[string]bool, len(featureFlags))
	for name, enabled := range featureFlags {
		flags[name] = enabled
	}
	return &directAccessAuthenticator{
		key:          key,
		instanceURL:  strings.TrimRight(instanceURL, "/"),
		featureFlags: flags,
		client:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *directAccessAuthenticator) invalidate() {
	a.mu.Lock()
	a.token = directAccessResponse{}
	a.expiresAt = time.Time{}
	a.mu.Unlock()
}

func (a *directAccessAuthenticator) Authorize(ctx context.Context, request *http.Request) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	for name, value := range token.Headers {
		if strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "authorization") {
			continue
		}
		request.Header.Set(name, value)
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	return nil
}

func (a *directAccessAuthenticator) getToken(ctx context.Context) (directAccessResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token.Token != "" && time.Now().Before(a.expiresAt) {
		return a.token, nil
	}

	body, err := json.Marshal(struct {
		FeatureFlags map[string]bool `json:"feature_flags,omitempty"`
	}{FeatureFlags: a.featureFlags})
	if err != nil {
		return directAccessResponse{}, &DirectAccessError{Reason: "encode exchange request"}
	}
	endpoint := a.instanceURL + "/api/v4/ai/third_party_agents/direct_access"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return directAccessResponse{}, &DirectAccessError{Reason: "build exchange request"}
	}
	request.Header.Set("Authorization", "Bearer "+string(a.key))
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return directAccessResponse{}, &DirectAccessError{Reason: "exchange request failed", Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return directAccessResponse{}, &DirectAccessError{Status: response.StatusCode, Reason: "GitLab rejected direct-access-token exchange"}
	}
	var token directAccessResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		return directAccessResponse{}, &DirectAccessError{Reason: "decode direct-access-token response", Err: err}
	}
	if token.Token == "" {
		return directAccessResponse{}, &DirectAccessError{Reason: "direct-access-token response did not contain a token"}
	}
	a.token = token
	a.expiresAt = time.Now().Add(directAccessTTL)
	return token, nil
}

func validateInstanceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return &DirectAccessError{Reason: "provider URL must be an absolute HTTP(S) URL without userinfo"}
	}
	return nil
}

var _ auth.Authenticator = (*directAccessAuthenticator)(nil)
