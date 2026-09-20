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
	release := make(chan struct{})
	modelRequests := make(chan struct{}, 4)
	replyCounter := &atomic.Int32{}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
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
	viewer.Close()
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
	reconnected, continued, _ := connectBrowserViewer(t, base)
	defer reconnected.Close()
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
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(fmt.Errorf("Stop: %w", err))
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after Stop")
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
