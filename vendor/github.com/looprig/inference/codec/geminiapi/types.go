package geminiapi

import (
	"encoding/json"

	"github.com/looprig/inference/internal/usagenorm"
)

// Wire roles for a Gemini `contents` entry. Gemini names the assistant turn
// "model" (not "assistant"), has no "system" role in `contents` (the system
// prompt goes to the top-level systemInstruction), and carries tool results as a
// "user" turn holding a functionResponse part.
const (
	roleUser  = "user"
	roleModel = "model"

	responseMIMETypeJSON   = "application/json"
	functionCallingModeAny = "ANY"
)

// GenerateContentRequest is the Gemini generateContent / streamGenerateContent
// wire request body. The two endpoints share an identical body — streaming is a
// URL + `?alt=sse` concern owned by the transport, not a body field — so there is
// no "stream" flag here (unlike the OpenAI dialect). Exported so provider
// packages can embed it in a typed extension struct without round-tripping
// through map[string]json.RawMessage.
type GenerateContentRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

// geminiContent is one turn in `contents` (or the systemInstruction). Role is
// "user" or "model" for `contents` entries and omitted for systemInstruction.
// The same type serves the response `candidate.content`, so it is shared by both
// encode and decode.
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is one part of a turn. Exactly one payload field is set per part on
// encode; on decode the unset fields stay at their zero value. The type is shared
// across request and response: Text/FunctionCall/Thought appear in responses;
// InlineData/FileData/FunctionResponse appear only in requests.
type geminiPart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FileData         *fileData         `json:"fileData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`

	// ThoughtSignature is Gemini 2.5+'s opaque, provider-private continuation
	// token for a thought (or the function-call part it accompanies). It is a
	// plain base64-ish string on the wire, not structured JSON, so it is
	// carried as a Go string here — the domain-model mapping into
	// content.ThinkingBlock.ProviderState (a json.RawMessage) stores this
	// string's JSON-marshaled form, the same "wrap the opaque wire string as
	// a JSON string value" convention codec/openairesponses uses for its own
	// analogous encrypted_content field (see opaqueStateFromWire/
	// opaqueStateToWire, decode.go/encode.go). A same-dialect round trip must
	// echo it back byte-for-byte on a replayed thought part; this codec never
	// interprets its contents.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// inlineData carries raw image (or other media) bytes inline as base64. This is
// Gemini's primary multimodal input mechanism.
type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded bytes
}

// fileData references media by URI. Gemini's fileUri accepts a File API URI, a
// gs:// object, or a YouTube URL — NOT an arbitrary web URL. It is used here only
// as the closest structural mapping for a URL-sourced image (see imagePart).
type fileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

// functionCall is a model-issued tool call. Args is the raw JSON object of the
// arguments (a Struct on the wire). ID is present on models that support parallel
// calls and is used to match the paired functionResponse. Shared by encode (the
// model's prior call, echoed back) and decode (a fresh call from the model).
type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// functionResponse carries a tool result back to the model. Name MUST match the
// paired functionCall's name (Gemini matches on name, not id); ID echoes the call
// id for parallel-call disambiguation. Response is a JSON object (Struct).
type functionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// geminiTool wraps a set of function declarations. Gemini groups declarations
// under a single tool entry (unlike OpenAI's one-tool-per-function shape).
type geminiTool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations,omitempty"`
}

// functionDeclaration is a callable function exposed to the model. Parameters is
// a JSON-Schema object describing the arguments.
type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// generationConfig maps dialect-neutral Sampling to Gemini's sampling knobs.
// Field names are camelCase per the Gemini wire (topP, maxOutputTokens,
// stopSequences), unlike the OpenAI snake_case dialect.
type generationConfig struct {
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	MaxOutputTokens    *int            `json:"maxOutputTokens,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	ThinkingConfig     *thinkingConfig `json:"thinkingConfig,omitempty"`
	ResponseMIMEType   string          `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type functionCallingConfig struct {
	Mode string `json:"mode"`
}

// thinkingConfig controls Gemini 2.5+ extended thinking. ThinkingBudget is a
// token budget: -1 requests dynamic (model-decided) thinking; a positive value
// caps it. IncludeThoughts asks the model to return thought summaries as parts
// tagged `"thought": true` (decoded into ThinkingBlock / ThinkingChunk).
type thinkingConfig struct {
	ThinkingBudget  *int `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
}

// GenerateContentResponse is the Gemini generateContent response body and the
// per-chunk streamGenerateContent event (identical shape; a streamed chunk is a
// partial GenerateContentResponse).
type GenerateContentResponse struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
	ModelVersion  string         `json:"modelVersion"`
}

// candidate is one generated alternative. The codec reads candidates[0] only.
type candidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
	Index        int           `json:"index"`
}

// usageMetadata reports token consumption.
type usageMetadata struct {
	PromptTokenCount        usagenorm.Count `json:"promptTokenCount"`
	CandidatesTokenCount    usagenorm.Count `json:"candidatesTokenCount"`
	CachedContentTokenCount usagenorm.Count `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      usagenorm.Count `json:"thoughtsTokenCount"`
	TotalTokenCount         usagenorm.Count `json:"totalTokenCount"`
}
