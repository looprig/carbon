package openaiapi

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/internal/usagenorm"
	usage "github.com/looprig/inference/usage"
)

// DecodeResponse parses an OpenAI chat completions JSON response body into
// a provider-neutral *inference.Response.
func DecodeResponse(body []byte) (*inference.Response, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}

	if len(wire.Choices) == 0 {
		return nil, failure.NewAPIError(0, "", "", 0)
	}

	msg := wire.Choices[0].Message
	blocks := buildBlocks(msg)

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
				Blocks: blocks,
			},
			Usage: messageUsage,
		},
		Model:        wire.Model,
		Usage:        usage,
		FinishReason: mapFinishReason(wire.Choices[0].FinishReason),
	}, nil
}

func normalizeUsage(wire *chatUsage) (*usage.Usage, error) {
	if wire == nil {
		return nil, nil
	}
	input, cacheRead, cacheCreation, err := normalizePromptUsage(*wire)
	if err != nil {
		return nil, err
	}
	output, reasoning, err := normalizeCompletionUsage(*wire)
	if err != nil {
		return nil, err
	}
	usage := usage.Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreation, ReasoningTokens: reasoning}
	if err := usagenorm.ValidateUsage(usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

func normalizePromptUsage(wire chatUsage) (content.TokenCount, content.TokenCount, content.TokenCount, error) {
	prompt, err := wire.PromptTokens.TokenCount(usagenorm.FieldInputTokens)
	if err != nil {
		return 0, 0, 0, err
	}
	cacheRead, err := wire.PromptTokensDetails.CachedTokens.OptionalTokenCount(usagenorm.FieldCacheReadTokens)
	if err != nil {
		return 0, 0, 0, err
	}
	cacheCreation, err := wire.PromptTokensDetails.CacheWriteTokens.OptionalTokenCount(usagenorm.FieldCacheCreationTokens)
	if err != nil {
		return 0, 0, 0, err
	}
	input, err := usagenorm.SubtractTokenCounts(usagenorm.FieldInputTokens, prompt, cacheRead, cacheCreation)
	if err != nil {
		return 0, 0, 0, err
	}
	return input, cacheRead, cacheCreation, nil
}

func normalizeCompletionUsage(wire chatUsage) (content.TokenCount, content.TokenCount, error) {
	output, err := wire.CompletionTokens.TokenCount(usagenorm.FieldOutputTokens)
	if err != nil {
		return 0, 0, err
	}
	reasoning, err := wire.CompletionTokensDetails.ReasoningTokens.OptionalTokenCount(usagenorm.FieldReasoningTokens)
	return output, reasoning, err
}

// buildBlocks constructs an ordered slice of content blocks from a decoded
// chatMessage. Reasoning comes first, then text, then tool calls.
func buildBlocks(msg chatMessage) []content.Block {
	var blocks []content.Block

	if msg.ReasoningContent != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: msg.ReasoningContent})
	}

	if s, ok := msg.Content.(string); ok && s != "" {
		blocks = append(blocks, &content.TextBlock{Text: s})
	}

	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, &content.ToolUseBlock{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	return blocks
}
