package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("errors.Unwrap(ModelConfigError) = %v, want nil for raw-cause safety", unwrapped)
	}
	for name, value := range map[string]string{
		"rendered error": err.Error(),
		"operation":      err.operation,
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

type secretBearingModelConfigCause struct{}

func (secretBearingModelConfigCause) Error() string {
	return "secret-bearing nested cause: test-secret-nested-cause"
}

func (secretBearingModelConfigCause) Unwrap() error { return errModelConfigMigrationConcurrent }

func TestModelConfigMigrationErrorClosesRawCauseChain(t *testing.T) {
	const secret = "test-secret-nested-cause"
	raw := secretBearingModelConfigCause{}
	err := modelConfigMigrationFailure(fmt.Errorf("migration detail: %s: %w", secret, raw))

	if errors.Unwrap(err) != nil {
		t.Fatal("migration error exposes an unwrap chain")
	}
	if !errors.Is(err, errModelConfigMigrationConcurrent) {
		t.Fatal("migration error lost safe concurrent-modification classification")
	}
	var extracted secretBearingModelConfigCause
	if errors.As(err, &extracted) {
		t.Fatal("errors.As reached the secret-bearing raw cause")
	}

	var logBuffer bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuffer, nil)).Error("migration", "error", err)
	encoded, marshalErr := json.Marshal(struct {
		Error error `json:"error"`
	}{err})
	if marshalErr != nil {
		t.Fatalf("json.Marshal(ModelConfigError) error = %v", marshalErr)
	}
	formatted := fmt.Sprintf("%v|%+v|%#v|%s|%s", err, err, err, logBuffer.String(), encoded)
	if strings.Contains(formatted, secret) {
		t.Fatalf("secret-bearing cause escaped through formatting or structured logging: %s", formatted)
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

func TestMigrateModelConfigV2ToV3IsExplicitDeterministicAndValidated(t *testing.T) {
	const secret = "test-secret-do-not-log-migration"
	input := strings.NewReplacer(
		`"provider": "lmstudio"`, `"provider": "openai"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
		`"api_key": ""`, `"api_key": "`+secret+`"`,
	).Replace(validLMStudioModelConfig)

	first, err := migrateModelConfigV2ToV3([]byte(input))
	if err != nil {
		t.Fatalf("migrateModelConfigV2ToV3() error = %v", err)
	}
	second, err := migrateModelConfigV2ToV3([]byte(input))
	if err != nil {
		t.Fatalf("second migrateModelConfigV2ToV3() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("migration output is nondeterministic: first=%q second=%q", first, second)
	}
	if !strings.Contains(string(first), `"version":3`) || !strings.Contains(string(first), secret) {
		t.Fatalf("migration output = %s, want v3 and preserved inline key", first)
	}
	decoded, err := decodeModelConfig(first)
	if err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatalf("normalize migrated config: %v", err)
	}
	if normalized.Version != modelConfigVersionV3 || normalized.Models[0].client.APIKey != secret {
		t.Fatalf("normalized migrated config = %#v, want v3 with inline key", normalized)
	}

	for name, invalid := range map[string]string{
		"already v3":      strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 3`, 1),
		"unknown version": strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 9`, 1),
		"ambiguous auth":  strings.Replace(validLMStudioModelConfig, `"api_key": "",`, `"api_key": "", "credential_ref": "credential://openai/personal",`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := migrateModelConfigV2ToV3([]byte(invalid)); err == nil {
				t.Fatal("migrateModelConfigV2ToV3() error = nil, want rejection")
			} else if strings.Contains(err.Error(), secret) {
				t.Fatalf("migration error exposed credential bytes: %v", err)
			}
		})
	}
}

func TestWriteMigratedModelConfigV2ToV3IsAtomicOwnerOnlyAndExplicit(t *testing.T) {
	const secret = "test-secret-do-not-log-writer"
	input := strings.NewReplacer(
		`"provider": "lmstudio"`, `"provider": "openai"`,
		`"api_format": "openai"`, `"api_format": "openai-responses"`,
		`"base_url": "http://localhost:1234/v1"`, `"base_url": "https://api.openai.com/v1"`,
		`"api_key": ""`, `"api_key": "`+secret+`"`,
	).Replace(validLMStudioModelConfig)
	path := filepath.Join(t.TempDir(), "models.json")
	writeModelConfigFixture(t, path, []byte(input), 0o600)
	want, err := migrateModelConfigV2ToV3([]byte(input))
	if err != nil {
		t.Fatalf("migrateModelConfigV2ToV3() error = %v", err)
	}

	if err := writeMigratedModelConfigV2ToV3(path); err != nil {
		t.Fatalf("writeMigratedModelConfigV2ToV3() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written migration = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat migrated config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("migrated config permissions = %04o, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read migration directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("migration directory entries = %#v, want only destination", entries)
	}

	beforeSecondWrite := append([]byte(nil), got...)
	if err := writeMigratedModelConfigV2ToV3(path); err == nil {
		t.Fatal("second writeMigratedModelConfigV2ToV3() error = nil, want already-v3 rejection")
	}
	afterSecondWrite, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after rejected migration: %v", err)
	}
	if !bytes.Equal(afterSecondWrite, beforeSecondWrite) {
		t.Fatal("rejected already-v3 migration changed the destination")
	}
}

// TestModelConfigV3LabelRoundTrips pins the optional display name through the v3 wire: it
// decodes, it survives an encode, and a file that omits it encodes back to a file that still
// omits it -- so adding the field cannot rewrite configurations that never used it.
func TestModelConfigV3LabelRoundTrips(t *testing.T) {
	t.Parallel()

	v3, err := migrateModelConfigV2ToV3([]byte(validLMStudioModelConfig))
	if err != nil {
		t.Fatalf("migrateModelConfigV2ToV3() error = %v", err)
	}
	if strings.Contains(string(v3), `"label"`) {
		t.Fatalf("migrated v2 config emitted a label it never had: %s", v3)
	}

	labelled := strings.Replace(string(v3), `"alias":"local",`, `"alias":"local","label":"Qwen3 Coder",`, 1)
	if labelled == string(v3) {
		t.Fatalf("fixture shape changed; could not insert a label into %s", v3)
	}
	decoded, err := decodeModelConfig([]byte(labelled))
	if err != nil {
		t.Fatalf("decodeModelConfig(labelled) error = %v", err)
	}
	if got, want := decoded.Models[0].Label, "Qwen3 Coder"; got != want {
		t.Fatalf("decoded label = %q, want %q", got, want)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	encoded, err := encodeModelConfigV3(normalized)
	if err != nil {
		t.Fatalf("encodeModelConfigV3() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"label":"Qwen3 Coder"`) {
		t.Fatalf("encoded config dropped the label: %s", encoded)
	}
}

// TestModelConfigLabelMovesTheDigest pins that a label is part of the configuration the
// secret-free digest describes, exactly as the description beside it is. A cosmetic edit is
// still an edit, and a revision that could not see it would report a file as unchanged when
// it is not.
func TestModelConfigLabelMovesTheDigest(t *testing.T) {
	t.Parallel()

	base := validDecodedModelConfig(t)
	normalizedBase, err := normalizeModelConfig(base)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	before, err := modelConfigDigest(normalizedBase)
	if err != nil {
		t.Fatalf("modelConfigDigest() error = %v", err)
	}

	labelled := validDecodedModelConfig(t)
	labelled.Models[0].Label = "Qwen3 Coder"
	normalizedLabelled, err := normalizeModelConfig(labelled)
	if err != nil {
		t.Fatalf("normalizeModelConfig(labelled) error = %v", err)
	}
	after, err := modelConfigDigest(normalizedLabelled)
	if err != nil {
		t.Fatalf("modelConfigDigest(labelled) error = %v", err)
	}
	if before == after {
		t.Fatal("digest did not change when a label was added")
	}
}

func TestMigrateModelConfigV2ToV3EmitsNativeACPUnionWithoutInternalFields(t *testing.T) {
	input := strings.Replace(
		validLMStudioModelConfig,
		"\n}",
		",\n  \"native_acp\": {\"codex\": {\"enabled\": true, \"models\": [\"local\"]}}\n}",
		1,
	)
	migrated, err := migrateModelConfigV2ToV3([]byte(input))
	if err != nil {
		t.Fatalf("migrateModelConfigV2ToV3(native_acp) error = %v", err)
	}
	if strings.Contains(string(migrated), `"Legacy"`) || strings.Contains(string(migrated), `"Model"`) {
		t.Fatalf("migrated native ACP output exposed internal fields: %s", migrated)
	}
	decoded, err := decodeModelConfig(migrated)
	if err != nil {
		t.Fatalf("decode migrated native ACP config: %v", err)
	}
	if _, err := normalizeModelConfig(decoded); err != nil {
		t.Fatalf("normalize migrated native ACP config: %v", err)
	}
}

func TestWriteMigratedModelConfigV2ToV3RejectsInvalidInputWithoutChangingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	want := []byte(validLMStudioModelConfig)
	writeModelConfigFixture(t, path, want, 0o600)
	invalid := strings.Replace(validLMStudioModelConfig, `"version": 2`, `"version": 9`, 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatalf("write invalid fixture: %v", err)
	}
	if err := writeMigratedModelConfigV2ToV3(path); err == nil {
		t.Fatal("writeMigratedModelConfigV2ToV3() error = nil, want invalid-version rejection")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read invalid config after rejected migration: %v", err)
	}
	if !bytes.Equal(got, []byte(invalid)) {
		t.Fatal("rejected invalid migration changed the destination")
	}
}

func TestWriteMigratedModelConfigV2ToV3RejectsCooperatingConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := []byte(validLMStudioModelConfig)
	writeModelConfigFixture(t, path, original, 0o600)
	changed := bytes.Replace(original, []byte(`"primer_default": "local"`), []byte(`"primer_default": "changed"`), 1)
	if bytes.Equal(changed, original) {
		t.Fatal("test fixture did not change source bytes")
	}

	err := writeMigratedModelConfigV2ToV3WithHooks(path, modelConfigMigrationHooks{
		beforeCAS: func() error {
			return os.WriteFile(path, changed, 0o600)
		},
	})
	if !errors.Is(err, errModelConfigMigrationConcurrent) {
		t.Fatalf("concurrent migration error = %v, want concurrent-modification error", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read concurrently edited config: %v", readErr)
	}
	if !bytes.Equal(got, changed) {
		t.Fatalf("concurrent edit was overwritten: got %q, want %q", got, changed)
	}
}

func TestWriteMigratedModelConfigV2ToV3SyncsOwnerModeAndReportsDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	writeModelConfigFixture(t, path, []byte(validLMStudioModelConfig), 0o600)
	var syncedMode os.FileMode
	err := writeMigratedModelConfigV2ToV3WithHooks(path, modelConfigMigrationHooks{
		syncFile: func(file *os.File) error {
			info, statErr := file.Stat()
			if statErr != nil {
				return statErr
			}
			syncedMode = info.Mode().Perm()
			return errors.New("injected file durability failure")
		},
	})
	if !errors.Is(err, errModelConfigMigrationDurability) {
		t.Fatalf("durability error = %v, want durability error", err)
	}
	if syncedMode != 0o600 {
		t.Fatalf("temporary mode at sync = %04o, want 0600", syncedMode)
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
