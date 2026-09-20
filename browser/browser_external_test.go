package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	centrifugego "github.com/centrifugal/centrifuge-go"
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

func TestInvalidFactoryAuthorizerRefusesBeforeStorageOrRuntime(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Factory.Authorizer = nil
	calls := 0
	build := cfg.ClientBuilder
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		calls++
		return build()
	}
	s, err := browser.Start(context.Background(), cfg)
	if s != nil || err == nil || calls != 0 {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("invalid Factory Start = (%v, %v), runtime builds = %d", s, err, calls)
	}
}

func TestPublicBindFailureUnwindsPublishedHostAndStorage(t *testing.T) {
	cfg := browserFixture(t)
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	cfg.Address = public.Addr().String()
	internal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host.ListenAddress = internal.Addr().String()
	internal.Close()
	s, err := browser.Start(context.Background(), cfg)
	if err == nil {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatal("public bind unexpectedly succeeded")
	}
	if s != nil {
		t.Fatalf("public bind failure retained ownership: %v", err)
	}
	probe, err := net.Listen("tcp", cfg.Host.ListenAddress)
	if err != nil {
		t.Fatalf("internal Host listener orphaned: %v", err)
	}
	probe.Close()
	// The same provider root can be reopened after startup unwind.
	cfg.Address = "127.0.0.1:0"
	s, err = browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen after bind failure: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reopened owner: %v", err)
	}
}

func TestFactoryCompositionFailureUnwindsPublishedHostAndStorage(t *testing.T) {
	cfg := browserFixture(t)
	internal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host.ListenAddress = internal.Addr().String()
	internal.Close()
	cfg.Factory.HostLinkToken = "wrong-host-token"
	s, err := browser.Start(context.Background(), cfg)
	if err == nil || s != nil {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("Factory composition failure = (%v, %v)", s, err)
	}
	probe, err := net.Listen("tcp", cfg.Host.ListenAddress)
	if err != nil {
		t.Fatalf("Host listener orphaned: %v", err)
	}
	probe.Close()
	cfg.Factory.HostLinkToken = "host-token"
	s, err = browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen after Factory composition failure: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reopened owner: %v", err)
	}
}

func TestInternalBindFailureClosesStorageBeforeRetry(t *testing.T) {
	cfg := browserFixture(t)
	internal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host.ListenAddress = internal.Addr().String()
	s, err := browser.Start(context.Background(), cfg)
	if err == nil || s != nil {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("occupied internal bind = (%v, %v)", s, err)
	}
	internal.Close()
	s, err = browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen after internal bind failure: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reopened owner: %v (net.ErrClosed=%t)", err, errors.Is(err, net.ErrClosed))
	}
}

func TestCancelledStartupClosesStorageBeforeRetry(t *testing.T) {
	cfg := browserFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := browser.Start(ctx, cfg)
	if err == nil || s != nil {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("cancelled startup = (%v, %v)", s, err)
	}
	s, err = browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen after cancelled startup: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reopened owner: %v (net.ErrClosed=%t)", err, errors.Is(err, net.ErrClosed))
	}
}

type verifier struct{}

func (verifier) VerifyCredential(_ context.Context, credential identity.Credential) (identity.Claims, error) {
	if credential.Value() != "browser-test-token" {
		return identity.Claims{}, identity.ErrUnauthenticated
	}
	return identity.Claims{Subject: "browser-user", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type client struct {
	release  <-chan struct{}
	requests chan<- struct{}
	replies  *atomic.Int32
}

func (client) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}
func (c client) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	reply := "browser reply"
	if c.replies != nil {
		reply = fmt.Sprintf("browser reply %d", c.replies.Add(1))
	}
	if c.requests != nil {
		select {
		case c.requests <- struct{}{}:
		default:
		}
	}
	used := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if used {
			return nil, io.EOF
		}
		used = true
		if c.release != nil {
			<-c.release
		}
		return &content.TextChunk{Text: reply}, nil
	}, nil), nil
}

func browserFixture(t *testing.T) browser.Config {
	t.Helper()
	const tenant = sessionwire.TenantID("local")
	reconcile := factory.DefaultReconcileLimits()
	reconcile.Interval = time.Second
	clientLinks := factory.DefaultClientLinkLimits()
	clientLinks.DemandReleaseDebounce = time.Second
	cfg := browser.Config{
		Runtime: browser.RuntimeConfig{HomeDir: t.TempDir(), AccessProfile: "trusted"},
		ClientBuilder: func() (inference.Client, func() model.Model, error) {
			return client{}, func() model.Model {
				return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI, "http://localhost:1234/v1", "browser-test", model.WithTools(), model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
			}, nil
		},
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
			ReconcileLimits:  reconcile,
			ClientLinkLimits: clientLinks,
			CSRF: identity.CSRFConfig{SharedKey: bytes.Repeat([]byte{'k'}, identity.MinCSRFSharedKeyBytes), TokenTTL: time.Hour,
				TrustedOrigins: []string{"http://127.0.0.1"}}},
		Address: "127.0.0.1:0",
	}
	return cfg
}

func connectBrowserViewer(t *testing.T, base string) (*centrifugego.Client, <-chan sessionwire.EnduringPublication, <-chan sessionwire.SessionReset) {
	t.Helper()
	viewer := centrifugego.NewJsonClient("ws"+strings.TrimPrefix(base, "http")+"/v1/realtime", centrifugego.Config{
		Token: "browser-test-token", Data: []byte(`{"protocol_version":"1"}`),
		Header:           http.Header{"Authorization": {"Bearer browser-test-token"}, "Origin": {"http://127.0.0.1"}},
		HandshakeTimeout: 10 * time.Second, LogLevel: centrifugego.LogLevelNone,
	})
	connected := make(chan struct{}, 1)
	viewer.OnConnected(func(centrifugego.ConnectedEvent) {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	if err := viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not connect")
	}
	sub, err := viewer.NewSubscription("session:local:browser-session-1")
	if err != nil {
		t.Fatal(err)
	}
	subscribed := make(chan struct{}, 1)
	live := make(chan sessionwire.EnduringPublication, 8)
	resets := make(chan sessionwire.SessionReset, 8)
	sub.OnSubscribed(func(centrifugego.SubscribedEvent) {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})
	sub.OnPublication(func(event centrifugego.PublicationEvent) {
		var reset sessionwire.SessionReset
		if reset.UnmarshalJSON(event.Data) == nil {
			select {
			case resets <- reset:
			default:
			}
		}
		var publication sessionwire.EnduringPublication
		if publication.UnmarshalJSON(event.Data) == nil && strings.Contains(string(publication.Body), "browser reply") {
			select {
			case live <- publication:
			default:
			}
		}
	})
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not subscribe")
	}
	return viewer, live, resets
}

func TestExternalApplicationCanStartCreateAndStop(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Factory.ReconcileLimits.Interval = 25 * time.Millisecond
	release := make(chan struct{})
	modelRequests := make(chan struct{}, 4)
	replyCounter := &atomic.Int32{}
	runtimeBuilds := &atomic.Int32{}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		runtimeBuilds.Add(1)
		return client{release: release, requests: modelRequests, replies: replyCounter}, func() model.Model {
			return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
				"http://localhost:1234/v1", "browser-test", model.WithTools(),
				model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
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
	postInput := func(command string) {
		t.Helper()
		input := sessionwire.InputRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
			CommandID: sessionwire.CommandID(command)}, SessionID: "browser-session-1", Blocks: json.RawMessage(`[{"type":"text","text":"again"}]`)}
		inputBody, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		response := request(http.MethodPost, "/v1/sessions/browser-session-1/input", inputBody)
		message, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("input %s = %d %s", command, response.StatusCode, message)
		}
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
	viewer, live, resets := connectBrowserViewer(t, base)
	defer viewer.Close()
	releaseOnce.Do(func() { close(release) })
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
	select {
	case reset := <-resets:
		if reset.TenantID != "local" || reset.SessionID != "browser-session-1" || reset.JournalTip == 0 {
			t.Fatalf("live reset has wrong scope/tip: %+v", reset)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("viewer never received a reset covering the first output")
	}
	select {
	case <-modelRequests:
	default:
		t.Fatal("first input did not reach model")
	}
	postInput("browser-input-2")
	select {
	case <-modelRequests:
	case <-time.After(15 * time.Second):
		t.Fatal("second input never reached model")
	}
	var firstLive sessionwire.EnduringPublication
	select {
	case publication := <-live:
		firstLive = publication
		if publication.TenantID != "local" || publication.SessionID != "browser-session-1" || publication.JournalSeq == 0 {
			t.Fatalf("live output has wrong scope/sequence: %+v", publication)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("model output reached journal but not live browser viewer")
	}
	buildsBeforeDisconnect := runtimeBuilds.Load()
	viewer.Close()
	// Let Factory observe the lost viewer before the offline command. The
	// configured demand debounce is shorter than a production browser refresh.
	select {
	case <-time.After(cfg.Factory.ClientLinkLimits.DemandReleaseDebounce + cfg.Factory.ReconcileLimits.Interval):
	case <-s.Done():
		t.Fatal("server stopped while viewer was disconnected")
	}
	postInput("browser-input-3")
	select {
	case <-modelRequests:
	case <-time.After(15 * time.Second):
		t.Fatal("offline input never reached model")
	}
	var offlineTip uint64
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		page := request(http.MethodGet, "/v1/sessions/browser-session-1/journal", nil)
		body, _ := io.ReadAll(page.Body)
		page.Body.Close()
		var journal sessionwire.JournalPage
		if page.StatusCode == http.StatusOK && json.Unmarshal(body, &journal) == nil &&
			journal.CapturedTip > firstLive.JournalSeq && strings.Contains(string(body), "browser reply 3") {
			offlineTip = journal.CapturedTip
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if offlineTip == 0 {
		t.Fatal("offline model output was not replayable from the public journal")
	}
	if runtimeBuilds.Load() != buildsBeforeDisconnect {
		t.Fatalf("browser disconnect rebuilt runtime: before=%d after=%d", buildsBeforeDisconnect, runtimeBuilds.Load())
	}
	reconnected, continued, _ := connectBrowserViewer(t, base)
	defer reconnected.Close()
	// A refreshed viewer recovers the gap from Factory's durable journal
	// starting just after the last event it saw before disconnect.
	recovery := request(http.MethodGet, fmt.Sprintf("/v1/sessions/browser-session-1/journal?from_seq=%d&limit=100", firstLive.JournalSeq+1), nil)
	recoveredBody, _ := io.ReadAll(recovery.Body)
	recovery.Body.Close()
	if recovery.StatusCode != http.StatusOK || !strings.Contains(string(recoveredBody), "browser reply 3") {
		t.Fatalf("reconnected viewer could not recover offline output: %d %s", recovery.StatusCode, recoveredBody)
	}
	postInput("browser-input-4")
	select {
	case <-modelRequests:
	case <-time.After(15 * time.Second):
		t.Fatal("reconnected input never reached model")
	}
	continuedDeadline := time.After(15 * time.Second)
	seenContinued := false
	for !seenContinued {
		select {
		case publication := <-continued:
			if publication.TenantID != "local" || publication.SessionID != "browser-session-1" {
				t.Fatalf("reconnected live output has wrong scope: %+v", publication)
			}
			if publication.JournalSeq > offlineTip {
				seenContinued = true
			}
		case <-continuedDeadline:
			t.Fatal("reconnected browser viewer missed output after offline journal tip")
		}
	}
	if runtimeBuilds.Load() != buildsBeforeDisconnect {
		t.Fatalf("browser reconnect rebuilt runtime: before=%d after=%d", buildsBeforeDisconnect, runtimeBuilds.Load())
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

func TestColdSessionReadsDoNotLaunchUntilExplicitInput(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Factory.ReconcileLimits.Interval = 25 * time.Millisecond
	builds := &atomic.Int32{}
	buildNotices := make(chan struct{}, 8)
	modelCalls := make(chan struct{}, 4)
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		builds.Add(1)
		select {
		case buildNotices <- struct{}{}:
		default:
		}
		return client{requests: modelCalls}, func() model.Model {
			return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
				"http://localhost:1234/v1", "browser-test", model.WithTools(),
				model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
	}
	request := func(base, method, path string, payload any) (int, []byte) {
		t.Helper()
		var body []byte
		if payload != nil {
			var err error
			body, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		r, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer browser-test-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	first, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + first.Addr().String()
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "cold-create-1"}, SessionID: "browser-session-1", AgentID: "carbon",
		Blocks: json.RawMessage(`[{"type":"text","text":"first"}]`)}
	if status, body := request(base, http.MethodPost, "/v1/sessions", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	select {
	case <-modelCalls:
	case <-time.After(15 * time.Second):
		t.Fatal("seed create did not reach model")
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Host.Generation++
	reopened, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen cold session: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Stop(context.Background()) })
	base = "http://" + reopened.Addr().String()
	buildsBeforeReads := builds.Load()
	for len(buildNotices) > 0 {
		<-buildNotices
	}
	for _, path := range []string{"/v1/sessions", "/v1/sessions/browser-session-1/status", "/v1/sessions/browser-session-1/journal"} {
		status, body := request(base, http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("cold GET %s = %d %s", path, status, body)
		}
		if path == "/v1/sessions" && !strings.Contains(string(body), "browser-session-1") {
			t.Fatalf("cold list omitted seeded session: %s", body)
		}
		if strings.HasSuffix(path, "/journal") && !strings.Contains(string(body), "browser reply") {
			t.Fatalf("cold journal omitted seeded history: %s", body)
		}
		if strings.HasSuffix(path, "/status") {
			var status sessionwire.SessionStatus
			if err := json.Unmarshal(body, &status); err != nil || status.Residency != sessionwire.SessionResidencyCold {
				t.Fatalf("status after reopen = %+v (%s), decode %v; want cold", status, body, err)
			}
		}
	}
	viewer, _, _ := connectBrowserViewer(t, base)
	// Factory exposes no sweep-complete hook. Observe beyond two nominal
	// 16-shard rounds at this fixture's 25ms cadence while the viewer is live.
	// This bounds delayed placement; it does not claim every sweep completed.
	observation := time.NewTimer(1200 * time.Millisecond)
	defer observation.Stop()
	select {
	case <-buildNotices:
		t.Fatal("cold reads or subscription launched a runtime during observation")
	case <-modelCalls:
		t.Fatal("cold reads or subscription reached model during observation")
	case <-observation.C:
	}
	viewer.Close()
	if got := builds.Load(); got != buildsBeforeReads {
		t.Fatalf("cold reads and subscription caused %d additional runtime builds", got-buildsBeforeReads)
	}
	select {
	case <-modelCalls:
		t.Fatal("cold reads reached model")
	default:
	}
	input := sessionwire.InputRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "cold-input-1"}, SessionID: "browser-session-1",
		Blocks: json.RawMessage(`[{"type":"text","text":"again"}]`)}
	if status, body := request(base, http.MethodPost, "/v1/sessions/browser-session-1/input", input); status != http.StatusOK {
		t.Fatalf("cold input = %d %s", status, body)
	}
	select {
	case <-modelCalls:
	case <-time.After(15 * time.Second):
		t.Fatal("explicit input did not start cold session")
	}
	if got := builds.Load(); got <= buildsBeforeReads {
		t.Fatal("explicit input reached model without constructing a runtime")
	}
}

func TestFactoryComposeFailureUnwindsStartedHost(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Factory.HostLinkToken = "mismatched-token"
	s, err := browser.Start(context.Background(), cfg)
	if err == nil || s != nil {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("Start with incompatible Factory = (%v, %v), want cleaned nil/error", s, err)
	}
}
