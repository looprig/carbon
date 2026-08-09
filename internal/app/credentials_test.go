package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/looprig/credentials"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm"
	anthropicsubscription "github.com/looprig/llm/providers/anthropic/subscription"
	openaisubscription "github.com/looprig/llm/providers/openai/subscription"
	"github.com/looprig/secrets"
)

func credentialTestModel(provider llm.Provider, format model.APIFormat, name string) model.Model {
	return model.CustomModel(model.ProviderName(provider), format, "https://api.example.test/v1", name, model.WithTools())
}

func seedCredential(t *testing.T, runtime *credentialRuntime, ref credentials.Reference, descriptor credentials.Descriptor, value string) {
	t.Helper()
	state, err := secrets.NewReference("local", "credentials/"+ref.Provider()+"/"+ref.Name())
	if err != nil {
		t.Fatalf("state reference: %v", err)
	}
	secret, err := secrets.New([]byte(value))
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if _, err := runtime.store.Put(context.Background(), state, secret, secrets.UnconditionalPut()); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, state, now, now)
	if err != nil {
		t.Fatalf("credential record: %v", err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
}

func TestCredentialRuntimeBuildsOneSourcePerReference(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	selected := credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test")
	policy, err := llm.AuthPolicyForModel(selected)
	if err != nil || len(policy.Accepted) != 1 {
		t.Fatalf("auth policy: %v", err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	ref, err := credentials.ParseReference("credential://openai/personal")
	if err != nil {
		t.Fatalf("credential reference: %v", err)
	}
	seedCredential(t, runtime, ref, descriptor, "sk-test-secret")

	first, err := runtime.sourceFor(context.Background(), selected, ref)
	if err != nil {
		t.Fatalf("first sourceFor: %v", err)
	}
	second, err := runtime.sourceFor(context.Background(), selected, ref)
	if err != nil {
		t.Fatalf("second sourceFor: %v", err)
	}
	if first != second {
		t.Fatal("sourceFor returned different sources for one safe reference")
	}
	lease, err := first.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Authorizer().Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-secret" {
		t.Fatalf("authorization = %q, want explicit bearer header", got)
	}
	if got := first.Reference().String(); got != ref.String() {
		t.Fatalf("source reference = %q, want %q", got, ref.String())
	}

	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime.Close: %v", err)
	}
	if _, err := first.Acquire(context.Background()); !errors.Is(err, credentials.ErrSourceClosed) {
		t.Fatalf("Acquire after close = %v, want source-closed", err)
	}
	if strings.Contains(runtimeString(first), "sk-test-secret") {
		t.Fatal("source diagnostic exposed provider secret")
	}
}

func TestCredentialAuthorizerMatchesStaticProviderHeaders(t *testing.T) {
	secret, err := secrets.New([]byte("header-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		provider  string
		transport string
		header    string
		want      string
	}{
		{name: "openai bearer", provider: "openai", transport: "responses", header: "Authorization", want: "Bearer header-test-secret"},
		{name: "anthropic", provider: "anthropic", transport: "messages", header: "x-api-key", want: "header-test-secret"},
		{name: "azure", provider: "azure", transport: "responses", header: "api-key", want: "header-test-secret"},
		{name: "azure anthropic", provider: "azure-cognitive-services", transport: "anthropic", header: "x-api-key", want: "header-test-secret"},
		{name: "google", provider: "google", transport: "generate-content", header: "x-goog-api-key", want: "header-test-secret"},
		{name: "deepinfra anthropic", provider: "deepinfra", transport: "anthropic", header: "x-api-key", want: "header-test-secret"},
		{name: "deepinfra openai", provider: "deepinfra", transport: "openai", header: "Authorization", want: "Bearer header-test-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor, err := credentials.NewDescriptor(tt.provider, tt.transport, credentials.SchemeAPIKey, credentials.UsageMeteredAPI, "https://issuer.test", "https://audience.test", "")
			if err != nil {
				t.Fatal(err)
			}
			authorizer, err := credentialAuthorizer(descriptor, secret)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodGet, "https://provider.example.test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := authorizer.Authorize(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get(tt.header); got != tt.want {
				t.Fatalf("%s = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestProductionModelLoaderUsesCredentialReferenceFactory(t *testing.T) {
	home := t.TempDir()
	configJSON := strings.NewReplacer(
		`"version": 2`, `"version": 3`,
		`"provider": "lmstudio"`, `"provider": "openai"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
		`"api_key": ""`, `"credential_ref": "credential://openai/personal"`,
	).Replace(validLMStudioModelConfig)
	path, err := defaultModelConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	writeModelConfigFixture(t, path, []byte(configJSON), 0o600)

	seedRuntime, err := newCredentialRuntime(home)
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	decoded, err := decodeModelConfig([]byte(configJSON))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	selected := normalized.Models[0].Model
	policy, err := llm.AuthPolicyForModel(selected)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := credentials.ParseReference("credential://openai/personal")
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	seedCredential(t, seedRuntime, ref, descriptor, "sk-loader-secret")
	if err := seedRuntime.Close(); err != nil {
		t.Fatalf("close seed runtime: %v", err)
	}

	configured, err := loadProductionModels(home)
	if err != nil {
		var compositionErr *CredentialCompositionError
		if errors.As(err, &compositionErr) {
			t.Fatalf("loadProductionModels: %v (cause=%v)", err, compositionErr.Cause)
		}
		t.Fatalf("loadProductionModels: %v", err)
	}
	if configured.credentialRuntime == nil {
		t.Fatal("production model configuration did not retain credential runtime")
	}
	if len(configured.credentialRefs) != 1 || configured.credentialRefs[0] != ref {
		t.Fatalf("credential refs = %v, want one %s", configured.credentialRefs, ref)
	}
	if err := configured.credentialRuntime.Close(); err != nil {
		t.Fatalf("close production credential runtime: %v", err)
	}
}

func runtimeString(source credentials.Source) string {
	return source.Reference().String() + " " + source.Descriptor().Canonical()
}

func TestCredentialRuntimeMissingReferenceFailsBeforeClientConstruction(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	selected := credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test")
	ref, err := credentials.ParseReference("credential://openai/missing")
	if err != nil {
		t.Fatal(err)
	}
	_, err = credentialClientFor(runtime, selected, modelClientInput{CredentialRef: ref})
	if err == nil {
		t.Fatal("credentialClientFor(missing) error = nil")
	}
	var compositionErr *CredentialCompositionError
	if !errors.As(err, &compositionErr) {
		t.Fatalf("error type = %T, want CredentialCompositionError", err)
	}
	if strings.Contains(err.Error(), "sk-") || strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("composition error contains provider material: %v", err)
	}
}

func TestCredentialRuntimeDescriptorMismatchAndLogoutBlockNewSessions(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	responses := credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test")
	chat := credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAI, "gpt-test")
	policy, err := llm.AuthPolicyForModel(responses)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := credentials.ParseReference("credential://openai/personal")
	if err != nil {
		t.Fatal(err)
	}
	seedCredential(t, runtime, ref, descriptor, "sk-test-secret")
	if _, err := runtime.sourceFor(context.Background(), responses, ref); err != nil {
		t.Fatalf("sourceFor responses: %v", err)
	}
	if _, err := runtime.sourceFor(context.Background(), chat, ref); err == nil {
		t.Fatal("sourceFor(chat) error = nil for mismatched transport")
	}
	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession: %v", err)
	}
	logoutCtx := context.Background()
	logoutDone := make(chan error, 1)
	go func() {
		_, logoutErr := runtime.logout(logoutCtx, ref)
		logoutDone <- logoutErr
	}()
	// Logout marks the reference before waiting, so any new session is refused
	// while the existing one is still draining.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.mu.Lock()
		blocked := runtime.blocked[ref]
		runtime.mu.Unlock()
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("logout did not block the reference")
		}
	}
	if err := runtime.beginSession(); !errors.Is(err, ErrCredentialLogoutBlocked) {
		t.Fatalf("beginSession during logout = %v, want logout-blocked", err)
	}
	runtime.endSession()
	err = <-logoutDone
	if err != nil {
		t.Fatalf("drained logout error = %v", err)
	}
	// The reference remains unavailable after local logout; a new explicit
	// login/catalog composition is required to re-establish it.
	if err := runtime.beginSession(); err == nil {
		t.Fatal("beginSession after logout = nil, want lifecycle/blocked error")
	}
}

func TestCredentialRuntimeMissingLogoutDoesNotBlockSessions(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.logout(context.Background(), ref); err == nil {
		t.Fatal("logout missing reference error = nil")
	}
	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession after missing logout: %v", err)
	}
	runtime.endSession()
}

func TestCredentialRuntimeCloseAndLogoutRaceDrainsInDependencyOrder(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	selected := credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test")
	policy, err := llm.AuthPolicyForModel(selected)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := credentials.ParseReference("credential://openai/race")
	if err != nil {
		t.Fatal(err)
	}
	seedCredential(t, runtime, ref, descriptor, "sk-race-secret")
	if _, err := runtime.sourceFor(context.Background(), selected, ref); err != nil {
		t.Fatal(err)
	}
	if err := runtime.beginSession(); err != nil {
		t.Fatal(err)
	}
	logoutDone := make(chan error, 1)
	go func() {
		_, logoutErr := runtime.logout(context.Background(), ref)
		logoutDone <- logoutErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.mu.Lock()
		blocked := runtime.blocked[ref]
		runtime.mu.Unlock()
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("logout did not enter blocked state")
		}
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	runtime.endSession()
	select {
	case err := <-logoutDone:
		if err != nil {
			t.Fatalf("logout error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logout did not drain")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wait for logout operation")
	}
}

func TestCredentialRuntimeConcurrentCloseIsIdempotent(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- runtime.Close() }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent runtime.Close: %v", err)
		}
	}
}

func TestRuntimeAgentConcurrentCloseDrainsCredentialRuntimeOnce(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	agent := &RuntimeAgent{credentialRuntime: runtime}
	results := make(chan error, 8)
	for range 8 {
		go func() { results <- agent.Close(context.Background()) }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent agent.Close: %v", err)
		}
	}
}

func TestLoginCredentialCurrentGatesAreTypedAndDoNotOpenBrowser(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		err := LoginCredential(context.Background(), Config{HomeDir: t.TempDir()}, provider)
		if err == nil {
			t.Fatalf("LoginCredential(%q) error = nil", provider)
		}
		switch provider {
		case "openai":
			var gateErr *openaisubscription.UnsupportedRegistrationError
			if !errors.As(err, &gateErr) {
				t.Fatalf("LoginCredential(%q) error type = %T, want typed OpenAI gate", provider, err)
			}
		case "anthropic":
			var gateErr *anthropicsubscription.UnsupportedRegistrationError
			if !errors.As(err, &gateErr) {
				t.Fatalf("LoginCredential(%q) error type = %T, want typed Anthropic gate", provider, err)
			}
		}
	}
}
