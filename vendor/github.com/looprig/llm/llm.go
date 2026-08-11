// Package llm is the batteries-included provider SDK layered on the neutral
// github.com/looprig/inference model-call contract. It owns the provider POLICY
// that inference deliberately does not carry: the known-provider registry, the
// provider/API-format truth table, provider auth requirements, provider default
// endpoint behavior, and the fail-closed model-validation preset. It also hosts
// the self-contained provider-security machinery (aci, e2e, tee) and the
// provider-specific SigV4 authenticator (llm/auth). inference never imports llm.
package llm

// Provider names the concrete backend an llm/auto factory dispatches on. Unknown
// values are rejected by ValidateModel; a provider constructor additionally
// enforces each provider's auth requirement. It is the provider-policy analogue
// of the opaque model.ProviderName label: inference carries no provider
// constants, no auth requirements, and no default endpoints — those live here.
type Provider string

const (
	ProviderLMStudio               Provider = "lmstudio"
	ProviderPhala                  Provider = "phala"
	ProviderChutes                 Provider = "chutes"
	ProviderOpenRouter             Provider = "openrouter"
	ProviderOpenAI                 Provider = "openai"
	ProviderAzure                  Provider = "azure"
	ProviderAzureCognitiveServices Provider = "azure-cognitive-services"
	ProviderAnthropic              Provider = "anthropic"
	ProviderXAI                    Provider = "xai"
	ProviderBedrock                Provider = "bedrock"
	Provider302AI                  Provider = "302ai"
	ProviderAtomicChat             Provider = "atomic-chat"
	ProviderBaseten                Provider = "baseten"
	ProviderCerebras               Provider = "cerebras"
	ProviderCloudflareAIGateway    Provider = "cloudflare-ai-gateway"
	ProviderCloudflareWorkersAI    Provider = "cloudflare-workers-ai"
	ProviderCortecs                Provider = "cortecs"
	ProviderDeepSeek               Provider = "deepseek"
	ProviderDeepInfra              Provider = "deepinfra"
	ProviderDigitalOcean           Provider = "digitalocean"
	ProviderFrogBot                Provider = "frogbot"
	ProviderFireworks              Provider = "fireworks-ai"
	ProviderGitLab                 Provider = "gitlab"
	ProviderGitHubCopilot          Provider = "github-copilot"
	ProviderGMICloud               Provider = "gmicloud"
	ProviderGoogleVertex           Provider = "google-vertex"
	ProviderGoogleVertexAnthropic  Provider = "google-vertex-anthropic"
	ProviderGroq                   Provider = "groq"
	ProviderHuggingFace            Provider = "huggingface"
	ProviderHelicone               Provider = "helicone"
	ProviderLlama                  Provider = "llama"
	ProviderLlamaCPP               Provider = "llama.cpp"
	ProviderIONet                  Provider = "io-net"
	ProviderMoonshot               Provider = "moonshotai"
	ProviderMiniMax                Provider = "minimax"
	ProviderNVIDIA                 Provider = "nvidia"
	ProviderNebius                 Provider = "nebius"
	ProviderOllama                 Provider = "ollama"
	ProviderOllamaCloud            Provider = "ollama-cloud"
	ProviderOpenCode               Provider = "opencode"
	ProviderOpenCodeGo             Provider = "opencode-go"
	ProviderLLMGateway             Provider = "llmgateway"
	ProviderSAP                    Provider = "sap-ai-core"
	ProviderSTACKIT                Provider = "stackit"
	ProviderOVHCloud               Provider = "ovhcloud"
	ProviderScaleway               Provider = "scaleway"
	ProviderSnowflakeCortex        Provider = "snowflake-cortex"
	ProviderSynthetic              Provider = "synthetics"
	ProviderTogetherAI             Provider = "togetherai"
	ProviderVenice                 Provider = "venice"
	ProviderVercel                 Provider = "vercel"
	ProviderZAI                    Provider = "zai"
	ProviderZenMux                 Provider = "zenmux"
	// ProviderGoogle is Google's Gemini generateContent backend. The provider (the
	// backend "google") and the dialect (APIFormatGemini) are distinct axes: google
	// speaks only the Gemini wire format, authenticated with an x-goog-api-key header
	// (RequiredAuth → AuthAPIKey), and is served by the bespoke providers/gemini
	// client (the generic transport assumes a static /chat/completions path).
	ProviderGoogle Provider = "google"
)
