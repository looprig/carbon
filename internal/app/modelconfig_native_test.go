package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
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
			if tt.name == "enabled managed" && profile.Models != nil {
				t.Fatalf("managed profile models = %#v, want nil", profile.Models)
			}
			if tt.name == "enabled explicit" && strings.Join(profile.Models, ",") != "alpha,zeta" {
				t.Fatalf("explicit profile models = %#v, want sorted aliases", profile.Models)
			}
		})
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeModelConfig(decodeModelConfigWithNativeACP(t, tt.nativeJSON))
			assertNativeModelConfigValidationError(t, err)
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

func TestDecodeModelConfigRejectsNullNativeACPModels(t *testing.T) {
	_, err := decodeModelConfig([]byte(modelConfigJSONWithNativeACP(`{"codex":{"enabled":true,"models":null}}`)))
	if err == nil {
		t.Fatal("decodeModelConfig() error = nil, want null models rejection")
	}
	var configErr *ModelConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("decodeModelConfig() error = %T %v, want *ModelConfigError", err, err)
	}
}

func TestModelConfigNativeDelegateDefaults(t *testing.T) {
	t.Run("managed allows omitted model and effort", func(t *testing.T) {
		config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true}}`)
		config = setAllCodexDefaults(config, "", "")
		normalized, err := normalizeModelConfig(config)
		if err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
		for _, value := range normalized.DelegateDefaults {
			if value.Source != loop.RuntimeSourceNative || value.Model != "" || value.Effort != model.EffortNone {
				t.Fatalf("native managed default = %#v, want native with no model/effort identity", value)
			}
		}
	})

	t.Run("explicit profile requires a configured model alias", func(t *testing.T) {
		config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":["native-codex"]}}`)
		config = setAllCodexDefaults(config, "native-codex", "")
		if _, err := normalizeModelConfig(config); err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}

		missing := setAllCodexDefaults(config, "", "")
		if err := normalizeOnly(missing); err == nil {
			t.Fatal("normalizeModelConfig() omitted explicit model error = nil, want validation error")
		}

		invalid := setAllCodexDefaults(config, "not-configured", "")
		assertNativeModelConfigValidationError(t, normalizeOnly(invalid))
	})

	t.Run("native effort override is rejected", func(t *testing.T) {
		config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true}}`)
		config = setAllCodexDefaults(config, "", "high")
		assertNativeModelConfigValidationError(t, normalizeOnly(config))
	})

	t.Run("disabled profile is unavailable", func(t *testing.T) {
		config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":false}}`)
		config = setAllCodexDefaults(config, "", "")
		assertNativeModelConfigValidationError(t, normalizeOnly(config))
	})

	t.Run("gateway defaults remain concrete", func(t *testing.T) {
		config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true}}`)
		normalized, err := normalizeModelConfig(config)
		if err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
		for _, value := range normalized.DelegateDefaults {
			if value.Source != loop.RuntimeSourceGateway || value.Model != "local" || value.Effort != model.EffortNone {
				t.Fatalf("gateway default = %#v, want unchanged concrete gateway selection", value)
			}
		}
	})
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

	sourceChanged := cloneNormalizedModelConfig(base)
	sourceChanged.DelegateDefaults[0].Source = loop.RuntimeSourceNative
	sourceChangedDigest, err := modelConfigDigest(sourceChanged)
	if err != nil {
		t.Fatalf("digest source change: %v", err)
	}
	if sourceChangedDigest == baseDigest {
		t.Fatal("delegate source identity change did not change digest")
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
	return strings.Replace(validLMStudioModelConfig, `"version": 1,`, `"version": 1, "native_acp":`+nativeJSON+`,`, 1)
}

func setAllCodexDefaults(config modelConfigFile, modelAlias, effort string) modelConfigFile {
	for role := range config.DelegateDefaults {
		value := config.DelegateDefaults[role]
		value.Harness = "codex"
		value.Source = "native"
		value.Model = modelAlias
		value.Effort = effort
		config.DelegateDefaults[role] = value
	}
	return config
}

func normalizeOnly(config modelConfigFile) error {
	_, err := normalizeModelConfig(config)
	return err
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
