package auto

import (
	"github.com/looprig/inference/auth"

	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
	anthropicprovider "github.com/looprig/llm/providers/anthropic"
	geminiprovider "github.com/looprig/llm/providers/gemini"
	openaiprovider "github.com/looprig/llm/providers/openai"
	xaiprovider "github.com/looprig/llm/providers/xai"
)

// NewCounter validates model and resolves its exact provider context counter from
// the same (Model, APIKey) inputs accepted by New. It never performs provider I/O
// and never silently substitutes an estimator.
//
// Error ordering is intentional: model validation runs first so unknown or
// contradictory models remain validation failures. Exact-counter support is then
// classified before API-key presence, so a known unsupported provider cannot be
// mistaken for a supported counter with a missing key. Providers with an exact
// counter perform the final fail-closed API-key validation in their constructors.
func NewCounter(model model.Model, key auth.APIKey) (contextcount.ContextCounter, error) {
	if err := llm.ValidateModel(model); err != nil {
		return nil, err
	}
	return resolveCounter(llm.Provider(model.Provider), model.APIFormat, key)
}

// resolveCounter classifies an already validated provider/dialect pair. Keeping
// dialect dispatch explicit here makes registry expansion fail closed even before
// ValidateModel is updated to admit a new provider format.
func resolveCounter(provider llm.Provider, apiFormat model.APIFormat, key auth.APIKey) (contextcount.ContextCounter, error) {
	switch provider {
	case llm.ProviderGoogle:
		if apiFormat != model.APIFormatGemini {
			return nil, &llm.CounterSupportError{
				Provider:  provider,
				Reason:    llm.CounterSupportAPIFormatUnavailable,
				APIFormat: apiFormat,
			}
		}
		return geminiprovider.NewCounter(key)
	case llm.ProviderOpenAI:
		if apiFormat != model.APIFormatOpenAIResponses {
			return nil, &llm.CounterSupportError{
				Provider:  provider,
				Reason:    llm.CounterSupportAPIFormatUnavailable,
				APIFormat: apiFormat,
			}
		}
		return openaiprovider.NewCounter(key)
	case llm.ProviderAnthropic:
		if apiFormat != model.APIFormatAnthropic {
			return nil, &llm.CounterSupportError{
				Provider:  provider,
				Reason:    llm.CounterSupportAPIFormatUnavailable,
				APIFormat: apiFormat,
			}
		}
		return anthropicprovider.NewCounter(key)
	case llm.ProviderXAI:
		if apiFormat != model.APIFormatOpenAIResponses {
			return nil, &llm.CounterSupportError{
				Provider:  provider,
				Reason:    llm.CounterSupportAPIFormatUnavailable,
				APIFormat: apiFormat,
			}
		}
		return xaiprovider.NewCounter(key)
	case llm.ProviderBedrock:
		if apiFormat == model.APIFormatAnthropic || apiFormat == model.APIFormatBedrockConverse {
			return nil, &llm.CounterDirectConstructionError{
				Provider: provider,
				Reason:   llm.CounterDirectConstructionNeedsSigV4,
				Use:      llm.CounterConstructorBedrock,
			}
		}
		return nil, &llm.CounterSupportError{
			Provider:  provider,
			Reason:    llm.CounterSupportAPIFormatUnavailable,
			APIFormat: apiFormat,
		}
	case llm.ProviderAzure, llm.ProviderChutes, llm.ProviderPhala, llm.ProviderOpenRouter, llm.ProviderLMStudio,
		llm.ProviderAzureCognitiveServices, llm.Provider302AI, llm.ProviderAtomicChat, llm.ProviderBaseten,
		llm.ProviderCerebras, llm.ProviderCloudflareAIGateway, llm.ProviderCloudflareWorkersAI, llm.ProviderCortecs,
		llm.ProviderDeepSeek, llm.ProviderDeepInfra, llm.ProviderDigitalOcean, llm.ProviderFrogBot, llm.ProviderFireworks,
		llm.ProviderGitLab, llm.ProviderGitHubCopilot, llm.ProviderGMICloud, llm.ProviderGoogleVertex,
		llm.ProviderGoogleVertexAnthropic, llm.ProviderGroq, llm.ProviderHuggingFace, llm.ProviderHelicone,
		llm.ProviderLlama, llm.ProviderLlamaCPP, llm.ProviderIONet, llm.ProviderMoonshot, llm.ProviderMiniMax, llm.ProviderNVIDIA,
		llm.ProviderNebius, llm.ProviderOllama, llm.ProviderOllamaCloud, llm.ProviderOpenCode, llm.ProviderOpenCodeGo,
		llm.ProviderLLMGateway, llm.ProviderSAP, llm.ProviderSTACKIT, llm.ProviderOVHCloud, llm.ProviderScaleway,
		llm.ProviderSnowflakeCortex, llm.ProviderSynthetic, llm.ProviderTogetherAI, llm.ProviderVenice, llm.ProviderVercel, llm.ProviderZAI,
		llm.ProviderZenMux:
		return nil, &llm.CounterSupportError{
			Provider:  provider,
			Reason:    llm.CounterSupportExactUnavailable,
			APIFormat: apiFormat,
		}
	default:
		// ValidateModel rejects every provider not in llm's canonical registry.
		// Keep the default fail-closed so a future registry expansion cannot
		// accidentally acquire a counter or estimator through fallthrough.
		return nil, &model.ValidationError{Field: "Provider", Reason: "context counter support is unclassified"}
	}
}
