package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

// EncodeRequest converts a provider-neutral inference.Request into an OpenAI
// Responses `POST /v1/responses` JSON body. stream=true adds "stream":true to
// the body. Request.System (plus any in-thread SystemMessage) becomes the
// top-level `instructions` field: Responses has no in-thread system role, so
// a SystemMessage folds into instructions exactly as anthropicapi folds it
// into the top-level `system` field.
func EncodeRequest(req inference.Request, stream bool) ([]byte, error) {
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return nil, err
	}
	r, err := buildResponsesRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// buildResponsesRequest assembles the typed request struct. Split from
// marshaling so the mapping is unit-testable without a JSON round-trip.
func buildResponsesRequest(req inference.Request, stream bool) (wireRequest, error) {
	sampling := req.Model.Sampling
	if req.Override != nil {
		sampling = *req.Override
	}

	instructions := req.System
	var items []wireItem
	for _, conv := range req.Messages {
		switch m := conv.(type) {
		case *content.SystemMessage:
			instructions = appendSystem(instructions, textOf(m.Blocks))
		case *content.UserMessage:
			parts, err := encodeUserContentParts(m.Blocks)
			if err != nil {
				return wireRequest{}, err
			}
			items = append(items, wireItem{Type: itemTypeMessage, Role: roleUser, Content: parts})
		case *content.AIMessage:
			encoded, err := blocksToItems(m.Blocks, nil)
			if err != nil {
				return wireRequest{}, err
			}
			items = append(items, encoded...)
		case *content.ToolResultMessage:
			item, err := encodeToolResultItem(m)
			if err != nil {
				return wireRequest{}, err
			}
			items = append(items, item)
		default:
			return wireRequest{}, &UnsupportedConversationError{Conversation: fmt.Sprintf("%T", conv)}
		}
	}

	r := wireRequest{
		Model:           req.Model.Name,
		Instructions:    instructions,
		Input:           items,
		MaxOutputTokens: sampling.MaxTokens,
		Temperature:     sampling.Temperature,
		TopP:            sampling.TopP,
		// Store is always explicit false: this project excludes server-stored
		// Responses conversations/previous_response_id, and the provider's
		// unstated default for an omitted `store` cannot be relied upon.
		Store:  false,
		Stream: stream,
	}

	// Sampling.Stop has no Responses wire representation: the real API
	// rejects an unrecognized "stop" field outright ("Unknown parameter:
	// 'stop'"), unlike Chat Completions. Silently omitting it (rather than
	// failing the request) matches this codec's other dialect-specific
	// omissions, e.g. anthropicapi dropping temperature/top_p under thinking.

	for _, t := range req.Tools {
		r.Tools = append(r.Tools, wireTool{
			Type:        toolTypeFunction,
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schemaOrDefault(t.Schema),
		})
	}
	if req.ToolChoice == inference.ToolChoiceRequired {
		r.ToolChoice = toolChoiceRequired
	}

	if req.Output != nil {
		r.Text = &wireText{Format: &wireTextFormat{
			Type:   textFormatJSONSchema,
			Name:   req.Output.Name,
			Schema: req.Output.Schema,
			Strict: req.Output.Strict,
		}}
	}

	// effort -> reasoning, gated by the model's advertised Thinking
	// capability, mirroring exactly how anthropicapi gates its own thinking
	// config on req.Model.Caps.Thinking. When reasoning is enabled, `include`
	// requests encrypted reasoning content back so a same-dialect follow-up
	// request can replay it (ThinkingBlock.ProviderState).
	if req.Model.Caps.Thinking {
		if ev := effortValue(sampling.Effort); ev != "" {
			r.Reasoning = &wireReasoningConfig{Effort: ev, Summary: "auto"}
			r.Include = append(r.Include, includeEncryptedReasoningContent)
		}
	}

	return r, nil
}

// blocksToItems maps a slice of neutral content blocks (an AIMessage's
// Blocks, whether from an outbound-request replay or a server-encoded
// response) to Responses items, preserving block order and grouping
// consecutive text blocks into one message item — the inverse of the
// items-based wire shape's grouping on decode. ids synthesizes a
// function_call id when a ToolUseBlock arrives with none; nil means leave an
// empty id as-is (the client-encode replay direction, where every id should
// already be real because it came from a prior actual response).
func blocksToItems(blocks []content.Block, ids func() string) ([]wireItem, error) {
	var items []wireItem
	var pendingText []wireContentPart

	flush := func() {
		if len(pendingText) > 0 {
			items = append(items, wireItem{Type: itemTypeMessage, Role: roleAssistant, Content: pendingText})
			pendingText = nil
		}
	}

	for _, b := range blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			pendingText = append(pendingText, wireContentPart{Type: contentTypeOutputText, Text: b.Text})
		case *content.ToolUseBlock:
			flush()
			id := b.ID
			if id == "" && ids != nil {
				id = ids()
			}
			items = append(items, wireItem{
				Type:      itemTypeFunctionCall,
				CallID:    id,
				Name:      b.Name,
				Arguments: argumentsOrEmpty(b.Input),
			})
		case *content.ThinkingBlock:
			flush()
			item := wireItem{Type: itemTypeReasoning}
			if b.Thinking != "" {
				item.Summary = []wireSummaryPart{{Type: summaryTypeText, Text: b.Thinking}}
			}
			if b.ReplayableAs(providerStateFormatOpenAIResponses) {
				s, err := opaqueStateToWire(b.ProviderState)
				if err != nil {
					return nil, err
				}
				item.EncryptedContent = s
			}
			items = append(items, item)
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	flush()
	return items, nil
}

// encodeToolResultItem builds the function_call_output item from a
// ToolResultMessage. Output is a plain string on the wire (unlike
// Anthropic's structured tool_result content), so non-text blocks fail
// closed rather than being silently dropped — matching openaiapi's
// toolResultText. IsError has no Responses wire representation (no
// function_call_output field models it); this is a documented, intentional
// loss for this ingress-independent direction, mirroring how openaiapi never
// emits ToolResultMessage.IsError either.
func encodeToolResultItem(m *content.ToolResultMessage) (wireItem, error) {
	text, err := toolResultText(m.Blocks)
	if err != nil {
		return wireItem{}, err
	}
	return wireItem{Type: itemTypeFunctionCallOutput, CallID: m.ToolUseID, Output: text}, nil
}

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

// encodeUserContentParts maps a user message's blocks to input_text /
// input_image content parts. A block type this dialect does not model in a
// user turn (audio, document, …) yields a typed *UnsupportedBlockError.
func encodeUserContentParts(blocks []content.Block) ([]wireContentPart, error) {
	parts := make([]wireContentPart, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *content.TextBlock:
			parts = append(parts, wireContentPart{Type: contentTypeInputText, Text: b.Text})
		case *content.ImageBlock:
			parts = append(parts, wireContentPart{
				Type:     contentTypeInputImage,
				ImageURL: imageURLOf(b),
				Detail:   imageDetailAuto,
			})
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	return parts, nil
}

// imageURLOf builds the `image_url` string for an ImageBlock. A URL takes
// precedence over inline Data; Data becomes a data: URI.
func imageURLOf(img *content.ImageBlock) string {
	if img.Source.URL != "" {
		return img.Source.URL
	}
	encoded := base64.StdEncoding.EncodeToString(img.Source.Data)
	return dataURIPrefix + string(img.MediaType) + ";base64," + encoded
}

// argumentsOrEmpty returns raw's text as the wire `arguments` string,
// defaulting to "{}" for an empty/absent input (Responses requires function
// call arguments to be a JSON object).
func argumentsOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return emptyObject
	}
	return string(raw)
}

// schemaOrDefault guarantees a tool's `parameters` is a JSON object.
func schemaOrDefault(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(defaultSchema)
	}
	return schema
}

// effortValue maps the dialect-neutral model.Effort to Responses'
// reasoning.effort wire value. Responses accepts only "low"|"medium"|"high"
// (no "max"), so model.EffortMax clamps to "high" — mirroring openaiapi's
// reasoningEffort for Chat Completions. EffortNone (and any unknown value,
// fail-safe) yields "", which suppresses the whole `reasoning` field.
func effortValue(e model.Effort) string {
	switch e {
	case model.EffortLow:
		return "low"
	case model.EffortMedium:
		return "medium"
	case model.EffortHigh, model.EffortMax:
		return "high"
	default: // EffortNone or unknown -> omit
		return ""
	}
}

// appendSystem joins two instruction fragments, inserting a blank-line
// separator only when both sides are non-empty.
func appendSystem(base, add string) string {
	switch {
	case add == "":
		return base
	case base == "":
		return add
	default:
		return base + "\n\n" + add
	}
}

// textOf concatenates the text of all TextBlocks in a slice, ignoring others.
func textOf(blocks []content.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if t, ok := b.(*content.TextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// opaqueStateToWire unmarshals a ThinkingBlock.ProviderState (which this
// codec always stores as the JSON-marshaled form of the wire
// encrypted_content string — see opaqueStateFromWire, decode.go) back into
// the plain string the wire `encrypted_content` field carries.
func opaqueStateToWire(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("openairesponses: invalid ThinkingBlock.ProviderState: %w", err)
	}
	return s, nil
}
