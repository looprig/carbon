package llm

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/looprig/credentials"
	auth "github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
)

// AuthBinding is the complete authority identity accepted by one provider
// transport. Scheme and usage are intentionally not enough to identify a
// credential: provider, transport, issuer, and audience are part of the
// binding as well.
type AuthBinding struct {
	Provider  string
	Transport string
	Scheme    credentials.Scheme
	Usage     credentials.UsageClass
	Issuer    string
	Audience  string
}

// Descriptor converts the policy binding into the credential descriptor used
// by a source and its leases. Label is presentation metadata and is therefore
// deliberately absent from an AuthBinding.
func (b AuthBinding) Descriptor() (credentials.Descriptor, error) {
	return credentials.NewDescriptor(b.Provider, b.Transport, b.Scheme, b.Usage, b.Issuer, b.Audience, "")
}

// Valid reports whether the complete binding can be represented by a safe
// credential descriptor.
func (b AuthBinding) Valid() bool {
	_, err := b.Descriptor()
	return err == nil
}

// AuthPolicy is the immutable set of complete credential identities accepted
// by one provider client. A policy with no accepted bindings is invalid.
type AuthPolicy struct {
	Accepted []AuthBinding
}

// Validate checks every accepted identity and rejects duplicate bindings. The
// input slice is never mutated; callers should treat a policy as immutable once
// it has been used to construct a client.
func (p AuthPolicy) Validate() error {
	if len(p.Accepted) == 0 {
		return &InvalidAuthPolicyError{Reason: "accepted binding set must not be empty"}
	}
	seen := make(map[string]struct{}, len(p.Accepted))
	for _, binding := range p.Accepted {
		descriptor, err := binding.Descriptor()
		if err != nil {
			return &InvalidAuthPolicyError{Reason: "accepted binding is invalid", Err: err}
		}
		key := descriptor.Canonical()
		if _, ok := seen[key]; ok {
			return &InvalidAuthPolicyError{Reason: "accepted bindings must be unique"}
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Accepts reports whether descriptor has exactly one of the policy's accepted
// six-field identities. Descriptor.Label is presentation-only and is ignored.
func (p AuthPolicy) Accepts(descriptor credentials.Descriptor) bool {
	if !descriptor.Valid() || p.Validate() != nil {
		return false
	}
	for _, accepted := range p.Accepted {
		if descriptorMatchesBinding(descriptor, accepted) {
			return true
		}
	}
	return false
}

// Match validates descriptor against the policy and returns a safe typed
// mismatch error when it is not accepted.
func (p AuthPolicy) Match(descriptor credentials.Descriptor) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !descriptor.Valid() {
		return &AuthPolicyMismatchError{Reason: "credential descriptor is invalid"}
	}
	if !p.Accepts(descriptor) {
		return &AuthPolicyMismatchError{Reason: "credential descriptor does not match the exact provider policy"}
	}
	return nil
}

func descriptorMatchesBinding(descriptor credentials.Descriptor, binding AuthBinding) bool {
	expected, err := binding.Descriptor()
	if err != nil {
		return false
	}
	return descriptor.Provider == expected.Provider &&
		descriptor.Transport == expected.Transport &&
		descriptor.Scheme == expected.Scheme &&
		descriptor.Usage == expected.Usage &&
		descriptor.Issuer == expected.Issuer &&
		descriptor.Audience == expected.Audience
}

// InvalidAuthPolicyError reports malformed provider policy metadata. It carries
// no credential or provider response data.
type InvalidAuthPolicyError struct {
	Reason string
	Err    error
}

func (e *InvalidAuthPolicyError) Error() string {
	if e == nil {
		return "llm: invalid auth policy"
	}
	if e.Err != nil {
		return "llm: invalid auth policy: " + e.Reason + ": " + e.Err.Error()
	}
	return "llm: invalid auth policy: " + e.Reason
}

func (e *InvalidAuthPolicyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AuthPolicyMismatchError reports a source or lease identity that is not
// authorized for the bound provider transport.
type AuthPolicyMismatchError struct{ Reason string }

func (e *AuthPolicyMismatchError) Error() string {
	if e == nil || e.Reason == "" {
		return "llm: credential binding does not match provider auth policy"
	}
	return "llm: credential binding does not match provider auth policy: " + e.Reason
}

// AuthPolicyForModel is the one-way compatibility bridge from the legacy
// Provider.RequiredAuth registry to an exact credentials policy. It is kept in
// llm because inference must remain unaware of provider identity and policy.
func AuthPolicyForModel(selected model.Model) (AuthPolicy, error) {
	if err := ValidateModel(selected); err != nil {
		return AuthPolicy{}, err
	}
	p := Provider(selected.Provider)
	origin, err := requestOriginForModel(selected, p.RequiredAuth)
	if err != nil {
		return AuthPolicy{}, err
	}
	return authPolicyForProviderAtOrigin(p, selected.APIFormat, origin)
}

// authPolicyForProvider returns the exact policy bridge for a provider/API
// format pair. It deliberately derives the credential scheme from RequiredAuth
// so the legacy provider registry remains the single compatibility authority.
func authPolicyForProvider(p Provider, format model.APIFormat) (AuthPolicy, error) {
	origin, err := reviewedProviderOrigin(p, format)
	if err != nil {
		return AuthPolicy{}, err
	}
	return authPolicyForProviderAtOrigin(p, format, origin)
}

func authPolicyForProviderAtOrigin(p Provider, format model.APIFormat, origin string) (AuthPolicy, error) {
	if !p.supportsAPIFormat(format) {
		return AuthPolicy{}, &InvalidAuthPolicyError{Reason: fmt.Sprintf("provider %q does not support API format %q", p, format)}
	}
	legacyKind, err := p.RequiredAuth()
	if err != nil {
		return AuthPolicy{}, err
	}
	binding := AuthBinding{
		Provider:  string(p),
		Transport: transportIdentity(p, format),
		Issuer:    issuerIdentity(p),
		Audience:  origin,
	}
	switch legacyKind {
	case authKindNone:
		binding.Scheme = credentials.SchemeNone
		binding.Usage = credentials.UsageLocal
		binding.Issuer = ""
		binding.Audience = ""
	case authKindAPIKey:
		binding.Scheme = credentials.SchemeAPIKey
		binding.Usage = credentials.UsageMeteredAPI
	case AuthOAuth:
		binding.Scheme = credentials.SchemeOAuth
		binding.Usage = credentials.UsageSubscription
	case AuthGCP:
		binding.Scheme = credentials.SchemeWorkloadIdentity
		binding.Usage = credentials.UsageMeteredAPI
	case AuthSigV4:
		binding.Scheme = credentials.SchemeSigV4
		binding.Usage = credentials.UsageMeteredAPI
	case AuthServiceKey, AuthToken:
		// These legacy kinds carry provider-specific bearer/service material. The
		// credentials descriptor still needs a concrete scheme; workload identity
		// is the conservative non-API-key classification until a provider-specific
		// source supplies a narrower policy.
		binding.Scheme = credentials.SchemeWorkloadIdentity
		binding.Usage = credentials.UsageMeteredAPI
	default:
		return AuthPolicy{}, &InvalidAuthPolicyError{Reason: fmt.Sprintf("unsupported legacy auth kind %q", legacyKind)}
	}
	policy := AuthPolicy{Accepted: []AuthBinding{binding}}
	if err := policy.Validate(); err != nil {
		return AuthPolicy{}, err
	}
	return policy, nil
}

// requestOriginForModel resolves the exact origin that the constructed client
// is expected to send to. Explicit model bases are authoritative; empty bases
// use the reviewed provider registry. Authenticated providers are HTTPS-only.
// Local no-auth transports are the sole exception and may use loopback HTTP.
func requestOriginForModel(selected model.Model, required func() (auth.AuthKind, error)) (string, error) {
	kind, err := required()
	if err != nil {
		return "", err
	}
	if kind == auth.AuthNone {
		if strings.TrimSpace(selected.BaseURL) == "" {
			return "", nil
		}
		return canonicalRequestOrigin(selected.BaseURL, true)
	}
	if strings.TrimSpace(selected.BaseURL) == "" {
		return reviewedProviderOrigin(Provider(selected.Provider), selected.APIFormat)
	}
	return canonicalRequestOrigin(selected.BaseURL, false)
}

func canonicalRequestOrigin(raw string, localAllowed bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", &InvalidAuthPolicyError{Reason: "request base URL is not a canonical origin"}
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return "", &InvalidAuthPolicyError{Reason: "request base URL has no host"}
	}
	if scheme != "https" {
		if !localAllowed || !isLoopbackOriginHost(host) || scheme != "http" {
			return "", &InvalidAuthPolicyError{Reason: "authenticated request origin must use HTTPS"}
		}
	}
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host, nil
}

func isLoopbackOriginHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func reviewedProviderOrigin(provider Provider, format model.APIFormat) (string, error) {
	if isLocalProvider(provider) {
		return "", nil
	}
	origin, ok := reviewedOrigins[provider]
	if !ok || origin == "" {
		return "", &InvalidAuthPolicyError{Reason: fmt.Sprintf("provider %q has no reviewed request origin", provider)}
	}
	return canonicalRequestOrigin(origin, false)
}

func isLocalProvider(provider Provider) bool {
	switch provider {
	case ProviderLMStudio, ProviderAtomicChat, ProviderLlamaCPP, ProviderOllama:
		return true
	default:
		return false
	}
}

var reviewedOrigins = map[Provider]string{
	ProviderPhala: "https://inference.phala.com", ProviderChutes: "https://api.chutes.ai", ProviderOpenRouter: "https://openrouter.ai", ProviderOpenAI: "https://api.openai.com", ProviderAzure: "https://cognitiveservices.azure.com", ProviderAzureCognitiveServices: "https://cognitiveservices.azure.com", ProviderAnthropic: "https://api.anthropic.com", ProviderXAI: "https://api.x.ai", ProviderBedrock: "https://bedrock-runtime.amazonaws.com", Provider302AI: "https://api.302.ai", ProviderBaseten: "https://inference.baseten.co", ProviderCerebras: "https://api.cerebras.ai", ProviderCloudflareAIGateway: "https://gateway.ai.cloudflare.com", ProviderCloudflareWorkersAI: "https://api.cloudflare.com", ProviderCortecs: "https://api.cortecs.ai", ProviderDeepSeek: "https://api.deepseek.com", ProviderDeepInfra: "https://api.deepinfra.com", ProviderDigitalOcean: "https://inference.do-ai.run", ProviderFrogBot: "https://app.frogbot.ai", ProviderFireworks: "https://api.fireworks.ai", ProviderGitLab: "https://cloud.gitlab.com", ProviderGitHubCopilot: "https://api.githubcopilot.com", ProviderGMICloud: "https://api.gmi-serving.com", ProviderGoogleVertex: "https://aiplatform.googleapis.com", ProviderGoogleVertexAnthropic: "https://aiplatform.googleapis.com", ProviderGroq: "https://api.groq.com", ProviderHuggingFace: "https://router.huggingface.co", ProviderHelicone: "https://ai-gateway.helicone.ai", ProviderLlama: "https://api.llama.com", ProviderIONet: "https://api.intelligence.io.solutions", ProviderMoonshot: "https://api.moonshot.ai", ProviderMiniMax: "https://api.minimax.io", ProviderNVIDIA: "https://integrate.api.nvidia.com", ProviderNebius: "https://api.tokenfactory.nebius.com", ProviderOllamaCloud: "https://ollama.com", ProviderOpenCode: "https://opencode.ai", ProviderOpenCodeGo: "https://opencode.ai", ProviderLLMGateway: "https://api.llmgateway.io", ProviderSAP: "https://ai.sap.com", ProviderSTACKIT: "https://api.onstackit.cloud", ProviderOVHCloud: "https://oai.endpoints.kepler.ai.cloud.ovh.net", ProviderScaleway: "https://api.scaleway.ai", ProviderSnowflakeCortex: "https://snowflakecomputing.com", ProviderSynthetic: "https://api.synthetic.new", ProviderTogetherAI: "https://api.together.ai", ProviderVenice: "https://api.venice.ai", ProviderVercel: "https://ai-gateway.vercel.sh", ProviderZAI: "https://api.z.ai", ProviderZenMux: "https://zenmux.ai", ProviderGoogle: "https://generativelanguage.googleapis.com",
}

// Keep the bridge readable without importing the legacy auth package into every
// policy comparison. These values are the stable names returned by RequiredAuth.
const (
	authKindNone   = "none"
	authKindAPIKey = "api_key"
)

func transportIdentity(provider Provider, format model.APIFormat) string {
	switch provider {
	case ProviderOpenAI:
		if format == model.APIFormatOpenAIResponses {
			return "responses"
		}
		return "chat"
	case ProviderAnthropic:
		return "messages"
	case ProviderGoogle:
		return "generate-content"
	case ProviderLMStudio, ProviderAtomicChat, ProviderLlamaCPP, ProviderOllama:
		return "local"
	default:
		identity := strings.ToLower(strings.TrimSpace(string(format)))
		if identity == "" {
			return "inference"
		}
		return identity
	}
}

func issuerIdentity(provider Provider) string {
	if origin, ok := reviewedOrigins[provider]; ok {
		return origin
	}
	return ""
}
