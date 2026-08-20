package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestModelConfigNativeACPProfilesDistinguishAbsentManagedAndExplicit(t *testing.T) {
	tests := []struct {
		name       string
		nativeJSON string
		want       []string
		wantNil    bool
	}{
		{name: "absent", wantNil: true},
		{name: "enabled managed", nativeJSON: `{"codex":{"enabled":true}}`, want: []string{"codex"}},
		{name: "enabled null managed", nativeJSON: `{"codex":{"enabled":true,"models":null}}`, want: []string{"codex"}},
		{name: "enabled explicit", nativeJSON: `{"codex":{"enabled":true,"models":["zeta","alpha"]}}`, want: []string{"codex"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validDecodedModelConfig(t)
			if tt.nativeJSON != "" {
				config = decodeModelConfigWithNativeACP(t, tt.nativeJSON)
			}
			if tt.wantNil && config.NativeACP != nil {
				t.Fatalf("decoded NativeACP = %#v, want nil", config.NativeACP)
			}
			normalized, err := normalizeModelConfig(config)
			if err != nil {
				t.Fatalf("normalizeModelConfig() error = %v", err)
			}
			if tt.wantNil && normalized.NativeACP != nil {
				t.Fatalf("normalized NativeACP = %#v, want nil", normalized.NativeACP)
			}
			if tt.wantNil {
				return
			}
			if len(normalized.NativeACP) != len(tt.want) {
				t.Fatalf("normalized NativeACP = %#v, want %d profile", normalized.NativeACP, len(tt.want))
			}
			profile, ok := normalized.NativeACP[tt.want[0]]
			if !ok {
				t.Fatalf("normalized NativeACP = %#v, want profile %q", normalized.NativeACP, tt.want[0])
			}
			if profile.Harness != tt.want[0] || !profile.Enabled {
				t.Fatalf("normalized native profile = %#v, want enabled %q", profile, tt.want[0])
			}
			if (tt.name == "enabled managed" || tt.name == "enabled null managed") && (profile.Models != nil || profile.ModelOptions != nil) {
				t.Fatalf("managed profile models/options = %#v/%#v, want nil", profile.Models, profile.ModelOptions)
			}
			if tt.name == "enabled explicit" && strings.Join(profile.Models, ",") != "alpha,zeta" {
				t.Fatalf("explicit profile models = %#v, want sorted aliases", profile.Models)
			}
		})
	}
}

func TestModelConfigNativeACPModelsAcceptLegacyAndStructuredEntries(t *testing.T) {
	config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		"legacy-model",
		{"model":"gpt-5.6-sol","efforts":["max","minimal","high","none","xhigh","low","medium"],"default_effort":"xhigh"}
	]}}`)
	normalized, err := normalizeModelConfig(config)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	profile := normalized.NativeACP["codex"]
	if got, want := strings.Join(profile.Models, ","), "gpt-5.6-sol,legacy-model"; got != want {
		t.Fatalf("normalized model IDs = %q, want %q", got, want)
	}
	options := profile.ModelOptions
	if len(options) != 2 {
		t.Fatalf("normalized model options = %#v, want 2 entries", options)
	}
	assertNativeACPModelOption(t, options[0], "gpt-5.6-sol", []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, "xhigh")
	assertNativeACPModelOption(t, options[1], "legacy-model", []string{"none"}, "none")
}

func TestModelConfigNativeACPStructuredEntryOrderDoesNotMutateInput(t *testing.T) {
	config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		{"model":"zeta","efforts":["max","none"],"default_effort":"max"},
		{"model":"alpha","efforts":["high","low"],"default_effort":"low"}
	]}}`)
	before := cloneNativeACPWireConfig(config)
	if _, err := normalizeModelConfig(config); err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	if !reflect.DeepEqual(config, before) {
		t.Fatalf("normalizeModelConfig mutated unrelated wire config: got %#v, want %#v", config, before)
	}
}

func TestModelConfigRejectsInvalidNativeACPProfiles(t *testing.T) {
	tests := []struct {
		name       string
		nativeJSON string
	}{
		{name: "explicit empty models", nativeJSON: `{"codex":{"enabled":true,"models":[]}}`},
		{name: "unknown profile", nativeJSON: `{"cursor":{"enabled":true}}`},
		{name: "invalid alias", nativeJSON: `{"codex":{"enabled":true,"models":["bad/model"]}}`},
		{name: "duplicate aliases", nativeJSON: `{"codex":{"enabled":true,"models":["same","same"]}}`},
		{name: "duplicate structured model IDs", nativeJSON: `{"codex":{"enabled":true,"models":[
			{"model":"same","efforts":["medium"],"default_effort":"medium"},
			{"model":"same","efforts":["high"],"default_effort":"high"}
		]}}`},
		{name: "blank structured model", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"   ","efforts":["medium"],"default_effort":"medium"}]}}`},
		{name: "empty structured model", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"","efforts":["medium"],"default_effort":"medium"}]}}`},
		{name: "missing structured model", nativeJSON: `{"codex":{"enabled":true,"models":[{"efforts":["medium"],"default_effort":"medium"}]}}`},
		{name: "empty structured efforts", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":[],"default_effort":"medium"}]}}`},
		{name: "missing structured efforts", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","default_effort":"medium"}]}}`},
		{name: "duplicate structured efforts", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":["medium","medium"],"default_effort":"medium"}]}}`},
		{name: "invalid structured effort", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":["unsupported"],"default_effort":"unsupported"}]}}`},
		{name: "invalid structured default with valid efforts", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":["medium"],"default_effort":"unsupported"}]}}`},
		{name: "missing structured default", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":["medium"]}]}}`},
		{name: "default outside structured efforts", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"same","efforts":["medium"],"default_effort":"high"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertNativeACPRejected(t, tt.nativeJSON)
		})
	}
}

func TestDecodeModelConfigRejectsUnknownNativeACPFields(t *testing.T) {
	input := modelConfigJSONWithNativeACP(`{"codex":{"enabled":true,"future":true}}`)
	_, err := decodeModelConfig([]byte(input))
	if err == nil {
		t.Fatal("decodeModelConfig() error = nil, want unknown native profile field rejection")
	}
	if !strings.Contains(err.Error(), "model configuration decode failed") {
		t.Fatalf("decodeModelConfig() error = %v, want bounded decode failure", err)
	}
}

func TestDecodeModelConfigAcceptsNullNativeACPModelsAsManaged(t *testing.T) {
	config, err := decodeModelConfig([]byte(modelConfigJSONWithNativeACP(`{"codex":{"enabled":true,"models":null}}`)))
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v, want explicit null to mean managed selection", err)
	}
	profile := config.NativeACP["codex"]
	if profile.Models != nil {
		t.Fatalf("decoded null models = %#v, want nil managed selection", profile.Models)
	}
}

func TestDecodeModelConfigRejectsMalformedNativeACPModelEntries(t *testing.T) {
	tests := []struct {
		name       string
		nativeJSON string
	}{
		{name: "unknown object field", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"m","efforts":["medium"],"default_effort":"medium","future":true}]}}`},
		{name: "models wrong type", nativeJSON: `{"codex":{"enabled":true,"models":"m"}}`},
		{name: "entry wrong type", nativeJSON: `{"codex":{"enabled":true,"models":[42]}}`},
		{name: "model wrong type", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":42,"efforts":["medium"],"default_effort":"medium"}]}}`},
		{name: "efforts wrong type", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"m","efforts":"medium","default_effort":"medium"}]}}`},
		{name: "default wrong type", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"m","efforts":["medium"],"default_effort":42}]}}`},
		{name: "trailing entry tokens", nativeJSON: `{"codex":{"enabled":true,"models":[{"model":"m","efforts":["medium"],"default_effort":"medium"} {"model":"other"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(modelConfigJSONWithNativeACP(tt.nativeJSON))
			_, err := decodeModelConfig(input)
			if err == nil {
				t.Fatal("decodeModelConfig() error = nil, want malformed native ACP entry rejection")
			}
			var configErr *ModelConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("decodeModelConfig() error = %T %v, want *ModelConfigError", err, err)
			}
		})
	}
}

func TestNativeACPModelEntryRejectsTrailingJSONTokens(t *testing.T) {
	tests := []string{
		`"legacy" "trailing"`,
		`{"model":"m","efforts":["medium"],"default_effort":"medium"} {"trailing":true}`,
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			var entry nativeACPModelConfig
			if err := entry.UnmarshalJSON([]byte(input)); err == nil {
				t.Fatal("nativeACPModelConfig.UnmarshalJSON() error = nil, want trailing-token rejection")
			}
		})
	}
}

func TestNativeACPModelEntryRejectsDuplicateJSONKeysWhenUnmarshaledDirectly(t *testing.T) {
	modelEntry := `{"model":"first","efforts":["medium"],"default_effort":"medium","model":"last"}`
	var entry nativeACPModelConfig
	if err := entry.UnmarshalJSON([]byte(modelEntry)); err == nil {
		t.Fatal("nativeACPModelConfig.UnmarshalJSON() error = nil, want duplicate-key rejection")
	}

	profileEntry := `{"enabled":true,"models":[{"model":"first","efforts":["medium"],"default_effort":"medium","default_effort":"high"}]}`
	var profile nativeACPProfileConfig
	if err := profile.UnmarshalJSON([]byte(profileEntry)); err == nil {
		t.Fatal("nativeACPProfileConfig.UnmarshalJSON() error = nil, want nested duplicate-key rejection")
	}
}

func TestModelConfigNativeACPIdentityChangesDigestWithoutSecrets(t *testing.T) {
	baseConfig := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true}}`)
	base, err := normalizeModelConfig(baseConfig)
	if err != nil {
		t.Fatalf("normalize base: %v", err)
	}
	baseDigest, err := modelConfigDigest(base)
	if err != nil {
		t.Fatalf("digest base: %v", err)
	}

	changedConfig := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":["native-codex"]}}`)
	changed, err := normalizeModelConfig(changedConfig)
	if err != nil {
		t.Fatalf("normalize changed: %v", err)
	}
	changedDigest, err := modelConfigDigest(changed)
	if err != nil {
		t.Fatalf("digest changed: %v", err)
	}
	if changedDigest == baseDigest {
		t.Fatal("native profile identity change did not change digest")
	}

	material, err := secretFreeModelConfigJSON(changed)
	if err != nil {
		t.Fatalf("secret-free projection: %v", err)
	}
	for _, value := range []string{string(material), fmt.Sprintf("%v|%+v|%#v", changed, changed, changed)} {
		if strings.Contains(value, "test-secret-do-not-log") || strings.Contains(value, "HOME=") || strings.Contains(value, "API_KEY") {
			t.Fatalf("native digest material exposed secret or login identity: %q", value)
		}
	}
}

func decodeModelConfigWithNativeACP(t *testing.T, nativeJSON string) modelConfigFile {
	t.Helper()
	config, err := decodeModelConfig([]byte(modelConfigJSONWithNativeACP(nativeJSON)))
	if err != nil {
		t.Fatalf("decode native config: %v", err)
	}
	return config
}

func modelConfigJSONWithNativeACP(nativeJSON string) string {
	return strings.Replace(validLMStudioModelConfig, `"version": 2,`, `"version": 2, "native_acp":`+nativeJSON+`,`, 1)
}

func assertNativeModelConfigValidationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("normalizeModelConfig() error = nil, want validation error")
	}
	var configErr *ModelConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %T %v, want *ModelConfigError", err, err)
	}
	if len(err.Error()) > maxModelConfigErrorBytes {
		t.Fatalf("error length = %d, want bounded", len(err.Error()))
	}
}

func assertNativeACPRejected(t *testing.T, nativeJSON string) {
	t.Helper()
	config, decodeErr := decodeModelConfig([]byte(modelConfigJSONWithNativeACP(nativeJSON)))
	if decodeErr != nil {
		assertNativeModelConfigValidationError(t, decodeErr)
		return
	}
	_, normalizeErr := normalizeModelConfig(config)
	assertNativeModelConfigValidationError(t, normalizeErr)
}

func assertNativeACPModelOption(t *testing.T, option normalizedNativeACPModel, wantModel string, wantEfforts []string, wantDefault string) {
	t.Helper()
	if got := option.Model; got != wantModel {
		t.Errorf("native model option model = %q, want %q", got, wantModel)
	}
	if len(option.Efforts) != len(wantEfforts) {
		t.Fatalf("native model option efforts = %#v, want %#v", option.Efforts, wantEfforts)
	}
	for i, want := range wantEfforts {
		if got := modelConfigEffortName(option.Efforts[i]); got != want {
			t.Errorf("native model option effort[%d] = %q, want %q", i, got, want)
		}
	}
	if got := modelConfigEffortName(option.DefaultEffort); got != wantDefault {
		t.Errorf("native model option default effort = %q, want %q", got, wantDefault)
	}
}

func cloneNativeACPWireConfig(config modelConfigFile) modelConfigFile {
	clone := config
	clone.Models = append([]modelTargetConfig(nil), config.Models...)
	if config.NativeACP != nil {
		clone.NativeACP = make(map[string]nativeACPProfileConfig, len(config.NativeACP))
		for harness, profile := range config.NativeACP {
			clonedProfile := profile
			if profile.Models != nil {
				models := append([]nativeACPModelConfig(nil), (*profile.Models)...)
				for i := range models {
					models[i].Efforts = append([]string(nil), models[i].Efforts...)
				}
				clonedProfile.Models = &models
			}
			clone.NativeACP[harness] = clonedProfile
		}
	}
	return clone
}
