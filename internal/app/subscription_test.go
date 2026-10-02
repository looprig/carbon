package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/credentials"
	"github.com/looprig/credentials/refresh"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm/providers/openaisubscription"
	"github.com/looprig/secrets"
)

func fixtureSubscription(t *testing.T, host string) (refresh.State, openaisubscription.Registration) {
	t.Helper()
	r := openaisubscription.Registration{ClientID: "oaiapp_fixture", Subject: "fixture-subject", HostID: host, Scopes: "offline_access resource.invoke chatgpt.tokens.use.direct"}
	data, _ := json.Marshal(r)
	access, _ := secrets.New([]byte("fixture-access"))
	renewable, _ := secrets.New([]byte("fixture-refresh"))
	generation, _ := credentials.NewGeneration("fixture-generation")
	return refresh.State{Schema: 1, Generation: generation, AccessToken: access, RefreshToken: renewable, ExpiresAt: time.Now().Add(time.Hour), ProviderData: data, PersistAccessToken: true}, r
}

func TestCarbonSubscriptionLogoutSerializesOtherProcessReauthentication(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	r, err := newCredentialRuntime(home)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var registration openaisubscription.Registration
	login := func(ctx context.Context, o openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error) {
		state, account := fixtureSubscription(t, o.HostID)
		registration = account
		return state, account, nil
	}
	if err := loginSubscription(ctx, r, "", login); err != nil {
		t.Fatal(err)
	}
	ref, _ := credentials.ParseReference("credential://openai-subscription/" + registration.AccountName())
	entered, release := make(chan struct{}), make(chan struct{})
	r.httpClient = &http.Client{Transport: subscriptionRoundTrip(func(req *http.Request) (*http.Response, error) {
		body := `{"issuer":"https://auth.openai.com","revocation_endpoint":"https://auth.openai.com/api/accounts/oauth/revoke"}`
		if req.URL.String() != openaisubscription.DiscoveryURL {
			close(entered)
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			body = ""
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	done := make(chan error, 1)
	go func() { _, err := r.logout(ctx, ref); done <- err }()
	<-entered
	other, err := refresh.NewFileCoordinator(filepath.Join(home, "credentials", "catalog"))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer other.Close()
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := other.WithLock(bounded, ref, func(context.Context) error { t.Error("reauthentication entered during revocation"); return nil }); err == nil {
		t.Error("expected blocked credential lock")
	}
	queued := make(chan error, 1)
	go func() {
		queuedCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		queued <- other.WithLock(queuedCtx, ref, func(ctx context.Context) error {
			if _, err := r.catalog.Get(ctx, ref); !errors.Is(err, credentials.ErrCatalogNotFound) {
				return errors.New("credential lock released before catalog deletion")
			}
			stateRef, _ := secrets.NewReference("local", "credentials/openai-subscription/"+ref.Name())
			if _, err := r.store.Resolve(ctx, stateRef); !errors.Is(err, secrets.ErrNotFound) {
				return errors.New("credential lock released before state deletion")
			}
			return nil
		})
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	second, err := newCredentialRuntime(home)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := loginSubscription(ctx, second, ref.String(), login); err != nil {
		t.Fatal(err)
	}
	if _, err := second.catalog.Get(ctx, ref); err != nil {
		t.Fatal("subsequent reauthentication was deleted", err)
	}
}
func TestCarbonSubscriptionLoginComposesSource(t *testing.T) {
	ctx := context.Background()
	r, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var registration openaisubscription.Registration
	login := func(ctx context.Context, options openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error) {
		state, account := fixtureSubscription(t, options.HostID)
		registration = account
		return state, account, nil
	}
	if err := loginSubscription(ctx, r, "", login); err != nil {
		t.Fatal(err)
	}
	ref, _ := credentials.ParseReference("credential://openai-subscription/" + registration.AccountName())
	selected := model.CustomModel("openai-subscription", model.APIFormatOpenAIResponses, "", "gpt-test")
	source, err := r.sourceFor(ctx, selected, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	summaries, err := r.list(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].Scheme != "oauth" || summaries[0].Usage != "subscription" {
		t.Fatalf("wrong catalog: %v %v", summaries, err)
	}
	host := registration.HostID
	if err := loginSubscription(ctx, r, ref.String(), func(ctx context.Context, options openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error) {
		if options.HostID != host || options.Previous == nil || options.Previous.ClientID != registration.ClientID {
			t.Fatal("reauth lost account or host")
		}
		return refresh.State{}, openaisubscription.Registration{}, openaisubscription.ErrLogin
	}); !errors.Is(err, openaisubscription.ErrLogin) {
		t.Fatalf("wrong error: %v", err)
	}
	if _, err := source.Acquire(ctx); err != nil {
		t.Fatal("failed sign-in changed active source")
	}
}

func TestCarbonSubscriptionHostIdentitySurvivesRestart(t *testing.T) {
	home := t.TempDir()
	r, err := newCredentialRuntime(home)
	if err != nil {
		t.Fatal(err)
	}
	first, err := subscriptionHostID(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = newCredentialRuntime(home)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	second, err := subscriptionHostID(context.Background(), r)
	if err != nil || second != first {
		t.Fatalf("host ID rotated: %v", err)
	}
}

type subscriptionRoundTrip func(*http.Request) (*http.Response, error)

func TestSubscriptionBrowserRefusesOtherEndpoints(t *testing.T) {
	for _, endpoint := range []string{"--help", "file:///tmp/fixture", "https://other.example/api/accounts/authorize", "https://auth.openai.com/api/accounts/authorize#fragment", "https://user@auth.openai.com/api/accounts/authorize"} {
		if err := openSubscriptionBrowser(context.Background(), endpoint); !errors.Is(err, openaisubscription.ErrLogin) {
			t.Fatal("accepted untrusted browser endpoint")
		}
	}
}

func (f subscriptionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCarbonSubscriptionLogoutRevokesAndRetainsRegistration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "remote-failed"}[fail], func(t *testing.T) {
			ctx := context.Background()
			r, err := newCredentialRuntime(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			var registration openaisubscription.Registration
			if err := loginSubscription(ctx, r, "", func(ctx context.Context, o openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error) {
				state, account := fixtureSubscription(t, o.HostID)
				registration = account
				return state, account, nil
			}); err != nil {
				t.Fatal(err)
			}
			ref, _ := credentials.ParseReference("credential://openai-subscription/" + registration.AccountName())
			r.httpClient = &http.Client{Transport: subscriptionRoundTrip(func(req *http.Request) (*http.Response, error) {
				status := 200
				body := ""
				if req.URL.String() == openaisubscription.DiscoveryURL {
					body = `{"issuer":"https://auth.openai.com","revocation_endpoint":"https://auth.openai.com/api/accounts/oauth/revoke"}`
				} else {
					req.ParseForm()
					if req.Form.Get("token") != "fixture-refresh" {
						t.Fatal("wrong revocation token")
					}
					if fail {
						status = 500
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			outcome, err := r.logout(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.LocalDeleted || !outcome.RemoteRevocationAttempted || outcome.RemoteRevoked == fail || outcome.RemoteRevocationError != fail {
				t.Fatalf("wrong logout: %+v", outcome)
			}
			saved, err := savedSubscriptionRegistration(ctx, r, ref)
			if err != nil || saved.ClientID != registration.ClientID || saved.IDToken != "" {
				t.Fatalf("lost registration: %v", err)
			}
		})
	}
}

func TestProductionModelLoaderComposesSubscriptionCredential(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	seed, err := newCredentialRuntime(home)
	if err != nil {
		t.Fatal(err)
	}
	var registration openaisubscription.Registration
	if err := loginSubscription(ctx, seed, "", func(ctx context.Context, o openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error) {
		state, account := fixtureSubscription(t, o.HostID)
		registration = account
		return state, account, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	ref := "credential://openai-subscription/" + registration.AccountName()
	// The documented configuration: no base_url, no api_key, an account reference.
	configJSON := strings.NewReplacer(
		`"version": 2`, `"version": 3`,
		`"provider": "lmstudio"`, `"provider": "openai-subscription"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1",`, ``,
		`"api_key": ""`, `"credential_ref": "`+ref+`"`,
	).Replace(validLMStudioModelConfig)
	path, err := defaultModelConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	writeModelConfigFixture(t, path, []byte(configJSON), 0o600)
	configured, err := loadProductionModels(home)
	if err != nil {
		var compositionErr *CredentialCompositionError
		if errors.As(err, &compositionErr) {
			t.Fatalf("loadProductionModels: %v (cause=%v)", err, compositionErr.Cause)
		}
		t.Fatalf("loadProductionModels: %v", err)
	}
	defer configured.credentialRuntime.Close()
	if len(configured.credentialRefs) != 1 || configured.credentialRefs[0].String() != ref {
		t.Fatalf("credential refs = %v, want %s", configured.credentialRefs, ref)
	}

	for name, replacement := range map[string]*strings.Replacer{
		"api key":         strings.NewReplacer(`"credential_ref": "`+ref+`"`, `"api_key": "sk-static"`),
		"custom endpoint": strings.NewReplacer(`"provider": "openai-subscription",`, `"provider": "openai-subscription", "base_url": "https://chatgpt.com/backend-api/codex",`),
		"chat format":     strings.NewReplacer(`"api_format": "openai-responses"`, `"api_format": "openai"`),
	} {
		decoded, err := decodeModelConfig([]byte(replacement.Replace(configJSON)))
		if err == nil {
			_, err = normalizeModelConfig(decoded)
		}
		if err == nil {
			t.Errorf("%s: accepted invalid subscription model configuration", name)
		}
	}
}
