package app

import (
	"errors"
	"testing"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// TestModelsAreValid proves every Carbon-owned model descriptor is well-formed,
// secret-free model.Model that passes the fail-closed llm.ValidateModel provider preset,
// and pins the exact wire identity (provider / API format / endpoint / model id / caps) each
// row was confirmed with. If a hardcoded row drifts, this fails loud here rather than at the
// first provider call.
func TestModelsAreValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		got        model.Model
		provider   llm.Provider
		apiFormat  model.APIFormat
		baseURL    string
		modelID    string
		wantCaps   model.Capabilities
		wantLimits model.ContextLimits
	}{
		{
			name:       "chutes kimi k2.6 (default)",
			got:        chutesKimiK26(),
			provider:   llm.ProviderChutes,
			apiFormat:  model.APIFormatOpenAI,
			baseURL:    "https://api.chutes.ai",
			modelID:    "moonshotai/Kimi-K2.6-TEE",
			wantCaps:   model.Capabilities{Tools: true, Thinking: true},
			wantLimits: model.ContextLimits{WindowTokens: 128_000},
		},
		{
			name:       "phala glm 5.2",
			got:        phalaGLM52(),
			provider:   llm.ProviderPhala,
			apiFormat:  model.APIFormatOpenAI,
			baseURL:    "https://inference.phala.com/v1",
			modelID:    "z-ai/glm-5.2",
			wantCaps:   model.Capabilities{Tools: true},
			wantLimits: model.ContextLimits{WindowTokens: 128_000},
		},
		{
			name:      "lmstudio local",
			got:       lmStudioLocal("some-local-model"),
			provider:  llm.ProviderLMStudio,
			apiFormat: model.APIFormatOpenAI,
			baseURL:   "http://localhost:1234/v1",
			modelID:   "some-local-model",
			wantCaps:  model.Capabilities{Tools: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := llm.ValidateModel(tt.got); err != nil {
				t.Fatalf("llm.ValidateModel(%s) = %v, want nil", tt.name, err)
			}
			if model.ProviderName(tt.provider) != tt.got.Provider {
				t.Errorf("Provider = %q, want %q", tt.got.Provider, tt.provider)
			}
			if tt.got.APIFormat != tt.apiFormat {
				t.Errorf("APIFormat = %q, want %q", tt.got.APIFormat, tt.apiFormat)
			}
			if tt.got.BaseURL != tt.baseURL {
				t.Errorf("BaseURL = %q, want %q", tt.got.BaseURL, tt.baseURL)
			}
			if tt.got.Name != tt.modelID {
				t.Errorf("Name = %q, want %q", tt.got.Name, tt.modelID)
			}
			if tt.got.Caps != tt.wantCaps {
				t.Errorf("Caps = %+v, want %+v", tt.got.Caps, tt.wantCaps)
			}
			if tt.got.Limits != tt.wantLimits {
				t.Errorf("Limits = %+v, want %+v", tt.got.Limits, tt.wantLimits)
			}
		})
	}
}

// TestMustValidModelRejectsMalformed proves mustValidModel fails loud (panics with a typed
// *InvalidDefaultModelError) on a hardcoded descriptor that cannot pass the provider preset.
func TestMustValidModelRejectsMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   model.Model
	}{
		{
			name: "unknown provider",
			in: model.CustomModel(
				model.ProviderName("bogus"), model.APIFormatOpenAI,
				"https://example.com", "some-model",
			),
		},
		{
			name: "provider/format mismatch",
			in: model.CustomModel(
				model.ProviderName(llm.ProviderGoogle), model.APIFormatOpenAI,
				"https://example.com", "some-model",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("mustValidModel did not panic on a malformed row")
				}
				err, ok := r.(error)
				if !ok {
					t.Fatalf("panic value = %T, want error", r)
				}
				var invalid *InvalidDefaultModelError
				if !errors.As(err, &invalid) {
					t.Fatalf("panic error = %v, want *InvalidDefaultModelError", err)
				}
			}()
			_ = mustValidModel(tt.in)
		})
	}
}
