package model

// Capabilities is secret-free gating/informational data about a model: never serialized onto
// the wire, read locally (e.g. a TUI deciding whether to allow image attachments).
type Capabilities struct {
	AcceptsImages             bool
	Tools                     bool
	Thinking                  bool
	StructuredOutput          bool
	StructuredOutputWithTools bool

	// PromptCaching marks an endpoint that honors explicit cache_control
	// breakpoints (Anthropic API, Bedrock-Anthropic). It is deliberately a
	// per-model capability, not derived from the API format: a third-party
	// server speaking the Anthropic dialect may instead cache prefixes
	// automatically server-side (or reject the unknown field), so emission
	// must be opt-in per endpoint. Codecs whose dialect has no request-side
	// cache hints (OpenAI, Gemini implicit caching) ignore it.
	PromptCaching bool
}
