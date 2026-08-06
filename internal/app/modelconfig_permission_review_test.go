package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecodeModelConfigPermissionReviewSection covers the five Step 1 cases
// for the optional permission_review section: absent, present, missing
// model, an unknown nested field, and an explicit top-level null.
//
// The "missing model" case only asserts that decoding succeeds with an empty
// Model field. Per the task's own text, "Validation of Model != "" happens
// in Task 4's resolution step" — decodeModelConfig has no place to reject an
// empty (but present) model string today, so that rejection is not asserted
// here. See the task report for detail.
func TestDecodeModelConfigPermissionReviewSection(t *testing.T) {
	t.Run("absent section", func(t *testing.T) {
		config, err := decodeModelConfig([]byte(validLMStudioModelConfig))
		if err != nil {
			t.Fatalf("decodeModelConfig() error = %v", err)
		}
		if config.PermissionReview != nil {
			t.Fatalf("PermissionReview = %#v, want nil", config.PermissionReview)
		}
	})

	t.Run("present model and strict", func(t *testing.T) {
		config := decodeModelConfigWithPermissionReview(t, `{"model":"haiku","strict":true}`)
		if config.PermissionReview == nil {
			t.Fatal("PermissionReview = nil, want parsed section")
		}
		if config.PermissionReview.Model != "haiku" || !config.PermissionReview.Strict {
			t.Fatalf("PermissionReview = %#v, want {Model:\"haiku\" Strict:true}", config.PermissionReview)
		}
	})

	t.Run("strict without model decodes; Task 4 enforces the required field", func(t *testing.T) {
		config := decodeModelConfigWithPermissionReview(t, `{"strict":true}`)
		if config.PermissionReview == nil {
			t.Fatal("PermissionReview = nil, want parsed section")
		}
		if config.PermissionReview.Model != "" || !config.PermissionReview.Strict {
			t.Fatalf("PermissionReview = %#v, want {Model:\"\" Strict:true}", config.PermissionReview)
		}
	})

	t.Run("unknown field inside the section is rejected", func(t *testing.T) {
		input := modelConfigJSONWithPermissionReview(`{"model":"haiku","future":true}`)
		_, err := decodeModelConfig([]byte(input))
		if err == nil {
			t.Fatal("decodeModelConfig() error = nil, want unknown permission_review field rejection")
		}
		var configErr *ModelConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("decodeModelConfig() error = %T %v, want *ModelConfigError", err, err)
		}
		if strings.Contains(err.Error(), "future") {
			t.Errorf("decodeModelConfig() exposed attacker-controlled field name: %v", err)
		}
	})

	t.Run("explicit null is rejected", func(t *testing.T) {
		input := modelConfigJSONWithPermissionReview(`null`)
		_, err := decodeModelConfig([]byte(input))
		if err == nil {
			t.Fatal("decodeModelConfig() error = nil, want explicit null rejection")
		}
		var configErr *ModelConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("decodeModelConfig() error = %T %v, want *ModelConfigError", err, err)
		}
		if !strings.Contains(err.Error(), "permission_review") {
			t.Fatalf("decodeModelConfig() error = %q, want to name permission_review", err)
		}
	})
}

// TestNormalizeModelConfigPermissionReviewSection covers Task 4's Layer 1
// resolution of the optional permission_review section: an absent section
// normalizes to a nil normalized.PermissionReview, and a present section
// naming a model with tools + structured_output_with_tools normalizes its
// Model alias and Strict flag unchanged. No `uses` tag is required on the
// bound model row (design's explicit deviation from claude_code_small_model's
// "uses" precedent): permission_review.model is the binding.
func TestNormalizeModelConfigPermissionReviewSection(t *testing.T) {
	t.Run("absent section normalizes to nil", func(t *testing.T) {
		config := validDecodedModelConfig(t)
		normalized, err := normalizeModelConfig(config)
		if err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
		if normalized.PermissionReview != nil {
			t.Fatalf("PermissionReview = %+v, want nil", normalized.PermissionReview)
		}
	})

	t.Run("present section normalizes Model and Strict", func(t *testing.T) {
		config := validDecodedModelConfig(t)
		classifier := config.Models[0]
		classifier.Alias = "classifier"
		classifier.Uses = []string{"delegate"}
		classifier.Capabilities.StructuredOutput = true
		classifier.Capabilities.StructuredOutputWithTools = true
		config.Models = append(config.Models, classifier)
		config.PermissionReview = &permissionReviewConfig{Model: "classifier", Strict: true}

		normalized, err := normalizeModelConfig(config)
		if err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
		if normalized.PermissionReview == nil || normalized.PermissionReview.Model != "classifier" || !normalized.PermissionReview.Strict {
			t.Fatalf("PermissionReview = %+v, want {Model:classifier Strict:true}", normalized.PermissionReview)
		}
	})
}

// TestDecodeModelConfigPermissionReviewFromDisk mirrors modelconfig_test.go's
// "write a temp file 0600 and load" fixture style, proving the section
// survives the on-disk read path (readModelConfigFile), not just an
// in-memory decodeModelConfig call.
func TestDecodeModelConfigPermissionReviewFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	content := []byte(modelConfigJSONWithPermissionReview(`{"model":"haiku","strict":false}`))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	data, exists, err := readModelConfigFile(path)
	if err != nil || !exists {
		t.Fatalf("readModelConfigFile() = (%v, %v, %v), want (data, true, nil)", data, exists, err)
	}
	config, err := decodeModelConfig(data)
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v", err)
	}
	if config.PermissionReview == nil || config.PermissionReview.Model != "haiku" || config.PermissionReview.Strict {
		t.Fatalf("PermissionReview = %#v, want {Model:\"haiku\" Strict:false}", config.PermissionReview)
	}
}

func decodeModelConfigWithPermissionReview(t *testing.T, permissionJSON string) modelConfigFile {
	t.Helper()
	config, err := decodeModelConfig([]byte(modelConfigJSONWithPermissionReview(permissionJSON)))
	if err != nil {
		t.Fatalf("decode permission_review config: %v", err)
	}
	return config
}

func modelConfigJSONWithPermissionReview(permissionJSON string) string {
	return strings.Replace(validLMStudioModelConfig, `"version": 2,`, `"version": 2, "permission_review":`+permissionJSON+`,`, 1)
}

// modelConfigJSONWithUnusedClassifier returns validLMStudioModelConfig
// extended with a second model row, "classifier", bound as
// permission_review.model. Its "uses" field is entirely omitted from the
// JSON object — not just set to an empty array — matching how an operator
// would actually write a dedicated permission-review classifier that is
// neither primer- nor delegate-capable.
func modelConfigJSONWithUnusedClassifier() string {
	const classifierModelJSON = `"default_effort": "none"
  },{
    "alias": "classifier",
    "provider": "openai",
    "api_format": "openai-responses",
    "base_url": "https://api.openai.com/v1",
    "model": "classifier-model",
    "api_key": "test-secret-do-not-log",
    "capabilities": {
      "tools": true,
      "structured_output": true,
      "structured_output_with_tools": true
    },
    "efforts": ["none"],
    "default_effort": "none"
  }]`
	withClassifier := strings.Replace(validLMStudioModelConfig, `"default_effort": "none"
  }]`, classifierModelJSON, 1)
	return strings.Replace(withClassifier, `"version": 2,`, `"version": 2, "permission_review": {"model": "classifier"},`, 1)
}

// TestNormalizeModelConfigUnusedClassifierUses proves the fix for the gap a
// phase-boundary review found: a model row whose "uses" field is entirely
// omitted from the JSON (not merely an empty array) decodes and normalizes
// successfully, ending up with an empty Uses rather than a validation error,
// and is still resolved as the permission_review binding by alias.
func TestNormalizeModelConfigUnusedClassifierUses(t *testing.T) {
	wire, err := decodeModelConfig([]byte(modelConfigJSONWithUnusedClassifier()))
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v", err)
	}
	classifierRow, ok := modelConfigTargetByAlias(wire.Models, "classifier")
	if !ok {
		t.Fatal("decoded config missing the classifier model row")
	}
	if classifierRow.Uses != nil {
		t.Fatalf("decoded classifier Uses = %#v, want nil (field omitted from JSON)", classifierRow.Uses)
	}

	normalized, err := normalizeModelConfig(wire)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	classifier, ok := modelConfigNormalizedTargetByAlias(normalized.Models, "classifier")
	if !ok {
		t.Fatal("normalized config missing the classifier model")
	}
	if len(classifier.Uses) != 0 {
		t.Fatalf("normalized classifier Uses = %v, want empty", classifier.Uses)
	}
	if normalized.PermissionReview == nil || normalized.PermissionReview.Model != "classifier" {
		t.Fatalf("PermissionReview = %+v, want Model=classifier", normalized.PermissionReview)
	}
}

func modelConfigTargetByAlias(targets []modelTargetConfig, alias string) (modelTargetConfig, bool) {
	for _, target := range targets {
		if target.Alias == alias {
			return target, true
		}
	}
	return modelTargetConfig{}, false
}

func modelConfigNormalizedTargetByAlias(targets []normalizedModelTarget, alias string) (normalizedModelTarget, bool) {
	for _, target := range targets {
		if target.Alias == alias {
			return target, true
		}
	}
	return normalizedModelTarget{}, false
}
