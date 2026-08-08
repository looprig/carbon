package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

func TestACPPostureForAccessProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile AccessProfile
		posture driver.Posture
	}{
		{name: "empty defaults to readonly", profile: "", posture: driver.PostureReadOnly},
		{name: "readonly", profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "trusted", profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "unconfined", profile: AccessUnconfined, posture: driver.PostureWorkspaceWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := acpPostureFor(tt.profile)
			if err != nil {
				t.Fatalf("acpPostureFor(%q): %v", tt.profile, err)
			}
			if got != tt.posture {
				t.Fatalf("acpPostureFor(%q) = %q, want %q", tt.profile, got, tt.posture)
			}
		})
	}
	for _, profile := range []AccessProfile{"write", "unknown"} {
		if _, err := acpPostureFor(profile); err == nil {
			t.Fatalf("acpPostureFor(%q) succeeded; invalid profile must fail closed", profile)
		}
	}
}

func TestNewACPCompositionCapturesEffectiveACPPosture(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		selected    AccessProfile
		effective   AccessProfile
		wantPosture driver.Posture
	}{
		{name: "empty profile defaults", selected: "", effective: DefaultAccessProfile, wantPosture: driver.PostureReadOnly},
		{name: "readonly", selected: AccessReadOnly, effective: AccessReadOnly, wantPosture: driver.PostureReadOnly},
		{name: "trusted", selected: AccessTrusted, effective: AccessTrusted, wantPosture: driver.PostureWorkspaceWrite},
		{name: "unconfined", selected: AccessUnconfined, effective: AccessUnconfined, wantPosture: driver.PostureWorkspaceWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			composition, err := NewACPComposition(ACPChildrenConfig{AccessProfile: tt.selected})
			if err != nil {
				t.Fatalf("NewACPComposition(%q): %v", tt.selected, err)
			}
			if composition.accessProfile != tt.effective || composition.posture != tt.wantPosture {
				t.Fatalf("composition access=%q posture=%q, want access=%q posture=%q", composition.accessProfile, composition.posture, tt.effective, tt.wantPosture)
			}
		})
	}
}

// TestACPPostureMatrixThroughProductionBuilders drives the real composition
// registry and child factory for every launch/source/profile combination. The
// launched ACP peer asks for an edit at a path inside the configured workspace;
// the response is therefore an observation of the permission handler created by
// the actual acpdriver.Config, rather than a recorder or a manually built config.
func TestACPPostureMatrixThroughProductionBuilders(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		launchPath string
		harness    loop.AgentHarnessName
		credential loop.CredentialMode
		profile    AccessProfile
		posture    driver.Posture
	}{
		{name: "live/codex/gateway/readonly", launchPath: "live", harness: "codex", credential: loop.CredentialGatewayBacked, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "live/codex/gateway/trusted", launchPath: "live", harness: "codex", credential: loop.CredentialGatewayBacked, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "live/codex/native/readonly", launchPath: "live", harness: "codex", credential: loop.CredentialNativeAuth, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "live/codex/native/trusted", launchPath: "live", harness: "codex", credential: loop.CredentialNativeAuth, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "live/claude-code/gateway/readonly", launchPath: "live", harness: "claude-code", credential: loop.CredentialGatewayBacked, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "live/claude-code/gateway/trusted", launchPath: "live", harness: "claude-code", credential: loop.CredentialGatewayBacked, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "live/claude-code/native/readonly", launchPath: "live", harness: "claude-code", credential: loop.CredentialNativeAuth, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "live/claude-code/native/trusted", launchPath: "live", harness: "claude-code", credential: loop.CredentialNativeAuth, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "restored/codex/gateway/readonly", launchPath: "restored", harness: "codex", credential: loop.CredentialGatewayBacked, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "restored/codex/gateway/trusted", launchPath: "restored", harness: "codex", credential: loop.CredentialGatewayBacked, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "restored/codex/native/readonly", launchPath: "restored", harness: "codex", credential: loop.CredentialNativeAuth, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "restored/codex/native/trusted", launchPath: "restored", harness: "codex", credential: loop.CredentialNativeAuth, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "restored/claude-code/gateway/readonly", launchPath: "restored", harness: "claude-code", credential: loop.CredentialGatewayBacked, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "restored/claude-code/gateway/trusted", launchPath: "restored", harness: "claude-code", credential: loop.CredentialGatewayBacked, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
		{name: "restored/claude-code/native/readonly", launchPath: "restored", harness: "claude-code", credential: loop.CredentialNativeAuth, profile: AccessReadOnly, posture: driver.PostureReadOnly},
		{name: "restored/claude-code/native/trusted", launchPath: "restored", harness: "claude-code", credential: loop.CredentialNativeAuth, profile: AccessTrusted, posture: driver.PostureWorkspaceWrite},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			compiled := acpPostureMatrixCatalog(t, tt.harness, tt.credential)
			composition, err := NewACPComposition(ACPChildrenConfig{
				Catalog:             compiled,
				AccessProfile:       tt.profile,
				Executables:         map[loop.AgentHarnessName]string{tt.harness: executable},
				WorkspaceRoot:       workspace,
				Env:                 []string{"PATH=" + taskACPPostureHelperPath},
				NativeEnvAllowlist:  []string{"PATH"},
				GatewayEnvAllowlist: []string{"PATH"},
				gatewayPreflightBinding: &launch.ProxyBinding{
					BaseURL: "http://127.0.0.1:1",
					Token:   "posture-matrix-preflight",
				},
				executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
					return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
				},
			})
			if err != nil {
				t.Fatalf("NewACPComposition(): %v", err)
			}

			bound := acpPostureMatrixBound(t, composition.Catalog, tt.harness, tt.credential)
			idGen := func() (uuid.UUID, error) { return uuid.New() }
			fac := event.NewFactory(idGen, time.Now)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var backend loop.Backend
			if tt.launchPath == "live" {
				backend, _, err = composition.Live(
					ctx, mustUUID(t), mustUUID(t), loop.Provenance{}, acpPostureMatrixPublisher{}, bound, idGen, fac,
				)
			} else {
				backend, err = composition.Restored(
					ctx, mustUUID(t), mustUUID(t), loop.Provenance{}, acpPostureMatrixPublisher{}, bound, idGen, fac,
					foreign.RestoredForeign{ForeignSID: "posture-matrix-foreign", AgentSessionID: "posture-matrix-agent"},
				)
			}
			if err != nil {
				t.Fatalf("%s builder(): %v", tt.launchPath, err)
			}
			if backend == nil {
				t.Fatal("ACP builder returned nil backend")
			}
			t.Cleanup(func() {
				cancel()
				select {
				case <-backend.DoneChan():
				case <-time.After(5 * time.Second):
					t.Errorf("ACP backend did not close after context cancellation")
				}
			})

			select {
			case backend.CommandSink() <- command.UserInput{
				Header: command.Header{CommandID: mustUUID(t)},
				Blocks: []content.Block{&content.TextBlock{Text: "request an in-workspace edit"}},
			}:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out submitting ACP posture probe")
			}

			got := waitACPPostureReceipt(t, filepath.Join(workspace, acpPostureReceiptName))
			want := "reject-once"
			if tt.posture == driver.PostureWorkspaceWrite {
				want = "allow-once"
			}
			if got != want {
				t.Fatalf("in-workspace permission outcome = %q, want %q for posture %q", got, want, tt.posture)
			}
			writePath := filepath.Join(workspace, acpPostureWriteName)
			_, statErr := os.Stat(writePath)
			if tt.posture == driver.PostureWorkspaceWrite {
				if statErr != nil {
					t.Fatalf("trusted ACP child did not perform in-workspace write: %v", statErr)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatalf("readonly ACP child performed in-workspace write: %v", statErr)
			}
		})
	}
}

func acpPostureMatrixCatalog(t *testing.T, harness loop.AgentHarnessName, credential loop.CredentialMode) ACPCompiledCatalog {
	t.Helper()
	alias := loop.ModelAlias("posture-matrix-codex")
	if harness == "claude-code" {
		alias = "sonnet-5"
	}
	input := AgentRuntimeCatalogInput{}
	if credential == loop.CredentialGatewayBacked {
		input.GatewayTargets = []ACPGatewaySource{{
			Alias:         alias,
			Client:        &fakeLLM{},
			Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "posture-matrix", model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortMedium,
			Efforts:       []model.Effort{model.EffortMedium},
		}}
		if harness == "claude-code" {
			input.ClaudeSmall = alias
		}
	} else {
		input.NativeACP = map[string]ACPNativeProfile{string(harness): {
			Harness: harness,
			Enabled: true,
		}}
		input.PrimerTarget = runtimeCatalogPrimer()
	}
	compiled, err := CompileAgentRuntimeCatalog(input)
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog(): %v", err)
	}
	return compiled
}

func acpPostureMatrixBound(t *testing.T, catalog ACPCompiledCatalog, harness loop.AgentHarnessName, credential loop.CredentialMode) loop.BoundDefinition {
	t.Helper()
	definition, err := loop.Define(
		loop.WithName(generic.Name),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithSystem("acp posture matrix"),
		loop.WithPolicyRevision("acp-posture-matrix"),
	)
	if err != nil {
		t.Fatalf("loop.Define(): %v", err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatalf("definition.Bind(): %v", err)
	}
	source := loop.RuntimeSourceNative
	effort := model.EffortNone
	alias := loop.ModelAlias("")
	if credential == loop.CredentialGatewayBacked {
		source = loop.RuntimeSourceGateway
		effort = model.EffortMedium
		alias = "posture-matrix-codex"
		if harness == "claude-code" {
			alias = "sonnet-5"
		}
	}
	if credential == loop.CredentialNativeAuth {
		bound, err = loop.OverrideBoundRuntimeManaged(bound, loop.RuntimeProfileName("acp/"+string(harness)))
		if err != nil {
			t.Fatalf("OverrideBoundRuntimeManaged(): %v", err)
		}
		return bound
	}
	resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, harness, source, alias, effort, true)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource(): %v", err)
	}
	bound, err = loop.OverrideBoundRuntimeSelectionWithIdentity(
		bound, resolved.Profile, resolved.ModelAlias, resolved.Target, resolved.Effort, resolved.Source, resolved.SelectionKind,
	)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeSelectionWithIdentity(): %v", err)
	}
	return bound
}

type acpPostureMatrixPublisher struct{}

func (acpPostureMatrixPublisher) PublishEvent(context.Context, event.Event) error        { return nil }
func (acpPostureMatrixPublisher) PublishEventChecked(context.Context, event.Event) error { return nil }

func waitACPPostureReceipt(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if len(data) > 0 {
				return string(data)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read ACP posture receipt: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for ACP posture receipt %q", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewACPCompositionRejectsInvalidAccessProfileBeforePreflight(t *testing.T) {
	t.Parallel()
	preflightCalls := 0
	_, err := NewACPComposition(ACPChildrenConfig{
		AccessProfile: AccessProfile("invalid"),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			preflightCalls++
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != errACPAccessProfileUnavailable {
		t.Fatalf("NewACPComposition(invalid) error = %v, want bounded access-profile error", err)
	}
	if preflightCalls != 0 {
		t.Fatalf("invalid access profile invoked %d preflight calls, want zero", preflightCalls)
	}
}

func TestBoundedACPChildErrorDoesNotExposeProcessDetails(t *testing.T) {
	t.Parallel()
	cause := errors.New("stdio: process exited at /private/login/home; https://provider.invalid/token")
	got := boundedACPChildError(cause)
	if got.Error() != "coderig: ACP child unavailable" {
		t.Fatalf("bounded error = %q, want fixed category", got)
	}
	if strings.Contains(got.Error(), "/private/login/home") || strings.Contains(got.Error(), "provider.invalid") {
		t.Fatalf("bounded error leaked process details: %q", got)
	}
	if boundedACPChildError(context.Canceled) != context.Canceled {
		t.Fatal("context cancellation was not preserved")
	}
}

func TestBoundedACPChildErrorProjectsOnlyProtocolCodeAndMessage(t *testing.T) {
	t.Parallel()
	const (
		pathSentinel  = "/private/acp/login"
		urlSentinel   = "https://provider.invalid/token"
		tokenSentinel = "token=do-not-disclose"
		causeSentinel = "child stderr: " + pathSentinel + " " + urlSentinel + " " + tokenSentinel
	)
	cause := errors.New(causeSentinel)
	fault := protocol.InternalError("usage limit\nreached\t\x00", cause).WithData(map[string]string{
		"path": pathSentinel, "url": urlSentinel, "token": tokenSentinel,
	})
	got := boundedACPChildError(fmt.Errorf("acp child setup: %w", fault))
	var modelFacing interface{ ModelFacingError() string }
	if !errors.As(got, &modelFacing) || modelFacing == nil {
		t.Fatalf("bounded protocol error = %T %v, want ModelFacingError", got, got)
	}
	if got := modelFacing.ModelFacingError(); got != "ACP error -32603: usage limit reached" {
		t.Fatalf("ModelFacingError() = %q, want normalized code/message", got)
	}
	if got.Error() != "ACP error -32603: usage limit reached" {
		t.Fatalf("Error() = %q, want only bounded detail", got)
	}
	for _, sentinel := range []string{pathSentinel, urlSentinel, tokenSentinel, causeSentinel, "\n", "\r", "\t", "\x00"} {
		if strings.Contains(got.Error(), sentinel) {
			t.Fatalf("bounded protocol error leaked %q: %q", sentinel, got)
		}
	}
	var gotFault *protocol.Fault
	if errors.As(got, &gotFault) {
		t.Fatal("bounded protocol error retained protocol fault chain")
	}
	if errors.Is(got, cause) {
		t.Fatal("bounded protocol error retained unwrapped cause")
	}
}

func TestBoundedACPChildErrorAcceptsWireErrorAndBoundsMalformedMessage(t *testing.T) {
	t.Parallel()
	const maxBytes = 512
	message := strings.Repeat("界", 400) + "\xff\n\r\t" + strings.Repeat("x", 400)
	got := boundedACPChildError(&protocol.Error{Code: protocol.ErrorCodeAuthenticationRequired, Message: message})
	var modelFacing interface{ ModelFacingError() string }
	if !errors.As(got, &modelFacing) || modelFacing == nil {
		t.Fatalf("bounded wire error = %T %v, want ModelFacingError", got, got)
	}
	detail := modelFacing.ModelFacingError()
	if !strings.HasPrefix(detail, "ACP error -32000: ") {
		t.Fatalf("wire detail = %q, want ACP code prefix", detail)
	}
	if len(detail) > maxBytes {
		t.Fatalf("wire detail bytes = %d, want <= %d", len(detail), maxBytes)
	}
	if !utf8.ValidString(detail) {
		t.Fatal("wire detail is invalid UTF-8")
	}
	if strings.ContainsAny(detail, "\r\n\t\x00") {
		t.Fatalf("wire detail contains control characters: %q", detail)
	}
	var gotWire *protocol.Error
	if errors.As(got, &gotWire) {
		t.Fatal("bounded wire error retained protocol error chain")
	}
}

func TestBoundedACPChildErrorKeepsCancellationAndDeadlineIdentity(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		got := boundedACPChildError(fmt.Errorf("child setup: %w", sentinel))
		if !errors.Is(got, sentinel) {
			t.Fatalf("bounded %v = %v, errors.Is false", sentinel, got)
		}
		var modelFacing interface{ ModelFacingError() string }
		if errors.As(got, &modelFacing) {
			t.Fatalf("bounded %v unexpectedly implements ModelFacingError", sentinel)
		}
	}
}

func TestBoundedACPChildErrorKeepsArbitraryFailuresGeneric(t *testing.T) {
	t.Parallel()
	got := boundedACPChildError(errors.New("internal path=/private/acp token=secret"))
	if got != errACPChildUnavailable {
		t.Fatalf("bounded arbitrary error = %v, want fixed sentinel", got)
	}
	var modelFacing interface{ ModelFacingError() string }
	if errors.As(got, &modelFacing) {
		t.Fatal("generic ACP child error unexpectedly implements ModelFacingError")
	}
}

func TestACPChildConfigReceivesNativeResolvedEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		harness loop.AgentHarnessName
		alias   loop.ModelAlias
	}{
		{name: "codex", harness: "codex", alias: "native-codex"},
		{name: "claude", harness: "claude-code", alias: "native-claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
				NativeACP: map[string]ACPNativeProfile{string(tt.harness): {
					Harness: tt.harness,
					Enabled: true,
					ModelOptions: []ACPNativeModelOption{{
						Alias: tt.alias, Model: string(tt.alias),
						Efforts: []model.Effort{model.EffortHigh}, DefaultEffort: model.EffortHigh,
					}},
				}},
				PrimerTarget: runtimeCatalogPrimer(),
			})
			if err != nil {
				t.Fatalf("CompileAgentRuntimeCatalog(): %v", err)
			}
			definition, err := loop.Define(
				loop.WithName(generic.Name),
				loop.WithInference(&fakeLLM{}, testModel()),
				loop.WithPolicyRevision("acp-child-effort-test"),
			)
			if err != nil {
				t.Fatalf("loop.Define(): %v", err)
			}
			bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
			if err != nil {
				t.Fatalf("definition.Bind(): %v", err)
			}
			resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, tt.harness, loop.RuntimeSourceNative, tt.alias, model.EffortHigh, true)
			if err != nil {
				t.Fatalf("ResolveWithExplicitSource(): %v", err)
			}
			bound, err = loop.OverrideBoundRuntimeSelectionWithIdentity(bound, resolved.Profile, resolved.ModelAlias, resolved.Target, resolved.Effort, resolved.Source, resolved.SelectionKind)
			if err != nil {
				t.Fatalf("OverrideBoundRuntimeSelectionWithIdentity(): %v", err)
			}
			factory := &acpChildFactory{config: ACPChildrenConfig{Catalog: compiled, AccessProfile: AccessReadOnly, posture: driver.PostureReadOnly}}
			_, config, ownedGateway, err := factory.configFor(context.Background(), bound, "")
			if err != nil {
				t.Fatalf("configFor(): %v", err)
			}
			if ownedGateway != nil {
				t.Fatal("native config unexpectedly owns a gateway")
			}
			if config.Effort != string(model.EffortHigh) {
				t.Fatalf("%s config.Effort = %q, want %q", tt.harness, config.Effort, model.EffortHigh)
			}
		})
	}
}

func TestACPChildConfigKeepsHarnessManagedModelAndEffortEmpty(t *testing.T) {
	t.Parallel()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP:    map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog(): %v", err)
	}
	definition, err := loop.Define(loop.WithName(generic.Name), loop.WithInference(&fakeLLM{}, testModel()), loop.WithPolicyRevision("acp-child-managed-test"))
	if err != nil {
		t.Fatalf("loop.Define(): %v", err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatalf("definition.Bind(): %v", err)
	}
	bound, err = loop.OverrideBoundRuntimeManaged(bound, "acp/codex")
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeManaged(): %v", err)
	}
	factory := &acpChildFactory{config: ACPChildrenConfig{Catalog: compiled, AccessProfile: AccessReadOnly, posture: driver.PostureReadOnly}}
	_, config, ownedGateway, err := factory.configFor(context.Background(), bound, "")
	if err != nil {
		t.Fatalf("configFor(): %v", err)
	}
	if ownedGateway != nil {
		t.Fatal("harness-managed native config unexpectedly owns a gateway")
	}
	if config.ModelAlias != "" || config.Effort != "" {
		t.Fatalf("harness-managed config model/effort = %q/%q, want empty", config.ModelAlias, config.Effort)
	}
}

func TestNewACPCompositionChecksExecutablePathsAndFiltersEnv(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:                 compiled,
		Executables:             map[loop.AgentHarnessName]string{"claude-code": executable, "codex": "relative/codex"},
		WorkspaceRoot:           t.TempDir(),
		Env:                     []string{"PATH=/bin", "SECRET=must-not-pass", "LANG=C"},
		EnvAllowlist:            []string{"PATH", "LANG"},
		gatewayPreflightBinding: &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "test-token"},
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"fable-5", "sonnet-5", "opus-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("Claude profile missing: %v", err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("Codex profile registered despite failed static executable check")
	}
	if !composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("Claude profile disappeared from the catalog")
	}
	if composition.Catalog.HasProfile("acp/codex") {
		t.Fatalf("catalog still advertises the invalid Codex executable: %#v", composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name))
	}
	if got := filterACPEnv([]string{"PATH=/bin", "SECRET=x", "LANG=C"}, []string{"PATH", "LANG"}); len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "LANG=C" {
		t.Fatalf("filtered env = %#v", got)
	}
}

func TestNewACPCompositionDoesNotDiagnoseTransientModelReduction(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	before := 0
	for _, entry := range compiled.entries {
		if entry.AgentHarness == "claude-code" {
			before += len(entry.Models)
		}
	}
	if before < 2 {
		t.Fatalf("fixture does not configure enough claude-code models to exercise a partial reduction: %d", before)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:                 compiled,
		Executables:             map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot:           t.TempDir(),
		gatewayPreflightBinding: &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "test-token"},
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			// fable-5 is the deterministic first configured alias. Omitting
			// opus-5 keeps claude-code admitted while reducing its model set.
			return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"fable-5", "fable-5@high", "sonnet-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("claude-code profile should remain admitted after static checks")
	}
	after := 0
	for _, entry := range composition.Catalog.entries {
		if entry.AgentHarness == "claude-code" {
			after += len(entry.Models)
		}
	}
	if after != before {
		t.Fatalf("transient availability changed configured claude-code rows, before=%d after=%d", before, after)
	}
	found := false
	for _, line := range composition.Diagnostics {
		if strings.Contains(line, "claude-code:") && strings.Contains(line, "not advertised") {
			found = true
		}
	}
	if found {
		t.Fatalf("unexpected transient reduced-model diagnostic: %v", composition.Diagnostics)
	}
}

func TestACPChildEnvIsCredentialScoped(t *testing.T) {
	config := ACPChildrenConfig{
		Env: []string{
			"HOME=/Users/alice",
			"XDG_CONFIG_HOME=/Users/alice/.config",
			"XDG_DATA_HOME=/Users/alice/.local/share",
			"PATH=/usr/bin",
			"LANG=C",
			"ANTHROPIC_API_KEY=should-not-pass",
			"SECRET_TOKEN=should-not-pass",
		},
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	}

	native := config.envForCredential(loop.CredentialNativeAuth)
	if !containsEnv(native, "HOME=/Users/alice") ||
		!containsEnv(native, "XDG_CONFIG_HOME=/Users/alice/.config") ||
		!containsEnv(native, "XDG_DATA_HOME=/Users/alice/.local/share") {
		t.Fatalf("native env lost login locations: %#v", native)
	}
	if !containsEnv(native, "PATH=/usr/bin") || !containsEnv(native, "LANG=C") {
		t.Fatalf("native env lost safe process configuration: %#v", native)
	}

	gateway := config.envForCredential(loop.CredentialGatewayBacked)
	if containsEnvKey(gateway, "HOME") || containsEnvKey(gateway, "XDG_CONFIG_HOME") || containsEnvKey(gateway, "XDG_DATA_HOME") {
		t.Fatalf("gateway env inherited harness login locations: %#v", gateway)
	}
	if containsEnvKey(gateway, "ANTHROPIC_API_KEY") || containsEnvKey(gateway, "SECRET_TOKEN") {
		t.Fatalf("gateway env inherited a secret: %#v", gateway)
	}
	if !containsEnv(gateway, "PATH=/usr/bin") || !containsEnv(gateway, "LANG=C") {
		t.Fatalf("gateway env lost safe process configuration: %#v", gateway)
	}
}

func containsEnv(env []string, wanted string) bool {
	for _, entry := range env {
		if entry == wanted {
			return true
		}
	}
	return false
}

func containsEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func TestNewACPCompositionBuildsNativeAuthProfileWithoutGateway(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-model"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "native-model", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile != "acp/codex" || resolved.Credential != loop.CredentialNativeAuth {
		t.Fatalf("native runtime = %+v, want ACP native-auth profile", resolved)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: t.TempDir(),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("native ACP profile missing from registry: %v", err)
	}
}

func TestACPChildEnvironmentIsCredentialScopedWithoutStartupPreflight(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: "/workspace/project",
		Env: []string{
			"HOME=/private/login",
			"XDG_CONFIG_HOME=/private/login/.config",
			"PATH=/usr/bin",
			"LANG=C",
			"PROVIDER_SENTINEL=task6-obvious-fake-provider-key",
			"MODELS_JSON_PATH=/private/login/.looprig/models.json",
			`MODELS_JSON_CONTENT={"api_key":"task6-obvious-fake-provider-key"}`,
			"NATIVE_PERMISSION_PATH=/private/login/.looprig/workspaces/hash/permissions.json",
			`NATIVE_PERMISSION_CONTENT={"rules":["must-not-pass"]}`,
			"ANTHROPIC_API_KEY=task6-obvious-fake-anthropic-key",
			"OPENAI_API_KEY=task6-obvious-fake-openai-key",
			"CLAUDE_CODE_ACP_NATIVE_MODELS=native=obsolete",
			"CODEX_ACP_NATIVE_MODELS=native=obsolete",
		},
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeEntries := composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name)
	var claude loop.RuntimeCatalogEntry
	for _, entry := range claudeEntries {
		if entry.AgentHarness == "claude-code" {
			claude = entry
			break
		}
	}
	if claude.AgentHarness != "claude-code" {
		t.Fatalf("Claude gateway entry missing: %#v", claudeEntries)
	}
	wantClaudeModels := 0
	for _, entry := range compiled.RuntimeCatalog.EntriesFor(generic.Name) {
		if entry.AgentHarness == "claude-code" {
			wantClaudeModels += len(entry.Models)
		}
	}
	if len(claude.Models) != wantClaudeModels {
		t.Fatalf("Claude configured aliases changed during composition: %#v", claude.Models)
	}
	for _, option := range claude.Models {
		if option.Alias == "sonnet-5" && len(option.Efforts) != 4 {
			t.Fatalf("Claude configured efforts changed during composition: %#v", option)
		}
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("gateway-only Claude profile was removed: %v", err)
	}
}

func TestNewACPCompositionRetainsTransientlyUnavailableACPEntries(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testGenericACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: "/workspace/project",
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "test-token",
		},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			if probe.Harness == "claude-code" {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() failed when first ACP entry was unavailable: %v", err)
	}
	if composition == nil {
		t.Fatal("NewACPComposition() returned nil composition")
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 3 {
		t.Fatalf("NewACPComposition() entries = %#v, want Generic native default plus both configured ACP rows", entries)
	}
	for _, entry := range entries {
		if entry.AgentHarness == looprigRuntimeHarness && !entry.Default {
			t.Fatalf("ordinary Generic row lost default after ACP filtering: %#v", entry)
		}
	}
	if !composition.Catalog.HasProfile("acp/claude-code") || !composition.Catalog.HasProfile("acp/codex") {
		t.Fatalf("transiently unavailable ACP profile was removed: %#v", entries)
	}
}

func testGenericACPGatewayCatalog(t *testing.T) ACPCompiledCatalog {
	t.Helper()
	targets := legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{},
		"openai":    &fakeLLM{},
	})
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: targets,
		PrimerTarget:   runtimeCatalogPrimer(),
		ClaudeSmall:    "sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestNewACPCompositionDoesNotInvokePreflightCallback(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	calls := 0
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: "/workspace/project",
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "test-token",
		},
		executablePreflight: func(_ context.Context, _ ACPExecutableProbe) ACPPreflightResult {
			calls++
			return ACPPreflightResult{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition == nil {
		t.Fatal("NewACPComposition() returned nil composition")
	}
	if calls != 0 {
		t.Fatalf("preflight callback calls = %d, want zero during composition", calls)
	}
}

func TestNewACPCompositionNativeEnvironmentIsDeferredToChildLaunch(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-model"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got ACPExecutableProbe
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: "/workspace/project",
		Env: []string{
			"HOME=/private/login",
			"XDG_CONFIG_HOME=/private/login/.config",
			"PATH=/usr/bin",
			"LANG=C",
			"SECRET=must-not-pass",
		},
		NativeEnvAllowlist: acpNativeAuthEnvAllowlist,
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			got = probe
			if probe.Credential != loop.CredentialNativeAuth || probe.SharedProxy != nil {
				return ACPPreflightResult{}
			}
			if !containsEnv(probe.Env, "HOME=/private/login") || !containsEnv(probe.Env, "XDG_CONFIG_HOME=/private/login/.config") {
				return ACPPreflightResult{}
			}
			if containsEnvKey(probe.Env, "SECRET") {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Harness != "" || got.Model != "" {
		t.Fatalf("native startup probe = %#v, want no probe", got)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("native profile was removed: %v", err)
	}
}

func TestACPChildModelAliasesUseConcreteGatewayTargetsAndNativeModels(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)
	claude, err := compiled.RuntimeCatalog.Resolve(generic.Name, "claude-code", "sonnet-5", model.EffortHigh)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err := acpChildModelAliases(compiled, generic.Name, "claude-code", claude)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "sonnet-5@high" || smallAlias != "sonnet-5" {
		t.Fatalf("Claude child aliases = %q/%q, want sonnet-5@high/sonnet-5", mainAlias, smallAlias)
	}

	codex, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(compiled, generic.Name, "codex", codex)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "gpt-5.6-luna@max" || smallAlias != "" {
		t.Fatalf("Codex child aliases = %q/%q, want gpt-5.6-luna@max/empty", mainAlias, smallAlias)
	}

	nativeCatalog, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-model"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := nativeCatalog.RuntimeCatalog.Resolve(generic.Name, "codex", "native-model", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(nativeCatalog, generic.Name, "codex", native)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != string(native.ModelAlias) || smallAlias != "" {
		t.Fatalf("native child aliases = %q/%q, want %q/empty", mainAlias, smallAlias, native.ModelAlias)
	}
}

func TestACPBoundRuntimeResolutionUsesPinnedSelectors(t *testing.T) {
	t.Parallel()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{}, "openai": &fakeLLM{},
		}),
		ClaudeSmall: "sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName(generic.Name),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithPolicyRevision("acp-child-test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	bound, err = loop.OverrideBoundRuntimeSelection(bound, resolved.Profile, resolved.TargetAlias, resolved.Target, resolved.Effort)
	if err != nil {
		t.Fatal(err)
	}
	got, harness, err := resolveACPBoundRuntime(compiled, bound)
	if err != nil {
		t.Fatal(err)
	}
	if harness != "codex" || got.ModelAlias != "gpt-5.6-luna" || got.TargetAlias != "gpt-5.6-luna@max" || got.Effort != model.EffortMax {
		t.Fatalf("resolved = %#v harness=%q", got, harness)
	}
}

func TestNewACPCompositionDiagnosticsNoExecutable(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{},
		WorkspaceRoot: t.TempDir(),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range composition.Diagnostics {
		if strings.Contains(line, "claude-code unavailable: no executable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a no-executable diagnostic for claude-code, got %v", composition.Diagnostics)
	}
}

func TestNewACPCompositionDiagnosticsExecutableNotRunnable(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": filepath.Join(t.TempDir(), "claude-code-acp")},
		WorkspaceRoot: t.TempDir(),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range composition.Diagnostics {
		if strings.Contains(line, "claude-code unavailable: configured executable not runnable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a configured-executable-not-runnable diagnostic for claude-code, got %v", composition.Diagnostics)
	}
}

func TestNewACPCompositionDoesNotDiagnoseLivePreflightFailure(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: t.TempDir(),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: false}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range composition.Diagnostics {
		if strings.Contains(line, "codex unavailable: preflight failed") {
			found = true
		}
	}
	if found {
		t.Fatalf("unexpected live preflight diagnostic: %v", composition.Diagnostics)
	}
	if !composition.Catalog.HasProfile("acp/codex") {
		t.Fatal("configured Codex profile was removed due to transient availability")
	}
}

func TestNewACPCompositionDoesNotDiagnoseLivePreflightFailureBothModes(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Codex gets both a gateway row (from GatewayTargets' openai entries) and a
	// native-auth row (from NativeAuth), so a universally failing preflight
	// exercises the "gateway or native" both-attempted branch.
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{},
			"openai":    &fakeLLM{},
		}),
		ClaudeSmall: "sonnet-5",
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-model"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: t.TempDir(),
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: false}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range composition.Diagnostics {
		if strings.Contains(line, "codex unavailable: preflight failed (gateway or native)") {
			found = true
		}
	}
	if found {
		t.Fatalf("unexpected both-modes live preflight diagnostic: %v", composition.Diagnostics)
	}
	if !composition.Catalog.HasProfile("acp/codex") {
		t.Fatal("configured Codex profile was removed due to transient availability")
	}
}

func TestNewACPCompositionNoDiagnosticWhenHarnessNotConfigured(t *testing.T) {
	t.Parallel()
	composition, err := NewACPComposition(ACPChildrenConfig{Executables: map[loop.AgentHarnessName]string{"codex": "/bin/sh"}})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	if len(composition.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics for an unconfigured harness, got %v", composition.Diagnostics)
	}
}

func TestNewACPCompositionWithoutCatalogHasNoProfiles(t *testing.T) {
	t.Parallel()
	composition, err := NewACPComposition(ACPChildrenConfig{Executables: map[loop.AgentHarnessName]string{"codex": "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("empty catalog registered ACP profile")
	}
	if composition.Live == nil || composition.Restored == nil {
		t.Fatal("composition did not install bounded dispatchers")
	}
	var unknown *foreign.UnknownProfileError
	_, _, err = composition.Registry.Builder("acp/codex")
	if !errors.As(err, &unknown) {
		t.Fatalf("empty registry error = %v, want UnknownProfileError", err)
	}
}
