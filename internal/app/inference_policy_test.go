package app

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

func TestNewModelInferencePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		model           model.Model
		wantTransport   contextcount.InferenceTransport
		wantRetention   contextcount.RetentionPosture
		wantIdentity    bool
		wantUnsupported bool
	}{
		{name: "chutes is end-to-end encrypted", model: chutesKimiK26(), wantTransport: contextcount.InferenceTransportEndToEndEncrypted, wantRetention: contextcount.RetentionUnknown, wantIdentity: true},
		{name: "phala is end-to-end encrypted", model: phalaGLM52(), wantTransport: contextcount.InferenceTransportEndToEndEncrypted, wantRetention: contextcount.RetentionUnknown, wantIdentity: true},
		{name: "lm studio stays local", model: lmStudioLocal("local-model"), wantTransport: contextcount.InferenceTransportLocal, wantRetention: contextcount.RetentionNone},
		{name: "unknown provider fails closed", model: func() model.Model {
			value := chutesKimiK26()
			value.Provider = model.ProviderName("unknown")
			return value
		}(), wantUnsupported: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			policy, err := newModelInferencePolicy(tt.model)
			if tt.wantUnsupported {
				var target *UnsupportedInferenceProviderError
				if !errors.As(err, &target) {
					t.Fatalf("newModelInferencePolicy() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
				}
				if target.Provider != tt.model.Provider {
					t.Errorf("Provider = %q, want %q", target.Provider, tt.model.Provider)
				}
				return
			}
			if err != nil {
				t.Fatalf("newModelInferencePolicy() error = %v", err)
			}

			capability := policy.InferenceCapability()
			if err := capability.Validate(); err != nil {
				t.Fatalf("InferenceCapability().Validate() error = %v", err)
			}
			if capability.Provider != contextcount.ProviderID(tt.model.Provider) {
				t.Errorf("Provider = %q, want %q", capability.Provider, tt.model.Provider)
			}
			if capability.Transport != tt.wantTransport {
				t.Errorf("Transport = %v, want %v", capability.Transport, tt.wantTransport)
			}
			if capability.Retention != tt.wantRetention {
				t.Errorf("Retention = %v, want %v", capability.Retention, tt.wantRetention)
			}
			if got := capability.SecurityIdentity != (contextcount.SecurityIdentity{}); got != tt.wantIdentity {
				t.Errorf("SecurityIdentity non-zero = %v, want %v", got, tt.wantIdentity)
			}

			counter := policy.ContextCounter()
			if counter == nil {
				t.Fatal("ContextCounter() = nil")
			}
			metadata := counter.CounterCapability()
			wantMetadata := contextcount.CounterCapability{
				Transport:    contextcount.CounterTransportLocal,
				Retention:    contextcount.RetentionNone,
				TokenizerRev: contextcount.EstimatorRevision,
				Quality:      contextcount.CountQualityHeuristicEstimate,
			}
			if metadata != wantMetadata {
				t.Errorf("CounterCapability() = %+v, want %+v", metadata, wantMetadata)
			}
		})
	}
}

func TestModelInferencePolicyIsFixedAcrossAllowedModelChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base model.Model
	}{
		{name: "chutes", base: chutesKimiK26()},
		{name: "phala", base: phalaGLM52()},
		{name: "lm studio", base: func() model.Model {
			value := lmStudioLocal("local-model")
			value.Limits = model.ContextLimits{MaxInputTokens: 32_000}
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			changed := tt.base.Clone()
			changed.Name = "another-model"
			changed.Limits = model.ContextLimits{WindowTokens: 64_000}
			changed.Caps.Tools = !changed.Caps.Tools
			effort := model.EffortHigh
			changed.Sampling.Effort = effort

			basePolicy, err := newModelInferencePolicy(tt.base)
			if err != nil {
				t.Fatalf("newModelInferencePolicy(base) error = %v", err)
			}
			changedPolicy, err := newModelInferencePolicy(changed)
			if err != nil {
				t.Fatalf("newModelInferencePolicy(changed) error = %v", err)
			}
			if got, want := changedPolicy.InferenceCapability(), basePolicy.InferenceCapability(); got != want {
				t.Errorf("capability changed with model-local fields: got %+v, want %+v", got, want)
			}
			if got, want := changedPolicy.ContextCounter().CounterCapability(), basePolicy.ContextCounter().CounterCapability(); got != want {
				t.Errorf("counter metadata changed: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestInferencePolicyTransportBinding(t *testing.T) {
	t.Parallel()

	base := chutesKimiK26()
	policy, err := newModelInferencePolicy(base)
	if err != nil {
		t.Fatalf("newModelInferencePolicy() error = %v", err)
	}
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("policy-test")),
		loop.WithInference(&fakeLLM{}, base),
		loop.WithContextCounter(policy.ContextCounter()),
		loop.WithInferenceCapability(policy.InferenceCapability()),
		loop.WithContextObservation(loop.ContextObservationPolicy{
			ReservedOutput: 1,
			SafetyMargin:   1,
			CountTimeout:   time.Second,
		}),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	allowed := base.Clone()
	allowed.Name = "another-model"
	allowed.Limits = model.ContextLimits{WindowTokens: 64_000}
	tests := []struct {
		name         string
		candidate    model.Model
		wantRejected bool
	}{
		{name: "model-local change is allowed", candidate: allowed},
		{name: "provider change is rejected", candidate: func() model.Model {
			value := allowed
			value.Provider = model.ProviderName(llm.ProviderPhala)
			return value
		}(), wantRejected: true},
		{name: "api format change is rejected", candidate: func() model.Model { value := allowed; value.APIFormat = model.APIFormatAnthropic; return value }(), wantRejected: true},
		{name: "base url change is rejected", candidate: func() model.Model { value := allowed; value.BaseURL = "https://other.example.test"; return value }(), wantRejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := definition.ValidateContextModel(tt.candidate)
			if !tt.wantRejected {
				if err != nil {
					t.Fatalf("ValidateContextModel() error = %v", err)
				}
				return
			}
			var target *loop.ContextTransportNotDeclaredError
			if !errors.As(err, &target) {
				t.Fatalf("ValidateContextModel() error = %T %v, want *loop.ContextTransportNotDeclaredError", err, err)
			}
			if target.Provider != tt.candidate.Provider || target.APIFormat != tt.candidate.APIFormat || target.BaseURL != tt.candidate.BaseURL {
				t.Errorf("error transport = {%q %q %q}, want candidate's {%q %q %q}", target.Provider, target.APIFormat, target.BaseURL, tt.candidate.Provider, tt.candidate.APIFormat, tt.candidate.BaseURL)
			}
		})
	}
}

func TestDeclaredContextTransportsMergesBasePrimerAndDelegateModels(t *testing.T) {
	t.Parallel()

	primer := testModel() // lmstudio
	primerCandidates := []PrimerCandidate{{Alias: "primer", Model: primer}}

	t.Run("base model is always included even with no candidates or delegates", func(t *testing.T) {
		t.Parallel()
		transports, err := declaredContextTransports(primer, nil, nil)
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 1 || transports[0].Provider != primer.Provider || transports[0].APIFormat != primer.APIFormat || transports[0].BaseURL != primer.BaseURL {
			t.Fatalf("transports = %#v, want exactly the base model's transport", transports)
		}
	})

	t.Run("base model is included even when absent from PrimerCandidates", func(t *testing.T) {
		t.Parallel()
		// PrimerCandidates deliberately does NOT include primer here — this is
		// the exact shape that broke the first draft of this fix (empty
		// PrimerCandidates, base model only implied by the definition's own
		// WithInference call).
		transports, err := declaredContextTransports(primer, nil, nil)
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		found := false
		for _, tr := range transports {
			if tr.Provider == primer.Provider && tr.APIFormat == primer.APIFormat && tr.BaseURL == primer.BaseURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("transports = %#v, want base model's own transport included", transports)
		}
	})

	t.Run("delegate on a foreign transport is included", func(t *testing.T) {
		t.Parallel()
		delegate := model.CustomModel(model.ProviderName(llm.ProviderOpenAI), model.APIFormatOpenAIResponses, "", "delegate-model", model.WithTools(), model.WithThinking())

		transports, err := declaredContextTransports(primer, primerCandidates, []model.Model{delegate})
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 2 {
			t.Fatalf("transports = %#v, want 2 (primer + delegate)", transports)
		}
		found := false
		for _, tr := range transports {
			if tr.Provider == delegate.Provider && tr.APIFormat == delegate.APIFormat && tr.BaseURL == delegate.BaseURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("transports = %#v, want delegate's transport included", transports)
		}
	})

	t.Run("delegate sharing the primer's transport does not duplicate", func(t *testing.T) {
		t.Parallel()
		delegate := model.CustomModel(primer.Provider, primer.APIFormat, primer.BaseURL, "delegate-same-transport", model.WithTools())

		transports, err := declaredContextTransports(primer, primerCandidates, []model.Model{delegate})
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 1 {
			t.Fatalf("transports = %#v, want 1 (shared transport deduped)", transports)
		}
	})

	t.Run("propagates unsupported delegate provider", func(t *testing.T) {
		t.Parallel()
		bad := model.CustomModel("not-a-real-provider", model.APIFormatOpenAI, "https://bad.example.test", "bad-delegate", model.WithTools())

		_, err := declaredContextTransports(primer, primerCandidates, []model.Model{bad})
		var target *UnsupportedInferenceProviderError
		if !errors.As(err, &target) {
			t.Fatalf("declaredContextTransports() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
		}
	})
}

func TestInferenceCapabilityForModelSupportsAnyKnownProvider(t *testing.T) {
	t.Parallel()

	t.Run("a provider outside the reviewed three gets a conservative default", func(t *testing.T) {
		t.Parallel()
		m := model.CustomModel(model.ProviderName(llm.ProviderOpenAI), model.APIFormatOpenAIResponses, "", "gpt-test", model.WithTools())

		capability, err := inferenceCapabilityForModel(m)
		if err != nil {
			t.Fatalf("inferenceCapabilityForModel() error = %v", err)
		}
		if err := capability.Validate(); err != nil {
			t.Fatalf("capability.Validate() error = %v", err)
		}
		if capability.Transport != contextcount.InferenceTransportTLS {
			t.Errorf("Transport = %v, want InferenceTransportTLS", capability.Transport)
		}
		if capability.Retention != contextcount.RetentionUnknown {
			t.Errorf("Retention = %v, want RetentionUnknown", capability.Retention)
		}
		if capability.SecurityIdentity == (contextcount.SecurityIdentity{}) {
			t.Errorf("SecurityIdentity is zero, want a derived non-zero identity (contextcount.InferenceCapability.Validate() requires non-zero SecurityIdentity for any transport at or above TLS)")
		}
	})

	t.Run("a provider llm itself doesn't recognize still fails closed", func(t *testing.T) {
		t.Parallel()
		m := model.CustomModel(model.ProviderName("not-a-real-provider"), model.APIFormatOpenAI, "https://bad.example.test", "bad-model", model.WithTools())

		_, err := inferenceCapabilityForModel(m)
		var target *UnsupportedInferenceProviderError
		if !errors.As(err, &target) {
			t.Fatalf("inferenceCapabilityForModel() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
		}
	})
}
