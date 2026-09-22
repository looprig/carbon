package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory/identity"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm"
	"github.com/looprig/sessionstore"
)

// foreignTenantVerifier authenticates a principal of a tenant the local Host
// does not serve, beside the default-tenant fixture credential.
type foreignTenantVerifier struct{}

func (foreignTenantVerifier) VerifyCredential(ctx context.Context, c identity.Credential) (identity.Claims, error) {
	if c.Value() == "acme-token" {
		return identity.Claims{Tenant: "acme", Subject: "acme-user", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	return verifier{}.VerifyCredential(ctx, c)
}

// The local pooled Host serves only the default tenant, but Factory admits a
// create for whatever tenant the credential names. Before the tenant pin such a
// create was answered 201, durably recorded, and never placed: the HostLink
// dial for that tenant is refused, so the command sat pending until expiry.
func TestCreateForUnservedTenantIsRefusedBeforeAnyDurableWrite(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Factory.ReconcileLimits.Interval = 25 * time.Millisecond
	cfg.Factory.Verifier = foreignTenantVerifier{}
	calls := make(chan struct{}, 4)
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		return client{requests: calls}, func() model.Model {
			return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI, "http://localhost:1234/v1", "browser-test",
				model.WithTools(), model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
	}
	s, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = s.Stop(context.Background())
		}
	})
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "acme-create-1"},
		SessionID: "acme-session-1", AgentID: "carbon", Blocks: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
	body, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, "http://"+s.Addr().String()+"/v1/sessions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer acme-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(msg), "not_authorized") {
		t.Fatalf("create for unserved tenant = %d %s, want 403 not_authorized", resp.StatusCode, msg)
	}
	select {
	case <-calls:
		t.Fatal("refused create reached the model")
	default:
	}
	stopped = true
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stores, err := carbon.OpenServeStorage(context.Background(), cfg.Runtime, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close(context.Background())
	if entry, err := stores.ControlStore().GetDispositionCommand(context.Background(), sessionstore.GetDispositionCommandRequest{
		TenantID: "acme", SessionID: "acme-session-1", CommandID: "acme-create-1"}); err == nil {
		t.Fatalf("refused create was durably recorded: %+v", entry.Record.State)
	}
}
