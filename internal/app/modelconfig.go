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
	"sync"
	"unicode/utf8"

	"github.com/looprig/core/content"
)

const maxModelConfigBytes = 1 << 20

const (
	modelConfigVersionV2 = 2
	modelConfigVersionV3 = 3
	// modelConfigVersion is the version emitted by new schema-aware callers.
	// The loader still accepts modelConfigVersionV2 explicitly for compatibility.
	modelConfigVersion = modelConfigVersionV3
)

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

func decodeStrictModelConfig(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return safeModelConfigDecodeError(err)
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return safeModelConfigDecodeError(err)
	}
	return nil
}

func decodeV3AuthField(raw json.RawMessage, field string) (value string, present bool, err error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, errors.New(field + " must be a string when present")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, errors.New(field + " must be a string when present")
	}
	return value, true, nil
}

func modelTargetConfigFromV2(wire modelTargetConfigV2Wire) modelTargetConfig {
	return modelTargetConfig{
		Alias: wire.Alias, Description: wire.Description, Provider: wire.Provider,
		APIFormat: wire.APIFormat, BaseURL: wire.BaseURL, Model: wire.Model,
		ContextLimits: wire.ContextLimits, APIKey: wire.APIKey, Uses: wire.Uses,
		Capabilities: wire.Capabilities, Efforts: wire.Efforts, DefaultEffort: wire.DefaultEffort,
	}
}

func modelTargetConfigFromV3(wire modelTargetConfigV3Wire) (modelTargetConfig, error) {
	apiKey, apiKeyPresent, err := decodeV3AuthField(wire.APIKey, "api_key")
	if err != nil {
		return modelTargetConfig{}, err
	}
	credentialRef, credentialRefPresent, err := decodeV3AuthField(wire.CredentialRef, "credential_ref")
	if err != nil {
		return modelTargetConfig{}, err
	}
	return modelTargetConfig{
		Alias: wire.Alias, Description: wire.Description, Provider: wire.Provider,
		APIFormat: wire.APIFormat, BaseURL: wire.BaseURL, Model: wire.Model,
		ContextLimits: wire.ContextLimits, APIKey: apiKey, CredentialRef: credentialRef,
		Uses: wire.Uses, Capabilities: wire.Capabilities, Efforts: wire.Efforts,
		DefaultEffort: wire.DefaultEffort, apiKeyPresent: apiKeyPresent,
		credentialRefPresent: credentialRefPresent,
	}, nil
}

func modelConfigFileFromV2(wire modelConfigV2Wire) modelConfigFile {
	config := modelConfigFile{
		Version: wire.Version, PrimerDefault: wire.PrimerDefault,
		ClaudeCodeSmallModel: wire.ClaudeCodeSmallModel, NativeACP: wire.NativeACP,
		PermissionReview: wire.PermissionReview, ACPLaunchers: wire.ACPLaunchers,
		Models: make([]modelTargetConfig, len(wire.Models)),
	}
	for i, target := range wire.Models {
		config.Models[i] = modelTargetConfigFromV2(target)
	}
	return config
}

func modelConfigFileFromV3(wire modelConfigV3Wire) (modelConfigFile, error) {
	config := modelConfigFile{
		Version: wire.Version, PrimerDefault: wire.PrimerDefault,
		ClaudeCodeSmallModel: wire.ClaudeCodeSmallModel, NativeACP: wire.NativeACP,
		PermissionReview: wire.PermissionReview, ACPLaunchers: wire.ACPLaunchers,
		Models: make([]modelTargetConfig, len(wire.Models)),
	}
	for i, target := range wire.Models {
		converted, err := modelTargetConfigFromV3(target)
		if err != nil {
			return modelConfigFile{}, err
		}
		config.Models[i] = converted
	}
	return config, nil
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
	CredentialRef string                   `json:"credential_ref,omitempty"`
	Uses          []string                 `json:"uses"`
	Capabilities  modelCapabilitiesConfig  `json:"capabilities"`
	Efforts       []string                 `json:"efforts"`
	DefaultEffort string                   `json:"default_effort"`

	// v3 wire decoding tracks presence separately from value. An explicitly
	// empty api_key/credential_ref is not the same as an omitted field: auth
	// ambiguity and local explicit-none validation must reject both cases.
	apiKeyPresent        bool
	credentialRefPresent bool
}

// modelConfigV2Wire and modelConfigV3Wire deliberately remain separate. v2's
// API-key field keeps its historical value semantics; v3 uses raw optional
// fields so it can distinguish omitted, empty, null, and populated auth
// values before normalization.
type modelConfigV2Wire struct {
	Version              int                               `json:"version"`
	PrimerDefault        string                            `json:"primer_default"`
	ClaudeCodeSmallModel string                            `json:"claude_code_small_model"`
	Models               []modelTargetConfigV2Wire         `json:"models"`
	NativeACP            map[string]nativeACPProfileConfig `json:"native_acp"`
	PermissionReview     *permissionReviewConfig           `json:"permission_review,omitempty"`
	ACPLaunchers         map[string]acpLauncherConfig      `json:"acp_launchers"`
}

type modelConfigV3Wire struct {
	Version              int                               `json:"version"`
	PrimerDefault        string                            `json:"primer_default"`
	ClaudeCodeSmallModel string                            `json:"claude_code_small_model"`
	Models               []modelTargetConfigV3Wire         `json:"models"`
	NativeACP            map[string]nativeACPProfileConfig `json:"native_acp"`
	PermissionReview     *permissionReviewConfig           `json:"permission_review,omitempty"`
	ACPLaunchers         map[string]acpLauncherConfig      `json:"acp_launchers"`
}

// modelConfigV3OutputWire keeps decode-only native ACP union metadata out of
// emitted JSON. nativeACPModelConfig.Legacy is an internal marker, not a v3
// wire field; output uses either the legacy string form or the strict object
// form accepted by nativeACPProfileConfig.UnmarshalJSON.
type modelConfigV3OutputWire struct {
	Version              int                                 `json:"version"`
	PrimerDefault        string                              `json:"primer_default"`
	ClaudeCodeSmallModel string                              `json:"claude_code_small_model"`
	Models               []modelTargetConfigV3Wire           `json:"models"`
	NativeACP            map[string]nativeACPProfileV3Output `json:"native_acp"`
	PermissionReview     *permissionReviewConfig             `json:"permission_review,omitempty"`
	ACPLaunchers         map[string]acpLauncherConfig        `json:"acp_launchers"`
}

type nativeACPProfileV3Output struct {
	Enabled bool   `json:"enabled"`
	Models  *[]any `json:"models,omitempty"`
}

type nativeACPModelV3Output struct {
	Model         string   `json:"model"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
}

type modelTargetConfigV2Wire struct {
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

type modelTargetConfigV3Wire struct {
	Alias         string                   `json:"alias"`
	Description   string                   `json:"description"`
	Provider      string                   `json:"provider"`
	APIFormat     string                   `json:"api_format"`
	BaseURL       string                   `json:"base_url"`
	Model         string                   `json:"model"`
	ContextLimits modelContextLimitsConfig `json:"context_limits"`
	APIKey        json.RawMessage          `json:"api_key,omitempty"`
	CredentialRef json.RawMessage          `json:"credential_ref,omitempty"`
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
type modelConfigErrorKind uint8

const (
	modelConfigErrorKindNone modelConfigErrorKind = iota
	modelConfigErrorKindMigrationConcurrent
	modelConfigErrorKindMigrationDurability
)

type ModelConfigError struct {
	operation string
	causeText string
	kind      modelConfigErrorKind
}

func (e *ModelConfigError) Error() string {
	operation := boundedModelConfigText(e.operation, maxModelConfigErrorOperationBytes)
	message := "coderig: model configuration " + operation + " failed"
	if e.causeText == "" {
		return boundedModelConfigText(message, maxModelConfigErrorBytes)
	}
	message += ": " + boundedModelConfigText(e.causeText, maxModelConfigErrorCauseBytes)
	return boundedModelConfigText(message, maxModelConfigErrorBytes)
}

func (e *ModelConfigError) Is(target error) bool {
	switch e.kind {
	case modelConfigErrorKindMigrationConcurrent:
		return target == errModelConfigMigrationConcurrent
	case modelConfigErrorKindMigrationDurability:
		return target == errModelConfigMigrationDurability
	default:
		return false
	}
}

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

	var probe struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return modelConfigFile{}, modelConfigFailure("decode", safeModelConfigDecodeError(err))
	}
	if len(probe.Version) == 0 {
		return modelConfigFile{}, modelConfigFailure("decode", errors.New("version is required"))
	}
	var version int
	if err := json.Unmarshal(probe.Version, &version); err != nil {
		return modelConfigFile{}, modelConfigFailure("decode", errors.New("version must be an integer"))
	}

	switch version {
	case modelConfigVersionV2:
		var wire modelConfigV2Wire
		if err := decodeStrictModelConfig(data, &wire); err != nil {
			return modelConfigFile{}, modelConfigFailure("decode", err)
		}
		return modelConfigFileFromV2(wire), nil
	case modelConfigVersionV3:
		var wire modelConfigV3Wire
		if err := decodeStrictModelConfig(data, &wire); err != nil {
			return modelConfigFile{}, modelConfigFailure("decode", err)
		}
		config, err := modelConfigFileFromV3(wire)
		if err != nil {
			return modelConfigFile{}, modelConfigFailure("decode", err)
		}
		return config, nil
	default:
		return modelConfigFile{}, modelConfigFailure("decode", errors.New("version must be exactly 2 or 3"))
	}
}

// migrateModelConfigV2ToV3 is the explicit, pure migration seam. Ordinary
// model loading never calls it: v2 files remain readable and byte-stable until
// an account/configuration operation explicitly requests migration.
func migrateModelConfigV2ToV3(data []byte) ([]byte, error) {
	if len(data) > maxModelConfigBytes {
		return nil, modelConfigFailure("migrate", errors.New("source exceeds the model configuration size limit"))
	}
	decoded, err := decodeModelConfig(data)
	if err != nil {
		return nil, err
	}
	if decoded.Version != modelConfigVersionV2 {
		return nil, modelConfigFailure("migrate", errors.New("source version must be exactly 2"))
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		return nil, err
	}
	normalized.Version = modelConfigVersionV3
	return encodeModelConfigV3(normalized)
}

// encodeModelConfigV3 emits only the validated schema-v3 wire representation.
// It accepts normalized input so credentials have already passed the schema's
// auth checks; the encoder never hashes, logs, or includes a credential in an
// error. The bound is enforced before callers can publish the bytes.
func encodeModelConfigV3(config normalizedModelConfig) ([]byte, error) {
	if config.Version != modelConfigVersionV3 {
		return nil, modelConfigFailure("encode", errors.New("normalized configuration must be schema version 3"))
	}
	wire := modelConfigV3OutputWire{
		Version:              modelConfigVersionV3,
		PrimerDefault:        config.PrimerDefault,
		ClaudeCodeSmallModel: config.ClaudeCodeSmallModel,
		Models:               make([]modelTargetConfigV3Wire, 0, len(config.Models)),
		NativeACP:            make(map[string]nativeACPProfileV3Output, len(config.NativeACP)),
		ACPLaunchers:         make(map[string]acpLauncherConfig, len(config.ACPLaunchers)),
	}
	if config.PermissionReview != nil {
		wire.PermissionReview = &permissionReviewConfig{
			Model: config.PermissionReview.Model, Strict: config.PermissionReview.Strict,
		}
	}
	for harness, launcher := range config.ACPLaunchers {
		wire.ACPLaunchers[harness] = acpLauncherConfig{Executable: launcher.Executable}
	}
	if len(wire.ACPLaunchers) == 0 {
		wire.ACPLaunchers = nil
	}
	for harness, profile := range config.NativeACP {
		entry := nativeACPProfileV3Output{Enabled: profile.Enabled}
		if profile.ModelOptions != nil {
			options := make([]any, 0, len(profile.ModelOptions))
			for _, option := range profile.ModelOptions {
				efforts := make([]string, len(option.Efforts))
				for i, effort := range option.Efforts {
					efforts[i] = modelConfigEffortName(effort)
				}
				options = append(options, nativeACPModelV3Output{
					Model: option.Model, Efforts: efforts,
					DefaultEffort: modelConfigEffortName(option.DefaultEffort),
				})
			}
			entry.Models = &options
		} else if profile.Models != nil {
			models := make([]any, len(profile.Models))
			for i, modelID := range profile.Models {
				models[i] = modelID
			}
			entry.Models = &models
		}
		wire.NativeACP[harness] = entry
	}
	if len(wire.NativeACP) == 0 {
		wire.NativeACP = nil
	}
	for _, target := range config.Models {
		if !target.client.valid() {
			return nil, modelConfigFailure("encode", errors.New("normalized model auth contains both credential forms"))
		}
		row := modelTargetConfigV3Wire{
			Alias: target.Alias, Description: target.Description,
			Provider: string(target.Model.Provider), APIFormat: string(target.Model.APIFormat),
			BaseURL: target.Model.BaseURL, Model: target.Model.Name,
			ContextLimits: modelContextLimitsConfig{
				WindowTokens:    target.Model.Limits.WindowTokens,
				MaxInputTokens:  target.Model.Limits.MaxInputTokens,
				MaxOutputTokens: target.Model.Limits.MaxOutputTokens,
			},
			Uses: append([]string(nil), target.Uses...),
			Capabilities: modelCapabilitiesConfig{
				Tools: target.Model.Caps.Tools, Thinking: target.Model.Caps.Thinking,
				Images: target.Model.Caps.AcceptsImages, PromptCaching: target.Model.Caps.PromptCaching,
				StructuredOutput:          target.Model.Caps.StructuredOutput,
				StructuredOutputWithTools: target.Model.Caps.StructuredOutputWithTools,
			},
			Efforts: make([]string, len(target.Efforts)), DefaultEffort: modelConfigEffortName(target.DefaultEffort),
		}
		for i, effort := range target.Efforts {
			row.Efforts[i] = modelConfigEffortName(effort)
		}
		if target.client.hasAPIKey() {
			row.APIKey, _ = json.Marshal(target.client.APIKey)
		} else if target.client.hasCredentialRef() {
			row.CredentialRef, _ = json.Marshal(target.client.CredentialRef.String())
		}
		wire.Models = append(wire.Models, row)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, modelConfigFailure("encode", errors.New("could not encode schema version 3"))
	}
	if len(data) > maxModelConfigBytes {
		return nil, modelConfigFailure("encode", errors.New("encoded configuration exceeds the model configuration size limit"))
	}
	return data, nil
}

var (
	errModelConfigMigrationConcurrent = errors.New("model configuration changed during migration")
	errModelConfigMigrationDurability = errors.New("model configuration migration durability failure")
	modelConfigMigrationLocks         sync.Map
)

type modelConfigMigrationHooks struct {
	beforeCAS     func() error
	syncFile      func(*os.File) error
	syncDirectory func(string) error
}

func writeMigratedModelConfigV2ToV3(path string) error {
	// The per-process lock serializes cooperating writers, and the migration
	// CAS checks bytes plus file identity immediately before publication work.
	// A non-cooperating edit after that check and before rename is outside the
	// portable guarantee; no platform-specific compare-and-swap is assumed.
	return writeMigratedModelConfigV2ToV3WithHooks(path, modelConfigMigrationHooks{})
}

// writeMigratedModelConfigV2ToV3WithHooks is the testable migration seam. The
// production entry point uses real fsyncs and no edit hook.
func writeMigratedModelConfigV2ToV3WithHooks(path string, hooks modelConfigMigrationHooks) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return modelConfigFailure("migrate", err)
	}
	lockValue, _ := modelConfigMigrationLocks.LoadOrStore(absPath, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	initialInfo, initialStatErr := os.Lstat(absPath)
	data, present, err := readModelConfigFile(absPath)
	if err != nil {
		return err
	}
	if !present {
		return modelConfigFailure("migrate", errors.New("model configuration is absent"))
	}
	readInfo, readStatErr := os.Lstat(absPath)
	if initialStatErr != nil || readStatErr != nil || !os.SameFile(initialInfo, readInfo) {
		return modelConfigMigrationFailure(errModelConfigMigrationConcurrent)
	}
	migrated, err := migrateModelConfigV2ToV3(data)
	if err != nil {
		return err
	}
	info := initialInfo
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return modelConfigFailure("migrate", errors.New("model configuration is not an owner-only regular file"))
	}
	if hooks.beforeCAS != nil {
		if err := hooks.beforeCAS(); err != nil {
			return modelConfigMigrationFailure(errModelConfigMigrationConcurrent)
		}
	}
	current, currentPresent, err := readModelConfigFile(absPath)
	if err != nil || !currentPresent {
		return modelConfigMigrationFailure(errModelConfigMigrationConcurrent)
	}
	currentInfo, err := os.Lstat(absPath)
	if err != nil || !os.SameFile(info, currentInfo) || !bytes.Equal(data, current) {
		return modelConfigMigrationFailure(errModelConfigMigrationConcurrent)
	}
	temp, err := os.CreateTemp(filepath.Dir(absPath), "."+filepath.Base(absPath)+".migration-*")
	if err != nil {
		return modelConfigFailure("migrate", errors.New("could not create migration temporary file"))
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if written, err := temp.Write(migrated); err != nil || written != len(migrated) {
		_ = temp.Close()
		return modelConfigFailure("migrate", errors.New("could not write migrated model configuration"))
	}
	if err := os.Chmod(tempPath, info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return modelConfigFailure("migrate", errors.New("could not set migration file permissions"))
	}
	syncFile := hooks.syncFile
	if syncFile == nil {
		syncFile = (*os.File).Sync
	}
	if err := syncFile(temp); err != nil {
		_ = temp.Close()
		return modelConfigMigrationFailure(errModelConfigMigrationDurability)
	}
	if err := temp.Close(); err != nil {
		return modelConfigMigrationFailure(errModelConfigMigrationDurability)
	}
	if err := os.Rename(tempPath, absPath); err != nil {
		return modelConfigFailure("migrate", errors.New("could not publish migrated model configuration"))
	}
	removeTemp = false
	syncDirectory := hooks.syncDirectory
	if syncDirectory == nil {
		syncDirectory = syncModelConfigMigrationDirectory
	}
	if err := syncDirectory(filepath.Dir(absPath)); err != nil {
		return modelConfigMigrationFailure(errModelConfigMigrationDurability)
	}
	return nil
}

func syncModelConfigMigrationDirectory(path string) error {
	directory, err := os.Open(path) // #nosec G304 -- path is the validated model-config directory used for the durability sync
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
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
		causeText: message,
	}
}

func modelConfigMigrationFailure(cause error) *ModelConfigError {
	kind := modelConfigErrorKindNone
	switch {
	case errors.Is(cause, errModelConfigMigrationConcurrent):
		kind = modelConfigErrorKindMigrationConcurrent
	case errors.Is(cause, errModelConfigMigrationDurability):
		kind = modelConfigErrorKindMigrationDurability
	}
	message := "migration failed"
	switch kind {
	case modelConfigErrorKindMigrationConcurrent:
		message = "migration concurrent modification"
	case modelConfigErrorKindMigrationDurability:
		message = "migration durability failure"
	}
	return &ModelConfigError{operation: "migrate", causeText: message, kind: kind}
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
