package app

import (
	"errors"
	"strings"
	"testing"

	model "github.com/looprig/inference/model"
)

func TestValidateModelConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*modelConfigFile)
	}{
		{name: "empty alias", mutate: func(c *modelConfigFile) { c.Models[0].Alias = "" }},
		{name: "padded alias", mutate: func(c *modelConfigFile) { c.Models[0].Alias = " local" }},
		{name: "empty provider", mutate: func(c *modelConfigFile) { c.Models[0].Provider = "" }},
		{name: "padded provider", mutate: func(c *modelConfigFile) { c.Models[0].Provider += " " }},
		{name: "empty format", mutate: func(c *modelConfigFile) { c.Models[0].APIFormat = "" }},
		{name: "padded format", mutate: func(c *modelConfigFile) { c.Models[0].APIFormat += " " }},
		{name: "empty model", mutate: func(c *modelConfigFile) { c.Models[0].Model = "" }},
		{name: "padded model", mutate: func(c *modelConfigFile) { c.Models[0].Model += " " }},
		{name: "duplicate aliases", mutate: func(c *modelConfigFile) { c.Models = append(c.Models, c.Models[0]) }},
		{name: "empty uses", mutate: func(c *modelConfigFile) { c.Models[0].Uses = nil }},
		{name: "empty use", mutate: func(c *modelConfigFile) { c.Models[0].Uses[0] = "" }},
		{name: "padded use", mutate: func(c *modelConfigFile) { c.Models[0].Uses[0] = "primer " }},
		{name: "duplicate uses", mutate: func(c *modelConfigFile) { c.Models[0].Uses = append(c.Models[0].Uses, "primer") }},
		{name: "invalid use", mutate: func(c *modelConfigFile) { c.Models[0].Uses[0] = "chat" }},
		{name: "no models", mutate: func(c *modelConfigFile) { c.Models = nil }},
		{name: "missing primer default", mutate: func(c *modelConfigFile) { c.PrimerDefault = "" }},
		{name: "padded primer default", mutate: func(c *modelConfigFile) { c.PrimerDefault = " local" }},
		{name: "non-primer primer default", mutate: func(c *modelConfigFile) { c.Models[0].Uses = []string{"delegate"} }},
		{name: "primer missing tools", mutate: func(c *modelConfigFile) { c.Models[0].Capabilities.Tools = false }},
		{name: "empty efforts", mutate: func(c *modelConfigFile) { c.Models[0].Efforts = nil }},
		{name: "empty effort", mutate: func(c *modelConfigFile) { c.Models[0].Efforts[0] = "" }},
		{name: "duplicate efforts", mutate: func(c *modelConfigFile) { c.Models[0].Efforts = append(c.Models[0].Efforts, "none") }},
		{name: "invalid xhigh effort", mutate: func(c *modelConfigFile) { c.Models[0].Efforts[0] = "xhigh" }},
		{name: "invalid ultra effort", mutate: func(c *modelConfigFile) { c.Models[0].Efforts[0] = "ultra" }},
		{name: "empty default effort", mutate: func(c *modelConfigFile) { c.Models[0].DefaultEffort = "" }},
		{name: "padded default effort", mutate: func(c *modelConfigFile) { c.Models[0].DefaultEffort = "none " }},
		{name: "default effort absent", mutate: func(c *modelConfigFile) { c.Models[0].DefaultEffort = "low" }},
		{name: "thinking effort without capability", mutate: func(c *modelConfigFile) { c.Models[0].Efforts = []string{"low"}; c.Models[0].DefaultEffort = "low" }},
		{name: "invalid base URL", mutate: func(c *modelConfigFile) { c.Models[0].BaseURL = "://bad" }},
		{name: "insecure base URL", mutate: func(c *modelConfigFile) { c.Models[0].BaseURL = "http://models.example.test/v1" }},
		{name: "base URL credentials", mutate: func(c *modelConfigFile) { c.Models[0].BaseURL = "https://user:pass@models.example.test/v1" }},
		{name: "unsupported provider format pair", mutate: func(c *modelConfigFile) { c.Models[0].APIFormat = "gemini" }},
		{name: "missing API key", mutate: func(c *modelConfigFile) { makeOpenAIModel(&c.Models[0]); c.Models[0].APIKey = "" }},
		{name: "API key on no-auth provider", mutate: func(c *modelConfigFile) { c.Models[0].APIKey = "test-secret-do-not-log" }},
		{name: "special credentials unsupported", mutate: func(c *modelConfigFile) {
			c.Models[0].Provider = "bedrock"
			c.Models[0].APIFormat = "anthropic"
			c.Models[0].BaseURL = ""
		}},
		{name: "unknown delegate role", mutate: func(c *modelConfigFile) { c.DelegateDefaults["operator"] = c.DelegateDefaults["planner"] }},
		{name: "missing delegate role", mutate: func(c *modelConfigFile) { delete(c.DelegateDefaults, "reviewer") }},
		{name: "invalid harness", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Harness = "cursor"
			c.DelegateDefaults["planner"] = d
		}},
		{name: "empty delegate default model", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Model = ""
			c.DelegateDefaults["planner"] = d
		}},
		{name: "padded default model", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Model = "local "
			c.DelegateDefaults["planner"] = d
		}},
		{name: "default model not delegate capable", mutate: func(c *modelConfigFile) { c.Models[0].Uses = []string{"primer"} }},
		{name: "default effort not admitted by model", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Effort = "low"
			c.DelegateDefaults["planner"] = d
		}},
		{name: "Claude default without small model", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Harness = "claude-code"
			c.DelegateDefaults["planner"] = d
		}},
		{name: "Claude small model not delegate capable", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Harness = "claude-code"
			c.DelegateDefaults["planner"] = d
			c.ClaudeCodeSmallModel = "local"
			c.Models[0].Uses = []string{"primer"}
		}},
		{name: "Claude small model lacks tools", mutate: func(c *modelConfigFile) {
			d := c.DelegateDefaults["planner"]
			d.Harness = "claude-code"
			c.DelegateDefaults["planner"] = d
			c.ClaudeCodeSmallModel = "small"
			small := c.Models[0]
			small.Alias = "small"
			small.Uses = []string{"delegate"}
			small.Capabilities.Tools = false
			c.Models = append(c.Models, small)
		}},
		{name: "structured output with tools lacks prerequisite", mutate: func(c *modelConfigFile) { c.Models[0].Capabilities.StructuredOutputWithTools = true }},
		{name: "permission_review missing model", mutate: func(c *modelConfigFile) { c.PermissionReview = &permissionReviewConfig{Strict: true} }},
		{name: "permission_review unknown alias", mutate: func(c *modelConfigFile) { c.PermissionReview = &permissionReviewConfig{Model: "does-not-exist"} }},
		{name: "permission_review model lacks structured_output_with_tools", mutate: func(c *modelConfigFile) { c.PermissionReview = &permissionReviewConfig{Model: "local"} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validDecodedModelConfig(t)
			tt.mutate(&config)
			_, err := normalizeModelConfig(config)
			if err == nil {
				t.Fatal("normalizeModelConfig() error = nil, want error")
			}
			var configErr *ModelConfigError
			if !errors.As(err, &configErr) {
				t.Errorf("normalizeModelConfig() error = %T, want *ModelConfigError", err)
			}
			if len(err.Error()) > 512 {
				t.Errorf("error length = %d, want bounded", len(err.Error()))
			}
			if strings.Contains(err.Error(), "test-secret-do-not-log") {
				t.Errorf("error leaked API key: %v", err)
			}
		})
	}
}

func TestValidateModelConfigRejectsUnsafeAliases(t *testing.T) {
	tests := []struct {
		name  string
		alias string
	}{
		{name: "slash", alias: "vendor/model"},
		{name: "backslash", alias: `vendor\model`},
		{name: "colon", alias: "vendor:model"},
		{name: "NUL", alias: "vendor\x00model"},
		{name: "control rune", alias: "vendor\u007fmodel"},
		{name: "internal ASCII whitespace", alias: "vendor model"},
		{name: "internal Unicode whitespace", alias: "vendor\u2003model"},
		{name: "oversized", alias: strings.Repeat("a", 129)},
		{name: "invalid UTF-8", alias: string([]byte{'a', 0xff, 'b'})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validDecodedModelConfig(t)
			setSingleModelConfigAlias(&config, tt.alias)
			_, err := normalizeModelConfig(config)
			if err == nil {
				t.Fatal("normalizeModelConfig() error = nil, want unsafe alias rejection")
			}
			var configErr *ModelConfigError
			if !errors.As(err, &configErr) {
				t.Errorf("normalizeModelConfig() error = %T, want *ModelConfigError", err)
			}
			if len(err.Error()) > 512 {
				t.Errorf("error length = %d, want bounded", len(err.Error()))
			}
		})
	}
}

func TestNormalizeModelConfigAcceptsMaximumLengthAlias(t *testing.T) {
	config := validDecodedModelConfig(t)
	alias := strings.Repeat("a", 128)
	setSingleModelConfigAlias(&config, alias)

	normalized, err := normalizeModelConfig(config)
	if err != nil {
		t.Fatalf("normalizeModelConfig(128-byte alias) error = %v", err)
	}
	if len(normalized.Models) != 1 || normalized.Models[0].Alias != alias {
		t.Fatalf("normalized alias = %+v, want 128-byte alias", normalized.Models)
	}
}

func TestNormalizeModelConfigDescriptions(t *testing.T) {
	t.Parallel()

	t.Run("normalizes bounded presentation text", func(t *testing.T) {
		config := validDecodedModelConfig(t)
		config.Models[0].Description = "  Local\tmodel  for  focused\nwork.  "
		// Newlines are not presentation whitespace: they must be rejected rather
		// than silently turned into a multi-line model-facing description.
		if _, err := normalizeModelConfig(config); err == nil {
			t.Fatal("normalizeModelConfig() error = nil, want newline rejection")
		}

		config.Models[0].Description = "  Local\tmodel  for  focused work.  "
		normalized, err := normalizeModelConfig(config)
		if err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
		if got, want := normalized.Models[0].Description, "Local model for focused work."; got != want {
			t.Fatalf("normalized description = %q, want %q", got, want)
		}
	})

	t.Run("delegate descriptions are required and secret-free", func(t *testing.T) {
		cases := []struct {
			name string
			text string
		}{
			{name: "missing", text: ""},
			{name: "blank", text: "   "},
			{name: "nul", text: "model\x00guidance"},
			{name: "newline", text: "model\nguidance"},
			{name: "too long", text: strings.Repeat("a", 257)},
			{name: "url", text: "See https://provider.example/model"},
			{name: "path", text: "Run /usr/local/bin/model"},
			{name: "credential", text: "Uses api_key credentials"},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				config := validDecodedModelConfig(t)
				config.Models[0].Description = tt.text
				if _, err := normalizeModelConfig(config); err == nil {
					t.Fatal("normalizeModelConfig() error = nil, want description validation error")
				}
			})
		}
	})

	t.Run("non-delegate models may omit descriptions", func(t *testing.T) {
		config := validDecodedModelConfig(t)
		primerOnly := config.Models[0]
		primerOnly.Alias = "primer-only"
		primerOnly.Uses = []string{"primer"}
		primerOnly.Description = ""
		config.Models = append(config.Models, primerOnly)
		if _, err := normalizeModelConfig(config); err != nil {
			t.Fatalf("normalizeModelConfig() error = %v", err)
		}
	})
}

func TestNormalizeModelConfigPreservesOptionalFields(t *testing.T) {
	input := `{
		"version": 2,
		"primer_default": "local",
		"delegate_defaults": {
			"planner": {"harness":"codex","model":"local","effort":"none"},
			"builder": {"harness":"codex","model":"local","effort":"none"},
			"reviewer": {"harness":"codex","model":"local","effort":"none"}
		},
		"models": [{
			"alias": "local",
			"description": "Local in-process coding model.",
			"provider": "lmstudio",
			"api_format": "openai",
			"model": "qwen3-coder",
			"uses": ["primer", "delegate"],
			"capabilities": {"tools": true},
			"efforts": ["none"],
			"default_effort": "none"
		}]
	}`
	wire, err := decodeModelConfig([]byte(input))
	if err != nil {
		t.Fatalf("decodeModelConfig(optional fields) error = %v", err)
	}
	normalized, err := normalizeModelConfig(wire)
	if err != nil {
		t.Fatalf("normalizeModelConfig(optional fields) error = %v", err)
	}
	if normalized.ClaudeCodeSmallModel != "" || len(normalized.Models) != 1 {
		t.Fatalf("normalized optional fields = %+v", normalized)
	}
	target := normalized.Models[0]
	if target.Model.BaseURL != "" || target.client.APIKey != "" {
		t.Errorf("optional client fields = base %q, key present %v", target.Model.BaseURL, target.client.APIKey != "")
	}
	if !target.Model.Caps.Tools || target.Model.Caps.Thinking || target.Model.Caps.AcceptsImages || target.Model.Caps.PromptCaching || target.Model.Caps.StructuredOutput || target.Model.Caps.StructuredOutputWithTools {
		t.Errorf("omitted capability fields = %+v, want only tools", target.Model.Caps)
	}
}

func TestNormalizeModelConfigAppliesConfiguredContextLimits(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		validLMStudioModelConfig,
		`"model": "qwen3-coder"`,
		`"model": "qwen3-coder", "context_limits": {"max_input_tokens": 256000}`,
		1,
	)
	decoded, err := decodeModelConfig([]byte(input))
	if err != nil {
		t.Fatalf("decodeModelConfig() error = %v", err)
	}
	normalized, err := normalizeModelConfig(decoded)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	want := model.ContextLimits{MaxInputTokens: 256_000}
	if got := normalized.Models[0].Model.Limits; got != want {
		t.Fatalf("configured context limits = %+v, want %+v", got, want)
	}
}

func TestNormalizeModelConfig(t *testing.T) {
	config := validDecodedModelConfig(t)
	makeOpenAIModel(&config.Models[0])
	config.Models[0].Alias = "zeta"
	config.Models[0].Capabilities = modelCapabilitiesConfig{
		Tools: true, Thinking: true, Images: true, PromptCaching: true,
		StructuredOutput: true, StructuredOutputWithTools: true,
	}
	config.Models[0].Uses = []string{"primer", "delegate"}
	config.Models[0].Efforts = []string{"high", "none", "low"}
	config.Models[0].DefaultEffort = "high"
	config.PrimerDefault = "zeta"
	for role, value := range config.DelegateDefaults {
		value.Model = "zeta"
		value.Effort = "high"
		config.DelegateDefaults[role] = value
	}
	alpha := config.Models[0]
	alpha.Alias = "alpha"
	alpha.Uses = []string{"delegate"}
	alpha.Efforts = []string{"none"}
	alpha.DefaultEffort = "none"
	config.Models = append(config.Models, alpha)

	got, err := normalizeModelConfig(config)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	if len(got.Models) != 2 || got.Models[0].Alias != "alpha" || got.Models[1].Alias != "zeta" {
		t.Fatalf("model order = %+v, want alpha then zeta", got.Models)
	}
	if strings.Join(got.Models[1].Uses, ",") != "delegate,primer" {
		t.Errorf("uses = %v, want lexical order", got.Models[1].Uses)
	}
	wantEfforts := []model.Effort{model.EffortNone, model.EffortLow, model.EffortHigh}
	if !equalEfforts(got.Models[1].Efforts, wantEfforts) {
		t.Errorf("efforts = %v, want %v", got.Models[1].Efforts, wantEfforts)
	}
	if len(got.DelegateDefaults) != 3 || got.DelegateDefaults[0].Role != "planner" || got.DelegateDefaults[1].Role != "builder" || got.DelegateDefaults[2].Role != "reviewer" {
		t.Errorf("delegate default order = %+v", got.DelegateDefaults)
	}
	constructed := got.Models[1].Model
	if constructed.Provider != "openai" || constructed.APIFormat != model.APIFormatOpenAIResponses || constructed.Name != "qwen3-coder" || constructed.Sampling.Effort != model.EffortHigh {
		t.Errorf("constructed model = %+v", constructed)
	}
	if !constructed.Caps.Tools || !constructed.Caps.Thinking || !constructed.Caps.AcceptsImages || !constructed.Caps.PromptCaching || !constructed.Caps.StructuredOutput || !constructed.Caps.StructuredOutputWithTools {
		t.Errorf("constructed capabilities = %+v", constructed.Caps)
	}
	if got.Models[1].client.APIKey != "test-secret-do-not-log" {
		t.Fatal("private client input did not retain API key")
	}
}

func validDecodedModelConfig(t *testing.T) modelConfigFile {
	t.Helper()
	config, err := decodeModelConfig([]byte(validLMStudioModelConfig))
	if err != nil {
		t.Fatalf("decode valid fixture: %v", err)
	}
	return config
}

func setSingleModelConfigAlias(config *modelConfigFile, alias string) {
	config.Models[0].Alias = alias
	config.PrimerDefault = alias
	for role, value := range config.DelegateDefaults {
		value.Model = alias
		config.DelegateDefaults[role] = value
	}
}

func makeOpenAIModel(target *modelTargetConfig) {
	target.Provider = "openai"
	target.APIFormat = "openai-responses"
	target.BaseURL = "https://api.openai.com/v1"
	target.APIKey = "test-secret-do-not-log"
}

func equalEfforts(got, want []model.Effort) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
