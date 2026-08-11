package geminiapi

import (
	"encoding/json"
	"net/http"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
)

// openGenerateContentStream begins the native streamGenerateContent
// streaming response: it commits the text/event-stream headers and the 200
// status, then returns the request-scoped encoder. Gemini's SSE convention
// has no leading "stream started" event and no [DONE]-style terminal
// sentinel (see stream.go's client-decode-direction doc comment: "Gemini has
// no terminal payload sentinel: the stream ends on the body's natural EOF"),
// so Finish's only job (beyond the terminal candidate itself) is to close —
// there is no special terminal frame to write.
func openGenerateContentStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	return &serverStreamEncoder{w: w, flusher: flusher}, nil
}

// serverStreamEncoder is the request-scoped codec.StreamEncoder for one
// in-flight native streamGenerateContent stream. It is never shared across
// requests. Unlike the other three dialects' server stream encoders, it
// holds no open-block/open-item bookkeeping: each content.Chunk maps
// directly to one partial-GenerateContentResponse SSE frame, since Gemini's
// wire convention has no block-start/stop or item-added/done events to
// track — every frame IS the (partial) response shape.
type serverStreamEncoder struct {
	w       http.ResponseWriter
	flusher http.Flusher
	done    bool
}

var _ codec.StreamEncoder = (*serverStreamEncoder)(nil)

// WriteChunk encodes and flushes one content.Chunk as one `data: {...}\n\n`
// SSE frame carrying a partial GenerateContentResponse with a single part.
// Per the client-decode direction's own doc comment (codec.go's decodeEvent),
// Gemini streams a COMPLETE functionCall (full name + args) per part, not
// argument fragments the way Chat Completions/Anthropic do — so a
// ToolUseChunk's InputJSON is written as the part's whole `args` object, not
// accumulated across chunks.
func (e *serverStreamEncoder) WriteChunk(chunk content.Chunk) error {
	if e.done {
		return &StreamTerminatedError{}
	}

	var part geminiPart
	switch c := chunk.(type) {
	case *content.TextChunk:
		if c.Text == "" {
			return nil
		}
		part = geminiPart{Text: c.Text}
	case *content.ThinkingChunk:
		if c.Thinking == "" {
			return nil
		}
		part = geminiPart{Thought: true, Text: c.Thinking}
	case *content.ToolUseChunk:
		if c.ID == "" && c.Name == "" && c.InputJSON == "" {
			return nil
		}
		part = geminiPart{FunctionCall: &functionCall{ID: c.ID, Name: c.Name, Args: toolCallArgs(c.InputJSON)}}
	default:
		return &UnsupportedChunkError{Chunk: unsupportedChunkTypeName(chunk)}
	}

	return e.writeEvent(encodeGenerateContentResponse{
		Candidates: []encodeCandidate{{Content: geminiContent{Role: roleModel, Parts: []geminiPart{part}}, Index: 0}},
	})
}

// toolCallArgs normalizes a streamed functionCall's argument payload for the
// wire: an empty payload becomes an empty JSON object so functionCall.args
// is always a valid Struct, mirroring argsJSON (encode.go).
func toolCallArgs(raw string) json.RawMessage {
	if raw == "" {
		return json.RawMessage(emptyObject)
	}
	return json.RawMessage(raw)
}

// Finish encodes the terminal candidate — carrying the authoritative
// finishReason, model, and usage metadata from result — as one final SSE
// frame, then closes. There is no separate terminal sentinel to write.
func (e *serverStreamEncoder) Finish(result stream.StreamResult) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	return e.writeEvent(encodeGenerateContentResponse{
		Candidates: []encodeCandidate{{
			Content:      geminiContent{Role: roleModel, Parts: []geminiPart{}},
			FinishReason: encodeFinishReason(result.FinishReason),
			Index:        0,
		}},
		UsageMetadata: buildUsageMetadata(result.Usage),
		ModelVersion:  result.Model,
	})
}

// Fail encodes a terminal error-shaped SSE record — the post-header
// counterpart to WriteError's pre-header error envelope, using the same
// classification and the same geminiErrorEnvelope wire shape — and closes
// the stream. This matches the design's "Gemini emits a terminal
// error-shaped SSE record" clause: unlike Chat Completions there is no
// [DONE]-style sentinel to also write, so Fail's only output is this one
// frame.
func (e *serverStreamEncoder) Fail(err error) error {
	if e.done {
		return &StreamTerminatedError{}
	}
	e.done = true

	status, wireStatus, message := classifyGeminiError(err)
	return e.writeEvent(geminiErrorEnvelope{Error: geminiErrorBody{Code: status, Message: message, Status: wireStatus}})
}

// writeEvent marshals payload and writes it as one SSE data-only frame
// ("data: <json>\n\n" — Gemini's real SSE convention uses unnamed data-only
// events, per the design doc's own description of its streaming route),
// flushing immediately so streaming progressively delivers bytes rather than
// buffering until the handler returns.
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
