package openaiapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/looprig/core/content"
	codec "github.com/looprig/inference/codec"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/inference/wire/sse"
)

// doneSentinel is OpenAI's terminal SSE payload. The wire/sse framer emits it as an
// ordinary frame (it does not interpret sentinels); the codec owns treating it as
// end-of-stream, mapping it to io.EOF.
const doneSentinel = "[DONE]"

// Compile-time proof that Codec is a full codec.StreamingCodec.
var _ codec.StreamingCodec = Codec{}

// DecodeStream frames a successful OpenAI streaming response with wire/sse and maps
// each frame through the codec's per-event decode logic. It owns resp.Body: the
// returned reader's Close closes it (and DecodeStreamFrames closes it if it errors
// before returning a reader).
func (Codec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	frames, err := sse.DecodeStreamFrames(resp.Body)
	if err != nil {
		return nil, err
	}
	collector := &streamResultCollector{}
	return stream.FramesToChunksWithResult(frames, collector.mapFrame, collector.result), nil
}

// NewStream adapts a raw OpenAI SSE body into a chunk stream. Exposed for provider
// extensions and dialect tests that drive a body directly; the transport uses
// DecodeStream. The caller must Close the returned reader when done.
func NewStream(body io.ReadCloser) *stream.StreamReader[content.Chunk] {
	frames, err := sse.DecodeStreamFrames(body)
	if err != nil {
		// Do not discard the framer error (e.g. a nil body): return a reader that surfaces
		// it on first Next rather than dereferencing a nil frames reader and panicking.
		return stream.NewStreamReader(
			func() (content.Chunk, error) { return nil, err },
			func() error { return nil },
		)
	}
	collector := &streamResultCollector{}
	return stream.FramesToChunksWithResult(frames, collector.mapFrame, collector.result)
}

// mapFrame maps one raw SSE frame to chunk(s): the [DONE] sentinel ends the stream
// (io.EOF), everything else runs through the shared per-event decoder.
func mapFrame(f stream.StreamFrame) ([]content.Chunk, error) {
	if string(f.Data) == doneSentinel {
		return nil, io.EOF
	}
	return decodeEvent(f.Data)
}

type streamResultCollector struct {
	resultValue stream.StreamResult
	doneSeen    bool
}

func (c *streamResultCollector) mapFrame(frame stream.StreamFrame) ([]content.Chunk, error) {
	if string(frame.Data) == doneSentinel {
		c.doneSeen = true
		return mapFrame(frame)
	}
	var event sseChunk
	if err := json.Unmarshal(frame.Data, &event); err == nil {
		if err := c.collect(event); err != nil {
			return nil, err
		}
	}
	return mapFrame(frame)
}

func (c *streamResultCollector) collect(event sseChunk) error {
	if event.Model != "" {
		c.resultValue.Model = event.Model
	}
	if len(event.Choices) > 0 && event.Choices[0].FinishReason != "" {
		c.resultValue.FinishReason = mapFinishReason(event.Choices[0].FinishReason)
	}
	if event.Usage == nil {
		return nil
	}
	usage, err := normalizeUsage(event.Usage)
	if err != nil {
		return err
	}
	c.resultValue.Usage = usage
	return nil
}

func (c *streamResultCollector) result() (stream.StreamResult, bool, error) {
	if !c.doneSeen {
		return stream.StreamResult{}, false, nil
	}
	return c.resultValue, true, nil
}

func mapFinishReason(reason string) stream.FinishReason {
	switch reason {
	case "stop":
		return stream.FinishReasonStop
	case "length":
		return stream.FinishReasonLength
	case "tool_calls", "function_call":
		return stream.FinishReasonToolUse
	case "content_filter":
		return stream.FinishReasonContentFilter
	default:
		return stream.FinishReasonUnknown
	}
}
