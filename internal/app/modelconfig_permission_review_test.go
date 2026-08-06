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
