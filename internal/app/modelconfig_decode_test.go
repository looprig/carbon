package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

func TestDecodeModelConfigErrorBoundsCompleteMessage(t *testing.T) {
	const secret = "test-secret-do-not-log"
	err := modelConfigFailure(
		strings.Repeat("very-long-operation/", 128)+secret,
		errors.New(strings.Repeat("very-long-cause/", 128)+secret),
	)
	if len(err.Error()) > 512 {
		t.Errorf("ModelConfigError.Error() length = %d, want <= 512", len(err.Error()))
	}
	if len(err.operation) > maxModelConfigErrorOperationBytes {
		t.Errorf("stored operation length = %d, want <= %d", len(err.operation), maxModelConfigErrorOperationBytes)
	}
	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		t.Fatal("errors.Unwrap(ModelConfigError) = nil, want bounded cause")
	}
	if len(unwrapped.Error()) > maxModelConfigErrorCauseBytes {
		t.Errorf("stored cause length = %d, want <= %d", len(unwrapped.Error()), maxModelConfigErrorCauseBytes)
	}
	for name, value := range map[string]string{
		"rendered error":  err.Error(),
		"operation":       err.operation,
		"unwrapped cause": unwrapped.Error(),
	} {
		if strings.Contains(value, secret) {
			t.Errorf("%s exposed API-key sentinel", name)
		}
	}
	var target *ModelConfigError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(%T) = false, want true", err)
	}
}

func TestDecodeModelConfigBoundsUnknownFieldWithoutAPIKeyValue(t *testing.T) {
	const secret = "test-secret-do-not-log"
	field := strings.Repeat("future_field_", 256)
	input := strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 2, "`+field+`": true`, 1)
	input = strings.Replace(input, `"api_key": ""`, `"api_key": "`+secret+`"`, 1)

	_, err := decodeModelConfig([]byte(input))
	if err == nil {
		t.Fatal("decodeModelConfig(long unknown field) error = nil, want error")
	}
	if len(err.Error()) > 512 {
		t.Errorf("decode error length = %d, want <= 512", len(err.Error()))
	}
	if strings.Contains(err.Error(), "future_field_") {
		t.Errorf("decode error exposed attacker-controlled field name: %q", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("decode error leaked API-key bytes: %v", err)
	}
}

const validLMStudioModelConfig = `{
  "version": 2,
  "primer_default": "local",
  "models": [{
    "alias": "local",
    "description": "Local in-process coding model.",
    "provider": "lmstudio",
    "api_format": "openai",
    "base_url": "http://localhost:1234/v1",
    "model": "qwen3-coder",
    "api_key": "",
    "uses": ["primer", "delegate"],
    "capabilities": {
      "tools": true,
      "thinking": false,
      "images": false,
      "prompt_caching": false,
      "structured_output": false,
      "structured_output_with_tools": false
    },
    "efforts": ["none"],
    "default_effort": "none"
  }]
}`

func TestModelConfigWithoutDelegateDefaultsIsValid(t *testing.T) {
	decoded, err := decodeModelConfig([]byte(validLMStudioModelConfig))
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v", err)
	}
	if _, err := normalizeModelConfig(decoded); err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
}

func TestDecodeModelConfigAcceptsSchemaV3CredentialReference(t *testing.T) {
	input := strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 3`, 1)
	input = strings.NewReplacer(
		`"provider": "lmstudio"`, `"provider": "openai"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
		`"api_key": ""`, `"credential_ref": "credential://openai/personal"`,
	).Replace(input)

	got, err := decodeModelConfig([]byte(input))
	if err != nil {
		t.Fatalf("decodeModelConfig(v3) error = %v", err)
	}
	if got.Version != 3 || got.Models[0].CredentialRef != "credential://openai/personal" {
		t.Fatalf("decodeModelConfig(v3) = %+v, want version 3 and credential reference", got)
	}
}

func TestDecodeModelConfigV3RejectsUnknownModelField(t *testing.T) {
	input := strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 3`, 1)
	input = strings.Replace(input, `"alias": "local"`, `"alias": "local", "future": true`, 1)
	if _, err := decodeModelConfig([]byte(input)); err == nil {
		t.Fatal("decodeModelConfig(v3 unknown field) error = nil, want strict rejection")
	}
}

func TestLoadV2ModelConfigDoesNotRewriteBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	want := []byte(validLMStudioModelConfig)
	writeModelConfigFixture(t, path, want, 0o600)

	if _, err := loadProductionModelsFrom(path, func(model.Model, modelClientInput) (inference.Client, error) {
		return &fakeLLM{}, nil
	}); err != nil {
		t.Fatalf("loadProductionModelsFrom(v2) error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read loaded v2 file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("v2 file changed after load: got %q, want %q", got, want)
	}
}

func TestDecodeModelConfigRejectsRemovedDelegateDefaults(t *testing.T) {
	// This wire value is a strict-rejection fixture for the removed field; it
	// must not be accepted or migrated.
	input := strings.Replace(
		validLMStudioModelConfig,
		`"models":`,
		`"delegate_defaults":{"planner":{"harness":"codex","model":"local","effort":"none"}}, "models":`,
		1,
	)

	_, err := decodeModelConfig([]byte(input))
	if err == nil {
		t.Fatal("decodeModelConfig(delegate_defaults) error = nil, want strict unknown-field rejection")
	}
	var configErr *ModelConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("decodeModelConfig(delegate_defaults) error = %T, want *ModelConfigError", err)
	}
	if configErr.operation != "decode" {
		t.Fatalf("decodeModelConfig(delegate_defaults) operation = %q, want decode", configErr.operation)
	}
	if len(err.Error()) > maxModelConfigErrorBytes {
		t.Fatalf("decodeModelConfig(delegate_defaults) error length = %d, want bounded", len(err.Error()))
	}
}

func TestDecodeModelConfig(t *testing.T) {
	t.Run("valid no-auth LM Studio file", func(t *testing.T) {
		got, err := decodeModelConfig([]byte(validLMStudioModelConfig))
		if err != nil {
			t.Fatalf("decodeModelConfig() error = %v", err)
		}
		if got.Version != 2 || len(got.Models) != 1 || got.Models[0].APIKey != "" {
			t.Fatalf("decodeModelConfig() = %+v", got)
		}
	})

	t.Run("valid API-key file", func(t *testing.T) {
		input := strings.NewReplacer(
			`"provider": "lmstudio"`, `"provider": "openai"`,
			`"api_format": "openai"`, `"api_format": "openai-responses"`,
			`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
			`"api_key": ""`, `"api_key": "test-secret-do-not-log"`,
		).Replace(validLMStudioModelConfig)
		got, err := decodeModelConfig([]byte(input))
		if err != nil {
			t.Fatalf("decodeModelConfig() error = %v", err)
		}
		if got.Models[0].APIKey != "test-secret-do-not-log" {
			t.Fatal("decodeModelConfig() did not retain the private credential input")
		}
	})

	invalid := []struct {
		name  string
		input []byte
		want  string
	}{
		{name: "empty input", input: nil},
		{name: "malformed JSON", input: []byte(`{"version":`)},
		{name: "two top-level values", input: []byte(validLMStudioModelConfig + ` {}`)},
		{name: "unknown top-level field", input: replaceOnce(t, validLMStudioModelConfig, `"version": 2`, `"version": 2, "future": true`)},
		{name: "unknown model row field", input: replaceOnce(t, validLMStudioModelConfig, `"alias": "local"`, `"alias": "local", "future": true`)},
		{name: "unknown capability field", input: replaceOnce(t, validLMStudioModelConfig, `"tools": true`, `"tools": true, "audio": true`)},
		{name: "missing version", input: replaceOnce(t, validLMStudioModelConfig, `"version": 2,`, ``), want: "version"},
		{name: "zero version", input: replaceOnce(t, validLMStudioModelConfig, `"version": 2`, `"version": 0`), want: "version"},
		{name: "version 1 is rejected", input: replaceOnce(t, validLMStudioModelConfig, `"version": 2`, `"version": 1`), want: "version"},
		{name: "future version", input: replaceOnce(t, validLMStudioModelConfig, `"version": 2`, `"version": 4`), want: "version"},
		{name: "invalid UTF-8", input: append([]byte(`{"version":1,"primer_default":"`), 0xff, '"', '}')},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeModelConfig(tt.input)
			assertModelConfigDecodeError(t, err, tt.want)
		})
	}
}

func TestDecodeModelConfigRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	tests := []struct {
		name  string
		input string
		key   string
	}{
		{name: "top level", input: `{"version":2,"version":2}`, key: "version"},
		{name: "model row in array", input: `{"version":2,"models":[{"alias":"a","alias":"b"}]}`, key: "alias"},
		{name: "capabilities", input: `{"version":2,"models":[{"capabilities":{"tools":true,"tools":false}}]}`, key: "tools"},
		{name: "nested object in array", input: `{"version":2,"models":[{"uses":[{"deep":1,"deep":2}]}]}`, key: "deep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeModelConfig([]byte(tt.input))
			assertModelConfigDecodeError(t, err, "")
			if strings.Contains(err.Error(), tt.key) {
				t.Errorf("decodeModelConfig() exposed attacker-controlled duplicate key %q: %v", tt.key, err)
			}
		})
	}
}

func TestDecodeModelConfigACPLaunchers(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "valid launchers block",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}],
				"acp_launchers":{"claude-code":{"executable":"/usr/local/bin/claude-code-acp"},"codex":{"executable":"/usr/local/bin/codex-acp"}}}`,
		},
		{
			name: "omitted launchers block is valid",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}]}`,
		},
		{
			name: "unknown field inside a launcher entry is rejected",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}],
				"acp_launchers":{"claude-code":{"executable":"/usr/local/bin/claude-code-acp","args":["x"]}}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeModelConfig([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeModelConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func assertModelConfigDecodeError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("decodeModelConfig() error = nil, want error")
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Errorf("decodeModelConfig() error = %q, want substring %q", err, want)
	}
	if len(err.Error()) > 512 {
		t.Errorf("decodeModelConfig() error length = %d, want bounded", len(err.Error()))
	}
	if strings.Contains(err.Error(), "test-secret-do-not-log") {
		t.Errorf("decodeModelConfig() leaked API key: %v", err)
	}
}

func replaceOnce(t *testing.T, input, old, replacement string) []byte {
	t.Helper()
	if !strings.Contains(input, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return []byte(strings.Replace(input, old, replacement, 1))
}
