package openaiapi

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/inference/wire/jsonbody"
)

// Wire object/role/type tags shared by the non-streaming and streaming
// server-encode paths.
const (
	objectChatCompletion      = "chat.completion"
	objectChatCompletionChunk = "chat.completion.chunk"
	roleAssistantWire         = "assistant"
	toolTypeFunctionWire      = "function"
)

// encodeChatResponse is the server-ENCODE-direction wire form of a
// non-streaming Chat Completions response. It deliberately does NOT reuse
// chatResponse (types.go): that backs the existing client-DECODE direction
// and carries Usage as *chatUsage, whose usagenorm.Count fields hold only an
// unexported raw-JSON capture with no MarshalJSON — they can decode a real
// count but cannot encode one.
type encodeChatResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Model   string             `json:"model"`
	Choices []encodeChatChoice `json:"choices"`
	Usage   *encodeChatUsage   `json:"usage,omitempty"`
}

type encodeChatChoice struct {
	Index        int               `json:"index"`
	Message      encodeChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

// encodeChatMessage is the server-encode-direction wire form of one
// choice's `message`. Content is a pointer so an assistant turn with only
// tool calls can encode `"content":null`, matching real Chat Completions
// responses.
type encodeChatMessage struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []encodeToolCall `json:"tool_calls,omitempty"`
}

type encodeToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function encodeToolCallFunction `json:"function"`
}

// encodeToolCallFunction carries Arguments as a plain Go string: json.Marshal
// of a string automatically produces the JSON-encoded-string wire form the
// dialect requires (unlike the request-encode direction's toolCallFunction,
// which stores json.RawMessage and must be quoted manually before assignment
// — see encodeAIMessage, encode.go).
type encodeToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// encodeChatUsage is the encode-direction counterpart to chatUsage: plain
// exported uint64 fields that json.Marshal can actually serialize.
type encodeChatUsage struct {
	PromptTokens            uint64                        `json:"prompt_tokens"`
	CompletionTokens        uint64                        `json:"completion_tokens"`
	TotalTokens             uint64                        `json:"total_tokens"`
	PromptTokensDetails     encodePromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails encodeCompletionTokensDetails `json:"completion_tokens_details"`
}

type encodePromptTokensDetails struct {
	CachedTokens     uint64 `json:"cached_tokens"`
	CacheWriteTokens uint64 `json:"cache_write_tokens"`
}

type encodeCompletionTokensDetails struct {
	ReasoningTokens uint64 `json:"reasoning_tokens"`
}

// writeChatResponse encodes a complete inference.Response as the native Chat
// Completions non-streaming response and writes a 200 with it.
func writeChatResponse(w http.ResponseWriter, resp *inference.Response) error {
	wire, err := buildChatResponse(resp)
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return err
}

func buildChatResponse(resp *inference.Response) (encodeChatResponse, error) {
	if resp == nil {
		resp = &inference.Response{}
	}
	msg := encodeChatMessage{Role: roleAssistantWire}
	if resp.Message != nil {
		built, err := buildResponseMessage(resp.Message.Blocks, newToolIDGenerator())
		if err != nil {
			return encodeChatResponse{}, err
		}
		msg = built
	}

	return encodeChatResponse{
		ID:      "chatcmpl-" + randomHex(12),
		Object:  objectChatCompletion,
		Model:   resp.Model,
		Choices: []encodeChatChoice{{Index: 0, Message: msg, FinishReason: encodeFinishReason(resp.FinishReason)}},
		Usage:   encodeUsage(resp.Usage),
	}, nil
}

// buildResponseMessage maps a slice of neutral content blocks (a
// non-streaming response's AIMessage.Blocks) to the native Chat Completions
// `message` shape, mirroring buildBlocks' (decode.go) order in reverse:
// reasoning, then text, then tool calls. ids synthesizes a tool_call id when
// a ToolUseBlock arrives with none (a cross-dialect upstream target might
// not supply one); the client-encode direction (encode.go's encodeAIMessage)
// never needs this because a request replay always carries the id a prior
// real response assigned.
func buildResponseMessage(blocks []content.Block, ids func() string) (encodeChatMessage, error) {
	var reasoning strings.Builder
	var text strings.Builder
	var hasText bool
	var calls []encodeToolCall

	for _, b := range blocks {
		switch b := b.(type) {
		case *content.ThinkingBlock:
			reasoning.WriteString(b.Thinking)
		case *content.TextBlock:
			hasText = true
			text.WriteString(b.Text)
		case *content.ToolUseBlock:
			id := b.ID
			if id == "" {
				id = ids()
			}
			raw := string(b.Input)
			if raw == "" {
				raw = emptyObject
			}
			calls = append(calls, encodeToolCall{
				ID:       id,
				Type:     toolTypeFunctionWire,
				Function: encodeToolCallFunction{Name: b.Name, Arguments: raw},
			})
		default:
			return encodeChatMessage{}, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}

	var contentPtr *string
	if hasText {
		s := text.String()
		contentPtr = &s
	}

	return encodeChatMessage{
		Role:             roleAssistantWire,
		Content:          contentPtr,
		ReasoningContent: reasoning.String(),
		ToolCalls:        calls,
	}, nil
}

// encodeFinishReason maps the neutral stream.FinishReason to Chat
// Completions' finish_reason, inverting mapFinishReason (stream.go).
// FinishReasonUnknown has no dedicated wire value; "stop" is the least
// presumptive default for a response that otherwise succeeded (this encoder
// is never reached for a failed response — WriteError/Fail own that path).
func encodeFinishReason(r stream.FinishReason) string {
	switch r {
	case stream.FinishReasonStop:
		return "stop"
	case stream.FinishReasonLength:
		return "length"
	case stream.FinishReasonToolUse:
		return "tool_calls"
	case stream.FinishReasonContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// encodeUsage inverts normalizeUsage/normalizePromptUsage/
// normalizeCompletionUsage (decode.go): prompt_tokens is reconstructed as
// the gross prompt total (neutral InputTokens plus CacheReadTokens plus
// CacheCreationTokens), and completion_tokens is the neutral OutputTokens
// directly — decode.go's normalizeCompletionUsage never subtracts
// ReasoningTokens from it, so on this codec's model OutputTokens is already
// the gross completion total and completion_tokens_details.reasoning_tokens
// is a breakdown subset, not an additive component.
func encodeUsage(u *content.Usage) *encodeChatUsage {
	if u == nil {
		return nil
	}
	grossPrompt := uint64(u.InputTokens) + uint64(u.CacheReadTokens) + uint64(u.CacheCreationTokens)
	completion := uint64(u.OutputTokens)
	return &encodeChatUsage{
		PromptTokens:            grossPrompt,
		CompletionTokens:        completion,
		TotalTokens:             grossPrompt + completion,
		PromptTokensDetails:     encodePromptTokensDetails{CachedTokens: uint64(u.CacheReadTokens), CacheWriteTokens: uint64(u.CacheCreationTokens)},
		CompletionTokensDetails: encodeCompletionTokensDetails{ReasoningTokens: uint64(u.ReasoningTokens)},
	}
}

// --- native error envelope --------------------------------------------------

// chatErrorEnvelope is this codec's own best-effort choice for the native
// Chat Completions error shape (`{"error":{"message":...,"type":...,"code":...}}`,
// matching OpenAI's real documented error envelope); it is not verified
// against a live endpoint.
type chatErrorEnvelope struct {
	Error chatErrorBody `json:"error"`
}

type chatErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeChatError encodes err as a native Chat Completions error envelope and
// writes it with the classified HTTP status. It never panics, regardless of
// err's concrete type (including nil).
func writeChatError(w http.ResponseWriter, err error) {
	status, typ, code, message := classifyChatError(err)
	body, marshalErr := json.Marshal(chatErrorEnvelope{Error: chatErrorBody{Message: message, Type: typ, Code: code}})
	if marshalErr != nil {
		// Marshaling a plain string-only struct cannot realistically fail,
		// but WriteError must never panic: fall back to a fixed, valid
		// envelope.
		body = []byte(`{"error":{"message":"internal encode error","type":"api_error"}}`)
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// classifyChatError maps an arbitrary Go error to the HTTP status and a
// short machine-readable type/code, used by both writeChatError
// (pre-header) and StreamEncoder.Fail (post-header, via a bounded gateway
// error event carrying the message half of this same classification). A nil
// err classifies as a generic 500 api_error so callers never need a nil
// guard before calling this.
func classifyChatError(err error) (status int, typ string, code string, message string) {
	if err == nil {
		return http.StatusInternalServerError, "api_error", "", "unknown error"
	}

	var decErr *ServerDecodeError
	if errors.As(err, &decErr) {
		return http.StatusBadRequest, "invalid_request_error", "", decErr.Error()
	}
	var dupErr *DuplicateKeyError
	if errors.As(err, &dupErr) {
		return http.StatusBadRequest, "invalid_request_error", "", dupErr.Error()
	}
	var nErr *UnsupportedChoiceCountError
	if errors.As(err, &nErr) {
		return http.StatusBadRequest, "invalid_request_error", "", nErr.Error()
	}
	var blockErr *UnsupportedBlockError
	if errors.As(err, &blockErr) {
		return http.StatusBadRequest, "invalid_request_error", "", blockErr.Error()
	}
	var chunkErr *UnsupportedChunkError
	if errors.As(err, &chunkErr) {
		return http.StatusBadRequest, "invalid_request_error", "", chunkErr.Error()
	}
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status < 100 || status > 599 {
			status = http.StatusBadGateway
		}
		return status, codeForChatStatus(status), "", err.Error()
	}

	return http.StatusInternalServerError, "api_error", "", err.Error()
}

// codeForChatStatus maps an HTTP status to a short error type for statuses
// this codec did not otherwise classify from a typed error (e.g. an opaque
// upstream *failure.APIError).
func codeForChatStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

// --- id synthesis -------------------------------------------------------

// newToolIDGenerator returns a closure that yields fresh, collision-resistant,
// call-scoped synthetic tool_call ids. It combines a per-call random prefix
// (guarding against collision across different responses/streams) with a
// monotonic counter (guarding against collision within one response/stream,
// even with zero entropy available).
func newToolIDGenerator() func() string {
	prefix := randomHex(6)
	counter := 0
	return func() string {
		counter++
		return fmt.Sprintf("call_gw_%s_%d", prefix, counter)
	}
}

// randomHex returns n random bytes hex-encoded. On the practically-impossible
// event crypto/rand fails, it falls back to a fixed all-zero value rather than
// panicking or failing the response.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
