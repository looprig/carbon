package geminiapi

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

// DecodeStream frames a successful Gemini streamGenerateContent response (served as SSE
// via ?alt=sse) with wire/sse and maps each frame through the codec's per-event decode
// logic. Gemini has no terminal payload sentinel: the stream ends on the body's natural
// EOF. It owns resp.Body: the returned reader's Close closes it.
func (Codec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	frames, err := sse.DecodeStreamFrames(resp.Body)
	if err != nil {
		return nil, err
	}
	collector := &streamResultCollector{}
	return stream.FramesToChunksWithResult(frames, collector.mapFrame, collector.result), nil
}

// mapFrame decodes one raw SSE frame's Data (a partial GenerateContentResponse) via the
// shared per-event decoder.
func mapFrame(f stream.StreamFrame) ([]content.Chunk, error) {
	return decodeEvent(f.Data)
}

type streamResultCollector struct {
	resultValue      stream.StreamResult
	functionCallSeen bool
}

func (c *streamResultCollector) mapFrame(frame stream.StreamFrame) ([]content.Chunk, error) {
	var event GenerateContentResponse
	if err := json.Unmarshal(frame.Data, &event); err == nil {
		if err := c.collect(event); err != nil {
			return nil, err
		}
	}
	return mapFrame(frame)
}

func (c *streamResultCollector) collect(event GenerateContentResponse) error {
	if event.ModelVersion != "" {
		c.resultValue.Model = event.ModelVersion
	}
	if len(event.Candidates) > 0 {
		candidate := event.Candidates[0]
		if hasFunctionCall(candidate.Content.Parts) {
			c.functionCallSeen = true
		}
		if candidate.FinishReason != "" {
			c.resultValue.FinishReason = mapFinishReason(candidate.FinishReason)
		}
		if c.functionCallSeen && (candidate.FinishReason == "" || candidate.FinishReason == "STOP") {
			c.resultValue.FinishReason = stream.FinishReasonToolUse
		}
	}
	if event.UsageMetadata == nil {
		return nil
	}
	usage, err := normalizeUsage(event.UsageMetadata)
	if err != nil {
		return err
	}
	c.resultValue.Usage = usage
	return nil
}

func hasFunctionCall(parts []geminiPart) bool {
	for _, part := range parts {
		if part.FunctionCall != nil {
			return true
		}
	}
	return false
}

func (c *streamResultCollector) result() (stream.StreamResult, bool, error) {
	return c.resultValue, true, nil
}

func mapFinishReason(reason string) stream.FinishReason {
	switch reason {
	case "STOP":
		return stream.FinishReasonStop
	case "MAX_TOKENS":
		return stream.FinishReasonLength
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return stream.FinishReasonContentFilter
	default:
		return stream.FinishReasonUnknown
	}
}
