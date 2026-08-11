package anthropicapi

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

// DecodeStream frames a successful Anthropic Messages streaming response with wire/sse
// and maps each frame through the codec's per-event decode logic. The message_stop
// marker authorizes the terminal result but yields no chunk; the body's natural
// EOF ends the transport stream. It owns resp.Body: the returned reader's Close
// closes it.
func (Codec) DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error) {
	frames, err := sse.DecodeStreamFrames(resp.Body)
	if err != nil {
		return nil, err
	}
	collector := &streamResultCollector{}
	return stream.FramesToChunksWithResult(frames, collector.mapFrame, collector.result), nil
}

// mapFrame decodes one raw SSE frame's Data via the shared per-event decoder. The
// Anthropic event type lives inside the JSON payload (decodeEvent reads it), so the
// SSE event Name on the frame is not needed here.
func mapFrame(f stream.StreamFrame) ([]content.Chunk, error) {
	return decodeEvent(f.Data)
}

type streamResultCollector struct {
	wireUsage       messageUsage
	usageSeen       bool
	messageStopSeen bool
	resultValue     stream.StreamResult
}

func (c *streamResultCollector) mapFrame(frame stream.StreamFrame) ([]content.Chunk, error) {
	var event streamEvent
	if err := json.Unmarshal(frame.Data, &event); err == nil {
		if err := c.collect(event); err != nil {
			return nil, err
		}
	}
	return mapFrame(frame)
}

func (c *streamResultCollector) collect(event streamEvent) error {
	if event.Type == responseTypeError {
		streamErr := &StreamAPIError{}
		if event.Error != nil {
			streamErr.Type = event.Error.Type
			streamErr.Message = event.Error.Message
		}
		return streamErr
	}
	if event.Type == eventMessageStop {
		c.messageStopSeen = true
	}
	if event.Type == eventMessageStart && event.Message != nil {
		if event.Message.Model != "" {
			c.resultValue.Model = event.Message.Model
		}
		if err := c.mergeUsage(event.Message.Usage); err != nil {
			return err
		}
	}
	if event.Type == eventMessageDelta {
		if event.Delta != nil && event.Delta.StopReason != "" {
			c.resultValue.FinishReason = mapFinishReason(event.Delta.StopReason)
		}
		if err := c.mergeUsage(event.Usage); err != nil {
			return err
		}
	}
	return nil
}

func (c *streamResultCollector) mergeUsage(update *messageUsage) error {
	if update == nil {
		return nil
	}
	c.usageSeen = true
	if update.InputTokens.Present() {
		c.wireUsage.InputTokens = update.InputTokens
	}
	if update.OutputTokens.Present() {
		c.wireUsage.OutputTokens = update.OutputTokens
	}
	if update.CacheReadTokens.Present() {
		c.wireUsage.CacheReadTokens = update.CacheReadTokens
	}
	if update.CacheCreationTokens.Present() {
		c.wireUsage.CacheCreationTokens = update.CacheCreationTokens
	}
	usage, err := normalizeUsage(&c.wireUsage)
	if err != nil {
		return err
	}
	c.resultValue.Usage = usage
	return nil
}

func (c *streamResultCollector) result() (stream.StreamResult, bool, error) {
	if !c.messageStopSeen {
		return stream.StreamResult{}, false, nil
	}
	if !c.usageSeen {
		c.resultValue.Usage = nil
	}
	return c.resultValue, true, nil
}

func mapFinishReason(reason string) stream.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return stream.FinishReasonStop
	case "max_tokens":
		return stream.FinishReasonLength
	case "tool_use":
		return stream.FinishReasonToolUse
	case "refusal":
		return stream.FinishReasonContentFilter
	default:
		return stream.FinishReasonUnknown
	}
}
