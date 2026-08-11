package openaiapi

import (
	"encoding/json"

	"github.com/looprig/inference/internal/usagenorm"
)

const (
	responseFormatJSONSchema = "json_schema"
	toolChoiceRequired       = "required"
)

// ChatRequest is the OpenAI chat completions wire request. Exported so
// provider packages can embed it in a typed extension struct (e.g. adding an
// encrypted-response public key) without round-tripping through map[string]json.RawMessage.
type ChatRequest struct {
	Model          string             `json:"model"`
	Messages       []chatMessage      `json:"messages"`
	Tools          []chatTool         `json:"tools,omitempty"`
	ResponseFormat *responseFormat    `json:"response_format,omitempty"`
	ToolChoice     string             `json:"tool_choice,omitempty"`
	Temperature    *float64           `json:"temperature,omitempty"`
	TopP           *float64           `json:"top_p,omitempty"`
	MaxTokens      *int               `json:"max_tokens,omitempty"`
	Stop           []string           `json:"stop,omitempty"`
	Stream         bool               `json:"stream,omitempty"`
	StreamOptions  *chatStreamOptions `json:"stream_options,omitempty"`

	// o-series reasoning
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string      `json:"role"`
	Content          interface{} `json:"content"`                     // string or []chatContentPart; interface{} required at JSON serialization boundary
	ReasoningContent string      `json:"reasoning_content,omitempty"` // DeepSeek / o-series
	ToolCalls        []toolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name string `json:"name"`
	// Arguments is json.RawMessage to tolerate both wire shapes on DECODE
	// (some servers send a JSON string, others a bare object). On ENCODE it
	// MUST be a JSON-encoded string — see encodeAIMessage, which quotes the
	// raw object before assigning it here. Do not assign a bare object.
	Arguments json.RawMessage `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"` // always "function"
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatResponse is the OpenAI chat completions wire response.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens            usagenorm.Count             `json:"prompt_tokens"`
	CompletionTokens        usagenorm.Count             `json:"completion_tokens"`
	PromptTokensDetails     chatPromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails chatCompletionTokensDetails `json:"completion_tokens_details"`
}

type chatPromptTokensDetails struct {
	CachedTokens     usagenorm.Count `json:"cached_tokens"`
	CacheWriteTokens usagenorm.Count `json:"cache_write_tokens"`
}

type chatCompletionTokensDetails struct {
	ReasoningTokens usagenorm.Count `json:"reasoning_tokens"`
}

// sseChunk is one streaming delta or terminal-usage event.
type sseChunk struct {
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
	Usage   *chatUsage  `json:"usage"`
}

type sseChoice struct {
	Delta        sseMessageDelta `json:"delta"`
	FinishReason string          `json:"finish_reason"`
}

type sseMessageDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"` // DeepSeek / o-series
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

// sseToolCallDelta is one streaming tool-call delta entry. Unlike the
// non-streaming toolCall, it carries a per-call Index and delivers
// Function.Arguments as string fragments that the runner concatenates by Index.
type sseToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"` // first delta only
	Function struct {
		Name      string `json:"name"`      // first delta only
		Arguments string `json:"arguments"` // FRAGMENT — concatenate across deltas
	} `json:"function"`
}
