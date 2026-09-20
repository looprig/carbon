package localbrowser

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

type testVerifier struct{}

func (testVerifier) VerifyCredential(context.Context, identity.Credential) (identity.Claims, error) {
	return identity.Claims{}, identity.ErrUnauthenticated
}

func validInputs(t *testing.T) (Settings, Dependencies) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	return Settings{
			HomeDir: filepath.Join(t.TempDir(), "home"), DataDir: filepath.Join(t.TempDir(), "store"),
			Tenant: "local", PublicAddress: address, TrustedOrigin: "http://" + address,
			HostID: "local-host", HostGeneration: 1, ReplicaID: "local-factory",
			StorageBindingID: "local-store-v1", BindingVersion: "v1",
		}, Dependencies{
			Verifier: testVerifier{}, Authorizer: factory.TenantAuthorizer{},
			HostLinkToken: "injected-hostlink-secret", CSRFKey: []byte("0123456789abcdef0123456789abcdef"),
			ClientBuilder: func() (inference.Client, func() model.Model, error) { return nil, nil, errors.New("test only") },
		}
}

func TestNewConfigBindsLocalDiskAndFiniteBudgets(t *testing.T) {
	settings, deps := validInputs(t)
	cfg, err := NewConfig(settings, deps)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.DataDir != settings.DataDir || cfg.Storage.DefaultTenant != sessionwire.TenantID(settings.Tenant) {
		t.Fatalf("local storage = %+v", cfg.Storage)
	}
	if cfg.Host.ListenAddress != "127.0.0.1:0" || cfg.Host.Options.Capacity != 2 || cfg.Host.Options.CommandQueueSize != 16 {
		t.Fatalf("pooled Host bounds = %+v", cfg.Host)
	}
	if cfg.Factory.ClientLinkLimits.MaxConnections != 256 || cfg.Factory.ClientLinkLimits.PerConnectionQueueBytes != 64<<10 {
		t.Fatalf("ClientLink budgets = %+v", cfg.Factory.ClientLinkLimits)
	}
	if cfg.Factory.ClientLinkLimits.MaxConnections*cfg.Factory.ClientLinkLimits.PerConnectionQueueBytes != 16<<20 {
		t.Fatal("queue memory envelope changed")
	}
	if cfg.Factory.HostLinkToken != cfg.Host.AuthToken || cfg.Factory.StorageBindingID != cfg.Host.StorageBindingID {
		t.Fatal("Host and Factory link inputs differ")
	}
	if cfg.Factory.CSRF.TokenTTL != time.Hour || len(cfg.Factory.CSRF.TrustedOrigins) != 1 || cfg.Factory.CSRF.TrustedOrigins[0] != settings.TrustedOrigin {
		t.Fatalf("CSRF settings = %+v", cfg.Factory.CSRF)
	}
	if _, err := cfg.EffectiveShutdownPolicy(); err != nil {
		t.Fatalf("invalid shutdown policy: %v", err)
	}
}

func TestNewConfigRejectsMissingSecurityInputsAndPublicHost(t *testing.T) {
	settings, deps := validInputs(t)
	cases := []struct {
		name   string
		mutate func(*Settings, *Dependencies)
	}{
		{"verifier", func(_ *Settings, d *Dependencies) { d.Verifier = nil }},
		{"authorizer", func(_ *Settings, d *Dependencies) { d.Authorizer = nil }},
		{"csrf key", func(_ *Settings, d *Dependencies) { d.CSRFKey = nil }},
		{"hostlink token", func(_ *Settings, d *Dependencies) { d.HostLinkToken = "" }},
		{"model builder", func(_ *Settings, d *Dependencies) { d.ClientBuilder = nil }},
		{"public listener", func(s *Settings, _ *Dependencies) { s.PublicAddress = "0.0.0.0:8080" }},
		{"relative data dir", func(s *Settings, _ *Dependencies) { s.DataDir = "relative" }},
		{"missing origin", func(s *Settings, _ *Dependencies) { s.TrustedOrigin = "" }},
		{"different origin port", func(s *Settings, _ *Dependencies) { s.TrustedOrigin = "http://127.0.0.1:1" }},
		{"different loopback host", func(s *Settings, _ *Dependencies) {
			s.TrustedOrigin = "http://127.0.0.2" + strings.TrimPrefix(s.TrustedOrigin, "http://127.0.0.1")
		}},
		{"ephemeral public port", func(s *Settings, _ *Dependencies) { s.PublicAddress = "127.0.0.1:0" }},
		{"zero generation", func(s *Settings, _ *Dependencies) { s.HostGeneration = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d := settings, deps
			tc.mutate(&s, &d)
			if _, err := NewConfig(s, d); err == nil {
				t.Fatal("invalid local composition accepted")
			}
		})
	}
}

type idleClient struct{}

func (idleClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("no inference request expected")
}
func (idleClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("no inference request expected")
}

func TestConfigStartsAndStopsOneLocalBrowser(t *testing.T) {
	settings, deps := validInputs(t)
	deps.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		return idleClient{}, func() model.Model {
			return model.CustomModel("example", model.APIFormatOpenAI, "http://127.0.0.1:1234/v1", "example",
				model.WithTools(), model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
	}
	cfg, err := NewConfig(settings, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server, err := browser.Start(ctx, cfg)
	if err != nil {
		if server != nil {
			_ = server.Stop(context.Background())
		}
		t.Fatalf("Start local composition: %v", err)
	}
	if server.Addr() == nil {
		t.Fatal("no public listener")
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("Stop local composition: %v", err)
	}
}
