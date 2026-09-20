//go:build integration

package browser

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

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
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
	_, err := composeFactory(nil, nil, FactoryConfig{})
	if !errors.Is(err, ErrVerifierRequired) {
		t.Fatalf("composeFactory without verifier = %v, want verifier refusal", err)
	}
}

func TestServeFactoryAuthenticatesBootstrapAndProductUI(t *testing.T) {
	ctx := context.Background()
	modelRequests := make(chan inference.Request, 4)
	dataDir := t.TempDir()
	homeDir := t.TempDir()
	stores, err := carbon.OpenServeStorage(ctx, carbon.Config{HomeDir: homeDir}, carbon.ServeStorageConfig{DataDir: dataDir, DefaultTenant: "local"},
		carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
			client := &scriptedClient{fn: func(_ int, req inference.Request) []content.Chunk {
				select {
				case modelRequests <- req:
				default:
				}
				return []content.Chunk{&content.TextChunk{Text: "reply"}}
			}}
			return client, func() model.Model { return testServeModel() }, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close(context.Background()) })
	hostService, err := carbon.OpenServePooledHost(ctx, stores, carbon.ServePooledHostConfig{
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
	factoryCfg := FactoryConfig{
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
	if _, err := composeFactory(stores, hostService, wrongToken); err == nil {
		t.Fatal("Factory accepted a HostLink token that differs from its local Host")
	}
	wrongBinding := factoryCfg
	wrongBinding.StorageBindingID = "other-binding"
	if _, err := composeFactory(stores, hostService, wrongBinding); err == nil {
		t.Fatal("Factory accepted a binding ID that differs from its local Host")
	}
	otherStores, err := carbon.OpenServeStorage(ctx, carbon.Config{HomeDir: homeDir}, carbon.ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"},
		carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
			return &scriptedClient{fn: func(int, inference.Request) []content.Chunk { return nil }}, func() model.Model { return testServeModel() }, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStores.Close(context.Background()) })
	if _, err := composeFactory(otherStores, hostService, factoryCfg); err == nil {
		t.Fatal("Factory accepted a Host from a different storage root")
	}
	legacyCfg := factoryCfg
	legacyCfg.UIRoutes, legacyCfg.AuthorizeUI = nil, nil
	legacyUI, err := composeFactory(stores, hostService, legacyCfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/ui/live"},
		{http.MethodGet, "/ui/session-presentation"},
		{http.MethodPost, "/ui/handoff"},
	} {
		anonymous := httptest.NewRequest(route.method, "http://localhost:8765"+route.path, nil)
		anonymous.Header.Set("Origin", "http://localhost:8765")
		denied := httptest.NewRecorder()
		legacyUI.Handler().ServeHTTP(denied, anonymous)
		if denied.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous retired %s %s = %d, want 401", route.method, route.path, denied.Code)
		}
		request := httptest.NewRequest(route.method, "http://localhost:8765"+route.path, nil)
		request.Header.Set("Authorization", "Bearer test-browser-token")
		request.Header.Set("Origin", "http://localhost:8765")
		response := httptest.NewRecorder()
		legacyUI.Handler().ServeHTTP(response, request)
		var unavailable struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &unavailable); err != nil {
			t.Fatalf("retired %s %s response is not JSON: %v", route.method, route.path, err)
		}
		if response.Code != http.StatusServiceUnavailable || unavailable.Error.Code != "ui_route_unavailable" || unavailable.Error.Retryable {
			t.Fatalf("retired %s %s = %d %q", route.method, route.path, response.Code, response.Body.String())
		}
	}
	server, err := composeFactory(stores, hostService, factoryCfg)
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
	if got := request("/ui/denied", "test-browser-token"); got.Code != http.StatusForbidden || uiCalls != 1 {
		t.Fatalf("UI authorization refusal = %d, handler calls %d", got.Code, uiCalls)
	}
	if got := request("/", ""); got.Code != http.StatusOK || !strings.Contains(strings.ToLower(got.Body.String()), "html") {
		t.Fatalf("official WUI asset shell = %d %q", got.Code, got.Body.String())
	}
	untrusted := httptest.NewRequest(http.MethodGet, "http://localhost:8765/ui/check", nil)
	untrusted.Header.Set("Authorization", "Bearer test-browser-token")
	untrusted.Header.Set("Origin", "https://evil.example")
	untrustedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(untrustedResponse, untrusted)
	if untrustedResponse.Code != http.StatusForbidden || uiCalls != 1 {
		t.Fatalf("untrusted UI origin = %d, handler calls %d", untrustedResponse.Code, uiCalls)
	}
	spoofedHost := httptest.NewRequest(http.MethodGet, "http://attacker.example/ui/check", nil)
	spoofedHost.Header.Set("Authorization", "Bearer test-browser-token")
	spoofedHostResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(spoofedHostResponse, spoofedHost)
	if spoofedHostResponse.Code != http.StatusForbidden || uiCalls != 1 {
		t.Fatalf("spoofed UI Host = %d, handler calls %d", spoofedHostResponse.Code, uiCalls)
	}
	cookieRequest := func(method, path, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost:8765"+path, strings.NewReader(`{}`))
		r.AddCookie(&http.Cookie{Name: "carbon_session", Value: "test-browser-token"})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:8765")
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	issued := cookieRequest(http.MethodGet, "/v1/csrf-token", "")
	if issued.Code != http.StatusOK {
		t.Fatalf("configured cookie CSRF mint = %d %q", issued.Code, issued.Body.String())
	}
	var csrf struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &csrf); err != nil || csrf.Token == "" {
		t.Fatalf("configured cookie minted no CSRF token: %v %q", err, issued.Body.String())
	}
	for _, tc := range []struct{ name, token string }{{"missing", ""}, {"bad", "invalid"}} {
		if got := cookieRequest(http.MethodPost, "/v1/sessions", tc.token); got.Code != http.StatusForbidden {
			t.Fatalf("%s CSRF = %d %q, want 403", tc.name, got.Code, got.Body.String())
		}
	}
	if got := cookieRequest(http.MethodPost, "/v1/sessions", csrf.Token); got.Code == http.StatusForbidden {
		t.Fatalf("valid cookie CSRF was refused: %s", got.Body.String())
	}
	if got := cookieRequest(http.MethodGet, "/v1/bootstrap", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "local") {
		t.Fatalf("cookie bootstrap = %d %q", got.Code, got.Body.String())
	}
	if err := hostService.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "create-browser-1"}, SessionID: "browser-session-1", AgentID: carbon.CarbonAgentID,
		Blocks: json.RawMessage(`[{"type":"text","text":"hello from browser"}]`)}
	body, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, "http://localhost:8765/v1/sessions", bytes.NewReader(body))
	post.AddCookie(&http.Cookie{Name: "carbon_session", Value: "test-browser-token"})
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Origin", "http://localhost:8765")
	post.Header.Set("X-CSRF-Token", csrf.Token)
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
	select {
	case modelRequest := <-modelRequests:
		encoded, _ := json.Marshal(modelRequest.Messages)
		if !strings.Contains(string(encoded), "hello from browser") {
			t.Fatalf("browser's first model request = %s", encoded)
		}
	case <-time.After(5 * time.Second):
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
	reopened, err := carbon.OpenServeStorage(ctx, carbon.Config{HomeDir: homeDir}, carbon.ServeStorageConfig{DataDir: dataDir, DefaultTenant: "local"},
		carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
			return &scriptedClient{fn: func(int, inference.Request) []content.Chunk { return nil }}, func() model.Model { return testServeModel() }, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	reopenedReader, err := carbon.NewServeSessionReader(reopened.ControlStore(), reopened.Launcher(), "carbon-local-v1", "v1")
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
