package openairesponses

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
)

// itemKind identifies which native output item variant is currently open on
// the wire, so WriteChunk knows whether an incoming chunk continues the open
// item or must close it and start a new one.
type itemKind uint8

const (
	itemKindNone itemKind = iota
	itemKindText
	itemKindThinking
	itemKindTool
)

// openResponsesStream begins the native Responses streaming response: it
// commits the text/event-stream headers and the 200 status, then emits the
// leading response.created event before returning the request-scoped
// encoder. The response id is generated here (not per the design's
// abbreviated example ids) since OpenStream receives no request/target
// context to derive one from.
func openResponsesStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	enc := &serverStreamEncoder{w: w, flusher: flusher, ids: newToolIDGenerator(), responseID: "resp_" + randomHex(12)}
	if err := enc.writeResponseCreated(); err != nil {
		return nil, err
	}
	return enc, nil
}

// serverStreamEncoder is the request-scoped codec.StreamEncoder for one
// in-flight native Responses stream. It is never shared across requests: it
// holds per-stream state (the open item bookkeeping, accumulated text for
// the eventual *.done/output_item.done payloads, and the id generator) that
// OpenStream fills in fresh each call.
type serverStreamEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ids     func() string

	responseID string
	done       bool

	nextOutputIndex int
	closedItems     []wireItem

	openKind        itemKind
	openOutputIndex int
	openItemID      string
	openToolIndex   int // valid only when openKind == itemKindTool
	openToolCallID  string
	openToolName    string

	textAccum     strings.Builder
	thinkingAccum strings.Builder
	toolArgsAccum strings.Builder
}

var _ codec.StreamEncoder = (*serverStreamEncoder)(nil)

func (e *serverStreamEncoder) writeResponseCreated() error {
	return e.writeEvent(eventResponseCreated, sseResponseEnvelope{
		Type:     eventResponseCreated,
		Response: encodeWireResponse{ID: e.responseID, Status: statusInProgress, Output: []wireItem{}},
	})
}

// WriteChunk encodes and flushes one content.Chunk as native stream event(s):
// opening/closing output_item (and, for text, content_part) events as the
// active item kind (or, for tool calls, the active neutral Index) changes,
// and the appropriate delta event for the chunk's own payload.
func (e *serverStreamEncoder) WriteChunk(chunk content.Chunk) error {
	if e.done {
		return &StreamTerminatedError{}
	}

	switch c := chunk.(type) {
	case *content.TextChunk:
		if err := e.ensureItem(itemKindText, 0, "", ""); err != nil {
			return err
		}
		if c.Text == "" {
			return nil
		}
		e.textAccum.WriteString(c.Text)
		return e.writeEvent(eventOutputTextDelta, sseOutputTextDelta{
			Type: eventOutputTextDelta, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, ContentIndex: 0, Delta: c.Text,
		})
	case *content.ThinkingChunk:
		if err := e.ensureItem(itemKindThinking, 0, "", ""); err != nil {
			return err
		}
		if c.Thinking == "" {
			return nil
		}
		e.thinkingAccum.WriteString(c.Thinking)
		return e.writeEvent(eventReasoningSummaryDelta, sseReasoningSummaryDelta{
			Type: eventReasoningSummaryDelta, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, SummaryIndex: 0, Delta: c.Thinking,
		})
	case *content.ToolUseChunk:
		if err := e.ensureItem(itemKindTool, c.Index, c.ID, c.Name); err != nil {
			return err
		}
		if c.InputJSON == "" {
			return nil
		}
		e.toolArgsAccum.WriteString(c.InputJSON)
		return e.writeEvent(eventFunctionCallArgsDelta, sseFunctionCallArgsDelta{
			Type: eventFunctionCallArgsDelta, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, Delta: c.InputJSON,
		})
	default:
		return &UnsupportedChunkError{Chunk: unsupportedChunkTypeName(chunk)}
	}
}

// ensureItem makes kind (identified, for a tool call, by the neutral index)
// the active open item, closing whatever was open first if it differs.
// Responses' wire protocol only ever streams deltas for one open item's
// content at a time in a single-branch response; a target that genuinely
// interleaves multiple tool calls' deltas is re-serialized as a sequence of
// (possibly repeated) single-item add/done pairs rather than true
// interleaving, matching anthropicapi's identical trade-off for
// content_block_start/stop.
func (e *serverStreamEncoder) ensureItem(kind itemKind, toolIndex int, toolID, toolName string) error {
	if e.openKind == kind && (kind != itemKindTool || e.openToolIndex == toolIndex) {
		return nil
	}
	if err := e.closeOpenItem(); err != nil {
		return err
	}

	outputIndex := e.nextOutputIndex
	e.nextOutputIndex++
	itemID := e.ids()

	e.openKind = kind
	e.openOutputIndex = outputIndex
	e.openItemID = itemID
	e.openToolIndex = toolIndex
	e.textAccum.Reset()
	e.thinkingAccum.Reset()
	e.toolArgsAccum.Reset()

	switch kind {
	case itemKindText:
		item := wireItem{Type: itemTypeMessage, ID: itemID, Role: roleAssistant, Content: []wireContentPart{}}
		if err := e.writeEvent(eventOutputItemAdded, sseOutputItemAdded{Type: eventOutputItemAdded, OutputIndex: outputIndex, Item: item}); err != nil {
			return err
		}
		part := wireContentPart{Type: contentTypeOutputText, Text: "", Annotations: []json.RawMessage{}}
		return e.writeEvent(eventContentPartAdded, sseContentPartAdded{
			Type: eventContentPartAdded, ItemID: itemID, OutputIndex: outputIndex, ContentIndex: 0, Part: part,
		})
	case itemKindThinking:
		item := wireItem{Type: itemTypeReasoning, ID: itemID, Summary: []wireSummaryPart{}}
		return e.writeEvent(eventOutputItemAdded, sseOutputItemAdded{Type: eventOutputItemAdded, OutputIndex: outputIndex, Item: item})
	case itemKindTool:
		id := toolID
		if id == "" {
			id = e.ids()
		}
		e.openToolCallID = id
		e.openToolName = toolName
		item := wireItem{Type: itemTypeFunctionCall, ID: itemID, CallID: id, Name: toolName, Arguments: ""}
		return e.writeEvent(eventOutputItemAdded, sseOutputItemAdded{Type: eventOutputItemAdded, OutputIndex: outputIndex, Item: item})
	}
	return nil
}

// closeOpenItem emits the terminal *.done event(s) for the currently open
// item (if any) using its accumulated payload, records the finished item for
// the eventual response.completed snapshot, and clears the open-item state.
func (e *serverStreamEncoder) closeOpenItem() error {
	switch e.openKind {
	case itemKindNone:
		return nil
	case itemKindText:
		text := e.textAccum.String()
		part := wireContentPart{Type: contentTypeOutputText, Text: text, Annotations: []json.RawMessage{}}
		if err := e.writeEvent(eventContentPartDone, sseContentPartDone{
			Type: eventContentPartDone, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, ContentIndex: 0, Part: part,
		}); err != nil {
			return err
		}
		item := wireItem{Type: itemTypeMessage, ID: e.openItemID, Role: roleAssistant, Content: []wireContentPart{part}}
		if err := e.writeEvent(eventOutputItemDone, sseOutputItemDone{Type: eventOutputItemDone, OutputIndex: e.openOutputIndex, Item: item}); err != nil {
			return err
		}
		e.closedItems = append(e.closedItems, item)
	case itemKindThinking:
		text := e.thinkingAccum.String()
		if err := e.writeEvent(eventReasoningSummaryDone, sseReasoningSummaryDone{
			Type: eventReasoningSummaryDone, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, SummaryIndex: 0, Text: text,
		}); err != nil {
			return err
		}
		item := wireItem{Type: itemTypeReasoning, ID: e.openItemID, Summary: []wireSummaryPart{{Type: summaryTypeText, Text: text}}}
		if err := e.writeEvent(eventOutputItemDone, sseOutputItemDone{Type: eventOutputItemDone, OutputIndex: e.openOutputIndex, Item: item}); err != nil {
			return err
		}
		e.closedItems = append(e.closedItems, item)
	case itemKindTool:
		args := e.toolArgsAccum.String()
		if args == "" {
			args = emptyObject
		}
		if err := e.writeEvent(eventFunctionCallArgsDone, sseFunctionCallArgsDone{
			Type: eventFunctionCallArgsDone, ItemID: e.openItemID, OutputIndex: e.openOutputIndex, Arguments: args,
		}); err != nil {
			return err
		}
		item := wireItem{Type: itemTypeFunctionCall, ID: e.openItemID, CallID: e.openToolCallID, Name: e.openToolName, Arguments: args}
		if err := e.writeEvent(eventOutputItemDone, sseOutputItemDone{Type: eventOutputItemDone, OutputIndex: e.openOutputIndex, Item: item}); err != nil {
			return err
		}
		e.closedItems = append(e.closedItems, item)
	}
	e.openKind = itemKindNone
	return nil
}

// Finish encodes the native stream-completion event — closing any still-open
// item, then response.completed carrying the authoritative terminal status,
// model, usage, and the full accumulated output array — from result.
func (e *serverStreamEncoder) Finish(result stream.StreamResult) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	if err := e.closeOpenItem(); err != nil {
		return err
	}

	status, incomplete := statusForFinishReason(result.FinishReason)
	resp := encodeWireResponse{
		ID:                e.responseID,
		Status:            status,
		Model:             result.Model,
		Output:            e.closedItems,
		Usage:             encodeUsage(result.Usage),
		IncompleteDetails: incomplete,
	}
	return e.writeEvent(eventResponseCompleted, sseResponseEnvelope{Type: eventResponseCompleted, Response: resp})
}

// Fail encodes a native Responses `response.failed` event — the post-header
// counterpart to WriteError's pre-header error envelope, using the same
// classification — and terminates the stream. It does not attempt to close
// any still-open item first: an in-stream failure is abrupt, not a clean
// completion.
func (e *serverStreamEncoder) Fail(err error) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	_, code, message := classifyError(err)
	resp := encodeWireResponse{ID: e.responseID, Status: statusFailed, Error: &wireResponseError{Code: code, Message: message}}
	return e.writeEvent(eventResponseFailed, sseResponseEnvelope{Type: eventResponseFailed, Response: resp})
}

// writeEvent marshals payload and writes it as one SSE frame
// ("event: <name>\ndata: <json>\n\n"), flushing immediately so streaming
// progressively delivers bytes rather than buffering until the handler
// returns. wire/sse (this module) only frames the read side; this dialect's
// write-side SSE framing is small enough that it is not worth a shared
// package, so it lives here, local to this dialect — matching
// anthropicapi's identical local writeEvent.
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

type sseResponseEnvelope struct {
	Type     string             `json:"type"`
	Response encodeWireResponse `json:"response"`
}

type sseOutputItemAdded struct {
	Type        string   `json:"type"`
	OutputIndex int      `json:"output_index"`
	Item        wireItem `json:"item"`
}

type sseOutputItemDone struct {
	Type        string   `json:"type"`
	OutputIndex int      `json:"output_index"`
	Item        wireItem `json:"item"`
}

type sseContentPartAdded struct {
	Type         string          `json:"type"`
	ItemID       string          `json:"item_id"`
	OutputIndex  int             `json:"output_index"`
	ContentIndex int             `json:"content_index"`
	Part         wireContentPart `json:"part"`
}

type sseContentPartDone struct {
	Type         string          `json:"type"`
	ItemID       string          `json:"item_id"`
	OutputIndex  int             `json:"output_index"`
	ContentIndex int             `json:"content_index"`
	Part         wireContentPart `json:"part"`
}

type sseOutputTextDelta struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type sseFunctionCallArgsDelta struct {
	Type        string `json:"type"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
}

type sseFunctionCallArgsDone struct {
	Type        string `json:"type"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Arguments   string `json:"arguments"`
}

type sseReasoningSummaryDelta struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Delta        string `json:"delta"`
}

type sseReasoningSummaryDone struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	SummaryIndex int    `json:"summary_index"`
	Text         string `json:"text"`
}
