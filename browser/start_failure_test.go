package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
	"github.com/looprig/sessionstore"
)

type startupProbeClient struct{}

func (startupProbeClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}
func (startupProbeClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return stream.NewStreamReader(func() (content.Chunk, error) { return nil, io.EOF }, nil), nil
}

type startupProbeVerifier struct{}

func (startupProbeVerifier) VerifyCredential(context.Context, identity.Credential) (identity.Claims, error) {
	return identity.Claims{Subject: "test", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func factoryStartFailureFixture(t *testing.T) Config {
	t.Helper()
	const tenant = sessionwire.TenantID("local")
	return Config{
		Runtime: carbon.Config{HomeDir: t.TempDir(), AccessProfile: "trusted"},
		Storage: StorageConfig{DataDir: t.TempDir(), DefaultTenant: tenant},
		Host: HostConfig{ListenAddress: "127.0.0.1:0", AuthToken: "host-token", StorageBindingID: "carbon-local-v1",
			Options: host.Options{HostID: "factory-start-failure", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
				Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
				RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
				CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
			Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
			Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
			CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second},
		Factory: FactoryConfig{DefaultTenant: tenant, StorageBindingID: "carbon-local-v1", BindingVersion: "v1", HostLinkToken: "host-token",
			ReplicaID: "factory-start-failure", CookieName: "browser_session", Verifier: startupProbeVerifier{}, Authorizer: factory.TenantAuthorizer{},
			CSRF: identity.CSRFConfig{SharedKey: bytes.Repeat([]byte{'k'}, identity.MinCSRFSharedKeyBytes), TokenTTL: time.Hour,
				TrustedOrigins: []string{"http://127.0.0.1"}}},
		Address: "127.0.0.1:0",
		ClientBuilder: func() (inference.Client, func() model.Model, error) {
			return startupProbeClient{}, func() model.Model {
				return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI, "http://localhost:1234/v1", "probe", model.WithTools(), model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
			}, nil
		},
	}
}

func TestFactoryStartFailureWithdrawsHostAndReleasesOwners(t *testing.T) {
	cfg := factoryStartFailureFixture(t)
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host.ListenAddress = port.Addr().String()
	port.Close()
	want := errors.New("injected Factory Start failure")
	key := sessionstore.HostTargetKey{AgentID: carbon.CarbonAgentID, RuntimeCompatibilityID: "", Placement: sessionwire.HostPlacementPooled}
	cfg.startFactory = func(_ context.Context, owner *Server) error {
		key.RuntimeCompatibilityID = string(owner.host.CompatibilityID())
		page, err := owner.storage.ControlStore().ListCompatibleHosts(context.Background(), sessionstore.ListCompatibleHostsRequest{Key: key})
		if err != nil || len(page.Hosts) != 1 {
			t.Fatalf("Host capacity before Factory Start = (%+v, %v)", page, err)
		}
		return want
	}
	s, err := Start(context.Background(), cfg)
	if s != nil || !errors.Is(err, want) {
		if s != nil {
			_ = s.Stop(context.Background())
		}
		t.Fatalf("Factory Start failure = (%v, %v)", s, err)
	}
	probe, err := net.Listen("tcp", cfg.Host.ListenAddress)
	if err != nil {
		t.Fatalf("HostLink listener orphaned: %v", err)
	}
	probe.Close()
	stores, err := carbon.OpenServeStorage(context.Background(), cfg.Runtime, cfg.Storage,
		carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
			client, build, err := cfg.ClientBuilder()
			return client, carbon.ModelFactory(build), err
		}))
	if err != nil {
		t.Fatalf("same storage root remains owned after failed Start: %v", err)
	}
	page, err := stores.ControlStore().ListCompatibleHosts(context.Background(), sessionstore.ListCompatibleHostsRequest{Key: key})
	if err != nil || len(page.Hosts) != 0 {
		t.Fatalf("Host capacity after failed Factory Start = (%+v, %v)", page, err)
	}
	if err := stores.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.startFactory = nil
	s, err = Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reopen same root: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reopened server: %v", err)
	}
}
