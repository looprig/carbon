package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/content"
)

const maxModelConfigBytes = 1 << 20

const modelConfigVersion = 2

const (
	maxModelConfigErrorBytes          = 512
	maxModelConfigErrorOperationBytes = 160
	maxModelConfigErrorCauseBytes     = 256
)

type modelConfigFile struct {
	Version              int                               `json:"version"`
	PrimerDefault        string                            `json:"primer_default"`
	ClaudeCodeSmallModel string                            `json:"claude_code_small_model"`
	Models               []modelTargetConfig               `json:"models"`
	NativeACP            map[string]nativeACPProfileConfig `json:"native_acp"`
	PermissionReview     *permissionReviewConfig           `json:"permission_review,omitempty"`
	ACPLaunchers         map[string]acpLauncherConfig      `json:"acp_launchers"`
}

// acpLauncherConfig is one harness's configured ACP adapter executable
// location. It is machine-local launcher configuration, not a model
// credential: it never enters the model-configuration digest.
type acpLauncherConfig struct {
	Executable string `json:"executable"`
}

type nativeACPProfileConfig struct {
	Enabled bool                    `json:"enabled"`
	Models  *[]nativeACPModelConfig `json:"models"`
}

// nativeACPModelConfig is the compatibility union accepted by a native ACP
// profile's models list. Legacy string entries retain the existing model-only
// semantics; structured entries carry an explicit neutral effort allowlist.
// Legacy is decode-only metadata and is not part of normalized configuration
// identity: a legacy entry is equivalent to a structured entry with only
// effort "none" and default "none".
type nativeACPModelConfig struct {
	Model         string
	Efforts       []string
	DefaultEffort string
	Legacy        bool
}

// UnmarshalJSON accepts one legacy string or one strict structured object.
// The explicit decoder EOF check keeps this union safe when it is used outside
// the enclosing model-config decoder as well.
func (c *nativeACPModelConfig) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("native_acp model entry is empty")
	}
	if err := rejectDuplicateJSONKeys(trimmed); err != nil {
		return errors.New("invalid native_acp model entry")
	}

	if trimmed[0] == '"' {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		var modelID string
		if err := decoder.Decode(&modelID); err != nil {
			return errors.New("native_acp model entry must be a string or object")
		}
		if err := rejectTrailingJSON(decoder); err != nil {
			return errors.New("native_acp model entry contains trailing JSON")
		}
		*c = nativeACPModelConfig{Model: modelID, Legacy: true}
		return nil
	}
	if trimmed[0] != '{' {
		return errors.New("native_acp model entry must be a string or object")
	}

	var wire struct {
		Model         *string   `json:"model"`
		Efforts       *[]string `json:"efforts"`
		DefaultEffort *string   `json:"default_effort"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return errors.New("invalid native_acp model entry")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return errors.New("native_acp model entry contains trailing JSON")
	}
	if wire.Model == nil {
		return errors.New("native_acp structured model entry requires model")
	}
	if wire.Efforts == nil {
		return errors.New("native_acp structured model entry requires efforts")
	}
	if wire.DefaultEffort == nil {
		return errors.New("native_acp structured model entry requires default_effort")
	}
	*c = nativeACPModelConfig{
		Model:         *wire.Model,
		Efforts:       append([]string(nil), (*wire.Efforts)...),
		DefaultEffort: *wire.DefaultEffort,
	}
	return nil
}

// UnmarshalJSON keeps the wire representation strict enough to preserve the
// harness-managed mode for both an omitted models field and an explicit null;
// any non-null models value must be an array.
func (c *nativeACPProfileConfig) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateJSONKeys(bytes.TrimSpace(data)); err != nil {
		return errors.New("invalid native_acp profile")
	}
	var wire struct {
		Enabled bool            `json:"enabled"`
		Models  json.RawMessage `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return errors.New("invalid native_acp profile")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return errors.New("native_acp profile contains trailing JSON")
	}
	*c = nativeACPProfileConfig{Enabled: wire.Enabled}
	if len(wire.Models) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(wire.Models), []byte("null")) {
		// An explicit null has the same meaning as omission: leave Models nil so
		// downstream compilation can preserve harness-managed selection.
		return nil
	}
	decoder = json.NewDecoder(bytes.NewReader(wire.Models))
	decoder.DisallowUnknownFields()
	var models []nativeACPModelConfig
	if err := decoder.Decode(&models); err != nil {
		return errors.New("native_acp models must be an array of strings or objects")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return errors.New("native_acp models contains trailing JSON")
	}
	c.Models = &models
	return nil
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

// permissionReviewConfig holds the optional classifier-based automatic
// permission review section. It is parsing plumbing only: whether Model is
// required, whether it names a configured alias, and whether review may
// take effect for the session's access profile are all resolved later (see
// Task 4 in docs/plans/2026-08-05-coderig-mcp-and-permission-review-implementation.md).
type permissionReviewConfig struct {
	Model  string `json:"model"`
	Strict bool   `json:"strict"`
}

// UnmarshalJSON keeps decoding of this section strict (unknown fields
// rejected), matching every other section in this file.
func (c *permissionReviewConfig) UnmarshalJSON(data []byte) error {
	var wire struct {
		Model  string `json:"model"`
		Strict bool   `json:"strict"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*c = permissionReviewConfig{Model: wire.Model, Strict: wire.Strict}
	return nil
}

type modelTargetConfig struct {
	Alias         string                   `json:"alias"`
	Description   string                   `json:"description"`
	Provider      string                   `json:"provider"`
	APIFormat     string                   `json:"api_format"`
	BaseURL       string                   `json:"base_url"`
	Model         string                   `json:"model"`
	ContextLimits modelContextLimitsConfig `json:"context_limits"`
	APIKey        string                   `json:"api_key"`
	Uses          []string                 `json:"uses"`
	Capabilities  modelCapabilitiesConfig  `json:"capabilities"`
	Efforts       []string                 `json:"efforts"`
	DefaultEffort string                   `json:"default_effort"`
}

type modelContextLimitsConfig struct {
	WindowTokens    content.TokenCount `json:"window_tokens"`
	MaxInputTokens  content.TokenCount `json:"max_input_tokens"`
	MaxOutputTokens content.TokenCount `json:"max_output_tokens"`
}

type modelCapabilitiesConfig struct {
	Tools                     bool `json:"tools"`
	Thinking                  bool `json:"thinking"`
	Images                    bool `json:"images"`
	PromptCaching             bool `json:"prompt_caching"`
	StructuredOutput          bool `json:"structured_output"`
	StructuredOutputWithTools bool `json:"structured_output_with_tools"`
}

// ModelConfigError reports a failure to locate or read CodeRig's global model
// configuration.
type ModelConfigError struct {
	operation string
	cause     error
}

func (e *ModelConfigError) Error() string {
	operation := boundedModelConfigText(e.operation, maxModelConfigErrorOperationBytes)
	message := "coderig: model configuration " + operation + " failed"
	if e.cause == nil {
		return boundedModelConfigText(message, maxModelConfigErrorBytes)
	}
	message += ": " + boundedModelConfigText(e.cause.Error(), maxModelConfigErrorCauseBytes)
	return boundedModelConfigText(message, maxModelConfigErrorBytes)
}

func (e *ModelConfigError) Unwrap() error { return e.cause }

func decodeModelConfig(data []byte) (modelConfigFile, error) {
	var config modelConfigFile
	if !utf8.Valid(data) {
		return config, modelConfigFailure("decode", errors.New("input is not valid UTF-8"))
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return config, modelConfigFailure("decode", err)
	}
	if err := rejectNullPermissionReview(data); err != nil {
		return config, modelConfigFailure("decode", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return modelConfigFile{}, modelConfigFailure("decode", safeModelConfigDecodeError(err))
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple top-level JSON values")
		} else {
			err = safeModelConfigDecodeError(err)
		}
		return modelConfigFile{}, modelConfigFailure("decode", err)
	}
	if config.Version != modelConfigVersion {
		return modelConfigFile{}, modelConfigFailure("decode", errors.New("version must be exactly 2"))
	}
	return config, nil
}

func safeModelConfigDecodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("model configuration JSON is empty")
	}
	return errors.New("invalid JSON model configuration")
}

// rejectNullPermissionReview rejects an explicit "permission_review": null.
//
// encoding/json's indirect() never calls a settable pointer field's
// UnmarshalJSON when the wire value is a JSON null: it sets the field to nil
// directly instead. That means permissionReviewConfig.UnmarshalJSON (a
// method on the pointee, not the *modelConfigFile.PermissionReview pointer
// field itself) is never invoked for that case and so cannot distinguish an
// explicit null from an absent key — both leave PermissionReview nil with no
// error. A lightweight raw-message probe, run before the real decode, is the
// simplest way to see the wire bytes for this one key and catch the null
// case explicitly, matching nativeACPProfileConfig's explicit-null handling
// for its own (nested) optional field.
func rejectNullPermissionReview(data []byte) error {
	var probe struct {
		PermissionReview json.RawMessage `json:"permission_review"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		// Malformed or non-object JSON is reported by the caller's later,
		// stricter decode; this probe only needs to see a well-formed
		// "permission_review" key when one is present.
		return nil
	}
	if len(probe.PermissionReview) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(probe.PermissionReview), []byte("null")) {
		return errors.New("permission_review must be an object")
	}
	return nil
}

func modelConfigFailure(operation string, cause error) *ModelConfigError {
	message := "unknown error"
	if cause != nil {
		message = boundedModelConfigText(cause.Error(), maxModelConfigErrorCauseBytes)
	}
	return &ModelConfigError{
		operation: boundedModelConfigText(operation, maxModelConfigErrorOperationBytes),
		cause:     errors.New(message),
	}
}

func boundedModelConfigText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	end := limit - 3
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "..."
}

// defaultModelConfigPath computes CodeRig's models.json path under the
// resolved looprig home directory: <home>/models.json. home is the
// already-resolved looprig base directory (looprigHome's result, e.g.
// ~/.looprig/coderig or Config.HomeDir) — this function no longer resolves HOME
// itself, so it retains its (string, error) signature for call-site
// consistency but cannot fail today.
func defaultModelConfigPath(home string) (string, error) {
	return filepath.Join(home, "models.json"), nil
}

func readModelConfigFile(path string) ([]byte, bool, error) {
	return readModelConfigFileWithOpen(path, openModelConfigNoFollow)
}

func readModelConfigFileWithOpen(path string, openFile func(string) (*os.File, error)) ([]byte, bool, error) {
	return readHygienicConfigFile(path, maxModelConfigBytes, openFile, func(op string, cause error) error {
		return modelConfigFailure(op, cause)
	})
}

func modelConfigIsUnix() bool {
	switch runtime.GOOS {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
}
