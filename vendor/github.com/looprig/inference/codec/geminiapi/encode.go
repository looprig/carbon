package geminiapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

const terminalOutputValidationName = "terminal_output"

// BuildGenerateContentRequest converts a provider-neutral inference.Request into a
// GenerateContentRequest struct. Exported so provider packages can embed or
// extend the result before marshaling. The effective sampling is Request.Override
// when non-nil, else Model.Sampling — the same precedence every codec honors.
func BuildGenerateContentRequest(req inference.Request) (GenerateContentRequest, error) {
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return GenerateContentRequest{}, err
	}

	sampling := req.Model.Sampling
	if req.Override != nil {
		sampling = *req.Override
	}

	contents, systemParts, err := buildContents(req.System, req.Messages)
	if err != nil {
		return GenerateContentRequest{}, err
	}
	tools, err := buildTools(req.Tools)
	if err != nil {
		return GenerateContentRequest{}, err
	}

	out := GenerateContentRequest{
		Contents:         contents,
		Tools:            tools,
		GenerationConfig: buildGenerationConfig(sampling, req.Model.Caps),
	}
	if req.ToolChoice == inference.ToolChoiceRequired {
		out.ToolConfig = &toolConfig{FunctionCallingConfig: &functionCallingConfig{Mode: functionCallingModeAny}}
	}
	if req.Output != nil {
		projected, err := projectGeminiSchema(req.Output.Schema)
		if err != nil {
			return GenerateContentRequest{}, err
		}
		if out.GenerationConfig == nil {
			out.GenerationConfig = &generationConfig{}
		}
		out.GenerationConfig.ResponseMIMEType = responseMIMETypeJSON
		out.GenerationConfig.ResponseJSONSchema = projected
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiContent{Parts: systemParts}
	}
	return out, nil
}

// EncodeRequest converts a provider-neutral inference.Request to a Gemini
// generateContent JSON body. Note there is no stream parameter: Gemini's
// generateContent and streamGenerateContent bodies are byte-for-byte identical —
// the transport selects the endpoint and adds `?alt=sse`, so Codec.EncodeRequest
// ignores its RequestMode.
func EncodeRequest(req inference.Request) ([]byte, error) {
	gr, err := BuildGenerateContentRequest(req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(gr)
}

// buildContents splits the request thread into Gemini's two homes for message
// text: the top-level systemInstruction (Request.System plus any in-thread
// SystemMessage) and the `contents` array (user/model/tool turns). It threads a
// tool-use id -> name map forward so a ToolResultMessage — which carries only the
// id — can name its functionResponse, which Gemini matches on name. The thread is
// ordered, so a tool call is always recorded before its result is encoded.
func buildContents(system string, msgs content.AgenticMessages) ([]geminiContent, []geminiPart, error) {
	var contents []geminiContent
	var systemParts []geminiPart
	if system != "" {
		systemParts = append(systemParts, geminiPart{Text: system})
	}

	toolNames := make(map[string]string)
	for _, conv := range msgs {
		switch m := conv.(type) {
		case *content.SystemMessage:
			systemParts = append(systemParts, textParts(m.Blocks)...)
		case *content.UserMessage:
			parts, err := encodeUserParts(m.Blocks)
			if err != nil {
				return nil, nil, err
			}
			contents = append(contents, geminiContent{Role: roleUser, Parts: parts})
		case *content.AIMessage:
			parts, err := encodeAIParts(m, toolNames)
			if err != nil {
				return nil, nil, err
			}
			contents = append(contents, geminiContent{Role: roleModel, Parts: parts})
		case *content.ToolResultMessage:
			part, err := encodeToolResult(m, toolNames)
			if err != nil {
				return nil, nil, err
			}
			contents = append(contents, geminiContent{Role: roleUser, Parts: []geminiPart{part}})
		default:
			return nil, nil, &EncodeError{Reason: fmt.Sprintf("unknown conversation type %T", conv)}
		}
	}
	return contents, systemParts, nil
}

// textParts extracts the text blocks of a message as Gemini text parts,
// discarding non-text blocks. Used for SystemMessage, which folds into
// systemInstruction where only text is meaningful.
func textParts(blocks []content.Block) []geminiPart {
	var parts []geminiPart
	for _, b := range blocks {
		if t, ok := b.(*content.TextBlock); ok {
			parts = append(parts, geminiPart{Text: t.Text})
		}
	}
	return parts
}

// encodeUserParts maps a user turn's blocks to Gemini parts: text -> text,
// image -> inlineData (bytes) or fileData (URL). Block order is preserved. A block
// type the dialect does not model (audio, document, …) yields an
// *UnsupportedBlockError — fail-secure, never a silent drop.
func encodeUserParts(blocks []content.Block) ([]geminiPart, error) {
	parts := make([]geminiPart, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			parts = append(parts, geminiPart{Text: b.Text})
		case *content.ImageBlock:
			parts = append(parts, imagePart(b))
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	return parts, nil
}

// imagePart maps an ImageBlock to a Gemini part. Inline bytes are preferred
// (inlineData is Gemini's robust multimodal path); a URL-only image degrades to
// fileData, whose fileUri Gemini only accepts for File API / gs:// / YouTube URIs
// — an arbitrary web URL is expected to have been materialized to bytes upstream.
func imagePart(img *content.ImageBlock) geminiPart {
	if len(img.Source.Data) > 0 {
		return geminiPart{InlineData: &inlineData{
			MimeType: string(img.MediaType),
			Data:     base64.StdEncoding.EncodeToString(img.Source.Data),
		}}
	}
	return geminiPart{FileData: &fileData{MimeType: string(img.MediaType), FileURI: img.Source.URL}}
}

// encodeAIParts maps a model turn's blocks to Gemini parts: text -> text,
// tool_use -> functionCall, thinking -> a thought part IF it carries a
// thoughtSignature in ProviderState (added in an earlier task, so the
// domain model now DOES carry Gemini's opaque continuation token — this is
// no longer the silent, undocumented drop it once was). A ThinkingBlock with
// an empty/nil ProviderState (e.g. cross-dialect-sourced, or a same-dialect
// block that predates thinking) is still dropped: real Gemini 2.5+ models
// require a replayed thought part to carry the exact signature the model
// itself issued, and a synthesized or absent signature is not a faithful
// echo — sending a signature-less thought part risks the target rejecting
// the turn outright, so omitting it (as before) remains the safer, more
// defensible choice than fabricating or guessing a value. Each tool call's
// id -> name is recorded for the matching functionResponse. Any other
// unmodeled block type yields an *UnsupportedBlockError — fail-secure, never
// a silent drop.
func encodeAIParts(m *content.AIMessage, toolNames map[string]string) ([]geminiPart, error) {
	parts := make([]geminiPart, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			parts = append(parts, geminiPart{Text: b.Text})
		case *content.ToolUseBlock:
			toolNames[b.ID] = b.Name
			parts = append(parts, geminiPart{FunctionCall: &functionCall{
				ID:   b.ID,
				Name: b.Name,
				Args: argsJSON(b.Input),
			}})
		case *content.ThinkingBlock:
			if !b.ReplayableAs(providerStateFormatGemini) {
				continue
			}
			sig, err := providerStateToThoughtSignature(b.ProviderState)
			if err != nil {
				return nil, err
			}
			parts = append(parts, geminiPart{Thought: true, Text: b.Thinking, ThoughtSignature: sig})
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	return parts, nil
}

// providerStateToThoughtSignature unmarshals a ThinkingBlock.ProviderState
// (which this codec always stores as the JSON-marshaled form of the wire
// thoughtSignature string — see providerStateFromThoughtSignature, decode.go)
// back into the plain string the wire `thoughtSignature` field carries.
func providerStateToThoughtSignature(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", &EncodeError{Reason: "invalid ThinkingBlock.ProviderState", Err: err}
	}
	return s, nil
}

// functionResponsePayload is the JSON object wrapper for a tool result. Gemini's
// functionResponse.response is a Struct (object), but our tool output is text, so
// it is wrapped under "result" — the key the official Google GenAI SDK uses.
type functionResponsePayload struct {
	Result string `json:"result"`
}

// encodeToolResult maps a ToolResultMessage to a functionResponse part. The
// function name is looked up from the id->name map (empty if the paired call was
// not in this thread). Mirroring the OpenAI codec, IsError is NOT emitted — the
// classic Gemini functionResponse has no structured error flag, so the model
// learns of a failure through the (loop-prefixed) result text.
func encodeToolResult(m *content.ToolResultMessage, toolNames map[string]string) (geminiPart, error) {
	text, err := toolResultText(m.Blocks)
	if err != nil {
		return geminiPart{}, err
	}
	payload, err := json.Marshal(functionResponsePayload{Result: text})
	if err != nil {
		return geminiPart{}, &EncodeError{Reason: "marshal tool result", Err: err}
	}
	return geminiPart{FunctionResponse: &functionResponse{
		ID:       m.ToolUseID,
		Name:     toolNames[m.ToolUseID],
		Response: payload,
	}}, nil
}

// toolResultText flattens a tool result's blocks to the plain string the
// text-only functionResponse carries. Any non-text block yields a typed
// *UnsupportedBlockError — fail-secure, never a silent drop.
func toolResultText(blocks []content.Block) (string, error) {
	var out string
	for _, b := range blocks {
		t, ok := b.(*content.TextBlock)
		if !ok {
			return "", &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
		out += t.Text
	}
	return out, nil
}

// buildTools maps the exposed tools into Gemini's single tool entry holding all
// functionDeclarations. Returns nil when there are no tools (so the key is
// omitted). The reserved terminal-output tool is the sole declaration whose
// schema is validated and projected for Gemini's dialect.
func buildTools(tools []inference.Tool) ([]geminiTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	decls := make([]functionDeclaration, 0, len(tools))
	for _, t := range tools {
		parameters := t.Schema
		if t.Name == inference.StructuredOutputToolName {
			terminalOutput := inference.OutputSchema{Name: terminalOutputValidationName, Schema: t.Schema, Strict: true}
			if err := inference.ValidateOutputSchema(terminalOutput); err != nil {
				return nil, err
			}
			projected, err := projectGeminiSchema(t.Schema)
			if err != nil {
				return nil, err
			}
			parameters = projected
		}
		decls = append(decls, functionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  parameters,
		})
	}
	return []geminiTool{{FunctionDeclarations: decls}}, nil
}

// geminiSchemaNode is the serialization-boundary DTO for the already validated
// portable schema subset. AdditionalProperties is intentionally absent: Gemini's
// responseJsonSchema dialect rejects that otherwise-required portable keyword.
type geminiSchemaNode struct {
	Type        json.RawMessage             `json:"type"`
	Description json.RawMessage             `json:"description,omitempty"`
	Properties  *map[string]json.RawMessage `json:"properties,omitempty"`
	Items       json.RawMessage             `json:"items,omitempty"`
	Enum        json.RawMessage             `json:"enum,omitempty"`
	Required    json.RawMessage             `json:"required,omitempty"`
}

func projectGeminiSchema(schema json.RawMessage) (json.RawMessage, error) {
	var node geminiSchemaNode
	if err := json.Unmarshal(schema, &node); err != nil {
		return nil, &inference.SchemaValidationError{Field: inference.SchemaFieldSchema, ReasonCode: inference.SchemaReasonInvalid}
	}
	if node.Properties != nil {
		for name, property := range *node.Properties {
			projected, err := projectGeminiSchema(property)
			if err != nil {
				return nil, err
			}
			(*node.Properties)[name] = projected
		}
	}
	if len(node.Items) > 0 {
		projected, err := projectGeminiSchema(node.Items)
		if err != nil {
			return nil, err
		}
		node.Items = projected
	}
	projected, err := json.Marshal(node)
	if err != nil {
		return nil, &inference.SchemaValidationError{Field: inference.SchemaFieldSchema, ReasonCode: inference.SchemaReasonInvalid}
	}
	return projected, nil
}

// buildGenerationConfig maps effective sampling to Gemini's generationConfig,
// returning nil when nothing is set so the whole key is omitted. Sampling
// pointers/slices are referenced (read-only) directly, not cloned.
func buildGenerationConfig(s model.Sampling, caps model.Capabilities) *generationConfig {
	gc := &generationConfig{
		Temperature:     s.Temperature,
		TopP:            s.TopP,
		MaxOutputTokens: s.MaxTokens,
		StopSequences:   s.Stop,
		ThinkingConfig:  thinkingFor(s.Effort, caps),
	}
	if gc.Temperature == nil && gc.TopP == nil && gc.MaxOutputTokens == nil &&
		len(gc.StopSequences) == 0 && gc.ThinkingConfig == nil {
		return nil
	}
	return gc
}

// thinkingFor maps dialect-neutral Effort to a Gemini thinkingConfig. It is
// fail-safe gated on Caps.Thinking: a thinkingConfig sent to a non-thinking model
// is a 400, so a model that does not advertise thinking never receives one.
// EffortNone (and any unknown value) yields nil — thinking untouched.
func thinkingFor(e model.Effort, caps model.Capabilities) *thinkingConfig {
	if !caps.Thinking {
		return nil
	}
	budget, ok := thinkingBudget(e)
	if !ok {
		return nil
	}
	return &thinkingConfig{ThinkingBudget: &budget, IncludeThoughts: true}
}

// thinkingBudget maps Effort to a thinkingBudget token value, ok=false meaning
// "omit". EffortMax uses -1 (dynamic: the model decides, the strongest signal and
// always valid); low/medium/high use fixed budgets within the valid range of
// Gemini 2.5 Flash and Pro. These fixed values are a conservative default and may
// warrant per-model tuning.
func thinkingBudget(e model.Effort) (int, bool) {
	switch e {
	case model.EffortLow:
		return 4096, true
	case model.EffortMedium:
		return 8192, true
	case model.EffortHigh:
		return 16384, true
	case model.EffortMax:
		return -1, true // dynamic thinking
	default: // EffortNone or unknown → omit
		return 0, false
	}
}

// argsJSON normalizes a tool-call argument payload for the wire: an empty payload
// becomes an empty JSON object so functionCall.args is always a valid Struct.
func argsJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
