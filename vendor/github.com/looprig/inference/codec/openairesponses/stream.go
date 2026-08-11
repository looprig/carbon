package openairesponses

import (
	"encoding/json"
	"net/http"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/inference/wire/sse"
)

// Compile-time proof that Codec is a full codec.StreamingCodec.
var _ codec.StreamingCodec = Codec{}

// DecodeStream frames a successful Responses streaming response with
// wire/sse and maps each frame through the codec's per-event decode logic.
// Unlike Chat Completions there is no explicit [DONE] sentinel: the stream
// ends when response.completed's terminal metadata has been observed and the
// body reaches natural EOF, matching how anthropicapi relies on
// message_stop plus EOF rather than a sentinel. It owns resp.Body: the
// returned reader's Close closes it.
func (Codec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	frames, err := sse.DecodeStreamFrames(resp.Body)
	if err != nil {
		return nil, err
	}
	collector := &streamResultCollector{}
	return stream.FramesToChunksWithResult(frames, collector.mapFrame, collector.result), nil
}

// DecodeEvent decodes one already-de-framed SSE event payload into the
// chunk(s) it yields. It is stateless and tolerant by contract: malformed
// JSON and every uninteresting or unknown event type return (nil, nil) — a
// skip, not an error. Cross-event assembly (concatenating a tool call's
// argument fragments, or a reasoning summary's text fragments) happens
// downstream in the stream accumulator, not here.
func (Codec) DecodeEvent(event []byte) ([]content.Chunk, error) {
	return decodeEvent(event)
}

// sseEnvelope is the union view of one de-framed Responses SSE event this
// codec cares about. Content-delta fields feed decodeEvent; Response feeds
// the stream result collector (model, usage, finish reason) without entering
// the content chunk vocabulary.
type sseEnvelope struct {
	Type        string        `json:"type"`
	OutputIndex int           `json:"output_index"`
	ItemID      string        `json:"item_id"`
	Delta       string        `json:"delta"`
	Item        *wireItem     `json:"item"`
	Response    *wireResponse `json:"response"`
}

// decodeEvent maps one Responses SSE event to the chunk(s) it yields, per the
// design's streaming section:
//   - response.output_item.added(function_call) -> one ToolUseChunk carrying
//     Index/ID/Name (the fragment that seeds the accumulator with the tool
//     call id + name), analogous to Anthropic's content_block_start(tool_use).
//   - response.output_text.delta       -> one TextChunk.
//   - response.function_call_arguments.delta -> one ToolUseChunk arg
//     fragment (Index + InputJSON), emitted verbatim (even when empty, like
//     Anthropic's input_json_delta) for the accumulator to concatenate.
//   - response.reasoning_summary_text.delta  -> one ThinkingChunk.
//   - everything else                  -> (nil, nil), a tolerant skip.
//
// output_index maps directly to content.Chunk's Index field: it is a stable,
// response-scoped per-item counter, exactly what parallel-tool-call indexing
// needs.
func decodeEvent(payload []byte) ([]content.Chunk, error) {
	var ev sseEnvelope
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // skip malformed events
	}

	switch ev.Type {
	case eventOutputItemAdded:
		if ev.Item != nil && ev.Item.Type == itemTypeFunctionCall {
			return []content.Chunk{&content.ToolUseChunk{
				Index: ev.OutputIndex,
				ID:    ev.Item.CallID,
				Name:  ev.Item.Name,
			}}, nil
		}
		return nil, nil
	case eventOutputTextDelta:
		if ev.Delta == "" {
			return nil, nil
		}
		return []content.Chunk{&content.TextChunk{Text: ev.Delta}}, nil
	case eventFunctionCallArgsDelta:
		return []content.Chunk{&content.ToolUseChunk{Index: ev.OutputIndex, InputJSON: ev.Delta}}, nil
	case eventReasoningSummaryDelta:
		if ev.Delta == "" {
			return nil, nil
		}
		return []content.Chunk{&content.ThinkingChunk{Thinking: ev.Delta}}, nil
	default:
		// response.created, response.content_part.added/done,
		// response.output_item.done, response.completed,
		// response.function_call_arguments.done,
		// response.reasoning_summary_text.done, and any unknown event type:
		// no chunk (response.failed is handled by the collector, not here).
		return nil, nil
	}
}

// streamResultCollector accumulates terminal stream metadata (model, usage,
// finish reason) from response.completed, and surfaces response.failed as a
// hard stream error — mirroring anthropicapi's streamResultCollector.
type streamResultCollector struct {
	completedSeen bool
	resultValue   stream.StreamResult
}

func (c *streamResultCollector) mapFrame(frame stream.StreamFrame) ([]content.Chunk, error) {
	var ev sseEnvelope
	if err := json.Unmarshal(frame.Data, &ev); err == nil {
		if err := c.collect(ev); err != nil {
			return nil, err
		}
	}
	return decodeEvent(frame.Data)
}

func (c *streamResultCollector) collect(ev sseEnvelope) error {
	switch ev.Type {
	case eventResponseFailed:
		streamErr := &StreamAPIError{}
		if ev.Response != nil && ev.Response.Error != nil {
			streamErr.Code = ev.Response.Error.Code
			streamErr.Message = ev.Response.Error.Message
		}
		return streamErr
	case eventResponseCompleted:
		c.completedSeen = true
		if ev.Response != nil {
			c.resultValue.Model = ev.Response.Model
			c.resultValue.FinishReason = deriveFinishReason(*ev.Response)
			u, err := normalizeUsage(ev.Response.Usage)
			if err != nil {
				return err
			}
			c.resultValue.Usage = u
		}
	}
	return nil
}

func (c *streamResultCollector) result() (stream.StreamResult, bool, error) {
	if !c.completedSeen {
		return stream.StreamResult{}, false, nil
	}
	return c.resultValue, true, nil
}
