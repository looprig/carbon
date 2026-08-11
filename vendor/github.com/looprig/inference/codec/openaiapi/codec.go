package openaiapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/wire/jsonbody"
)

// Codec is the OpenAI Chat Completions wire dialect expressed as an codec.Codec
// (and, via DecodeStream, an codec.StreamingCodec). It is stateless (an empty
// struct with value-receiver methods), so one value is safely shared across
// goroutines: the transport owns HTTP mechanics, the Codec owns the JSON body,
// per-event semantics, and SSE stream decoding. The methods delegate to package-level
// free functions kept for provider extensions and the existing tests, so the two
// surfaces cannot diverge.
type Codec struct{}

// Compile-time proof that Codec honors the codec.Codec contract.
var _ codec.Codec = Codec{}

// Compile-time proof that Codec also honors the ingress-side codec.ServerCodec
// contract: recognizing and decoding native POST /v1/chat/completions
// requests, and encoding results back into native Chat Completions
// responses/streams.
var _ codec.ServerCodec = Codec{}

// MatchRequest reports whether req is a POST /v1/chat/completions request.
func (Codec) MatchRequest(req *http.Request) bool {
	return matchChatCompletionsRequest(req)
}

// DecodeRequest decodes a matched POST /v1/chat/completions request into a
// codec.DecodedRequest, delegating to the free decodeChatCompletionsRequest.
func (Codec) DecodeRequest(req *http.Request) (codec.DecodedRequest, error) {
	return decodeChatCompletionsRequest(req)
}

// WriteResponse encodes a complete inference.Response as the native Chat
// Completions non-streaming response, delegating to the free
// writeChatResponse.
func (Codec) WriteResponse(w http.ResponseWriter, resp *inference.Response) error {
	return writeChatResponse(w, resp)
}

// OpenStream begins the native Chat Completions streaming response and
// returns its request-scoped StreamEncoder, delegating to the free
// openChatStream.
func (Codec) OpenStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	return openChatStream(w)
}

// WriteError encodes err as the native Chat Completions error envelope,
// delegating to the free writeChatError.
func (Codec) WriteError(w http.ResponseWriter, err error) {
	writeChatError(w, err)
}

// EncodeRequest builds the OpenAI chat completions request: a JSON body reader plus the
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

// DecodeResponse parses a non-streaming OpenAI chat completions body, delegating
// to the free DecodeResponse.
func (Codec) DecodeResponse(body []byte) (*inference.Response, error) {
	return DecodeResponse(body)
}

// DecodeEvent decodes one already-de-framed SSE data payload into the chunk(s) it
// yields. It is tolerant by contract (matching NewStream): malformed JSON, an
// event with no choices, and role-only/empty deltas return (nil, nil) — a skip,
// not an error — while a single delta line carrying multiple tool-call entries
// returns all of them. DecodeEvent is stateless: cross-event tool-argument
// assembly happens downstream in the stream accumulator, not here.
func (Codec) DecodeEvent(event []byte) ([]content.Chunk, error) {
	return decodeEvent(event)
}

// decodeEvent is the single per-event decoder shared by Codec.DecodeEvent and
// NewStream. NewStream drives it one SSE line at a time (buffering the
// multi-tool-call case and draining one chunk per Next()); Codec.DecodeEvent
// hands it a de-framed payload directly. Precedence — reasoning, then text, then
// tool calls — is preserved from the original NewStream loop, so at most one kind
// of chunk is produced per event.
func decodeEvent(payload []byte) ([]content.Chunk, error) {
	var ev sseChunk
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // skip malformed lines
	}
	if len(ev.Choices) == 0 {
		return nil, nil
	}
	delta := ev.Choices[0].Delta

	if delta.ReasoningContent != "" {
		return []content.Chunk{&content.ThinkingChunk{Thinking: delta.ReasoningContent}}, nil
	}
	if delta.Content != "" {
		return []content.Chunk{&content.TextChunk{Text: delta.Content}}, nil
	}
	if len(delta.ToolCalls) > 0 {
		var out []content.Chunk
		for _, tc := range delta.ToolCalls {
			// Drop wholly-empty entries (no id, name, or argument fragment).
			if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
				continue
			}
			out = append(out, &content.ToolUseChunk{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				InputJSON: tc.Function.Arguments,
			})
		}
		return out, nil
	}
	// Empty delta (role-only or finish): no chunk.
	return nil, nil
}
