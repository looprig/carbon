package anthropicapi

import (
	"encoding/json"
	"net/http"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
)

// blockKind identifies which native content_block variant is currently open on
// the wire, so WriteChunk knows whether an incoming chunk continues the open
// block or must close it and start a new one.
type blockKind uint8

const (
	blockKindNone blockKind = iota
	blockKindText
	blockKindThinking
	blockKindToolUse
)

// openMessagesStream begins the native Anthropic Messages streaming response:
// it commits the text/event-stream headers and the 200 status, then emits the
// leading message_start event before returning the request-scoped encoder. Model
// and usage are not yet known this early (OpenStream receives no request/target
// context), so message_start necessarily carries placeholder values; Finish
// later supplies the authoritative terminal Model/Usage via message_delta.
func openMessagesStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	enc := &serverStreamEncoder{w: w, flusher: flusher, ids: newToolIDGenerator()}
	if err := enc.writeMessageStart(); err != nil {
		return nil, err
	}
	return enc, nil
}

// serverStreamEncoder is the request-scoped codec.StreamEncoder for one
// in-flight native Anthropic stream. It is never shared across requests: it
// holds per-stream state (the open content_block bookkeeping and the tool_use
// id generator) that OpenStream fills in fresh each call.
type serverStreamEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ids     func() string

	done bool

	nextWireIndex int
	openKind      blockKind
	openWireIndex int
	openToolIndex int // valid only when openKind == blockKindToolUse
}

var _ codec.StreamEncoder = (*serverStreamEncoder)(nil)

func (e *serverStreamEncoder) writeMessageStart() error {
	return e.writeEvent(eventMessageStart, sseMessageStart{
		Type: eventMessageStart,
		Message: sseStartMessage{
			ID:      "msg_" + randomHex(12),
			Type:    "message",
			Role:    roleAssistant,
			Content: []anthropicBlock{},
			Usage:   wireUsage{},
		},
	})
}

// WriteChunk encodes and flushes one content.Chunk as native stream event(s):
// opening/closing content_block_start/stop events as the active block kind (or,
// for tool_use, the active neutral Index) changes, and a content_block_delta for
// the chunk's own payload.
func (e *serverStreamEncoder) WriteChunk(chunk content.Chunk) error {
	if e.done {
		return &StreamTerminatedError{}
	}

	switch c := chunk.(type) {
	case *content.TextChunk:
		if err := e.ensureBlock(blockKindText, 0, "", ""); err != nil {
			return err
		}
		if c.Text == "" {
			return nil
		}
		return e.writeEvent(eventContentBlockDelta, sseContentBlockDelta{
			Type:  eventContentBlockDelta,
			Index: e.openWireIndex,
			Delta: sseDelta{Type: deltaText, Text: c.Text},
		})
	case *content.ThinkingChunk:
		if err := e.ensureBlock(blockKindThinking, 0, "", ""); err != nil {
			return err
		}
		if c.Thinking == "" {
			return nil
		}
		return e.writeEvent(eventContentBlockDelta, sseContentBlockDelta{
			Type:  eventContentBlockDelta,
			Index: e.openWireIndex,
			Delta: sseDelta{Type: deltaThinking, Thinking: c.Thinking},
		})
	case *content.ToolUseChunk:
		if err := e.ensureBlock(blockKindToolUse, c.Index, c.ID, c.Name); err != nil {
			return err
		}
		if c.InputJSON == "" {
			return nil
		}
		return e.writeEvent(eventContentBlockDelta, sseContentBlockDelta{
			Type:  eventContentBlockDelta,
			Index: e.openWireIndex,
			Delta: sseDelta{Type: deltaInputJSON, PartialJSON: c.InputJSON},
		})
	default:
		return &UnsupportedChunkError{Chunk: unsupportedChunkTypeName(chunk)}
	}
}

// ensureBlock makes kind (identified, for tool_use, by the neutral index) the
// active open block, closing whatever was open first if it differs. Anthropic's
// wire protocol never interleaves content blocks — only one is ever open at a
// time — so a target that genuinely interleaves multiple tool calls' deltas is
// re-serialized as a sequence of (possibly repeated) single-block start/stop
// pairs rather than true interleaving; this stays wire-valid at the cost of
// being chattier than necessary for that uncommon case.
func (e *serverStreamEncoder) ensureBlock(kind blockKind, toolIndex int, toolID, toolName string) error {
	if e.openKind == kind && (kind != blockKindToolUse || e.openToolIndex == toolIndex) {
		return nil
	}
	if err := e.closeOpenBlock(); err != nil {
		return err
	}

	wireIndex := e.nextWireIndex
	e.nextWireIndex++

	var block anthropicBlock
	switch kind {
	case blockKindText:
		block = anthropicBlock{Type: blockTypeText, Text: ""}
	case blockKindThinking:
		block = anthropicBlock{Type: blockTypeThinking, Thinking: ""}
	case blockKindToolUse:
		id := toolID
		if id == "" {
			id = e.ids()
		}
		block = anthropicBlock{Type: blockTypeToolUse, ID: id, Name: toolName, Input: json.RawMessage(emptyObject)}
	}

	if err := e.writeEvent(eventContentBlockStart, sseContentBlockStart{
		Type:         eventContentBlockStart,
		Index:        wireIndex,
		ContentBlock: block,
	}); err != nil {
		return err
	}

	e.openKind = kind
	e.openWireIndex = wireIndex
	e.openToolIndex = toolIndex
	return nil
}

func (e *serverStreamEncoder) closeOpenBlock() error {
	if e.openKind == blockKindNone {
		return nil
	}
	index := e.openWireIndex
	e.openKind = blockKindNone
	return e.writeEvent(eventContentBlockStop, sseContentBlockStop{Type: eventContentBlockStop, Index: index})
}

// Finish encodes the native stream-completion events — closing any still-open
// content block, then message_delta (carrying the authoritative terminal
// stop_reason and cumulative output usage) and message_stop — from result.
func (e *serverStreamEncoder) Finish(result stream.StreamResult) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	if err := e.closeOpenBlock(); err != nil {
		return err
	}

	stopReason := encodeFinishReason(result.FinishReason)
	var outputTokens uint64
	if result.Usage != nil {
		outputTokens = uint64(result.Usage.OutputTokens)
	}
	if err := e.writeEvent(eventMessageDelta, sseMessageDelta{
		Type:  eventMessageDelta,
		Delta: sseMessageDeltaInfo{StopReason: &stopReason},
		Usage: sseMessageDeltaUsage{OutputTokens: outputTokens},
	}); err != nil {
		return err
	}

	return e.writeEvent(eventMessageStop, sseMessageStop{Type: eventMessageStop})
}

// Fail encodes a native Anthropic `error` SSE event — the post-header
// counterpart to WriteError's pre-header error envelope, using the same
// classification and the same anthropicError wire shape — and terminates the
// stream. It does not attempt to close any still-open content block first: an
// in-stream failure is abrupt, not a clean completion.
func (e *serverStreamEncoder) Fail(err error) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	_, wireType, message := classifyError(err)
	return e.writeEvent(responseTypeError, sseErrorEvent{
		Type:  responseTypeError,
		Error: anthropicError{Type: wireType, Message: message},
	})
}

// writeEvent marshals payload and writes it as one SSE frame
// ("event: <name>\ndata: <json>\n\n"), flushing immediately so streaming
// progressively delivers bytes rather than buffering until the handler returns.
// wire/sse (this module) only frames the read side; Anthropic's write-side SSE
// framing is small enough (two lines plus a blank line) that it is not worth a
// new shared package, so it lives here, local to this dialect.
func (e *serverStreamEncoder) writeEvent(name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := e.w.Write([]byte("event: " + name + "\ndata: " + string(body) + "\n\n")); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
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
//
// These parallel the decode-direction streamEvent/streamBlock/streamDelta
// (types.go) but are separate types for the same reason wireMessageResponse is
// separate from messageResponse: the decode-side messageUsage embeds
// usagenorm.Count, which cannot marshal a real value.

type sseMessageStart struct {
	Type    string          `json:"type"`
	Message sseStartMessage `json:"message"`
}

type sseStartMessage struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
	Usage   wireUsage        `json:"usage"`
}

type sseContentBlockStart struct {
	Type         string         `json:"type"`
	Index        int            `json:"index"`
	ContentBlock anthropicBlock `json:"content_block"`
}

type sseContentBlockDelta struct {
	Type  string   `json:"type"`
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type sseContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type sseMessageDelta struct {
	Type  string               `json:"type"`
	Delta sseMessageDeltaInfo  `json:"delta"`
	Usage sseMessageDeltaUsage `json:"usage"`
}

type sseMessageDeltaInfo struct {
	StopReason *string `json:"stop_reason"`
}

type sseMessageDeltaUsage struct {
	OutputTokens uint64 `json:"output_tokens"`
}

type sseMessageStop struct {
	Type string `json:"type"`
}

type sseErrorEvent struct {
	Type  string         `json:"type"`
	Error anthropicError `json:"error"`
}
