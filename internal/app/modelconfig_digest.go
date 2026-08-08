package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	model "github.com/looprig/inference/model"
)

type secretFreeModelConfig struct {
	Version              int                          `json:"version"`
	PrimerDefault        string                       `json:"primer_default"`
	ClaudeCodeSmallModel string                       `json:"claude_code_small_model"`
	Models               []secretFreeModelTarget      `json:"models"`
	NativeACP            []secretFreeNativeACPProfile `json:"native_acp"`
}

type secretFreeNativeACPProfile struct {
	Harness string `json:"harness"`
	Enabled bool   `json:"enabled"`
	Models  any    `json:"models,omitempty"`
}

type secretFreeNativeACPModel struct {
	Model         string   `json:"model"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
}

type secretFreeModelTarget struct {
	Alias         string                       `json:"alias"`
	Description   string                       `json:"description,omitempty"`
	Provider      string                       `json:"provider"`
	APIFormat     string                       `json:"api_format"`
	BaseURL       string                       `json:"base_url"`
	Model         string                       `json:"model"`
	ContextLimits secretFreeModelContextLimits `json:"context_limits"`
	Uses          []string                     `json:"uses"`
	Capabilities  secretFreeModelCapabilities  `json:"capabilities"`
	Efforts       []string                     `json:"efforts"`
	DefaultEffort string                       `json:"default_effort"`
}

type secretFreeModelContextLimits struct {
	WindowTokens    uint64 `json:"window_tokens,omitempty"`
	MaxInputTokens  uint64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens uint64 `json:"max_output_tokens,omitempty"`
}

type secretFreeModelCapabilities struct {
	Tools                     bool `json:"tools"`
	Thinking                  bool `json:"thinking"`
	Images                    bool `json:"images"`
	PromptCaching             bool `json:"prompt_caching"`
	StructuredOutput          bool `json:"structured_output"`
	StructuredOutputWithTools bool `json:"structured_output_with_tools"`
}

func modelConfigDigest(config normalizedModelConfig) (string, error) {
	material, err := secretFreeModelConfigJSON(config)
	if err != nil {
		return "", modelConfigFailure("digest", err)
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

func secretFreeModelConfigJSON(config normalizedModelConfig) ([]byte, error) {
	projection := secretFreeModelConfig{
		Version:              modelConfigVersion,
		PrimerDefault:        config.PrimerDefault,
		ClaudeCodeSmallModel: config.ClaudeCodeSmallModel,
		Models:               make([]secretFreeModelTarget, 0, len(config.Models)),
		NativeACP:            make([]secretFreeNativeACPProfile, 0, len(config.NativeACP)),
	}
	for _, profile := range config.NativeACP {
		models := secretFreeNativeACPModels(profile)
		projection.NativeACP = append(projection.NativeACP, secretFreeNativeACPProfile{
			Harness: profile.Harness, Enabled: profile.Enabled, Models: models,
		})
	}
	sort.Slice(projection.NativeACP, func(i, j int) bool {
		return projection.NativeACP[i].Harness < projection.NativeACP[j].Harness
	})
	for _, target := range config.Models {
		uses := append([]string(nil), target.Uses...)
		sort.Strings(uses)
		efforts := append([]model.Effort(nil), target.Efforts...)
		sort.Slice(efforts, func(i, j int) bool {
			return modelConfigEffortRank(efforts[i]) < modelConfigEffortRank(efforts[j])
		})
		effortNames := make([]string, len(efforts))
		for i, effort := range efforts {
			effortNames[i] = modelConfigEffortName(effort)
		}
		projection.Models = append(projection.Models, secretFreeModelTarget{
			Alias: target.Alias, Description: target.Description, Provider: string(target.Model.Provider), APIFormat: string(target.Model.APIFormat),
			BaseURL: target.Model.BaseURL, Model: target.Model.Name,
			ContextLimits: secretFreeModelContextLimits{
				WindowTokens: uint64(target.Model.Limits.WindowTokens), MaxInputTokens: uint64(target.Model.Limits.MaxInputTokens),
				MaxOutputTokens: uint64(target.Model.Limits.MaxOutputTokens),
			},
			Uses: uses,
			Capabilities: secretFreeModelCapabilities{
				Tools: target.Model.Caps.Tools, Thinking: target.Model.Caps.Thinking,
				Images: target.Model.Caps.AcceptsImages, PromptCaching: target.Model.Caps.PromptCaching,
				StructuredOutput:          target.Model.Caps.StructuredOutput,
				StructuredOutputWithTools: target.Model.Caps.StructuredOutputWithTools,
			},
			Efforts: effortNames, DefaultEffort: modelConfigEffortName(target.DefaultEffort),
		})
	}
	sort.Slice(projection.Models, func(i, j int) bool {
		return projection.Models[i].Alias < projection.Models[j].Alias
	})
	return json.Marshal(projection)
}

func secretFreeNativeACPModels(profile normalizedNativeACPProfile) any {
	if profile.Models == nil {
		return nil
	}
	if len(profile.ModelOptions) == 0 {
		// Keep lower-level normalized profiles that carry only the historical
		// model-ID projection stable as well.
		models := append([]string(nil), profile.Models...)
		sort.Strings(models)
		return models
	}

	options := append([]normalizedNativeACPModel(nil), profile.ModelOptions...)
	sort.Slice(options, func(i, j int) bool { return options[i].Model < options[j].Model })
	allModelOnly := true
	for _, option := range options {
		if !nativeACPModelOnly(option) {
			allModelOnly = false
			break
		}
	}
	if allModelOnly {
		// A legacy string and a structured row with only neutral effort are
		// semantically identical. Returning the historical string projection
		// also preserves the exact pre-feature digest for legacy-only files.
		models := make([]string, 0, len(options))
		for _, option := range options {
			models = append(models, option.Model)
		}
		return models
	}

	// Mixed profiles retain the semantic representation of each row instead of
	// switching every legacy/neutral row to an object when one non-none row is
	// configured. []any marshals the per-entry string/object union faithfully.
	models := make([]any, 0, len(options))
	for _, option := range options {
		if nativeACPModelOnly(option) {
			models = append(models, option.Model)
			continue
		}
		efforts := append([]model.Effort(nil), option.Efforts...)
		sort.Slice(efforts, func(i, j int) bool {
			return modelConfigEffortRank(efforts[i]) < modelConfigEffortRank(efforts[j])
		})
		effortNames := make([]string, len(efforts))
		for index, effort := range efforts {
			effortNames[index] = modelConfigEffortName(effort)
		}
		models = append(models, secretFreeNativeACPModel{
			Model: option.Model, Efforts: effortNames, DefaultEffort: modelConfigEffortName(option.DefaultEffort),
		})
	}
	return models
}

func nativeACPModelOnly(option normalizedNativeACPModel) bool {
	return option.DefaultEffort == model.EffortNone && len(option.Efforts) == 1 && option.Efforts[0] == model.EffortNone
}

func modelConfigEffortName(effort model.Effort) string {
	if effort == model.EffortNone {
		return "none"
	}
	return string(effort)
}

func (t normalizedModelTarget) String() string {
	return fmt.Sprintf("model target alias=%q provider=%q model=%q", t.Alias, t.Model.Provider, t.Model.Name)
}

func (t normalizedModelTarget) GoString() string { return t.String() }

func (modelClientInput) String() string { return "model client input (REDACTED)" }

func (c modelClientInput) GoString() string { return c.String() }
