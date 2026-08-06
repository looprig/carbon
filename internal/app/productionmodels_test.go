package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	"github.com/looprig/inference/model"
)

func TestProductionModelsConstructsCredentialBoundClients(t *testing.T) {
	const (
		primerKey   = "test-secret-do-not-log-primer"
		delegateKey = "test-secret-do-not-log-delegate"
	)
	primerModel := model.CustomModel("anthropic", model.APIFormatAnthropic, "", "primer-model", model.WithTools())
	delegateModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "delegate-model", model.WithTools(), model.WithThinking())
	localModel := model.CustomModel("ollama", model.APIFormatOpenAI, "http://localhost:11434/v1", "local-model", model.WithTools(), model.WithThinking())
	config := normalizedModelConfig{
		PrimerDefault:        "fixture-primer",
		ClaudeCodeSmallModel: "fixture-local",
		DelegateDefaults: []normalizedDelegateDefault{
			{Role: "planner", Harness: "claude-code", Model: "fixture-delegate", Effort: model.EffortHigh},
			{Role: "builder", Harness: "codex", Model: "fixture-local", Effort: model.EffortLow},
			{Role: "reviewer", Harness: "codex", Model: "fixture-delegate", Effort: model.EffortMedium},
		},
		Models: []normalizedModelTarget{
			{Alias: "fixture-primer", Model: primerModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone, client: modelClientInput{APIKey: primerKey}},
			{Alias: "fixture-delegate", Model: delegateModel, Uses: []string{"delegate"}, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortMedium, client: modelClientInput{APIKey: delegateKey}},
			{Alias: "fixture-local", Model: localModel, Uses: []string{"delegate"}, Efforts: []model.Effort{model.EffortLow}, DefaultEffort: model.EffortLow},
		},
	}

	type factoryCall struct {
		model model.Model
		key   auth.APIKey
	}
	var calls []factoryCall
	clients := make([]inference.Client, 0, len(config.Models))
	factory := func(gotModel model.Model, gotKey auth.APIKey) (inference.Client, error) {
		calls = append(calls, factoryCall{model: gotModel, key: gotKey})
		client := &fakeLLM{credential: string(gotKey)}
		clients = append(clients, client)
		return client, nil
	}

	got, err := compileProductionModels(config, factory)
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}
	if len(calls) != len(config.Models) {
		t.Fatalf("factory calls = %d, want %d", len(calls), len(config.Models))
	}
	wantKeys := []auth.APIKey{auth.APIKey(primerKey), auth.APIKey(delegateKey), ""}
	for index := range calls {
		if !reflect.DeepEqual(calls[index].model, config.Models[index].Model) {
			t.Errorf("factory call %d model = %#v, want %#v", index, calls[index].model, config.Models[index].Model)
		}
		if calls[index].key != wantKeys[index] {
			t.Errorf("factory call %d key mismatch", index)
		}
	}
	if got.PrimerClient != clients[0] || !reflect.DeepEqual(got.PrimerModel, primerModel) {
		t.Fatalf("primer binding did not match primer_default")
	}
	if got.RuntimeClient == nil {
		t.Fatal("RuntimeClient is nil, want credential-bound model router")
	}
	if got.PrimerAlias != "fixture-primer" {
		t.Fatalf("PrimerAlias = %q, want fixture-primer", got.PrimerAlias)
	}
	if !reflect.DeepEqual(got.PrimerEfforts, []model.Effort{model.EffortNone}) {
		t.Fatalf("PrimerEfforts = %v, want [none]", got.PrimerEfforts)
	}
	if len(got.ACP) != 2 {
		t.Fatalf("ACP sources = %d, want 2 delegate-capable rows", len(got.ACP))
	}
	for index, source := range got.ACP {
		want := config.Models[index+1]
		if source.Alias != loop.ModelAlias(want.Alias) || source.Description != want.Description || source.Client != clients[index+1] || !reflect.DeepEqual(source.Model, want.Model) || source.DefaultEffort != want.DefaultEffort || !reflect.DeepEqual(source.Efforts, want.Efforts) {
			t.Errorf("ACP source %d did not preserve normalized target", index)
		}
	}
	wantDefaults := map[identity.AgentName]configuredDelegateDefault{
		"planner":  {Harness: "claude-code", Model: "fixture-delegate", Effort: model.EffortHigh},
		"builder":  {Harness: "codex", Model: "fixture-local", Effort: model.EffortLow},
		"reviewer": {Harness: "codex", Model: "fixture-delegate", Effort: model.EffortMedium},
	}
	if !reflect.DeepEqual(got.Defaults, wantDefaults) {
		t.Fatalf("defaults = %#v, want %#v", got.Defaults, wantDefaults)
	}
	if got.ClaudeSmall != "fixture-local" {
		t.Fatalf("ClaudeSmall = %q, want fixture-local", got.ClaudeSmall)
	}
	wantRev, err := modelConfigDigest(config)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigRev != wantRev {
		t.Fatalf("ConfigRev = %q, want %q", got.ConfigRev, wantRev)
	}
	for _, formatted := range []string{fmt.Sprintf("%v", got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
		if strings.Contains(formatted, primerKey) || strings.Contains(formatted, delegateKey) {
			t.Fatalf("formatted productionModels exposed a key: %q", formatted)
		}
	}
}

func TestProductionModelsConstructionFailureIsSecretFree(t *testing.T) {
	const sentinel = "test-secret-do-not-log"
	config := normalizedModelConfig{
		PrimerDefault: "fixture-a",
		Models: []normalizedModelTarget{{
			Alias: "fixture-a",
			Model: model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "provider-model", model.WithTools()),
			Uses:  []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone,
			client: modelClientInput{APIKey: sentinel},
		}},
	}
	factory := func(model.Model, auth.APIKey) (inference.Client, error) {
		return nil, errors.New("factory rejected " + sentinel)
	}

	got, err := compileProductionModels(config, factory)
	if err == nil {
		t.Fatal("compileProductionModels() succeeded")
	}
	if got.PrimerClient != nil || len(got.ACP) != 0 || len(got.Defaults) != 0 || got.ConfigRev != "" {
		t.Fatalf("failure returned partial production models: %#v", got)
	}
	for _, formatted := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(formatted, sentinel) {
			t.Fatalf("formatted error exposed key: %q", formatted)
		}
		if !strings.Contains(formatted, "fixture-a") || !strings.Contains(formatted, "openai") {
			t.Fatalf("formatted error = %q, want alias and provider", formatted)
		}
	}
}

func TestProductionModelsCarriesNativeACPProfilesAndSources(t *testing.T) {
	config := normalizedModelConfig{
		PrimerDefault: "fixture-primer",
		NativeACP: map[string]normalizedNativeACPProfile{
			"claude-code": {Harness: "claude-code", Enabled: true},
			"codex":       {Harness: "codex", Enabled: true, Models: []string{"native-a", "native-b"}},
		},
		DelegateDefaults: []normalizedDelegateDefault{
			{Role: "planner", Harness: "codex", Source: loop.RuntimeSourceNative, Model: "native-a"},
			{Role: "builder", Harness: "codex", Source: loop.RuntimeSourceNative},
			{Role: "reviewer", Harness: "codex", Source: loop.RuntimeSourceNative},
		},
		Models: []normalizedModelTarget{{
			Alias: "fixture-primer", Model: model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234", "primer", model.WithTools()),
			Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone,
		}},
	}

	got, err := compileProductionModels(config, func(model.Model, auth.APIKey) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}
	if len(got.NativeACP) != len(config.NativeACP) {
		t.Fatalf("NativeACP = %#v, want %#v", got.NativeACP, config.NativeACP)
	}
	for harness, want := range config.NativeACP {
		gotProfile, ok := got.NativeACP[harness]
		if !ok || gotProfile.Harness != loop.AgentHarnessName(want.Harness) || gotProfile.Enabled != want.Enabled || !reflect.DeepEqual(gotProfile.Models, aliasesToLoop(want.Models)) {
			t.Fatalf("NativeACP[%q] = %#v, want %#v", harness, gotProfile, want)
		}
	}
	if got.Defaults["planner"].Source != loop.RuntimeSourceNative || got.Defaults["planner"].Model != "native-a" {
		t.Fatalf("native planner default = %#v", got.Defaults["planner"])
	}
	if got.Defaults["builder"].Source != loop.RuntimeSourceNative || got.Defaults["builder"].Model != "" {
		t.Fatalf("native managed builder default = %#v", got.Defaults["builder"])
	}
}

func TestProductionModelsCollectsAllPrimerCapableCandidates(t *testing.T) {
	primaryModel := model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234/v1", "primary", model.WithTools())
	altModel := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "alt-primer", model.WithTools(), model.WithThinking())
	delegateOnlyModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "delegate-only", model.WithTools())
	config := normalizedModelConfig{
		PrimerDefault: "fixture-primary",
		DelegateDefaults: []normalizedDelegateDefault{
			{Role: "planner", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
			{Role: "builder", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
			{Role: "reviewer", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
		},
		Models: []normalizedModelTarget{
			{Alias: "fixture-primary", Description: "Primary", Model: primaryModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
			{Alias: "fixture-alt-primer", Description: "Alternate primer", Model: altModel, Uses: []string{"primer", "delegate"}, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortHigh}, DefaultEffort: model.EffortLow},
			{Alias: "fixture-delegate-only", Description: "Delegate only", Model: delegateOnlyModel, Uses: []string{"delegate"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		},
	}

	got, err := compileProductionModels(config, func(model.Model, auth.APIKey) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}

	want := []PrimerCandidate{
		{Alias: "fixture-primary", Description: "Primary", Model: primaryModel, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "fixture-alt-primer", Description: "Alternate primer", Model: altModel, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
	if !reflect.DeepEqual(got.PrimerCandidates, want) {
		t.Fatalf("PrimerCandidates = %#v, want %#v", got.PrimerCandidates, want)
	}
}

// TestProductionModelsRejectsSharedPrimerProviderTargets proves compileProductionModels
// rejects two primer-tagged aliases that resolve to the identical provider target
// (provider+api_format+base_url+name), since currentPrimerCandidate/publicModelID
// key primer candidates by that identity and a collision would make them
// indistinguishable at runtime. Sharing a target is still explicitly permitted
// for non-primer aliasing (newModelRoutingClient's own documented shape), so
// primer+delegate and delegate+delegate collisions on the same target remain allowed.
func TestProductionModelsRejectsSharedPrimerProviderTargets(t *testing.T) {
	sharedModel := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "shared-target", model.WithTools())
	primaryModel := model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234/v1", "primary", model.WithTools())
	factory := func(model.Model, auth.APIKey) (inference.Client, error) { return &fakeLLM{}, nil }

	tests := []struct {
		name    string
		usesA   []string
		usesB   []string
		wantErr bool
	}{
		{name: "two primer-tagged aliases sharing a provider target", usesA: []string{"primer"}, usesB: []string{"primer"}, wantErr: true},
		{name: "primer and delegate-only aliases sharing a provider target", usesA: []string{"primer"}, usesB: []string{"delegate"}, wantErr: false},
		{name: "two delegate-only aliases sharing a provider target", usesA: []string{"delegate"}, usesB: []string{"delegate"}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := normalizedModelConfig{
				PrimerDefault: "fixture-primary",
				Models: []normalizedModelTarget{
					{Alias: "fixture-primary", Description: "Primary", Model: primaryModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
					{Alias: "fixture-a", Description: "A", Model: sharedModel, Uses: tt.usesA, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
					{Alias: "fixture-b", Description: "B", Model: sharedModel, Uses: tt.usesB, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
				},
			}

			_, err := compileProductionModels(config, factory)
			if tt.wantErr && err == nil {
				t.Fatal("compileProductionModels() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("compileProductionModels() error = %v", err)
			}
			if tt.wantErr {
				var configErr *ModelConfigError
				if !errors.As(err, &configErr) {
					t.Fatalf("error = %#v, want *ModelConfigError", err)
				}
			}
		})
	}
}

// TestProductionModelsResolvesPermissionReview proves compileProductionModels
// resolves a normalized permission_review section into
// PermissionReviewEnabled/PermissionReviewModel/PermissionReviewStrict on the
// output, and that all three stay at their zero value when the section is
// absent.
func TestProductionModelsResolvesPermissionReview(t *testing.T) {
	primerModel := model.CustomModel("anthropic", model.APIFormatAnthropic, "", "primer-model", model.WithTools())
	classifierModel := model.CustomModel(
		"openai", model.APIFormatOpenAIResponses, "", "classifier-model",
		model.WithTools(), model.WithStructuredOutput(), model.WithStructuredOutputWithTools(),
	)
	factory := func(model.Model, auth.APIKey) (inference.Client, error) { return &fakeLLM{}, nil }

	t.Run("present section resolves the enabled model and strict flag", func(t *testing.T) {
		config := normalizedModelConfig{
			PrimerDefault: "fixture-primer",
			Models: []normalizedModelTarget{
				{Alias: "fixture-primer", Model: primerModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
				{Alias: "fixture-classifier", Model: classifierModel, Uses: []string{"delegate"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
			},
			PermissionReview: &normalizedPermissionReview{Model: "fixture-classifier", Strict: true},
		}

		got, err := compileProductionModels(config, factory)
		if err != nil {
			t.Fatalf("compileProductionModels() error = %v", err)
		}
		if !got.PermissionReviewEnabled {
			t.Fatal("PermissionReviewEnabled = false, want true")
		}
		if !reflect.DeepEqual(got.PermissionReviewModel, classifierModel) {
			t.Fatalf("PermissionReviewModel = %#v, want %#v", got.PermissionReviewModel, classifierModel)
		}
		if !got.PermissionReviewStrict {
			t.Fatal("PermissionReviewStrict = false, want true")
		}
	})

	t.Run("absent section stays disabled with zero fields", func(t *testing.T) {
		config := normalizedModelConfig{
			PrimerDefault: "fixture-primer",
			Models: []normalizedModelTarget{
				{Alias: "fixture-primer", Model: primerModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
			},
		}

		got, err := compileProductionModels(config, factory)
		if err != nil {
			t.Fatalf("compileProductionModels() error = %v", err)
		}
		if got.PermissionReviewEnabled {
			t.Fatal("PermissionReviewEnabled = true, want false")
		}
		if got.PermissionReviewModel.Name != "" {
			t.Fatalf("PermissionReviewModel = %#v, want the zero model.Model", got.PermissionReviewModel)
		}
		if got.PermissionReviewStrict {
			t.Fatal("PermissionReviewStrict = true, want false")
		}
	})
}

// TestProductionModelsResolvesUnusedClassifierPermissionReview is the
// end-to-end (JSON in, compileProductionModels out) proof for the
// phase-boundary review's fix: a models.json row whose "uses" field is
// entirely omitted from the JSON binds through permission_review.model, and
// stays invisible to both the primer-picker (PrimerCandidates) and the
// delegate roster (ACP) — proving it is genuinely excluded from those
// rosters, not merely absent from them by coincidence.
func TestProductionModelsResolvesUnusedClassifierPermissionReview(t *testing.T) {
	wire, err := decodeModelConfig([]byte(modelConfigJSONWithUnusedClassifier()))
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v", err)
	}
	normalized, err := normalizeModelConfig(wire)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	classifier, ok := modelConfigNormalizedTargetByAlias(normalized.Models, "classifier")
	if !ok {
		t.Fatal("normalized config missing the classifier model")
	}

	factory := func(model.Model, auth.APIKey) (inference.Client, error) { return &fakeLLM{}, nil }
	got, err := compileProductionModels(normalized, factory)
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}

	if !got.PermissionReviewEnabled {
		t.Fatal("PermissionReviewEnabled = false, want true")
	}
	if !reflect.DeepEqual(got.PermissionReviewModel, classifier.Model) {
		t.Fatalf("PermissionReviewModel = %#v, want the classifier model %#v", got.PermissionReviewModel, classifier.Model)
	}
	for _, candidate := range got.PrimerCandidates {
		if candidate.Alias == "classifier" {
			t.Fatal("classifier model appeared in PrimerCandidates, want excluded")
		}
	}
	for _, source := range got.ACP {
		if source.Alias == "classifier" {
			t.Fatal("classifier model appeared in ACP delegate roster, want excluded")
		}
	}
}

func TestCompileProductionModelsCarriesACPLaunchers(t *testing.T) {
	config := normalizedModelConfig{
		PrimerDefault: "fixture-primer",
		Models: []normalizedModelTarget{{
			Alias: "fixture-primer", Model: model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234", "primer", model.WithTools()),
			Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone,
		}},
	}
	config.ACPLaunchers = map[string]normalizedACPLauncher{
		"claude-code": {Harness: "claude-code", Executable: "/usr/local/bin/claude-code-acp"},
	}

	produced, err := compileProductionModels(config, func(model.Model, auth.APIKey) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("compileProductionModels: %v", err)
	}
	if got := produced.ACPLaunchers["claude-code"]; got != "/usr/local/bin/claude-code-acp" {
		t.Fatalf("ACPLaunchers[claude-code] = %q, want the configured path", got)
	}
}

func aliasesToLoop(values []string) []loop.ModelAlias {
	if values == nil {
		return nil
	}
	result := make([]loop.ModelAlias, len(values))
	for i, value := range values {
		result[i] = loop.ModelAlias(value)
	}
	return result
}
