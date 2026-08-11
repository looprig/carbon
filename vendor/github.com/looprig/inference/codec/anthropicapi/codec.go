package anthropicapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/wire/jsonbody"
)

// Codec is the Anthropic Messages API wire dialect expressed as an codec.Codec
// (and, via DecodeStream, an codec.StreamingCodec). It is stateless (an empty
// struct with value-receiver methods), so one value is safely shared across
// goroutines: the transport owns HTTP mechanics, the Codec owns the JSON body,
// per-event semantics, and SSE stream decoding. The methods delegate to package-level
// free functions so the method surface and the free surface cannot diverge.
type Codec struct{}

// Compile-time proof that Codec honors the codec.Codec contract.
var _ codec.Codec = Codec{}

// Compile-time proof that Codec also honors the ingress-side codec.ServerCodec
// contract: recognizing and decoding native /v1/messages requests, and encoding
// results back into native Anthropic responses/streams.
var _ codec.ServerCodec = Codec{}

// MatchRequest reports whether req is a POST /v1/messages request.
func (Codec) MatchRequest(req *http.Request) bool {
	return matchMessagesRequest(req)
}

// DecodeRequest decodes a matched POST /v1/messages request into a
// codec.DecodedRequest, delegating to the free decodeMessagesRequest.
func (Codec) DecodeRequest(req *http.Request) (codec.DecodedRequest, error) {
	return decodeMessagesRequest(req)
}

// WriteResponse encodes a complete inference.Response as the native Anthropic
// Messages API non-streaming response, delegating to the free
// writeMessageResponse.
func (Codec) WriteResponse(w http.ResponseWriter, resp *inference.Response) error {
	return writeMessageResponse(w, resp)
}

// OpenStream begins the native Anthropic Messages streaming response and
// returns its request-scoped StreamEncoder, delegating to the free
// openMessagesStream.
func (Codec) OpenStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	return openMessagesStream(w)
}

// WriteError encodes err as the native Anthropic error envelope, delegating to
// the free writeMessageError.
func (Codec) WriteError(w http.ResponseWriter, err error) {
	writeMessageError(w, err)
}

// EncodeRequest builds the Anthropic Messages request: a JSON body reader plus the
// application/json content type as an EncodedRequest. RequestModeStream sets
// "stream":true in the body, every other mode omits it.
func (Codec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	body, err := EncodeRequest(req, mode == codec.RequestModeStream)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	h := http.Header{}
	h.Set("Content-Type", jsonbody.ContentType)
	return codec.EncodedRequest{Header: h, Body: bytes.NewReader(body)}, nil
}

// DecodeResponse parses a non-streaming Anthropic Messages response body,
// delegating to the free DecodeResponse.
func (Codec) DecodeResponse(body []byte) (*inference.Response, error) {
	return DecodeResponse(body)
}

// DecodeEvent decodes one already-de-framed SSE event payload into the chunk(s)
// it yields. It is stateless and tolerant by contract: malformed JSON and every
// uninteresting or unknown event (message_start, content_block_stop,
// message_delta, message_stop, ping, signature_delta, …) return (nil, nil) — a
// skip, not an error. Cross-event assembly (concatenating a tool call's start +
// input_json_delta fragments into a ToolUseBlock) happens downstream in the
// stream accumulator, not here.
func (Codec) DecodeEvent(event []byte) ([]content.Chunk, error) {
	return decodeEvent(event)
}

// decodeEvent is the single per-event decoder behind Codec.DecodeEvent. The
// mapping, per de-framed Anthropic SSE event:
//   - content_block_start(tool_use)  → one ToolUseChunk carrying Index/ID/Name
//     (the fragment that seeds the accumulator with the tool id + name).
//   - content_block_delta(text_delta)       → one TextChunk.
//   - content_block_delta(thinking_delta)   → one ThinkingChunk.
//   - content_block_delta(input_json_delta) → one ToolUseChunk arg fragment
//     (Index + InputJSON, emitted verbatim for the accumulator to concatenate).
//   - everything else                       → (nil, nil), a tolerant skip.
func decodeEvent(payload []byte) ([]content.Chunk, error) {
	var ev streamEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // skip malformed events
	}

	switch ev.Type {
	case eventContentBlockStart:
		if ev.ContentBlock != nil && ev.ContentBlock.Type == blockTypeToolUse {
			return []content.Chunk{&content.ToolUseChunk{
				Index: ev.Index,
				ID:    ev.ContentBlock.ID,
				Name:  ev.ContentBlock.Name,
			}}, nil
		}
		// A text/thinking block start carries no content yet; deltas follow.
		return nil, nil

	case eventContentBlockDelta:
		return decodeDelta(ev)

	default:
		// message_start, content_block_stop, message_delta, message_stop, ping,
		// and any unknown event type: no chunk.
		return nil, nil
	}
}

// decodeDelta maps a content_block_delta event to its chunk. Empty text and
// thinking deltas are skipped (they would fold into a spurious empty block);
// an input_json_delta fragment is emitted verbatim, carrying the block Index so
// the accumulator keys it to the right tool call.
func decodeDelta(ev streamEvent) ([]content.Chunk, error) {
	if ev.Delta == nil {
		return nil, nil
	}
	switch ev.Delta.Type {
	case deltaText:
		if ev.Delta.Text == "" {
			return nil, nil
		}
		return []content.Chunk{&content.TextChunk{Text: ev.Delta.Text}}, nil
	case deltaThinking:
		if ev.Delta.Thinking == "" {
			return nil, nil
		}
		return []content.Chunk{&content.ThinkingChunk{Thinking: ev.Delta.Thinking}}, nil
	case deltaInputJSON:
		return []content.Chunk{&content.ToolUseChunk{Index: ev.Index, InputJSON: ev.Delta.PartialJSON}}, nil
	default:
		// signature_delta, citations_delta, etc.: no neutral chunk.
		return nil, nil
	}
}
