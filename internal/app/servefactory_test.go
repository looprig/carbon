package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/sessionstore"
)

type serveTestVerifier struct{}

func (serveTestVerifier) VerifyCredential(_ context.Context, credential identity.Credential) (identity.Claims, error) {
	if credential.Value() != "test-browser-token" {
		return identity.Claims{}, identity.ErrUnauthenticated
	}
	return identity.Claims{Subject: "browser-user", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestServeFactoryRequiresInjectedVerifier(t *testing.T) {
	_, err := OpenServeFactory(nil, nil, ServeFactoryConfig{})
	if !errors.Is(err, ErrServeFactoryVerifierRequired) {
		t.Fatalf("OpenServeFactory without verifier = %v, want verifier refusal", err)
	}
}

func TestServeFactoryAuthenticatesBootstrapAndProductUI(t *testing.T) {
	ctx := context.Background()
	var created []*fakeLLM
	dataDir := t.TempDir()
	homeDir := t.TempDir()
	stores, err := OpenServeStorage(ctx, Config{HomeDir: homeDir}, ServeStorageConfig{DataDir: dataDir, DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			client := &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}
			created = append(created, client)
			return client, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close(context.Background()) })
	hostService, err := OpenServePooledHost(ctx, stores, ServePooledHostConfig{
		ListenAddress: "127.0.0.1:0", AuthToken: "test-host-token", StorageBindingID: "carbon-local-v1",
		Options: host.Options{HostID: "carbon-local", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
			CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
		Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = hostService.Stop(context.Background()) })
	var seen identity.Principal
	uiCalls := 0
	reconcile := factory.DefaultReconcileLimits()
	reconcile.Interval = time.Second
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiCalls++
		var ok bool
		seen, ok = factory.UIRoutePrincipal(r)
		if !ok {
			t.Error("protected product route has no verified principal")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	factoryCfg := ServeFactoryConfig{
		DefaultTenant: "local", StorageBindingID: "carbon-local-v1", BindingVersion: "v1",
		HostLinkToken: "test-host-token", ReplicaID: "carbon-local", CookieName: "carbon_session",
		Verifier: serveTestVerifier{}, Authorizer: factory.TenantAuthorizer{},
		CSRF: identity.CSRFConfig{SharedKey: bytes.Repeat([]byte{'k'}, identity.MinCSRFSharedKeyBytes), TokenTTL: time.Hour,
			TrustedOrigins: []string{"http://localhost:8765"}},
		ReconcileLimits: reconcile,
		UIRoutes:        ui,
		AuthorizeUI: func(_ context.Context, p identity.Principal, method, path string) error {
			if p.Tenant() != "local" || method != http.MethodGet || path != "/ui/check" {
				return identity.ErrUnauthorized
			}
			return nil
		},
	}
	wrongToken := factoryCfg
	wrongToken.HostLinkToken = "other-host-token"
	if _, err := OpenServeFactory(stores, hostService, wrongToken); err == nil {
		t.Fatal("Factory accepted a HostLink token that differs from its local Host")
	}
	wrongBinding := factoryCfg
	wrongBinding.StorageBindingID = "other-binding"
	if _, err := OpenServeFactory(stores, hostService, wrongBinding); err == nil {
		t.Fatal("Factory accepted a binding ID that differs from its local Host")
	}
	server, err := OpenServeFactory(stores, hostService, factoryCfg)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:8765"+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	if got := request("/v1/bootstrap", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous bootstrap = %d, want 401", got.Code)
	}
	if got := request("/v1/bootstrap", "test-browser-token"); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "local") {
		t.Fatalf("authenticated bootstrap = %d %q", got.Code, got.Body.String())
	}
	if got := request("/ui/check", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous UI = %d, want 401", got.Code)
	}
	if got := request("/ui/check", "test-browser-token"); got.Code != http.StatusNoContent || seen.Tenant() != "local" {
		t.Fatalf("authorized UI = %d, principal %q", got.Code, seen.Tenant())
	}
	untrusted := httptest.NewRequest(http.MethodGet, "http://localhost:8765/ui/check", nil)
	untrusted.Header.Set("Authorization", "Bearer test-browser-token")
	untrusted.Header.Set("Origin", "https://evil.example")
	untrustedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(untrustedResponse, untrusted)
	if untrustedResponse.Code != http.StatusForbidden || uiCalls != 1 {
		t.Fatalf("untrusted UI origin = %d, handler calls %d", untrustedResponse.Code, uiCalls)
	}
	if err := hostService.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "create-browser-1"}, SessionID: "browser-session-1", AgentID: CarbonAgentID,
		Blocks: json.RawMessage(`[{"type":"text","text":"hello from browser"}]`)}
	body, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, "http://localhost:8765/v1/sessions", bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer test-browser-token")
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Origin", "http://localhost:8765")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, post)
	if response.Code != http.StatusCreated {
		t.Fatalf("browser create = %d %q, want 201", response.Code, response.Body.String())
	}
	object := request("/v1/sessions/browser-session-1/objects/object-1", "test-browser-token")
	if object.Code != http.StatusServiceUnavailable {
		t.Fatalf("unsupported object route = %d %q, want explicit unavailable", object.Code, object.Body.String())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		entry, err := stores.ControlStore().GetDispositionCommand(ctx, sessionstore.GetDispositionCommandRequest{
			TenantID: "local", SessionID: create.SessionID, CommandID: create.CommandID})
		if err == nil && entry.Record.State == sessionstore.InboxStateApplied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("browser create did not settle Applied: %+v %v", entry.Record, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	seenInput := false
	for modelDeadline := time.Now().Add(5 * time.Second); !seenInput && time.Now().Before(modelDeadline); {
		for _, client := range created {
			streams, _ := client.capturedRequests()
			for _, modelRequest := range streams {
				encoded, _ := json.Marshal(modelRequest.Messages)
				seenInput = seenInput || strings.Contains(string(encoded), "hello from browser")
			}
		}
		if !seenInput {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !seenInput {
		t.Fatal("browser's first input never reached the model")
	}
	journal := request("/v1/sessions/browser-session-1/journal", "test-browser-token")
	if journal.Code != http.StatusOK || !strings.Contains(journal.Body.String(), "reply") {
		t.Fatalf("public journal = %d %q, want model reply", journal.Code, journal.Body.String())
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if report, err := hostService.Stop(ctx); err != nil || len(report.Failures) != 0 {
		t.Fatalf("Host stop = %+v %v", report, err)
	}
	if err := stores.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenServeStorage(ctx, Config{HomeDir: homeDir}, ServeStorageConfig{DataDir: dataDir, DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	reopenedReader, err := NewServeSessionReader(reopened.ControlStore(), reopened.Launcher(), "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	page, err := reopenedReader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "local", SessionID: create.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := json.Marshal(page)
	if !strings.Contains(string(pageBody), "reply") {
		t.Fatalf("reopened public journal lost model output: %s", pageBody)
	}
}
