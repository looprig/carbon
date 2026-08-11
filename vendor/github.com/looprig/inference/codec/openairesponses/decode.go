package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/internal/usagenorm"
	stream "github.com/looprig/inference/stream"
	usage "github.com/looprig/inference/usage"
)

// DecodeResponse parses a non-streaming OpenAI Responses API response body
// into a provider-neutral *inference.Response. A status:"failed" response is
// surfaced as a *failure.APIError, mirroring how anthropicapi.DecodeResponse
// handles its type:"error" envelope. An empty output array is a valid
// response, not an error.
func DecodeResponse(body []byte) (*inference.Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}

	if wire.Status == statusFailed {
		return nil, failure.NewAPIError(0, "", "", 0)
	}

	blocks, err := decodeOutputBlocks(wire.Output)
	if err != nil {
		return nil, err
	}

	u, err := normalizeUsage(wire.Usage)
	if err != nil {
		return nil, err
	}
	var messageUsage *content.Usage
	if u != nil {
		cloned := *u
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
		Usage:        u,
		FinishReason: deriveFinishReason(wire),
	}, nil
}

// deriveFinishReason derives the neutral stream.FinishReason from Responses'
// status/incomplete_details/output shape, per the design's explicit mapping:
// any function_call in output means the model wants to call a tool
// regardless of status; otherwise status:"incomplete" with
// incomplete_details.reason:"max_output_tokens" means length; any other
// incomplete reason (or a missing one) is unknown; status:"completed" is a
// clean stop. status:"failed" is handled earlier (DecodeResponse) as an
// error, never reaching here as a finish reason.
func deriveFinishReason(wire wireResponse) stream.FinishReason {
	for _, item := range wire.Output {
		if item.Type == itemTypeFunctionCall {
			return stream.FinishReasonToolUse
		}
	}
	switch wire.Status {
	case statusCompleted:
		return stream.FinishReasonStop
	case statusIncomplete:
		if wire.IncompleteDetails != nil && wire.IncompleteDetails.Reason == incompleteReasonMaxOutputTokens {
			return stream.FinishReasonLength
		}
		return stream.FinishReasonUnknown
	default:
		return stream.FinishReasonUnknown
	}
}

func normalizeUsage(wire *wireUsage) (*usage.Usage, error) {
	if wire == nil {
		return nil, nil
	}
	promptTotal, err := wire.InputTokens.TokenCount(usagenorm.FieldInputTokens)
	if err != nil {
		return nil, err
	}
	cacheRead, err := wire.InputTokensDetails.CachedTokens.TokenCount(usagenorm.FieldCacheReadTokens)
	if err != nil {
		return nil, err
	}
	// Responses reports input_tokens as the gross prompt total including
	// cached tokens (like Chat Completions' prompt_tokens), with cached_tokens
	// as a breakdown subset — not a separate additive count. Subtract it to
	// the neutral input/cache-read split, mirroring openaiapi's
	// normalizePromptUsage. Responses has no cache-CREATION concept (caching
	// is fully automatic server-side), so CacheCreationTokens stays zero.
	input, err := usagenorm.SubtractTokenCounts(usagenorm.FieldInputTokens, promptTotal, cacheRead, 0)
	if err != nil {
		return nil, err
	}
	output, err := wire.OutputTokens.TokenCount(usagenorm.FieldOutputTokens)
	if err != nil {
		return nil, err
	}
	reasoning, err := wire.OutputTokensDetails.ReasoningTokens.TokenCount(usagenorm.FieldReasoningTokens)
	if err != nil {
		return nil, err
	}
	u := usage.Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, ReasoningTokens: reasoning}
	if err := usagenorm.ValidateUsage(u); err != nil {
		return nil, err
	}
	return &u, nil
}

// decodeOutputBlocks maps a response's `output` items to neutral content
// blocks, in wire order: a message item's output_text parts become
// TextBlocks, a function_call item becomes a ToolUseBlock, and a reasoning
// item becomes a ThinkingBlock (its encrypted_content, if present, round-trips
// through ThinkingBlock.ProviderState for a same-dialect follow-up request —
// see opaqueStateFromWire). Unknown item types are skipped tolerantly, like
// anthropicapi.decodeBlocks skips unknown block types on response decode.
func decodeOutputBlocks(items []wireItem) ([]content.Block, error) {
	var out []content.Block
	for _, item := range items {
		switch item.Type {
		case itemTypeMessage:
			for _, part := range item.Content {
				if part.Type == contentTypeOutputText {
					out = append(out, &content.TextBlock{Text: part.Text})
				}
			}
		case itemTypeFunctionCall:
			args, err := decodeFunctionCallArguments(item.Arguments)
			if err != nil {
				return nil, err
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			out = append(out, &content.ToolUseBlock{ID: id, Name: item.Name, Input: args})
		case itemTypeReasoning:
			out = append(out, decodeReasoningItem(item.Summary, item.EncryptedContent))
		default:
			// Skip item types the neutral vocabulary does not model
			// (tolerant response decode, matching anthropicapi.decodeBlocks).
		}
	}
	return out, nil
}

// decodeFunctionCallArguments converts the wire `arguments` JSON-string into
// the json.RawMessage a neutral ToolUseBlock.Input carries, defaulting an
// absent value to "{}".
func decodeFunctionCallArguments(raw string) (json.RawMessage, error) {
	if raw == "" {
		return json.RawMessage(emptyObject), nil
	}
	return json.RawMessage(raw), nil
}

// providerStateFormatOpenAIResponses tags a ThinkingBlock.ProviderState as
// having been produced by this codec (i.e. containing an OpenAI Responses
// encrypted_content value). Per the invariant documented on
// content.ThinkingBlock, every site in this package that forwards
// ProviderState onto the wire as encrypted_content must first check
// ProviderStateFormat == providerStateFormatOpenAIResponses; a ProviderState
// tagged with any other format (or untagged) originated from a different
// dialect and must be treated as absent, never replayed here.
const providerStateFormatOpenAIResponses = "openai-responses"

// decodeReasoningItem builds a ThinkingBlock from a reasoning item's summary
// parts (concatenated) and its opaque encrypted_content, if present, via
// content.NewThinkingBlock so ProviderState is defensively copied.
func decodeReasoningItem(summary []wireSummaryPart, encryptedContent string) *content.ThinkingBlock {
	var sb strings.Builder
	for i, part := range summary {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(part.Text)
	}

	var providerState json.RawMessage
	var providerStateFormat string
	if encryptedContent != "" {
		providerState = opaqueStateFromWire(encryptedContent)
		providerStateFormat = providerStateFormatOpenAIResponses
	}
	return content.NewThinkingBlock(sb.String(), "", providerState, providerStateFormat)
}

// opaqueStateFromWire marshals the wire `encrypted_content` string into the
// json.RawMessage form ThinkingBlock.ProviderState carries. Pairing with
// opaqueStateToWire (encode.go) makes ProviderState always "the JSON-encoded
// form of the provider-opaque wire value" for this dialect, so it round-trips
// arbitrary bytes/characters through ordinary JSON string escaping.
func opaqueStateFromWire(s string) json.RawMessage {
	// json.Marshal of a string cannot fail.
	encoded, _ := json.Marshal(s)
	return encoded
}

// decodeImageURL parses an input_image `image_url`: a data: URI decodes to
// inline bytes, anything else is treated as a remote URL reference.
func decodeImageURL(raw string) (*content.ImageBlock, error) {
	if raw == "" {
		return nil, &ServerDecodeError{Reason: "missing_image_url"}
	}
	if strings.HasPrefix(raw, dataURIPrefix) {
		mediaType, data, err := parseDataURI(raw)
		if err != nil {
			return nil, err
		}
		return &content.ImageBlock{MediaType: content.MediaType(mediaType), Source: content.ImageSource{Data: data}}, nil
	}
	return &content.ImageBlock{Source: content.ImageSource{URL: raw}}, nil
}

func parseDataURI(raw string) (string, []byte, error) {
	rest := strings.TrimPrefix(raw, dataURIPrefix)
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, &ServerDecodeError{Reason: "invalid_image_data_uri"}
	}
	meta := rest[:comma]
	payload := rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", nil, &ServerDecodeError{Reason: "unsupported_image_data_uri_encoding"}
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, &ServerDecodeError{Reason: "invalid_image_data", Detail: err.Error()}
	}
	return mediaType, data, nil
}
