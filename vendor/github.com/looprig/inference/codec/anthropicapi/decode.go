package anthropicapi

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/internal/usagenorm"
	usage "github.com/looprig/inference/usage"
)

// DecodeResponse parses a non-streaming Anthropic Messages API response body into
// a provider-neutral *inference.Response. An `error`-type envelope (a 200 body carrying
// {"type":"error",...}) is surfaced as a *failure.APIError. An empty content array
// is a valid response (e.g. a refusal or a pure stop), not an error.
func DecodeResponse(body []byte) (*inference.Response, error) {
	var wire messageResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}

	if wire.Type == responseTypeError {
		return nil, failure.NewAPIError(0, "", "", 0)
	}

	usage, err := normalizeUsage(wire.Usage)
	if err != nil {
		return nil, err
	}
	var messageUsage *content.Usage
	if usage != nil {
		cloned := *usage
		messageUsage = &cloned
	}

	return &inference.Response{
		Message: &content.AIMessage{
			Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: decodeBlocks(wire.Content),
			},
			Usage: messageUsage,
		},
		Model:        wire.Model,
		Usage:        usage,
		FinishReason: mapFinishReason(wire.StopReason),
	}, nil
}

func normalizeUsage(wire *messageUsage) (*usage.Usage, error) {
	if wire == nil {
		return nil, nil
	}
	input, err := wire.InputTokens.TokenCount(usagenorm.FieldInputTokens)
	if err != nil {
		return nil, err
	}
	output, err := wire.OutputTokens.TokenCount(usagenorm.FieldOutputTokens)
	if err != nil {
		return nil, err
	}
	cacheRead, err := wire.CacheReadTokens.TokenCount(usagenorm.FieldCacheReadTokens)
	if err != nil {
		return nil, err
	}
	cacheCreation, err := wire.CacheCreationTokens.TokenCount(usagenorm.FieldCacheCreationTokens)
	if err != nil {
		return nil, err
	}
	usage := usage.Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreation}
	if err := usagenorm.ValidateUsage(usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

// decodeBlocks maps Anthropic response content blocks to provider-neutral blocks,
// preserving order. Unknown block types (redacted_thinking, server-tool blocks,
// etc.) are skipped tolerantly rather than erroring.
func decodeBlocks(blocks []anthropicBlock) []content.Block {
	var out []content.Block
	for _, b := range blocks {
		switch b.Type {
		case blockTypeText:
			out = append(out, &content.TextBlock{Text: b.Text})
		case blockTypeThinking:
			out = append(out, &content.ThinkingBlock{Thinking: b.Thinking, Signature: b.Signature})
		case blockTypeToolUse:
			out = append(out, &content.ToolUseBlock{ID: b.ID, Name: b.Name, Input: b.Input})
		default:
			// Skip block types the neutral vocabulary does not model.
		}
	}
	return out
}
