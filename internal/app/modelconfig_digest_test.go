package app

import (
	"fmt"
	"strings"
	"testing"

	model "github.com/looprig/inference/model"
)

func TestDigestModelConfigCanonicalizesOrder(t *testing.T) {
	first := digestModelConfigFixture(t, "test-secret-do-not-log")
	second := digestModelConfigFixture(t, "test-secret-do-not-log")
	second.Models[0], second.Models[1] = second.Models[1], second.Models[0]
	for i := range second.Models {
		reverseStrings(second.Models[i].Uses)
		reverseStrings(second.Models[i].Efforts)
	}

	firstNormalized, err := normalizeModelConfig(first)
	if err != nil {
		t.Fatalf("normalize first: %v", err)
	}
	secondNormalized, err := normalizeModelConfig(second)
	if err != nil {
		t.Fatalf("normalize second: %v", err)
	}
	firstDigest, err := modelConfigDigest(firstNormalized)
	if err != nil {
		t.Fatalf("modelConfigDigest(first) error = %v", err)
	}
	secondDigest, err := modelConfigDigest(secondNormalized)
	if err != nil {
		t.Fatalf("modelConfigDigest(second) error = %v", err)
	}
	if firstDigest != secondDigest {
		t.Errorf("reordered digest = %q, want %q", secondDigest, firstDigest)
	}
}

func TestDigestModelConfigCoversSecretFreeAdmissionFields(t *testing.T) {
	base, err := normalizeModelConfig(digestModelConfigFixture(t, "test-secret-do-not-log"))
	if err != nil {
		t.Fatalf("normalize fixture: %v", err)
	}
	baseDigest, err := modelConfigDigest(base)
	if err != nil {
		t.Fatalf("modelConfigDigest(base) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*normalizedModelConfig)
	}{
		{name: "alias", mutate: func(c *normalizedModelConfig) { c.Models[0].Alias += "-changed" }},
		{name: "provider", mutate: func(c *normalizedModelConfig) { c.Models[0].Model.Provider = "xai" }},
		{name: "format", mutate: func(c *normalizedModelConfig) { c.Models[0].Model.APIFormat = model.APIFormatOpenAI }},
		{name: "base URL", mutate: func(c *normalizedModelConfig) { c.Models[0].Model.BaseURL = "https://other.example.test/v1" }},
		{name: "model name", mutate: func(c *normalizedModelConfig) { c.Models[0].Model.Name += "-changed" }},
		{name: "context limit", mutate: func(c *normalizedModelConfig) { c.Models[0].Model.Limits.MaxInputTokens = 256_000 }},
		{name: "use", mutate: func(c *normalizedModelConfig) { c.Models[0].Uses[0] = "primer" }},
		{name: "capability", mutate: func(c *normalizedModelConfig) {
			c.Models[0].Model.Caps.AcceptsImages = !c.Models[0].Model.Caps.AcceptsImages
		}},
		{name: "effort", mutate: func(c *normalizedModelConfig) { c.Models[0].Efforts = append(c.Models[0].Efforts, model.EffortMedium) }},
		{name: "model default", mutate: func(c *normalizedModelConfig) {
			c.Models[0].DefaultEffort = model.EffortLow
			c.Models[0].Model.Sampling.Effort = model.EffortLow
		}},
		{name: "primer default", mutate: func(c *normalizedModelConfig) { c.PrimerDefault = "changed" }},
		{name: "Claude small model", mutate: func(c *normalizedModelConfig) { c.ClaudeCodeSmallModel = "zeta" }},
		{name: "description", mutate: func(c *normalizedModelConfig) { c.Models[0].Description = "different presentation guidance" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := cloneNormalizedModelConfig(base)
			tt.mutate(&changed)
			digest, err := modelConfigDigest(changed)
			if err != nil {
				t.Fatalf("modelConfigDigest(changed) error = %v", err)
			}
			if digest == baseDigest {
				t.Errorf("digest unchanged at %q", digest)
			}
		})
	}
}

func TestDigestModelConfigExcludesAPIKeyBytes(t *testing.T) {
	first, err := normalizeModelConfig(digestModelConfigFixture(t, "test-secret-do-not-log"))
	if err != nil {
		t.Fatalf("normalize first: %v", err)
	}
	second, err := normalizeModelConfig(digestModelConfigFixture(t, "different-secret-bytes"))
	if err != nil {
		t.Fatalf("normalize second: %v", err)
	}
	firstDigest, err := modelConfigDigest(first)
	if err != nil {
		t.Fatalf("digest first: %v", err)
	}
	secondDigest, err := modelConfigDigest(second)
	if err != nil {
		t.Fatalf("digest second: %v", err)
	}
	if firstDigest != secondDigest {
		t.Errorf("API-key-only change altered digest: %q != %q", firstDigest, secondDigest)
	}

	withoutKey := digestModelConfigFixture(t, "")
	if _, err := normalizeModelConfig(withoutKey); err == nil {
		t.Fatal("credential presence change was admitted, want validation error")
	}
}

func TestDigestModelConfigAndFormattingExcludeSecrets(t *testing.T) {
	const secret = "test-secret-do-not-log"
	normalized, err := normalizeModelConfig(digestModelConfigFixture(t, secret))
	if err != nil {
		t.Fatalf("normalize fixture: %v", err)
	}
	digest, err := modelConfigDigest(normalized)
	if err != nil {
		t.Fatalf("modelConfigDigest() error = %v", err)
	}
	material, err := secretFreeModelConfigJSON(normalized)
	if err != nil {
		t.Fatalf("secretFreeModelConfigJSON() error = %v", err)
	}
	formatted := fmt.Sprintf("%v|%+v|%#v", normalized, normalized, normalized)
	for _, target := range normalized.Models {
		formatted += fmt.Sprintf("|%v|%+v|%#v", target, target, target)
		formatted += fmt.Sprintf("|%v|%+v|%#v", target.client, target.client, target.client)
	}
	for name, value := range map[string]string{
		"digest": digest, "canonical JSON": string(material), "formatted normalized values": formatted,
	} {
		if strings.Contains(value, secret) {
			t.Errorf("%s exposed secret", name)
		}
	}
}

func TestModelConfigDigestExcludesACPLaunchers(t *testing.T) {
	base, err := normalizeModelConfig(digestModelConfigFixture(t, "test-secret-do-not-log"))
	if err != nil {
		t.Fatalf("normalize fixture: %v", err)
	}
	withLaunchers := cloneNormalizedModelConfig(base)
	withLaunchers.ACPLaunchers = map[string]normalizedACPLauncher{
		"codex": {Harness: "codex", Executable: "/usr/local/bin/codex-acp"},
	}
	baseDigest, err := modelConfigDigest(base)
	if err != nil {
		t.Fatalf("digest(base): %v", err)
	}
	launcherDigest, err := modelConfigDigest(withLaunchers)
	if err != nil {
		t.Fatalf("digest(withLaunchers): %v", err)
	}
	if baseDigest != launcherDigest {
		t.Fatalf("acp_launchers must not change the model config digest: base=%q withLaunchers=%q", baseDigest, launcherDigest)
	}
}

func TestModelConfigNativeACPStructuredDigestCanonicalizesOrderAndCoversAllowlist(t *testing.T) {
	first := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		{"model":"zeta","efforts":["high","medium"],"default_effort":"medium"},
		{"model":"alpha","efforts":["max","none"],"default_effort":"max"}
	]}}`)
	second := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		{"model":"alpha","efforts":["none","max"],"default_effort":"max"},
		{"model":"zeta","efforts":["medium","high"],"default_effort":"medium"}
	]}}`)
	firstDigest := digestNativeACPConfig(t, first)
	secondDigest := digestNativeACPConfig(t, second)
	if firstDigest != secondDigest {
		t.Fatalf("reordered native allowlist changed digest: first=%q second=%q", firstDigest, secondDigest)
	}

	mutations := []struct {
		name       string
		nativeJSON string
	}{
		{name: "model", nativeJSON: `{"codex":{"enabled":true,"models":[
			{"model":"other","efforts":["high","medium"],"default_effort":"medium"},
			{"model":"alpha","efforts":["max","none"],"default_effort":"max"}
		]}}`},
		{name: "allowed effort", nativeJSON: `{"codex":{"enabled":true,"models":[
			{"model":"zeta","efforts":["high","medium","max"],"default_effort":"medium"},
			{"model":"alpha","efforts":["max","none"],"default_effort":"max"}
		]}}`},
		{name: "default effort", nativeJSON: `{"codex":{"enabled":true,"models":[
			{"model":"zeta","efforts":["high","medium"],"default_effort":"high"},
			{"model":"alpha","efforts":["max","none"],"default_effort":"max"}
		]}}`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := digestNativeACPConfig(t, decodeModelConfigWithNativeACP(t, mutation.nativeJSON))
			if changed == firstDigest {
				t.Fatalf("native allowlist %s did not change digest %q", mutation.name, changed)
			}
		})
	}
}

func TestModelConfigNativeACPLegacyNoneMatchesStructuredNoneDigest(t *testing.T) {
	legacy := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":["same"]}}`)
	structured := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		{"model":"same","efforts":["none"],"default_effort":"none"}
	]}}`)
	if legacyDigest, structuredDigest := digestNativeACPConfig(t, legacy), digestNativeACPConfig(t, structured); legacyDigest != structuredDigest {
		t.Fatalf("semantically equivalent legacy/structured native entries changed digest: legacy=%q structured=%q", legacyDigest, structuredDigest)
	}
}

func digestNativeACPConfig(t *testing.T, config modelConfigFile) string {
	t.Helper()
	normalized, err := normalizeModelConfig(config)
	if err != nil {
		t.Fatalf("normalize native config: %v", err)
	}
	digest, err := modelConfigDigest(normalized)
	if err != nil {
		t.Fatalf("digest native config: %v", err)
	}
	return digest
}

func digestModelConfigFixture(t *testing.T, apiKey string) modelConfigFile {
	t.Helper()
	config := validDecodedModelConfig(t)
	makeOpenAIModel(&config.Models[0])
	config.Models[0].APIKey = apiKey
	config.Models[0].Alias = "zeta"
	config.Models[0].Uses = []string{"primer", "delegate"}
	config.Models[0].Capabilities.Thinking = true
	config.Models[0].Efforts = []string{"high", "none", "low"}
	config.Models[0].DefaultEffort = "high"
	config.PrimerDefault = "zeta"
	// alpha shares zeta's provider target but is delegate-only (not primer):
	// compileProductionModels rejects two primer-tagged aliases resolving to
	// the identical provider target (see
	// TestProductionModelsRejectsSharedPrimerProviderTargets), while a
	// delegate-only alias sharing a target with a primer is still the
	// explicitly supported multi-alias-per-target shape (newModelRoutingClient).
	alpha := config.Models[0]
	alpha.Alias = "alpha"
	alpha.Uses = []string{"delegate"}
	config.Models = append(config.Models, alpha)
	return config
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func cloneNormalizedModelConfig(input normalizedModelConfig) normalizedModelConfig {
	cloned := input
	if input.NativeACP != nil {
		cloned.NativeACP = make(map[string]normalizedNativeACPProfile, len(input.NativeACP))
		for harness, profile := range input.NativeACP {
			profile.Models = append([]string(nil), profile.Models...)
			if profile.ModelOptions != nil {
				profile.ModelOptions = make([]normalizedNativeACPModel, len(profile.ModelOptions))
				for i, option := range profile.ModelOptions {
					profile.ModelOptions[i] = option
					profile.ModelOptions[i].Efforts = append([]model.Effort(nil), option.Efforts...)
				}
			}
			cloned.NativeACP[harness] = profile
		}
	}
	cloned.Models = make([]normalizedModelTarget, len(input.Models))
	for i, target := range input.Models {
		cloned.Models[i] = target
		cloned.Models[i].Model = target.Model.Clone()
		cloned.Models[i].Uses = append([]string(nil), target.Uses...)
		cloned.Models[i].Efforts = append([]model.Effort(nil), target.Efforts...)
	}
	return cloned
}
