package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

type normalizedModelConfig struct {
	PrimerDefault        string
	ClaudeCodeSmallModel string
	Models               []normalizedModelTarget
	NativeACP            map[string]normalizedNativeACPProfile
	PermissionReview     *normalizedPermissionReview
	ACPLaunchers         map[string]normalizedACPLauncher
}

// normalizedPermissionReview is the resolved (but not yet client-bound)
// permission_review section: the alias string, not yet the model.Model value
// it names. compileProductionModels resolves the alias to a model.Model, the
// same shape PrimerAlias/config.PrimerDefault follow at this layer.
type normalizedPermissionReview struct {
	Model  string
	Strict bool
}

type normalizedNativeACPProfile struct {
	Harness             string
	Enabled             bool
	Models              []string
	ModelOptions        []normalizedNativeACPModel
	HasStructuredModels bool
}

// normalizedNativeACPModel is one static native ACP selection option. Models
// remains the legacy ID-only projection for existing composition callers;
// ModelOptions carries the complete model/effort/default tuple for catalogue
// compilation and digesting.
type normalizedNativeACPModel struct {
	Model         string
	Efforts       []model.Effort
	DefaultEffort model.Effort
}

type normalizedACPLauncher struct {
	Harness    string
	Executable string
}

type normalizedModelTarget struct {
	Alias         string
	Description   string
	Model         model.Model
	Uses          []string
	Efforts       []model.Effort
	DefaultEffort model.Effort
	client        modelClientInput
}

type modelClientInput struct {
	APIKey string
}

func normalizeModelConfig(config modelConfigFile) (normalizedModelConfig, error) {
	var normalized normalizedModelConfig
	if config.Version != modelConfigVersion {
		return normalized, modelConfigValidationError("version must be exactly 2")
	}
	if len(config.Models) == 0 {
		return normalized, modelConfigValidationError("models must contain at least one entry")
	}
	nativeACP, err := normalizeNativeACPProfiles(config.NativeACP)
	if err != nil {
		return normalizedModelConfig{}, err
	}
	normalized.NativeACP = nativeACP

	acpLaunchers, err := normalizeACPLaunchers(config.ACPLaunchers)
	if err != nil {
		return normalizedModelConfig{}, err
	}
	normalized.ACPLaunchers = acpLaunchers

	byAlias := make(map[string]*normalizedModelTarget, len(config.Models))
	normalized.Models = make([]normalizedModelTarget, 0, len(config.Models))
	for _, target := range config.Models {
		normalizedTarget, err := normalizeModelTarget(target)
		if err != nil {
			return normalizedModelConfig{}, err
		}
		if _, duplicate := byAlias[normalizedTarget.Alias]; duplicate {
			return normalizedModelConfig{}, modelConfigValidationError("model aliases must be unique")
		}
		normalized.Models = append(normalized.Models, normalizedTarget)
		byAlias[normalizedTarget.Alias] = &normalized.Models[len(normalized.Models)-1]
	}

	if !isExactNonEmptyModelConfigString(config.PrimerDefault) {
		return normalizedModelConfig{}, modelConfigValidationError("primer_default must be non-empty and unpadded")
	}
	primer, ok := byAlias[config.PrimerDefault]
	if !ok || !containsModelConfigUse(primer.Uses, "primer") {
		return normalizedModelConfig{}, modelConfigValidationError("primer_default must name a primer-capable model")
	}
	normalized.PrimerDefault = config.PrimerDefault

	if config.ClaudeCodeSmallModel != "" {
		if !isExactNonEmptyModelConfigString(config.ClaudeCodeSmallModel) {
			return normalizedModelConfig{}, modelConfigValidationError("claude_code_small_model must be unpadded")
		}
		small, exists := byAlias[config.ClaudeCodeSmallModel]
		if !exists || !containsModelConfigUse(small.Uses, "delegate") {
			return normalizedModelConfig{}, modelConfigValidationError("claude_code_small_model must name a delegate-capable model")
		}
		if !small.Model.Caps.Tools {
			return normalizedModelConfig{}, modelConfigValidationError("claude_code_small_model must support tools")
		}
		normalized.ClaudeCodeSmallModel = config.ClaudeCodeSmallModel
	}

	if config.PermissionReview != nil {
		if !isExactNonEmptyModelConfigString(config.PermissionReview.Model) {
			return normalizedModelConfig{}, modelConfigValidationError("permission_review.model must be non-empty and unpadded")
		}
		target, exists := byAlias[config.PermissionReview.Model]
		if !exists {
			// Unlike every other modelConfigValidationError call in this file, this
			// one (and the capability-shortfall error below) names the offending
			// alias via %q. Alias names are operator-chosen identifiers, not
			// secrets (unlike API keys/headers elsewhere in this file), and
			// boundedModelConfigText still caps the final message length — a
			// deliberate, narrow deviation for this field only, not a license to
			// interpolate elsewhere in this function.
			return normalizedModelConfig{}, modelConfigValidationError(fmt.Sprintf("permission_review.model %q is not a configured model alias", config.PermissionReview.Model))
		}
		if !target.Model.Caps.Tools || !target.Model.Caps.StructuredOutputWithTools {
			return normalizedModelConfig{}, modelConfigValidationError(fmt.Sprintf("permission_review.model %q must support tools and structured_output_with_tools", config.PermissionReview.Model))
		}
		normalized.PermissionReview = &normalizedPermissionReview{Model: config.PermissionReview.Model, Strict: config.PermissionReview.Strict}
	}

	sort.Slice(normalized.Models, func(i, j int) bool {
		return normalized.Models[i].Alias < normalized.Models[j].Alias
	})
	return normalized, nil
}

func normalizeNativeACPProfiles(config map[string]nativeACPProfileConfig) (map[string]normalizedNativeACPProfile, error) {
	if config == nil {
		return nil, nil
	}
	known := map[string]struct{}{"claude-code": {}, "codex": {}}
	for name := range config {
		if _, ok := known[name]; !ok {
			return nil, modelConfigValidationError("native_acp contains an unknown profile")
		}
	}
	profiles := make(map[string]normalizedNativeACPProfile, len(config))
	for harness, profile := range config {
		if profile.Models != nil && len(*profile.Models) == 0 {
			return nil, modelConfigValidationError("native_acp models must be omitted for harness-managed mode and non-empty when present")
		}
		var models []string
		var options []normalizedNativeACPModel
		hasStructuredModels := false
		if profile.Models != nil {
			models = make([]string, 0, len(*profile.Models))
			options = make([]normalizedNativeACPModel, 0, len(*profile.Models))
		}
		seenModels := make(map[string]struct{}, len(models))
		var configuredModels []nativeACPModelConfig
		if profile.Models != nil {
			configuredModels = *profile.Models
		}
		for _, configured := range configuredModels {
			if !validModelConfigAlias(configured.Model) {
				return nil, modelConfigValidationError("native_acp model alias violates the runtime identifier contract")
			}
			if _, duplicate := seenModels[configured.Model]; duplicate {
				return nil, modelConfigValidationError("native_acp model aliases must be unique")
			}
			seenModels[configured.Model] = struct{}{}

			var efforts []model.Effort
			var defaultEffort model.Effort
			if configured.Legacy {
				efforts = []model.Effort{model.EffortNone}
				defaultEffort = model.EffortNone
			} else {
				hasStructuredModels = true
				if len(configured.Efforts) == 0 {
					return nil, modelConfigValidationError("native_acp model efforts must be non-empty")
				}
				efforts = make([]model.Effort, 0, len(configured.Efforts))
				seenEfforts := make(map[model.Effort]struct{}, len(configured.Efforts))
				for _, rawEffort := range configured.Efforts {
					effort, valid := neutralModelConfigEffort(rawEffort)
					if !valid {
						return nil, modelConfigValidationError("native_acp model efforts contain an invalid effort")
					}
					if _, duplicate := seenEfforts[effort]; duplicate {
						return nil, modelConfigValidationError("native_acp model efforts must be unique")
					}
					seenEfforts[effort] = struct{}{}
					efforts = append(efforts, effort)
				}
				var valid bool
				defaultEffort, valid = neutralModelConfigEffort(configured.DefaultEffort)
				if !valid {
					return nil, modelConfigValidationError("native_acp model default_effort is invalid")
				}
				if _, admitted := seenEfforts[defaultEffort]; !admitted {
					return nil, modelConfigValidationError("native_acp model default_effort must be present in efforts")
				}
			}
			sort.Slice(efforts, func(i, j int) bool {
				return modelConfigEffortRank(efforts[i]) < modelConfigEffortRank(efforts[j])
			})
			models = append(models, configured.Model)
			options = append(options, normalizedNativeACPModel{
				Model: configured.Model, Efforts: efforts, DefaultEffort: defaultEffort,
			})
		}
		sort.Strings(models)
		sort.Slice(options, func(i, j int) bool {
			return options[i].Model < options[j].Model
		})
		profiles[harness] = normalizedNativeACPProfile{
			Harness: harness, Enabled: profile.Enabled, Models: models, ModelOptions: options,
			HasStructuredModels: hasStructuredModels,
		}
	}
	return profiles, nil
}

func normalizeACPLaunchers(config map[string]acpLauncherConfig) (map[string]normalizedACPLauncher, error) {
	if config == nil {
		return nil, nil
	}
	known := map[string]struct{}{"claude-code": {}, "codex": {}}
	launchers := make(map[string]normalizedACPLauncher, len(config))
	for harness, entry := range config {
		if _, ok := known[harness]; !ok {
			return nil, modelConfigValidationError("acp_launchers contains an unknown harness")
		}
		if !isExactNonEmptyModelConfigString(entry.Executable) {
			return nil, modelConfigValidationError("acp_launchers executable must be non-empty and unpadded")
		}
		if !filepath.IsAbs(entry.Executable) || filepath.Clean(entry.Executable) != entry.Executable {
			return nil, modelConfigValidationError("acp_launchers executable must be a clean absolute path")
		}
		launchers[harness] = normalizedACPLauncher{Harness: harness, Executable: entry.Executable}
	}
	return launchers, nil
}

func normalizeModelTarget(target modelTargetConfig) (normalizedModelTarget, error) {
	var normalized normalizedModelTarget
	if !validModelConfigAlias(target.Alias) {
		return normalized, modelConfigValidationError("alias violates the runtime identifier contract")
	}
	for field, value := range map[string]string{
		"provider": target.Provider, "api_format": target.APIFormat, "model": target.Model,
	} {
		if !isExactNonEmptyModelConfigString(value) {
			return normalized, modelConfigValidationError(field + " must be non-empty and unpadded")
		}
	}
	// An empty or absent uses is valid: the model is neither primer- nor
	// delegate-capable, addressable only by alias (today, only
	// permission_review.model resolves a model this way). The loop below
	// still rejects unknown values and duplicates when uses IS non-empty.
	usesSeen := make(map[string]struct{}, len(target.Uses))
	uses := make([]string, 0, len(target.Uses))
	for _, use := range target.Uses {
		if use != "primer" && use != "delegate" {
			return normalized, modelConfigValidationError("uses may contain only primer or delegate")
		}
		if _, duplicate := usesSeen[use]; duplicate {
			return normalized, modelConfigValidationError("uses must not contain duplicates")
		}
		usesSeen[use] = struct{}{}
		uses = append(uses, use)
	}
	description, err := normalizeModelDescription(target.Description, containsModelConfigUse(uses, "delegate"))
	if err != nil {
		return normalized, err
	}
	if !target.Capabilities.Tools {
		return normalized, modelConfigValidationError("models must support tools")
	}
	if target.Capabilities.StructuredOutputWithTools && (!target.Capabilities.Tools || !target.Capabilities.StructuredOutput) {
		return normalized, modelConfigValidationError("structured_output_with_tools requires tools and structured_output")
	}

	if len(target.Efforts) == 0 {
		return normalized, modelConfigValidationError("efforts must not be empty")
	}
	effortsSeen := make(map[model.Effort]struct{}, len(target.Efforts))
	efforts := make([]model.Effort, 0, len(target.Efforts))
	for _, configured := range target.Efforts {
		effort, valid := neutralModelConfigEffort(configured)
		if !valid {
			return normalized, modelConfigValidationError("efforts contains an invalid neutral effort")
		}
		if _, duplicate := effortsSeen[effort]; duplicate {
			return normalized, modelConfigValidationError("efforts must not contain duplicates")
		}
		if effort != model.EffortNone && !target.Capabilities.Thinking {
			return normalized, modelConfigValidationError("non-none effort requires thinking capability")
		}
		effortsSeen[effort] = struct{}{}
		efforts = append(efforts, effort)
	}
	defaultEffort, valid := neutralModelConfigEffort(target.DefaultEffort)
	if !valid {
		return normalized, modelConfigValidationError("default_effort is invalid")
	}
	if _, admitted := effortsSeen[defaultEffort]; !admitted {
		return normalized, modelConfigValidationError("default_effort must be present in efforts")
	}

	options := modelConfigCapabilityOptions(target.Capabilities)
	options = append(options, model.WithContextLimits(model.ContextLimits{
		WindowTokens:    target.ContextLimits.WindowTokens,
		MaxInputTokens:  target.ContextLimits.MaxInputTokens,
		MaxOutputTokens: target.ContextLimits.MaxOutputTokens,
	}))
	options = append(options, model.WithSampling(model.Sampling{Effort: defaultEffort}))
	constructed := model.CustomModel(
		model.ProviderName(target.Provider), model.APIFormat(target.APIFormat), target.BaseURL, target.Model, options...,
	)
	if err := constructed.Validate(); err != nil {
		return normalized, modelConfigFailure("validate", err)
	}
	if err := llm.ValidateModel(constructed); err != nil {
		return normalized, modelConfigFailure("validate", err)
	}
	requiredAuth, err := llm.Provider(target.Provider).RequiredAuth()
	if err != nil {
		return normalized, modelConfigFailure("validate", err)
	}
	switch requiredAuth {
	case auth.AuthNone:
		if target.APIKey != "" {
			return normalized, modelConfigValidationError("no-auth provider must not receive api_key")
		}
	case auth.AuthAPIKey:
		if target.APIKey == "" {
			return normalized, modelConfigValidationError("API-key provider requires api_key")
		}
	default:
		return normalized, modelConfigValidationError("provider requires credentials unsupported by this schema")
	}

	sort.Strings(uses)
	sort.Slice(efforts, func(i, j int) bool {
		return modelConfigEffortRank(efforts[i]) < modelConfigEffortRank(efforts[j])
	})
	return normalizedModelTarget{
		Alias: target.Alias, Description: description, Model: constructed, Uses: uses, Efforts: efforts,
		DefaultEffort: defaultEffort, client: modelClientInput{APIKey: target.APIKey},
	}, nil
}

const maxModelDescriptionBytes = 256

func normalizeModelDescription(value string, required bool) (string, error) {
	if value == "" {
		if required {
			return "", modelConfigValidationError("delegate model descriptions are required")
		}
		return "", nil
	}
	if !utf8.ValidString(value) {
		return "", modelConfigValidationError("model description must be valid UTF-8")
	}
	for _, r := range value {
		if r == 0 || r == '\r' || r == '\n' || (unicode.IsControl(r) && r != '\t') {
			return "", modelConfigValidationError("model description contains forbidden control characters")
		}
	}
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return "", modelConfigValidationError("model description must be nonblank")
	}
	if len(normalized) > maxModelDescriptionBytes {
		return "", modelConfigValidationError("model description exceeds the maximum length")
	}
	if forbiddenModelDescriptionMaterial(normalized) {
		return "", modelConfigValidationError("model description contains forbidden material")
	}
	return normalized, nil
}

func forbiddenModelDescriptionMaterial(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"api_key", "apikey", "access_key", "access token", "password", "secret", "credential", "bearer"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if strings.Contains(value, "://") {
		return true
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "~/") {
		return true
	}
	for _, marker := range []string{"/bin/", "/sbin/", "/usr/", "/opt/", "/var/", `\bin\`, `\program files\`} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func modelConfigCapabilityOptions(capabilities modelCapabilitiesConfig) []model.ModelOption {
	options := make([]model.ModelOption, 0, 6)
	if capabilities.Tools {
		options = append(options, model.WithTools())
	}
	if capabilities.Thinking {
		options = append(options, model.WithThinking())
	}
	if capabilities.Images {
		options = append(options, model.WithImages())
	}
	if capabilities.PromptCaching {
		options = append(options, model.WithPromptCaching())
	}
	if capabilities.StructuredOutput {
		options = append(options, model.WithStructuredOutput())
	}
	if capabilities.StructuredOutputWithTools {
		options = append(options, model.WithStructuredOutputWithTools())
	}
	return options
}

func neutralModelConfigEffort(value string) (model.Effort, bool) {
	switch value {
	case "none":
		return model.EffortNone, true
	case "low":
		return model.EffortLow, true
	case "medium":
		return model.EffortMedium, true
	case "high":
		return model.EffortHigh, true
	case "max":
		return model.EffortMax, true
	default:
		return model.EffortNone, false
	}
}

func modelConfigEffortRank(effort model.Effort) int {
	switch effort {
	case model.EffortNone:
		return 0
	case model.EffortLow:
		return 1
	case model.EffortMedium:
		return 2
	case model.EffortHigh:
		return 3
	case model.EffortMax:
		return 4
	default:
		return 5
	}
}

func isExactNonEmptyModelConfigString(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func validModelConfigAlias(value string) bool {
	const maxAliasBytes = 128
	if value == "" || len(value) > maxAliasBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r == '/' || r == '\\' || r == ':' || r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func containsModelConfigUse(uses []string, want string) bool {
	for _, use := range uses {
		if use == want {
			return true
		}
	}
	return false
}

func modelConfigValidationError(reason string) *ModelConfigError {
	return modelConfigFailure("validate", errors.New(reason))
}

func (c normalizedModelConfig) String() string {
	return fmt.Sprintf("model config primer=%q claude_small=%q models=%d native_profiles=%d", c.PrimerDefault, c.ClaudeCodeSmallModel, len(c.Models), len(c.NativeACP))
}

func (c normalizedModelConfig) GoString() string { return c.String() }
