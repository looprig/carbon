package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/credentials"
	credentialcatalog "github.com/looprig/credentials/catalog"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm"
	anthropicsubscription "github.com/looprig/llm/providers/anthropic/subscription"
	openaisubscription "github.com/looprig/llm/providers/openai/subscription"
	"github.com/looprig/secrets"
	secretslocal "github.com/looprig/secrets/local"
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
	_, err = credentialClientFor(context.Background(), runtime, selected, modelClientInput{CredentialRef: ref})
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

func TestCredentialRegistrySharesRuntimeAndExportedLogoutDrainsSession(t *testing.T) {
	home := t.TempDir()
	first, runtime, err := acquireCredentialRuntime(home)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, sameRuntime, err := acquireCredentialRuntime(home)
	if err != nil {
		first.Release()
		t.Fatalf("second acquire: %v", err)
	}
	if runtime != sameRuntime {
		t.Fatal("registry returned different runtime for one canonical home")
	}
	ref, err := credentials.ParseReference("credential://openai/registry")
	if err != nil {
		t.Fatal(err)
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
	seedCredential(t, runtime, ref, descriptor, "sk-registry-secret")
	if _, err := runtime.sourceFor(context.Background(), selected, ref); err != nil {
		t.Fatalf("sourceFor: %v", err)
	}
	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession: %v", err)
	}
	logoutDone := make(chan struct {
		out CredentialLogoutOutcome
		err error
	}, 1)
	go func() {
		out, logoutErr := LogoutCredential(context.Background(), Config{HomeDir: home}, ref.String())
		logoutDone <- struct {
			out CredentialLogoutOutcome
			err error
		}{out: out, err: logoutErr}
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
			t.Fatal("exported logout did not publish its blocked state")
		}
	}
	if err := runtime.beginSession(); !errors.Is(err, ErrCredentialLogoutBlocked) {
		t.Fatalf("new composition during logout = %v, want logout-blocked", err)
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); err != nil {
		t.Fatalf("catalog record disappeared before active session drained: %v", err)
	}
	runtime.endSession()
	select {
	case result := <-logoutDone:
		if result.err != nil {
			t.Fatalf("exported logout: %v", result.err)
		}
		if !result.out.LocalCatalogDeleted || !result.out.LocalStateDeleted || result.out.RemoteRevocationAttempted {
			t.Fatalf("logout outcome = %+v", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exported logout did not complete after session drain")
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); !errors.Is(err, credentials.ErrCatalogNotFound) {
		t.Fatalf("catalog after logout = %v, want not-found", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	closed, _ := runtime.lifecycleState()
	if !closed {
		t.Fatal("last registry release did not close the shared runtime")
	}
	third, freshRuntime, err := acquireCredentialRuntime(home)
	if err != nil {
		t.Fatalf("fresh acquire after close: %v", err)
	}
	if freshRuntime == runtime {
		t.Fatal("registry reused a runtime after its last borrow released")
	}
	if err := third.Release(); err != nil {
		t.Fatalf("fresh release: %v", err)
	}
}

func TestCredentialRegistryLastReleaseWaitsForFreshRuntime(t *testing.T) {
	home := t.TempDir()
	lease, runtime, err := acquireCredentialRuntime(home)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := runtime.beginSession(); err != nil {
		_ = lease.Release()
		t.Fatalf("beginSession: %v", err)
	}
	defer runtime.endSession()

	releaseDone := make(chan error, 1)
	go func() { releaseDone <- lease.Release() }()

	// The active session keeps runtime.Close paused after the registry has
	// marked the last entry closing. This gives the competing acquire a
	// deterministic point at which it must wait for the fresh runtime.
	deadline := time.Now().Add(2 * time.Second)
	for {
		processCredentialRegistry.mu.Lock()
		entry := processCredentialRegistry.entries[home]
		closing := false
		if entry != nil {
			closing = entry.closing
		}
		processCredentialRegistry.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("last release did not mark its registry entry closing")
		}
		time.Sleep(time.Millisecond)
	}

	acquireDone := make(chan struct {
		lease   *credentialRegistryLease
		runtime *credentialRuntime
		err     error
	}, 1)
	go func() {
		freshLease, freshRuntime, acquireErr := acquireCredentialRuntime(home)
		acquireDone <- struct {
			lease   *credentialRegistryLease
			runtime *credentialRuntime
			err     error
		}{lease: freshLease, runtime: freshRuntime, err: acquireErr}
	}()
	select {
	case result := <-acquireDone:
		if result.lease != nil {
			_ = result.lease.Release()
		}
		t.Fatalf("acquire completed while last runtime was closing: runtime=%p err=%v", result.runtime, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	runtime.endSession()
	select {
	case err := <-releaseDone:
		if err != nil {
			t.Fatalf("last release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("last release did not finish after session drain")
	}
	select {
	case result := <-acquireDone:
		if result.err != nil {
			t.Fatalf("fresh acquire: %v", result.err)
		}
		if result.runtime == runtime {
			_ = result.lease.Release()
			t.Fatal("acquire reused runtime after its last borrow closed")
		}
		if closed, closing := result.runtime.lifecycleState(); closed || closing {
			_ = result.lease.Release()
			t.Fatalf("fresh acquire returned unusable runtime: closed=%v closing=%v", closed, closing)
		}
		if err := result.lease.Release(); err != nil {
			t.Fatalf("fresh release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not resume after runtime close")
	}
}

func TestCredentialLogoutRejectsStateOutsideCredentialNamespace(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/namespace-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := credentialTestDescriptor(t)
	state, err := secrets.NewReference("local", "other/namespace-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, state, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}

	outcome, logoutErr := runtime.logout(context.Background(), ref)
	if logoutErr == nil {
		t.Fatal("logout error = nil, want state namespace failure")
	}
	if outcome.LocalCatalogDeleted || outcome.LocalStateDeleted || outcome.LocalDeleted {
		t.Fatalf("logout outcome = %+v, want no deletion before namespace validation", outcome)
	}
	var bounded *CredentialLogoutError
	if !errors.As(logoutErr, &bounded) || !bounded.State {
		t.Fatalf("logout error = %T %v, want bounded state failure", logoutErr, logoutErr)
	}
	if !errors.Is(logoutErr, credentials.ErrStateNamespace) {
		t.Fatalf("logout error = %v, want ErrStateNamespace", logoutErr)
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); err != nil {
		t.Fatalf("catalog record after namespace rejection = %v, want retained", err)
	}
}

func TestCredentialLogoutReportsMissingStateWithoutClaimingDeletion(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("newCredentialRuntime: %v", err)
	}
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/missing-state")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := credentialTestDescriptor(t)
	state, err := secrets.NewReference("local", "credentials/openai/missing-state")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, state, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}

	outcome, logoutErr := runtime.logout(context.Background(), ref)
	if logoutErr == nil {
		t.Fatal("logout error = nil, want incomplete state deletion")
	}
	if !outcome.LocalCatalogDeleted || outcome.LocalStateDeleted || outcome.LocalDeleted {
		t.Fatalf("logout outcome = %+v, want catalog deleted and state incomplete", outcome)
	}
	var bounded *CredentialLogoutError
	if !errors.As(logoutErr, &bounded) || !bounded.State {
		t.Fatalf("logout error = %T %v, want bounded state failure", logoutErr, logoutErr)
	}
	if !errors.Is(logoutErr, secrets.ErrNotFound) {
		t.Fatalf("logout error = %v, want secrets.ErrNotFound", logoutErr)
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); !errors.Is(err, credentials.ErrCatalogNotFound) {
		t.Fatalf("catalog after missing-state logout = %v, want not-found", err)
	}
}

func TestCredentialLogoutCASProtectsStateChangedAfterResolve(t *testing.T) {
	home := t.TempDir()
	stateRoot := filepath.Join(home, "state")
	catalogRoot := filepath.Join(home, "catalog")
	mutator, err := secretslocal.New(stateRoot)
	if err != nil {
		t.Fatalf("mutator store: %v", err)
	}
	defer mutator.Close()
	var stateRef secrets.Reference
	catalog, err := credentialcatalog.NewWithOptions(catalogRoot, credentialcatalog.Options{Hooks: credentialcatalog.Hooks{
		BeforeUnlink: func() error {
			value, valueErr := secrets.New([]byte("rotated-state"))
			if valueErr != nil {
				return valueErr
			}
			_, putErr := mutator.Put(context.Background(), stateRef, value, secrets.UnconditionalPut())
			return putErr
		},
	}})
	if err != nil {
		t.Fatalf("hooked catalog: %v", err)
	}
	runtime := newCredentialRuntimeWithStores(t, catalog, mustSecretStore(t, stateRoot))
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/changed-state")
	if err != nil {
		t.Fatal(err)
	}
	stateRef, err = secrets.NewReference("local", "credentials/openai/changed-state")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := credentialTestDescriptor(t)
	state, err := secrets.New([]byte("initial-state"))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := runtime.store.Put(context.Background(), stateRef, state, secrets.UnconditionalPut())
	if err != nil {
		t.Fatalf("initial state: %v", err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, stateRef, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}

	outcome, logoutErr := runtime.logout(context.Background(), ref)
	if logoutErr == nil {
		t.Fatal("logout error = nil, want CAS conflict")
	}
	if !outcome.LocalCatalogDeleted || outcome.LocalStateDeleted || outcome.LocalDeleted {
		t.Fatalf("logout outcome = %+v, want catalog deleted and changed state retained", outcome)
	}
	var bounded *CredentialLogoutError
	if !errors.As(logoutErr, &bounded) || !bounded.State {
		t.Fatalf("logout error = %T %v, want bounded state failure", logoutErr, logoutErr)
	}
	if !errors.Is(logoutErr, secrets.ErrConflict) {
		t.Fatalf("logout error = %v, want secrets.ErrConflict", logoutErr)
	}
	current, err := runtime.store.Resolve(context.Background(), stateRef)
	if err != nil {
		t.Fatalf("state after CAS conflict: %v", err)
	}
	if current.Version == initial.Version || string(current.Value.Bytes()) != "rotated-state" {
		t.Fatalf("state after CAS conflict = version %v value %q, want rotated state", current.Version, current.Value.Bytes())
	}
}

func TestCredentialLogoutStateDeletionErrorDoesNotClaimStateAfterCatalogDurabilityWarning(t *testing.T) {
	home := t.TempDir()
	stateRoot := filepath.Join(home, "state")
	catalogRoot := filepath.Join(home, "catalog")
	mutator, err := secretslocal.New(stateRoot)
	if err != nil {
		t.Fatalf("mutator store: %v", err)
	}
	defer mutator.Close()
	var stateRef secrets.Reference
	var catalogDeleteStarted atomic.Bool
	catalog, err := credentialcatalog.NewWithOptions(catalogRoot, credentialcatalog.Options{Hooks: credentialcatalog.Hooks{
		BeforeUnlink: func() error {
			value, valueErr := secrets.New([]byte("rotated-state"))
			if valueErr != nil {
				return valueErr
			}
			if _, putErr := mutator.Put(context.Background(), stateRef, value, secrets.UnconditionalPut()); putErr != nil {
				return putErr
			}
			catalogDeleteStarted.Store(true)
			return nil
		},
		SyncDir: func() error {
			if catalogDeleteStarted.Load() {
				return errors.New("test catalog durability failure")
			}
			return nil
		},
	}})
	if err != nil {
		t.Fatalf("hooked catalog: %v", err)
	}
	runtime := newCredentialRuntimeWithStores(t, catalog, mustSecretStore(t, stateRoot))
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/catalog-warning-conflict")
	if err != nil {
		t.Fatal(err)
	}
	stateRef, err = secrets.NewReference("local", "credentials/openai/catalog-warning-conflict")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := credentialTestDescriptor(t)
	state, err := secrets.New([]byte("initial-state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.Put(context.Background(), stateRef, state, secrets.UnconditionalPut()); err != nil {
		t.Fatalf("initial state: %v", err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, stateRef, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}

	outcome, logoutErr := runtime.logout(context.Background(), ref)
	if logoutErr == nil {
		t.Fatal("logout error = nil, want state deletion error")
	}
	if !outcome.LocalCatalogDeleted || outcome.LocalStateDeleted || outcome.LocalDeleted {
		t.Fatalf("logout outcome = %+v, want catalog-only deletion", outcome)
	}
	var stateDeletionErr *credentials.StateDeletionError
	if !errors.As(logoutErr, &stateDeletionErr) {
		t.Fatalf("logout error = %T %v, want StateDeletionError cause", logoutErr, logoutErr)
	}
	var bounded *CredentialLogoutError
	if !errors.As(logoutErr, &bounded) || !bounded.State || bounded.Catalog || bounded.Warning {
		t.Fatalf("logout error = %T %+v, want state failure without catalog/warning flags", logoutErr, bounded)
	}
	if !errors.Is(logoutErr, credentials.ErrCatalogDurabilityUnknown) || !errors.Is(logoutErr, secrets.ErrConflict) {
		t.Fatalf("logout error = %v, want catalog durability and state conflict causes", logoutErr)
	}
}

func TestCredentialLogoutCanceledStateDeletionRetainsCatalogOutcome(t *testing.T) {
	home := t.TempDir()
	stateRoot := filepath.Join(home, "state")
	catalogRoot := filepath.Join(home, "catalog")
	store := mustSecretStore(t, stateRoot)
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var armed atomic.Bool
	catalog, err := credentialcatalog.NewWithOptions(catalogRoot, credentialcatalog.Options{Hooks: credentialcatalog.Hooks{
		AfterRename: func() error {
			if armed.Load() {
				cancel()
			}
			return nil
		},
	}})
	if err != nil {
		t.Fatalf("hooked catalog: %v", err)
	}
	runtime := newCredentialRuntimeWithStores(t, catalog, store)
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/canceled-state")
	if err != nil {
		t.Fatal(err)
	}
	stateRef, err := secrets.NewReference("local", "credentials/openai/canceled-state")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := credentialTestDescriptor(t)
	state, err := secrets.New([]byte("state-to-retain"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.Put(context.Background(), stateRef, state, secrets.UnconditionalPut()); err != nil {
		t.Fatalf("initial state: %v", err)
	}
	now := time.Now().UTC()
	record, err := credentials.NewRecord(ref, descriptor, stateRef, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.catalog.Create(context.Background(), record); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	armed.Store(true)

	outcome, logoutErr := runtime.logout(ctx, ref)
	if logoutErr == nil {
		t.Fatal("logout error = nil, want canceled state deletion")
	}
	if !outcome.LocalCatalogDeleted || outcome.LocalStateDeleted || outcome.LocalDeleted {
		t.Fatalf("logout outcome = %+v, want catalog-only deletion", outcome)
	}
	var stateDeletionErr *credentials.StateDeletionError
	if !errors.As(logoutErr, &stateDeletionErr) {
		t.Fatalf("logout error = %T %v, want StateDeletionError cause", logoutErr, logoutErr)
	}
	var bounded *CredentialLogoutError
	if !errors.As(logoutErr, &bounded) || !bounded.Canceled || !bounded.State || bounded.Catalog {
		t.Fatalf("logout error = %T %+v, want canceled state failure", logoutErr, bounded)
	}
	if !errors.Is(logoutErr, context.Canceled) {
		t.Fatalf("logout error = %v, want context.Canceled", logoutErr)
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); !errors.Is(err, credentials.ErrCatalogNotFound) {
		t.Fatalf("catalog after canceled state deletion = %v, want not-found", err)
	}
	if _, err := runtime.store.Resolve(context.Background(), stateRef); err != nil {
		t.Fatalf("state after canceled deletion = %v, want retained", err)
	}
}

func credentialTestDescriptor(t *testing.T) credentials.Descriptor {
	t.Helper()
	policy, err := llm.AuthPolicyForModel(credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test"))
	if err != nil {
		t.Fatalf("auth policy: %v", err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	return descriptor
}

func mustSecretStore(t *testing.T, root string) *secretslocal.Store {
	t.Helper()
	store, err := secretslocal.New(root)
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	return store
}

func newCredentialRuntimeWithStores(t *testing.T, catalog *credentialcatalog.Local, store *secretslocal.Store) *credentialRuntime {
	t.Helper()
	namespace, err := secrets.NewNamespace("local", "credentials")
	if err != nil {
		t.Fatalf("credential namespace: %v", err)
	}
	closed := func() chan struct{} {
		done := make(chan struct{})
		close(done)
		return done
	}
	return &credentialRuntime{
		store: store, catalog: catalog, namespace: namespace,
		sources:    make(map[credentials.Reference]credentials.Source),
		refs:       make(map[credentials.Reference]struct{}),
		active:     make(map[credentials.Reference]int),
		blocked:    make(map[credentials.Reference]bool),
		activeDone: closed(), opDone: closed(),
		builder: credentials.Builder{
			Catalog: catalog, Store: store, StateNamespace: namespace,
			Providers: credentials.NewProviderFactories(nil),
		},
	}
}

func TestExportedLogoutSharesActualProductionSessionLifecycle(t *testing.T) {
	home := t.TempDir()
	configJSON := strings.NewReplacer(
		`"version": 2`, `"version": 3`,
		`"provider": "lmstudio"`, `"provider": "openai"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
		`"api_key": ""`, `"credential_ref": "credential://openai/production"`,
	).Replace(validLMStudioModelConfig)
	path, err := defaultModelConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	writeModelConfigFixture(t, path, []byte(configJSON), 0o600)
	seedLease, runtime, err := acquireCredentialRuntime(home)
	if err != nil {
		t.Fatalf("seed acquire: %v", err)
	}
	defer seedLease.Release()
	decoded, err := decodeModelConfig([]byte(configJSON))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := llm.AuthPolicyForModel(normalized.Models[0].Model)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := credentials.ParseReference("credential://openai/production")
	if err != nil {
		t.Fatal(err)
	}
	seedCredential(t, runtime, ref, descriptor, "sk-production-secret")
	factory, err := NewSessionStoreFactory(filepath.Join(home, "store"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, err := factory.Open(ctx, SessionSelector{}, Config{HomeDir: home})
	if err != nil {
		_ = factory.Close()
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "listen tcp") {
			t.Skipf("sandbox cannot open production session: %v", err)
		}
		t.Fatalf("production session Open: %v", err)
	}
	logoutDone := make(chan struct {
		out CredentialLogoutOutcome
		err error
	}, 1)
	go func() {
		out, logoutErr := LogoutCredential(ctx, Config{HomeDir: home}, ref.String())
		logoutDone <- struct {
			out CredentialLogoutOutcome
			err error
		}{out: out, err: logoutErr}
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
			_ = agent.Close(context.Background())
			_ = factory.Close()
			t.Fatal("production logout did not block the active session")
		}
	}
	if _, err := factory.Open(ctx, SessionSelector{}, Config{HomeDir: home}); !errors.Is(err, ErrCredentialLogoutBlocked) {
		t.Fatalf("new production Open during logout = %v, want logout-blocked", err)
	}
	if _, err := runtime.catalog.Get(context.Background(), ref); err != nil {
		t.Fatalf("credential deleted before production session drained: %v", err)
	}
	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("production agent Close: %v", err)
	}
	select {
	case result := <-logoutDone:
		if result.err != nil {
			t.Fatalf("production exported logout: %v", result.err)
		}
		if !result.out.LocalDeleted || result.out.RemoteRevocationAttempted {
			t.Fatalf("production logout outcome = %+v", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("production logout did not finish after session close")
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("factory Close: %v", err)
	}
}

func TestCredentialClientForHonorsCanceledContext(t *testing.T) {
	runtime, err := newCredentialRuntime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ref, err := credentials.ParseReference("credential://openai/canceled")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = credentialClientFor(ctx, runtime, credentialTestModel(llm.ProviderOpenAI, model.APIFormatOpenAIResponses, "gpt-test"), modelClientInput{CredentialRef: ref})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("credentialClientFor canceled context = %v, want context.Canceled", err)
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
