package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

var modelConfigDelegateRoleOrder = [...]string{"planner", "builder", "reviewer"}

type normalizedModelConfig struct {
	PrimerDefault        string
	ClaudeCodeSmallModel string
	DelegateDefaults     []normalizedDelegateDefault
	Models               []normalizedModelTarget
	NativeACP            map[string]normalizedNativeACPProfile
	ACPLaunchers         map[string]normalizedACPLauncher
}

type normalizedDelegateDefault struct {
	Role      string
	Harness   string
	Source    loop.RuntimeSourceName
	Model     string
	Effort    model.Effort
	EffortSet bool
}

type normalizedNativeACPProfile struct {
	Harness string
	Enabled bool
	Models  []string
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

	if len(config.DelegateDefaults) != len(modelConfigDelegateRoleOrder) {
		return normalizedModelConfig{}, modelConfigValidationError("delegate_defaults must contain exactly planner, builder, and reviewer")
	}
	normalized.DelegateDefaults = make([]normalizedDelegateDefault, 0, len(modelConfigDelegateRoleOrder))
	claudeDefault := false
	for _, role := range modelConfigDelegateRoleOrder {
		value, ok := config.DelegateDefaults[role]
		if !ok {
			return normalizedModelConfig{}, modelConfigValidationError("delegate_defaults is missing a required role")
		}
		if value.Harness != "codex" && value.Harness != "claude-code" {
			return normalizedModelConfig{}, modelConfigValidationError("delegate default harness must be codex or claude-code")
		}
		source := loop.RuntimeSourceGateway
		if value.Source != "" {
			switch value.Source {
			case string(loop.RuntimeSourceGateway):
				source = loop.RuntimeSourceGateway
			case string(loop.RuntimeSourceNative):
				source = loop.RuntimeSourceNative
			default:
				return normalizedModelConfig{}, modelConfigValidationError("delegate default source must be gateway or native")
			}
		}

		var effort model.Effort
		if source == loop.RuntimeSourceGateway {
			if !isExactNonEmptyModelConfigString(value.Model) {
				return normalizedModelConfig{}, modelConfigValidationError("delegate default model must be non-empty and unpadded")
			}
			target, exists := byAlias[value.Model]
			if !exists || !containsModelConfigUse(target.Uses, "delegate") {
				return normalizedModelConfig{}, modelConfigValidationError("delegate default model must be delegate-capable")
			}
			var valid bool
			effort, valid = neutralModelConfigEffort(value.Effort)
			if !valid || !containsModelConfigEffort(target.Efforts, effort) {
				return normalizedModelConfig{}, modelConfigValidationError("delegate default effort must be admitted by its model")
			}
			claudeDefault = claudeDefault || value.Harness == "claude-code"
		} else {
			profile, exists := nativeACPProfileFor(normalized.NativeACP, value.Harness)
			if !exists || !profile.Enabled {
				return normalizedModelConfig{}, modelConfigValidationError("native delegate default requires an enabled native_acp profile")
			}
			if value.Effort != "" {
				return normalizedModelConfig{}, modelConfigValidationError("native delegate defaults must not override effort")
			}
			if profile.Models == nil {
				if value.Model != "" {
					return normalizedModelConfig{}, modelConfigValidationError("native harness-managed defaults must omit model")
				}
			} else {
				if !isExactNonEmptyModelConfigString(value.Model) || !containsNativeACPString(profile.Models, value.Model) {
					return normalizedModelConfig{}, modelConfigValidationError("native delegate default model must name a configured native alias")
				}
			}
			effort = model.EffortNone
		}
		normalized.DelegateDefaults = append(normalized.DelegateDefaults, normalizedDelegateDefault{
			Role: role, Harness: value.Harness, Source: source, Model: value.Model, Effort: effort,
			EffortSet: source == loop.RuntimeSourceGateway,
		})
	}

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
	} else if claudeDefault {
		return normalizedModelConfig{}, modelConfigValidationError("claude-code defaults require claude_code_small_model")
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
		if profile.Models != nil {
			models = append([]string(nil), (*profile.Models)...)
		}
		seen := make(map[string]struct{}, len(models))
		for _, alias := range models {
			if !validModelConfigAlias(alias) {
				return nil, modelConfigValidationError("native_acp model alias violates the runtime identifier contract")
			}
			if _, duplicate := seen[alias]; duplicate {
				return nil, modelConfigValidationError("native_acp model aliases must be unique")
			}
			seen[alias] = struct{}{}
		}
		sort.Strings(models)
		profiles[harness] = normalizedNativeACPProfile{Harness: harness, Enabled: profile.Enabled, Models: models}
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

func nativeACPProfileFor(profiles map[string]normalizedNativeACPProfile, harness string) (normalizedNativeACPProfile, bool) {
	profile, ok := profiles[harness]
	return profile, ok
}

func containsNativeACPString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
	if len(target.Uses) == 0 {
		return normalized, modelConfigValidationError("uses must not be empty")
	}
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
		return normalized, modelConfigValidationError("primer and delegate models must support tools")
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

func containsModelConfigEffort(efforts []model.Effort, want model.Effort) bool {
	for _, effort := range efforts {
		if effort == want {
			return true
		}
	}
	return false
}

func modelConfigValidationError(reason string) *ModelConfigError {
	return modelConfigFailure("validate", errors.New(reason))
}

func (c normalizedModelConfig) String() string {
	return fmt.Sprintf("model config primer=%q claude_small=%q defaults=%d models=%d native_profiles=%d", c.PrimerDefault, c.ClaudeCodeSmallModel, len(c.DelegateDefaults), len(c.Models), len(c.NativeACP))
}

func (c normalizedModelConfig) GoString() string { return c.String() }
