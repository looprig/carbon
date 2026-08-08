package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
)

func compactionFingerprintFor(t *testing.T, root string, client *fakeLLM, policy conversationContextPolicy, registration conversationHustleRegistration) event.ConfigFingerprint {
	t.Helper()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definitions, err := swarmDefinitionsWithContextPolicy(client, testModel(), cfg, policy, access)
	if err != nil {
		t.Fatalf("swarmDefinitionsWithContextPolicy() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRigWithRegistration(
		definitions, stores, root, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration,
		permissionReviewRegistration{},
	)
	if err != nil {
		t.Fatalf("buildRigWithRegistration() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	return durableSessionFingerprint(t, stores, controller.SessionID())
}

func durableSessionFingerprint(t *testing.T, stores *swarmStores, sessionID uuid.UUID) event.ConfigFingerprint {
	t.Helper()
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("durable log has no SessionStarted")
		}
		if nextErr != nil {
			t.Fatalf("cursor.Next() error = %v", nextErr)
		}
		if started, ok := ev.(event.SessionStarted); ok {
			return started.Config
		}
	}
}

func compactionDefinitionForFingerprint(t *testing.T, promptRevision, parserRevision string) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(conversationCompactionName),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithCurrentLoopModel(),
		hustle.WithTimeout(conversationCompactionTimeout),
		hustle.WithLimits(hustle.Limits{InputBytes: conversationCompactionInputBytes, OutputBytes: conversationCompactionOutputBytes}),
		hustle.WithSystemPrompt(conversationCompactionPrompt, promptRevision),
		hustle.WithPolicyRevision(parserRevision),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

func TestCompactionCompositionFingerprintSensitivityAndSecretExclusion(t *testing.T) {
	t.Parallel()

	basePolicy, err := newConversationContextPolicy(testModel(), nil, nil)
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}
	baseRegistration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatalf("newConversationHustleRegistration() error = %v", err)
	}
	root := t.TempDir()
	base := compactionFingerprintFor(t, root, &fakeLLM{credential: "secret-a"}, basePolicy, baseRegistration)

	tests := []struct {
		name         string
		client       *fakeLLM
		policy       conversationContextPolicy
		registration conversationHustleRegistration
		wantEqual    bool
	}{
		{name: "client credential excluded", client: &fakeLLM{credential: "secret-b"}, policy: basePolicy, registration: baseRegistration, wantEqual: true},
		{name: "compaction policy", client: &fakeLLM{}, policy: func() conversationContextPolicy { value := basePolicy; value.compaction.CompactAt--; return value }(), registration: baseRegistration},
		{name: "summary revision", client: &fakeLLM{}, policy: func() conversationContextPolicy {
			value := basePolicy
			value.summaryRevision = "coderig-summary-consumption-v2"
			return value
		}(), registration: baseRegistration},
		{name: "hustle prompt revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, "coderig-compaction-prompt-v2", conversationCompactionParserRevision), limits: baseRegistration.limits}},
		{name: "hustle parser revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, conversationCompactionPromptRevision, "harness-compaction-parser-v2"), limits: baseRegistration.limits}},
		{name: "hustle lane limits", client: &fakeLLM{}, policy: basePolicy, registration: func() conversationHustleRegistration {
			value := baseRegistration
			value.limits.AuditTimeout += time.Second
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compactionFingerprintFor(t, root, tt.client, tt.policy, tt.registration)
			if equal := got.Equal(base); equal != tt.wantEqual {
				t.Errorf("ConfigFingerprint.Equal(base) = %v, want %v\ngot=%+v\nbase=%+v", equal, tt.wantEqual, got, base)
			}
		})
	}
}

func TestConversationContextPolicyDeclaresPrimerTransports(t *testing.T) {
	t.Parallel()

	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	candidates := []PrimerCandidate{
		{Alias: "candidate-a", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Model: b, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
	}

	policy, err := newConversationContextPolicy(a, candidates, nil)
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}

	// policy.options() already installs a complete, valid context policy
	// (WithContextCounter/WithInferenceCapability/WithContextTransports/
	// WithCompaction); WithContextObservation is a mutually exclusive
	// alternative admission policy and must not also be supplied here.
	definition, err := loop.Define(append(
		[]loop.Option{
			loop.WithName(identity.AgentName("policy-test")),
			loop.WithInference(&fakeLLM{}, a),
		},
		policy.options()...,
	)...)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	// b's transport must now be declared: ValidateContextModel must accept it,
	// where before this change (no WithContextTransports) it would reject any
	// transport other than a's own.
	if err := definition.ValidateContextModel(b); err != nil {
		t.Fatalf("ValidateContextModel(b) error = %v, want b's transport accepted", err)
	}
}

func TestConversationContextPolicyWithNoPrimerCandidatesStaysSingleTransport(t *testing.T) {
	t.Parallel()

	a := testModel()
	policy, err := newConversationContextPolicy(a, nil, nil)
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}

	// policy.options() already installs a complete, valid context policy
	// (WithContextCounter/WithInferenceCapability/WithContextTransports/
	// WithCompaction); WithContextObservation is a mutually exclusive
	// alternative admission policy and must not also be supplied here.
	definition, err := loop.Define(append(
		[]loop.Option{
			loop.WithName(identity.AgentName("policy-test")),
			loop.WithInference(&fakeLLM{}, a),
		},
		policy.options()...,
	)...)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	other := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "other-model", model.WithTools())
	var target *loop.ContextTransportNotDeclaredError
	if err := definition.ValidateContextModel(other); !errors.As(err, &target) {
		t.Fatalf("ValidateContextModel(other) error = %v, want *loop.ContextTransportNotDeclaredError (no primer candidates -> single-transport default)", err)
	}
}

func TestSecretRedactionAcrossModelCatalogueGatewayFingerprintAndDurableEvents(t *testing.T) {
	const sentinel = "task6-obvious-fake-provider-key"
	captured := make(map[string][]byte)
	capture := func(name string, value any) {
		t.Helper()
		switch typed := value.(type) {
		case []byte:
			captured[name] = append([]byte(nil), typed...)
		case string:
			captured[name] = []byte(typed)
		default:
			encoded, err := json.Marshal(typed)
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			captured[name] = encoded
		}
	}
	captureErrorFormats := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s error = nil", name)
		}
		capture(name+" %v", fmt.Sprintf("%v", err))
		capture(name+" %+v", fmt.Sprintf("%+v", err))
		capture(name+" %#v", fmt.Sprintf("%#v", err))
	}

	wireConfig := digestModelConfigFixture(t, sentinel)
	encodedConfig, err := json.Marshal(wireConfig)
	if err != nil {
		t.Fatalf("marshal model fixture: %v", err)
	}
	modelPath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(modelPath, encodedConfig, 0o600); err != nil {
		t.Fatalf("write model fixture: %v", err)
	}

	decoded, err := decodeModelConfig(encodedConfig)
	if err != nil {
		t.Fatalf("decodeModelConfig: %v", err)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatalf("normalizeModelConfig: %v", err)
	}
	if len(normalized.Models) == 0 || normalized.Models[0].client.APIKey != sentinel {
		t.Fatal("sentinel did not reach the private normalized client input")
	}
	secretFreeJSON, err := secretFreeModelConfigJSON(normalized)
	if err != nil {
		t.Fatalf("secretFreeModelConfigJSON: %v", err)
	}
	capture("normalized canonical JSON", secretFreeJSON)
	normalizedDigest, err := modelConfigDigest(normalized)
	if err != nil {
		t.Fatalf("modelConfigDigest: %v", err)
	}
	capture("normalized digest", normalizedDigest)

	factorySawSentinel := false
	configured, err := loadProductionModelsFrom(modelPath, func(_ model.Model, key auth.APIKey) (inference.Client, error) {
		factorySawSentinel = factorySawSentinel || string(key) == sentinel
		return &fakeLLM{credential: string(key)}, nil
	})
	if err != nil {
		t.Fatalf("loadProductionModelsFrom: %v", err)
	}
	if !factorySawSentinel {
		t.Fatal("loader did not deliver sentinel to the credential-binding factory")
	}
	capture("production model formats", fmt.Sprintf("%v|%+v|%#v", configured, configured, configured))

	_, factoryErr := loadProductionModelsFrom(modelPath, func(_ model.Model, key auth.APIKey) (inference.Client, error) {
		return nil, errors.New("fixture client factory rejected " + string(key))
	})
	captureErrorFormats("client factory failure", factoryErr)

	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:     []identity.AgentName{"planner", "builder", "reviewer"},
		GatewayTargets: configured.ACP,
		ClaudeSmall:    configured.ClaudeSmall,
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog: %v", err)
	}
	capture("runtime catalogue digest", compiled.RuntimeCatalog.Digest())
	for _, role := range []identity.AgentName{"planner", "builder", "reviewer"} {
		capture("runtime catalogue "+string(role), compiled.RuntimeCatalog.EntriesFor(role))
	}
	duplicateTargets := append(append([]ACPGatewaySource(nil), configured.ACP...), configured.ACP[0])
	_, catalogueErr := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:     []identity.AgentName{"builder"},
		GatewayTargets: duplicateTargets,
	})
	captureErrorFormats("catalogue compilation failure", catalogueErr)

	resolved, err := compiled.RuntimeCatalog.Resolve("builder", "codex", "zeta", model.EffortHigh)
	if err != nil {
		t.Fatalf("resolve gateway target: %v", err)
	}
	plan, err := buildACPGatewayPlan(compiled, resolved)
	if err != nil {
		t.Fatalf("buildACPGatewayPlan: %v", err)
	}
	_, gatewayErr := plan.resolver.Resolve(context.Background(), model.APIFormatOpenAIResponses, "fixture-not-configured")
	captureErrorFormats("gateway route failure", gatewayErr)
	badResolved := resolved
	badResolved.AgentHarness = "unsupported"
	_, gatewayConstructionErr := buildACPGatewayPlan(compiled, badResolved)
	captureErrorFormats("gateway construction failure", gatewayConstructionErr)

	_, preflightErr := NewACPComposition(ACPChildrenConfig{
		Catalog: compiled,
		Executables: map[loop.AgentHarnessName]string{
			"codex": "/fixture/" + sentinel,
		},
		WorkspaceRoot: "relative/" + sentinel,
		Env:           []string{"PROVIDER_KEY=" + sentinel},
	})
	captureErrorFormats("ACP preflight configuration failure", preflightErr)

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	preflightCalled := false
	failedPreflight, err := NewACPComposition(ACPChildrenConfig{
		Catalog: compiled,
		Executables: map[loop.AgentHarnessName]string{
			"codex": executable,
		},
		WorkspaceRoot:       t.TempDir(),
		Env:                 []string{"PATH=/fixture/bin", "PROVIDER_KEY=" + sentinel},
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			preflightCalled = true
			capture("failed ACP preflight probe", probe)
			return ACPPreflightResult{}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition(failed preflight): %v", err)
	}
	if !preflightCalled {
		t.Fatal("ACP preflight callback was not called")
	}
	capture("failed ACP preflight catalogue", failedPreflight.Catalog.RuntimeCatalog.EntriesFor("builder"))
	captureErrorFormats("ACP bounded preflight failure", boundedACPChildError(errors.New("preflight failed: "+sentinel)))

	fingerprintFields := agentFingerprintFields(Config{
		ModelConfigRev: configured.ConfigRev,
		RuntimeCatalog: compiled.RuntimeCatalog,
	})
	capture("fingerprint fields", fingerprintFields)
	capture("fingerprint formats", fmt.Sprintf("%v|%+v|%#v", fingerprintFields, fingerprintFields, fingerprintFields))
	fingerprint := event.ConfigFingerprint{
		AgentKind:         fingerprintFields.AgentKind,
		RuntimeCatalogRev: fingerprintFields.RuntimeCatalogRev,
	}
	durableFixture := []event.Event{
		event.SessionStarted{Config: fingerprint},
		event.LoopStarted{AgentRuntime: &event.AgentRuntime{
			Harness: "codex", Profile: "acp/codex", CredentialMode: string(loop.CredentialGatewayBacked), ModelAlias: string(resolved.TargetAlias),
		}},
	}
	durableBytes, err := json.Marshal(durableFixture)
	if err != nil {
		t.Fatalf("marshal durable event fixture: %v", err)
	}
	capture("durable event serialization", durableBytes)

	for name, value := range captured {
		if strings.Contains(string(value), sentinel) {
			t.Errorf("%s exposed sentinel", name)
		}
	}
	if len(captured) < 18 {
		t.Fatalf("redaction capture count = %d, want broad stage coverage", len(captured))
	}
}

// TestAgentFingerprintFields asserts the rig-level config-fingerprint fields the
// composition root injects via rig.WithFingerprintFields: AgentKind is the swarm+active-primer
// identity ("coderig:builder") and RuntimeSkills passes the human-set mode through verbatim. The
// workspace-root field is NOT set here — the rig's exclusive-workspace placement folds the
// canonical root into the fingerprint — so a restore still compares agent identity, skill
// mode, AND (via the placement) the repo root.
func TestAgentFingerprintFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want rig.ConfigFingerprintFields
	}{
		{
			name: "runtime skills off",
			cfg:  Config{RuntimeSkills: false},
			want: rig.ConfigFingerprintFields{AgentKind: "coderig:builder", RuntimeSkills: false},
		},
		{
			name: "runtime skills on",
			cfg:  Config{RuntimeSkills: true},
			want: rig.ConfigFingerprintFields{AgentKind: "coderig:builder", RuntimeSkills: true},
		},
		{
			name: "access profile and digest fold in",
			cfg:  Config{AccessProfile: AccessTrusted, AccessConfigRev: "coderig-access-v1:deadbeef"},
			want: rig.ConfigFingerprintFields{
				AgentKind:                 "coderig:builder",
				NativePermissionPolicyRev: "coderig-access-v1:deadbeef",
				AppFields:                 map[string]string{"access_profile": "trusted"},
			},
		},
		{
			name: "model configuration digest folds in",
			cfg:  Config{ModelConfigRev: "coderig-models-v1:deadbeef"},
			want: rig.ConfigFingerprintFields{
				AgentKind:         "coderig:builder",
				RuntimeCatalogRev: "coderig-models-v1:deadbeef",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agentFingerprintFields(tt.cfg)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("agentFingerprintFields = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestModelConfigInvalidatesAgentFingerprintWithoutCredentialRotation(t *testing.T) {
	t.Parallel()

	base := agentFingerprintFields(Config{ModelConfigRev: "model-rev-a"})
	if got := agentFingerprintFields(Config{ModelConfigRev: "model-rev-a"}); !reflect.DeepEqual(got, base) {
		t.Fatalf("identical model config produced different fields: got=%+v base=%+v", got, base)
	}
	if got := agentFingerprintFields(Config{ModelConfigRev: "model-rev-b"}); reflect.DeepEqual(got, base) {
		t.Fatal("changed ModelConfigRev did not change fingerprint fields")
	}
}

func TestAgentFingerprintCombinesModelAndRuntimeCatalogRevisionsAsValidIdentifier(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType:     "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex",
		Source:        loop.RuntimeSourceNative,
		Credential:    loop.CredentialNativeAuth,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
		Default:       true,
	}})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	got := agentFingerprintFields(Config{
		ModelConfigRev: "model-rev",
		RuntimeCatalog: catalog,
	})
	want := "model-rev/" + catalog.Digest()
	if got.RuntimeCatalogRev != want {
		t.Fatalf("RuntimeCatalogRev = %q, want %q", got.RuntimeCatalogRev, want)
	}
}

func TestAgentFingerprintBoundsProductionLengthModelAndRuntimeCatalogRevisions(t *testing.T) {
	t.Parallel()

	modelRevision := strings.Repeat("a", 64)
	otherModelRevision := strings.Repeat("b", 64)
	emptyCatalog := mustEmptyRuntimeCatalog()
	populatedCatalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType:     "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex",
		Credential:    loop.CredentialNativeAuth,
		Source:        loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
		Default:       true,
	}})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	fields := func(modelRevision string, catalog loop.RuntimeCatalog) rig.ConfigFingerprintFields {
		return agentFingerprintFields(Config{
			ModelConfigRev: modelRevision,
			RuntimeCatalog: catalog,
		})
	}
	base := fields(modelRevision, emptyCatalog).RuntimeCatalogRev
	if len(base) != 64 {
		t.Fatalf("combined RuntimeCatalogRev length = %d, want 64: %q", len(base), base)
	}
	if _, err := hex.DecodeString(base); err != nil {
		t.Fatalf("combined RuntimeCatalogRev = %q is not hex: %v", base, err)
	}
	if changedModel := fields(otherModelRevision, emptyCatalog).RuntimeCatalogRev; changedModel == base {
		t.Fatal("changing the model configuration revision did not change the combined revision")
	}
	if changedCatalog := fields(modelRevision, populatedCatalog).RuntimeCatalogRev; changedCatalog == base {
		t.Fatal("changing the runtime catalog revision did not change the combined revision")
	}
}

// TestAccessConfigInvalidatesFingerprintFields proves the durable access
// configuration is drift-detecting at the rig-fingerprint boundary: a
// product-profile, reviewer-restriction, or egress-boundary change (all folded
// into AccessConfigRev), or the selected profile name, changes the rig-level
// fingerprint fields, so a restore with different authority is a mismatch rather
// than a silent authority change.
func TestAccessConfigInvalidatesFingerprintFields(t *testing.T) {
	t.Parallel()

	base := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-a"})

	if got := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-a"}); !reflect.DeepEqual(got, base) {
		t.Fatalf("identical access config produced different fields:\n got=%+v\nbase=%+v", got, base)
	}
	// A changed access digest (profile/reviewer/egress change) must invalidate.
	if got := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-b"}); reflect.DeepEqual(got, base) {
		t.Error("changed AccessConfigRev did not change the fingerprint fields")
	}
	// A changed selected profile name must invalidate.
	if got := agentFingerprintFields(Config{AccessProfile: AccessTrusted, AccessConfigRev: "rev-a"}); reflect.DeepEqual(got, base) {
		t.Error("changed AccessProfile did not change the fingerprint fields")
	}
}

// TestAgentKindFormat pins the AgentKind to "<swarm>:<active primer>" so a rename of
// the builder's attribution name is reflected in the fingerprint (and a prior/other session,
// with a different or empty AgentKind, cannot resume as CodeRig).
func TestAgentKindFormat(t *testing.T) {
	t.Parallel()
	want := "coderig:" + string(builder.Name)
	if agentKind != want {
		t.Errorf("agentKind = %q, want %q", agentKind, want)
	}
	if agentKind != "coderig:builder" {
		t.Errorf("agentKind = %q, want %q", agentKind, "coderig:builder")
	}
}
