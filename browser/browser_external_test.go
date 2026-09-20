package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
)

func TestStartRequiresInjectedVerifier(t *testing.T) {
	server, err := browser.Start(context.Background(), browser.Config{})
	if server != nil || !errors.Is(err, browser.ErrVerifierRequired) {
		t.Fatalf("Start without verifier = (%v, %v)", server, err)
	}
}

type verifier struct{}

func (verifier) VerifyCredential(_ context.Context, credential identity.Credential) (identity.Claims, error) {
	if credential.Value() != "browser-test-token" {
		return identity.Claims{}, identity.ErrUnauthenticated
	}
	return identity.Claims{Subject: "browser-user", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type client struct{}

func (client) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}
func (client) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	used := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if used {
			return nil, io.EOF
		}
		used = true
		return &content.TextChunk{Text: "browser reply"}, nil
	}, nil), nil
}

func TestExternalApplicationCanStartCreateAndStop(t *testing.T) {
	const tenant = sessionwire.TenantID("local")
	reconcile := factory.DefaultReconcileLimits()
	reconcile.Interval = time.Second
	cfg := browser.Config{
		Runtime: browser.RuntimeConfig{HomeDir: t.TempDir(), AccessProfile: "trusted", ClientBuilder: func() (inference.Client, func() model.Model, error) {
			return client{}, func() model.Model {
				return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI, "http://localhost:1234/v1", "browser-test", model.WithTools(), model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
			}, nil
		}},
		Storage: browser.StorageConfig{DataDir: t.TempDir(), DefaultTenant: tenant},
		Host: browser.HostConfig{ListenAddress: "127.0.0.1:0", AuthToken: "host-token", StorageBindingID: "carbon-local-v1",
			Options: host.Options{HostID: "browser-test-host", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated, Placement: sessionwire.HostPlacementPooled,
				Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second, RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second,
				ApplyDeadline: 30 * time.Second, CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
			Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
			Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
			CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second},
		Factory: browser.FactoryConfig{DefaultTenant: tenant, StorageBindingID: "carbon-local-v1", BindingVersion: "v1", HostLinkToken: "host-token",
			ReplicaID: "browser-test", CookieName: "browser_session", Verifier: verifier{}, Authorizer: factory.TenantAuthorizer{},
			ReconcileLimits: reconcile,
			CSRF: identity.CSRFConfig{SharedKey: bytes.Repeat([]byte{'k'}, identity.MinCSRFSharedKeyBytes), TokenTTL: time.Hour,
				TrustedOrigins: []string{"http://127.0.0.1"}}},
		Address: "127.0.0.1:0",
	}
	s, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	base := "http://" + s.Addr().String()
	request := func(method, path string, body []byte) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer browser-test-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	boot := request(http.MethodGet, "/v1/bootstrap", nil)
	boot.Body.Close()
	if boot.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap = %d", boot.StatusCode)
	}
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "browser-create-1"},
		SessionID: "browser-session-1", AgentID: "carbon", Blocks: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
	body, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	resp := request(http.MethodPost, "/v1/sessions", body)
	message, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, message)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		page := request(http.MethodGet, "/v1/sessions/browser-session-1/journal", nil)
		data, _ := io.ReadAll(page.Body)
		page.Body.Close()
		if page.StatusCode == http.StatusOK && strings.Contains(string(data), "browser reply") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	page := request(http.MethodGet, "/v1/sessions/browser-session-1/journal", nil)
	data, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(data), "browser reply") {
		t.Fatalf("journal = %d %s", page.StatusCode, data)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(fmt.Errorf("Stop: %w", err))
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after Stop")
	}
}
