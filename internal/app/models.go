package app

import (
	"strconv"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// CodeRig owns the secret-free model descriptors it can select explicitly. Each
// descriptor is built with model.CustomModel and checked against the
// fail-closed provider truth table. Connection secrets bind to the Client and
// never live on a model descriptor.

// InvalidDefaultModelError reports a hardcoded model descriptor that fails the
// fail-closed provider validation. A malformed constant model row is a programming
// error, so mustValidModel panics with this typed value at package initialisation.
// The descriptor must be fixed before the binary can run. This is the regexp.MustCompile /
// uuid.MustParse idiom: a Must-constructor for a developer-authored constant, not a
// runtime input boundary. Untrusted model input is validated and returns an error.
type InvalidDefaultModelError struct {
	Name  string
	Cause error
}

func (e *InvalidDefaultModelError) Error() string {
	return "coderig: invalid default model " + strconv.Quote(e.Name) + ": " + e.Cause.Error()
}

func (e *InvalidDefaultModelError) Unwrap() error { return e.Cause }

// mustValidModel runs the fail-closed llm.ValidateModel provider check on a
// hardcoded model descriptor and returns it unchanged, panicking with an
// *InvalidDefaultModelError if the descriptor is malformed. It is called at the
// construction of every CodeRig model descriptor, so a bad descriptor fails loud
// at startup rather than at first provider call.
func mustValidModel(m model.Model) model.Model {
	if err := llm.ValidateModel(m); err != nil {
		panic(&InvalidDefaultModelError{Name: m.Name, Cause: err})
	}
	return m
}

// chutesKimiK26 is the Moonshot Kimi K2.6 model served through Chutes' TEE-attested,
// OpenAI-compatible tunnel — CodeRig's default model (see model.go). It is the newest
// Kimi Chutes serves (confirmed against llm.chutes.ai /v1/models); the id carries the
// -TEE suffix every Chutes confidential chute uses. Chutes resolves the model name to a
// chute at request time, so Name is the value sent on every request; the base is the
// explicit e2e apiBase (chutes.New does not default it). Tool- and thinking-capable,
// text-only agentic-coding model (AcceptsImages stays false).
func chutesKimiK26() model.Model {
	return mustValidModel(model.CustomModel(
		model.ProviderName(llm.ProviderChutes), model.APIFormatOpenAI,
		"https://api.chutes.ai", "moonshotai/Kimi-K2.6-TEE",
		model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}),
		model.WithTools(), model.WithThinking(),
		model.WithSampling(model.Sampling{Effort: model.EffortHigh}),
	))
}

// phalaGLM52 is the zai-org GLM 5.2 model served through Phala's aci confidential
// OpenAI-compatible gateway. The id and base are the client-facing pair Phala's
// gateway accepts (z-ai/glm-5.2 over https://inference.phala.com/v1); the gateway
// resolves it to its Chutes upstream (zai-org/GLM-5.2-TEE) transparently. Tool-capable.
func phalaGLM52() model.Model {
	return mustValidModel(model.CustomModel(
		model.ProviderName(llm.ProviderPhala), model.APIFormatOpenAI,
		"https://inference.phala.com/v1", "z-ai/glm-5.2",
		model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}),
		model.WithTools(),
	))
}

// lmStudioLocal is a Model for a local LM Studio server at its default loopback
// endpoint. LM Studio speaks the OpenAI-compatible dialect and needs no credentials
// (Provider.RequiredAuth → AuthNone); the http:// loopback host is permitted by the
// structural loopback exception. Capabilities are conservative: tool-calling is
// commonly supported by local OpenAI-compatible servers; image input and hidden
// thinking are model-specific and left off. Ported from the former llm.LMStudioLocal.
func lmStudioLocal(name string) model.Model {
	return mustValidModel(model.CustomModel(
		model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
		"http://localhost:1234/v1", name,
		model.WithTools(),
	))
}
