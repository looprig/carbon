package openaiapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

// BuildChatRequest converts a provider-neutral inference.Request into a ChatRequest
// struct. Exported so provider packages can embed or extend the result before
// marshaling (e.g. a provider extension adds an encrypted-response public-key field).
func BuildChatRequest(req inference.Request, stream bool) (ChatRequest, error) {
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return ChatRequest{}, err
	}

	// Effective sampling: a non-nil per-call Override wins over Model.Sampling.
	sampling := req.Model.Sampling
	if req.Override != nil {
		sampling = *req.Override
	}

	cr := ChatRequest{
		Model:           req.Model.Name,
		Temperature:     sampling.Temperature,
		TopP:            sampling.TopP,
		MaxTokens:       sampling.MaxTokens,
		Stop:            sampling.Stop,
		Stream:          stream,
		ReasoningEffort: reasoningEffort(sampling.Effort),
	}
	if stream {
		cr.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}

	if req.System != "" {
		cr.Messages = append(cr.Messages, chatMessage{
			Role:    "system",
			Content: req.System,
		})
	}

	for _, conv := range req.Messages {
		msgs, err := encodeConversation(conv)
		if err != nil {
			return ChatRequest{}, err
		}
		cr.Messages = append(cr.Messages, msgs...)
	}

	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	if req.Output != nil {
		cr.ResponseFormat = &responseFormat{
			Type: responseFormatJSONSchema,
			JSONSchema: &jsonSchema{
				Name:   req.Output.Name,
				Strict: req.Output.Strict,
				Schema: req.Output.Schema,
			},
		}
	}
	if req.ToolChoice == inference.ToolChoiceRequired {
		cr.ToolChoice = toolChoiceRequired
	}

	return cr, nil
}

// EncodeRequest converts a provider-neutral inference.Request to an OpenAI chat
// completions JSON body. stream=true adds "stream":true to the body.
// Request.System is prepended as a system message if non-empty.
func EncodeRequest(req inference.Request, stream bool) ([]byte, error) {
	cr, err := BuildChatRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}

// reasoningEffort maps the dialect-neutral model.Effort to the OpenAI Chat
// Completions reasoning_effort wire value. OpenAI's o-series accepts only
// "low" | "medium" | "high" (there is no "max"), so model.EffortMax clamps to
// "high" — the strongest value the wire accepts. EffortNone (and any unknown
// value, fail-safe) yields "", which the omitempty tag drops from the body.
func reasoningEffort(e model.Effort) string {
	switch e {
	case model.EffortLow:
		return "low"
	case model.EffortMedium:
		return "medium"
	case model.EffortHigh, model.EffortMax:
		return "high"
	default: // EffortNone or unknown → omit
		return ""
	}
}

// encodeConversation dispatches a content.Conversation to the appropriate
// chatMessage encoder.
func encodeConversation(conv content.Conversation) ([]chatMessage, error) {
	switch m := conv.(type) {
	case *content.SystemMessage:
		return []chatMessage{{
			Role:    "system",
			Content: textContent(m.Blocks),
		}}, nil

	case *content.UserMessage:
		parts, err := encodeContentParts(m.Blocks)
		if err != nil {
			return nil, err
		}
		return []chatMessage{{
			Role:    "user",
			Content: parts,
		}}, nil

	case *content.AIMessage:
		msg, err := encodeAIMessage(m)
		if err != nil {
			return nil, err
		}
		return []chatMessage{msg}, nil

	case *content.ToolResultMessage:
		// IsError reconciliation: the OpenAI Chat Completions tool message has no
		// structured error flag (unlike Anthropic's tool_result block), so
		// ToolResultMessage.IsError is intentionally NOT placed on the request —
		// emitting a non-standard is_error here would be a schema violation. The model
		// learns a tool errored via the result's text content, which for engine-level
		// failures (Go error, panic, empty result, pre-exec failure) the loop
		// error-prefixes; a tool's self-reported ToolResultBlock error passes through
		// verbatim, so there the message-level IsError is the only structured signal.
		// IsError exists for the internal wire form and the display layer, not for
		// this provider's request.
		text, err := toolResultText(m.Blocks)
		if err != nil {
			return nil, err
		}
		return []chatMessage{{
			Role:       "tool",
			Content:    text,
			ToolCallID: m.ToolUseID,
		}}, nil

	default:
		return nil, fmt.Errorf("openaiapi: unknown conversation type %T", conv)
	}
}

// textContent concatenates all text blocks into a single string.
func textContent(blocks []content.Block) string {
	var out string
	for _, b := range blocks {
		if t, ok := b.(*content.TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

// toolResultText flattens a tool result's blocks to the plain string the
// text-only OpenAI tool message carries. Any non-text block yields a typed
// *UnsupportedBlockError — fail-secure, never a silent drop.
func toolResultText(blocks []content.Block) (string, error) {
	var out string
	for _, b := range blocks {
		t, ok := b.(*content.TextBlock)
		if !ok {
			return "", &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
		out += t.Text
	}
	return out, nil
}

// encodeContentParts returns a plain string when all blocks are text, or a
// []chatContentPart slice when image blocks are present. A block type the
// dialect does not model in a user turn (audio, document, …) yields a typed
// *UnsupportedBlockError — fail-secure, never a silent drop.
func encodeContentParts(blocks []content.Block) (interface{}, error) {
	allText := true
	for _, b := range blocks {
		if _, ok := b.(*content.TextBlock); !ok {
			allText = false
			break
		}
	}
	if allText {
		return textContent(blocks), nil
	}

	parts := make([]chatContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			parts = append(parts, chatContentPart{Type: "text", Text: b.Text})
		case *content.ImageBlock:
			parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &imageURLPart{URL: imageURL(b)}})
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	return parts, nil
}

// imageURL builds the URL string for an ImageBlock. URL takes precedence over
// Data. If Data is set, a data URI is returned.
func imageURL(img *content.ImageBlock) string {
	if img.Source.URL != "" {
		return img.Source.URL
	}
	encoded := base64.StdEncoding.EncodeToString(img.Source.Data)
	return "data:" + string(img.MediaType) + ";base64," + encoded
}

// encodeAIMessage builds a chatMessage from an AIMessage, handling text,
// tool calls, and ignoring ThinkingBlock.
func encodeAIMessage(m *content.AIMessage) (chatMessage, error) {
	var text string
	var calls []toolCall

	for _, b := range m.Blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			text += b.Text
		case *content.ToolUseBlock:
			// OpenAI wire format: function.arguments MUST be a JSON-encoded
			// STRING (e.g. "{\"p\":1}"), never a raw object. b.Input holds the
			// raw JSON object, so quote it; empty input becomes "{}". Emitting a
			// bare object here makes strict OpenAI-compatible servers reject the
			// follow-up request with a 400.
			raw := string(b.Input)
			if raw == "" {
				raw = "{}"
			}
			quoted, err := json.Marshal(raw)
			if err != nil {
				return chatMessage{}, fmt.Errorf("openaiapi: encode tool arguments for %q: %w", b.Name, err)
			}
			calls = append(calls, toolCall{
				ID:       b.ID,
				Type:     "function",
				Function: toolCallFunction{Name: b.Name, Arguments: json.RawMessage(quoted)},
			})
		case *content.ThinkingBlock:
			// Deliberately ignored: thinking is not part of the OpenAI wire format.
		default:
			return chatMessage{}, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}

	return chatMessage{
		Role:      "assistant",
		Content:   text,
		ToolCalls: calls,
	}, nil
}
