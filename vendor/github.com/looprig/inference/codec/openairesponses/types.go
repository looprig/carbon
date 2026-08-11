// Package openairesponses is the OpenAI Responses API wire dialect
// (POST /v1/responses): a genuinely different, items-based shape from OpenAI
// Chat Completions (codec/openaiapi) — not a flat messages array. It is both
// a client-side codec.StreamingCodec (neutral -> wire, for calling a
// Responses-speaking target) and a server-side codec.ServerCodec (wire ->
// neutral, for serving a Responses-speaking harness such as Codex).
package openairesponses

import (
	"encoding/json"

	"github.com/looprig/inference/internal/usagenorm"
)

// Wire-value constants. Centralized so encode, decode, and stream-event paths
// cannot drift on a string literal.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
	// roleDeveloper is Responses' alternative to roleSystem for a system-role
	// input item; this codec treats it identically to roleSystem on decode.
	roleDeveloper = "developer"

	itemTypeMessage            = "message"
	itemTypeFunctionCall       = "function_call"
	itemTypeFunctionCallOutput = "function_call_output"
	itemTypeReasoning          = "reasoning"

	contentTypeInputText  = "input_text"
	contentTypeInputImage = "input_image"
	contentTypeOutputText = "output_text"

	toolTypeFunction = "function"

	toolChoiceAuto     = "auto"
	toolChoiceRequired = "required"
	toolChoiceNone     = "none"

	textFormatPlainText  = "text"
	textFormatJSONSchema = "json_schema"

	summaryTypeText = "summary_text"

	// includeEncryptedReasoningContent is the `include` value that requests
	// encrypted reasoning content back from a Responses target, so a
	// same-dialect follow-up request can replay it via a `reasoning` input
	// item's encrypted_content.
	includeEncryptedReasoningContent = "reasoning.encrypted_content"

	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"

	incompleteReasonMaxOutputTokens = "max_output_tokens"

	imageDetailAuto = "auto"

	dataURIPrefix = "data:"

	// emptyObject is the fallback for a function_call's `arguments` (and a
	// tool's `parameters` schema): Responses requires these to be JSON
	// objects, so an empty neutral value becomes "{}".
	emptyObject = "{}"

	// defaultSchema is the fallback for a tool with no schema.
	defaultSchema = `{"type":"object"}`
)

// SSE event `type` values this codec handles, per the design doc's native
// event order plus the tool/reasoning/failure events required for full
// support.
const (
	eventResponseCreated       = "response.created"
	eventOutputItemAdded       = "response.output_item.added"
	eventContentPartAdded      = "response.content_part.added"
	eventOutputTextDelta       = "response.output_text.delta"
	eventContentPartDone       = "response.content_part.done"
	eventOutputItemDone        = "response.output_item.done"
	eventResponseCompleted     = "response.completed"
	eventFunctionCallArgsDelta = "response.function_call_arguments.delta"
	eventFunctionCallArgsDone  = "response.function_call_arguments.done"
	eventReasoningSummaryDelta = "response.reasoning_summary_text.delta"
	eventReasoningSummaryDone  = "response.reasoning_summary_text.done"
	eventResponseFailed        = "response.failed"
)

// --- request-direction wire shape (shared by encode.go and server_decode.go) ---

// wireItem is the wire form of one `input` array entry: a tagged union
// discriminated by Type, following the same "one struct, omitempty per
// variant" pattern anthropicapi uses for its content blocks. ID doubles as
// the optional item id on a function_call item (decode-only tolerance,
// ignored) and the reasoning item's id.
type wireItem struct {
	Type string `json:"type"`

	// message
	Role    string            `json:"role,omitempty"`
	Content []wireContentPart `json:"content,omitempty"`

	// function_call / function_call_output
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // JSON-encoded STRING on the wire
	Output    string `json:"output,omitempty"`

	// reasoning
	Summary          []wireSummaryPart `json:"summary,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
}

// wireContentPart is one entry of an item's `content` array: input_text,
// input_image (request direction) or output_text (response direction,
// reused by wireOutputItem). Annotations is decode-tolerant only; this codec
// never populates or interprets it (citations are out of scope).
type wireContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	ImageURL    string            `json:"image_url,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
}

type wireSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

type wireText struct {
	Format *wireTextFormat `json:"format,omitempty"`
}

type wireTextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

type wireReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// wireRequest is the encode-direction (client, neutral -> wire) POST
// /v1/responses request body. ToolChoice is a plain string here (the codec
// only ever emits "" or "required"); the decode direction uses a
// json.RawMessage sibling (wireDecodeRequest, server_decode.go) so it can
// classify — and reject — the object/"none" forms real clients may send.
// Store is intentionally a non-omitempty bool: this project excludes
// server-stored conversations, so every request explicitly sends
// "store":false rather than relying on an unverified provider default.
type wireRequest struct {
	Model           string               `json:"model"`
	Instructions    string               `json:"instructions,omitempty"`
	Input           []wireItem           `json:"input"`
	Tools           []wireTool           `json:"tools,omitempty"`
	ToolChoice      string               `json:"tool_choice,omitempty"`
	MaxOutputTokens *int                 `json:"max_output_tokens,omitempty"`
	Temperature     *float64             `json:"temperature,omitempty"`
	TopP            *float64             `json:"top_p,omitempty"`
	Text            *wireText            `json:"text,omitempty"`
	Reasoning       *wireReasoningConfig `json:"reasoning,omitempty"`
	Include         []string             `json:"include,omitempty"`
	Store           bool                 `json:"store"`
	Stream          bool                 `json:"stream,omitempty"`
}

// --- response-direction wire shape ---------------------------------------
//
// A response's `output` array reuses wireItem (above) rather than a separate
// type: it is marshal-safe (no usagenorm.Count fields) and structurally
// identical to an input item for every item type this codec models (message,
// function_call, reasoning), so wireItem is shared verbatim by the
// client-decode direction (decode.go, stream.go) and the server-encode
// direction (server_encode.go, server_stream.go) — mirroring how
// anthropicapi's anthropicBlock is reused both ways for content blocks.

type wireIncompleteDetails struct {
	Reason string `json:"reason"`
}

type wireResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// wireResponse is the DECODE-direction (client, wire -> neutral) response
// envelope: Usage uses usagenorm.Count so it can decode a real provider
// value. It backs both the non-streaming response body (decode.go) and the
// `response` object embedded in response.created/completed/failed stream
// events (stream.go).
type wireResponse struct {
	ID                string                 `json:"id"`
	Status            string                 `json:"status"`
	Model             string                 `json:"model"`
	Output            []wireItem             `json:"output"`
	Usage             *wireUsage             `json:"usage"`
	IncompleteDetails *wireIncompleteDetails `json:"incomplete_details"`
	Error             *wireResponseError     `json:"error"`
}

type wireUsage struct {
	InputTokens         usagenorm.Count         `json:"input_tokens"`
	OutputTokens        usagenorm.Count         `json:"output_tokens"`
	InputTokensDetails  wireInputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails wireOutputTokensDetails `json:"output_tokens_details"`
}

type wireInputTokensDetails struct {
	CachedTokens usagenorm.Count `json:"cached_tokens"`
}

type wireOutputTokensDetails struct {
	ReasoningTokens usagenorm.Count `json:"reasoning_tokens"`
}
