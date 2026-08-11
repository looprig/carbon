package openaiapi

import (
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
)

// maxGatewayErrorMessageBytes bounds the message text a post-header Fail
// event may carry: a mid-stream gateway error is more exposed than a
// pre-header WriteError envelope (a client may already be mid-render of a
// partial response), so it is kept small and never includes secrets or raw
// upstream bodies — see classifyChatError, which only ever surfaces typed
// errors' own short .Error() text.
const maxGatewayErrorMessageBytes = 300

// gatewayErrorType tags a Fail-emitted SSE data event as gateway-authored,
// distinguishing it from a native upstream error a compatible client might
// otherwise try to interpret as a provider-specific error shape.
const gatewayErrorType = "gateway_error"

// openChatStream begins the native Chat Completions streaming response: it
// commits the text/event-stream headers and the 200 status, then returns the
// request-scoped encoder. Unlike Responses/Anthropic, Chat Completions has no
// leading "stream started" event — the first native event is the first
// WriteChunk call's own delta (carrying the one-time role marker).
func openChatStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	return &serverStreamEncoder{
		w:             w,
		flusher:       flusher,
		ids:           newToolIDGenerator(),
		id:            "chatcmpl-" + randomHex(12),
		seenToolIndex: make(map[int]bool),
	}, nil
}

// serverStreamEncoder is the request-scoped codec.StreamEncoder for one
// in-flight native Chat Completions stream. It is never shared across
// requests: it holds per-stream state (the one-time role marker, per-index
// tool-call id/name bookkeeping, and the id generator) that OpenStream fills
// in fresh each call.
type serverStreamEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ids     func() string
	id      string

	done          bool
	roleSent      bool
	seenToolIndex map[int]bool
}

var _ codec.StreamEncoder = (*serverStreamEncoder)(nil)

// WriteChunk encodes and flushes one content.Chunk as a native
// chat.completion.chunk SSE data event. The very first chunk of any kind
// also carries the one-time `delta.role":"assistant"` marker, matching real
// Chat Completions streaming. A ToolUseChunk's id/name/type appear only on
// the first delta observed for its neutral Index — matching
// `choices[0].delta.tool_calls[].index`'s role as Chat Completions' parallel
// tool-call ordering signal — with subsequent deltas for that index carrying
// only the argument fragment.
func (e *serverStreamEncoder) WriteChunk(chunk content.Chunk) error {
	if e.done {
		return &StreamTerminatedError{}
	}

	first := !e.roleSent
	delta := encodeSSEDelta{}
	if first {
		delta.Role = roleAssistantWire
		e.roleSent = true
	}

	meaningful := false
	switch c := chunk.(type) {
	case *content.TextChunk:
		if c.Text != "" {
			delta.Content = c.Text
			meaningful = true
		}
	case *content.ThinkingChunk:
		if c.Thinking != "" {
			delta.ReasoningContent = c.Thinking
			meaningful = true
		}
	case *content.ToolUseChunk:
		td := encodeSSEToolCallDelta{Index: c.Index}
		if !e.seenToolIndex[c.Index] {
			e.seenToolIndex[c.Index] = true
			id := c.ID
			if id == "" {
				id = e.ids()
			}
			td.ID = id
			td.Type = toolTypeFunctionWire
			td.Function.Name = c.Name
			meaningful = true
		}
		if c.InputJSON != "" {
			td.Function.Arguments = c.InputJSON
			meaningful = true
		}
		if meaningful {
			delta.ToolCalls = []encodeSSEToolCallDelta{td}
		}
	default:
		return &UnsupportedChunkError{Chunk: unsupportedChunkTypeName(chunk)}
	}

	if !meaningful && !first {
		return nil
	}
	return e.writeDeltaEvent(delta)
}

func (e *serverStreamEncoder) writeDeltaEvent(delta encodeSSEDelta) error {
	reason := (*string)(nil)
	chunk := encodeSSEChunk{
		ID:      e.id,
		Object:  objectChatCompletionChunk,
		Choices: []encodeSSEChoice{{Index: 0, Delta: delta, FinishReason: reason}},
	}
	return e.writeEvent(chunk)
}

// Finish encodes the native stream-completion event(s) from authoritative
// terminal metadata: a final chunk carrying `finish_reason` on an empty
// delta, then (only when result.Usage is present) a second chunk with an
// EMPTY choices array carrying `usage`, then the [DONE] sentinel. Emission is
// gated solely on result.Usage being present, not on whether the request set
// stream_options.include_usage: DecodeRequest does not thread that flag
// through to the StreamEncoder, so this codec always reports authoritative
// usage when available rather than silently withholding it from a harness
// that requested it under a header this layer never inspects the value of.
func (e *serverStreamEncoder) Finish(result stream.StreamResult) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	reason := encodeFinishReason(result.FinishReason)
	finalChunk := encodeSSEChunk{
		ID:      e.id,
		Object:  objectChatCompletionChunk,
		Model:   result.Model,
		Choices: []encodeSSEChoice{{Index: 0, Delta: encodeSSEDelta{}, FinishReason: &reason}},
	}
	if err := e.writeEvent(finalChunk); err != nil {
		return err
	}

	if result.Usage != nil {
		usageChunk := encodeSSEChunk{
			ID:      e.id,
			Object:  objectChatCompletionChunk,
			Model:   result.Model,
			Choices: []encodeSSEChoice{},
			Usage:   encodeUsage(result.Usage),
		}
		if err := e.writeEvent(usageChunk); err != nil {
			return err
		}
	}

	return e.writeDoneSentinel()
}

// Fail implements the design's "Chat Completions emits a bounded gateway
// error event and terminal sentinel only where doing so remains valid for
// compatible clients" clause: since there is no standardized native
// Chat Completions in-stream error event, it writes one small,
// gateway-authored `{"error":{"message":...,"type":"gateway_error"}}` data
// event (never upstream response bodies or secrets — only a typed error's own
// bounded .Error() text) followed by the ordinary [DONE] sentinel, then
// closes. Plain abrupt termination (no data, no sentinel) was the other
// reading available, but it risks a naive OpenAI-compatible client hanging
// on a read that never resolves; a bounded, clearly-gateway-tagged event
// plus the sentinel a compatible client already knows how to treat as
// end-of-stream is the more defensible choice and is explicitly sanctioned
// by the design doc's own hedge ("best effort, not a native contract").
func (e *serverStreamEncoder) Fail(err error) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	_, _, _, message := classifyChatError(err)
	evt := gatewayErrorEvent{Error: gatewayErrorBody{Message: boundedErrorMessage(message), Type: gatewayErrorType}}
	if writeErr := e.writeEvent(evt); writeErr != nil {
		return writeErr
	}
	return e.writeDoneSentinel()
}

// writeEvent marshals payload and writes it as one SSE data-only frame
// ("data: <json>\n\n" — Chat Completions' SSE convention has no named
// `event:` line, unlike Anthropic/Responses), flushing immediately so
// streaming progressively delivers bytes rather than buffering until the
// handler returns.
func (e *serverStreamEncoder) writeEvent(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := e.w.Write([]byte("data: " + string(body) + "\n\n")); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// writeDoneSentinel writes the literal `data: [DONE]\n\n` frame, reusing the
// same doneSentinel constant (stream.go) the client-decode direction treats
// as end-of-stream, so encode and decode cannot drift on the exact string.
func (e *serverStreamEncoder) writeDoneSentinel() error {
	if _, err := e.w.Write([]byte("data: " + doneSentinel + "\n\n")); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// boundedErrorMessage truncates s to at most maxGatewayErrorMessageBytes
// bytes without splitting a UTF-8 code point, for the bounded post-header
// Fail event.
func boundedErrorMessage(s string) string {
	if len(s) <= maxGatewayErrorMessageBytes {
		return s
	}
	end := maxGatewayErrorMessageBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func unsupportedChunkTypeName(c content.Chunk) string {
	if c == nil {
		return "<nil>"
	}
	switch c.(type) {
	case *content.TextChunk:
		return "TextChunk"
	case *content.ThinkingChunk:
		return "ThinkingChunk"
	case *content.ToolUseChunk:
		return "ToolUseChunk"
	default:
		return "unknown"
	}
}

// --- server-encode-direction SSE event payloads -----------------------------

type encodeSSEChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Model   string            `json:"model,omitempty"`
	Choices []encodeSSEChoice `json:"choices"`
	Usage   *encodeChatUsage  `json:"usage,omitempty"`
}

type encodeSSEChoice struct {
	Index        int            `json:"index"`
	Delta        encodeSSEDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type encodeSSEDelta struct {
	Role             string                   `json:"role,omitempty"`
	Content          string                   `json:"content,omitempty"`
	ReasoningContent string                   `json:"reasoning_content,omitempty"`
	ToolCalls        []encodeSSEToolCallDelta `json:"tool_calls,omitempty"`
}

type encodeSSEToolCallDelta struct {
	Index    int                            `json:"index"`
	ID       string                         `json:"id,omitempty"`
	Type     string                         `json:"type,omitempty"`
	Function encodeSSEToolCallFunctionDelta `json:"function,omitempty"`
}

type encodeSSEToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// gatewayErrorEvent is the bounded, gateway-authored post-header error event
// this codec's Fail writes — see Fail's doc comment for the design rationale.
type gatewayErrorEvent struct {
	Error gatewayErrorBody `json:"error"`
}

type gatewayErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}
