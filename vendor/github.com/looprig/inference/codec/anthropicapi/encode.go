package anthropicapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

// EncodeRequest converts a provider-neutral inference.Request into an Anthropic
// `POST /v1/messages` JSON body. stream=true adds "stream":true to the body.
// Request.System becomes the top-level `system` field; any SystemMessage in the
// thread is folded into it (Anthropic has no in-thread system role).
func EncodeRequest(req inference.Request, stream bool) ([]byte, error) {
	if err := inference.ValidateRequestFeatures(req); err != nil {
		return nil, err
	}
	r, err := buildMessagesRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// buildMessagesRequest assembles the typed request struct. Split from marshaling
// so the mapping is unit-testable without a JSON round-trip.
func buildMessagesRequest(req inference.Request, stream bool) (messagesRequest, error) {
	// Effective sampling: a non-nil per-call Override wins over Model.Sampling.
	sampling := req.Model.Sampling
	if req.Override != nil {
		sampling = *req.Override
	}

	system := req.System
	var messages []anthropicMessage
	committedSourceMessages := len(req.Messages) - req.TransientMessages
	committedEncodedMessages := 0
	transientSystemMessageNonEmpty := false
	for index, conv := range req.Messages {
		switch m := conv.(type) {
		case *content.SystemMessage:
			// Anthropic has no in-thread system role: fold system text into the
			// top-level `system` field, preserving order after Request.System.
			systemText := textOf(m.Blocks)
			if index >= committedSourceMessages && systemText != "" {
				transientSystemMessageNonEmpty = true
			}
			system = appendSystem(system, systemText)
		case *content.UserMessage:
			blocks, err := encodeBlocks(m.Blocks)
			if err != nil {
				return messagesRequest{}, err
			}
			messages = append(messages, anthropicMessage{Role: roleUser, Content: blocks})
			if index < committedSourceMessages {
				committedEncodedMessages++
			}
		case *content.AIMessage:
			blocks, err := encodeBlocks(m.Blocks)
			if err != nil {
				return messagesRequest{}, err
			}
			messages = append(messages, anthropicMessage{Role: roleAssistant, Content: blocks})
			if index < committedSourceMessages {
				committedEncodedMessages++
			}
		case *content.ToolResultMessage:
			block, err := encodeToolResult(m)
			if err != nil {
				return messagesRequest{}, err
			}
			// Anthropic delivers tool results as a user-role message whose sole
			// block is a tool_result. IsError is a first-class field here (unlike
			// the OpenAI dialect), so ToolResultMessage.IsError passes through.
			messages = append(messages, anthropicMessage{Role: roleUser, Content: []anthropicBlock{block}})
			if index < committedSourceMessages {
				committedEncodedMessages++
			}
		default:
			return messagesRequest{}, &UnsupportedConversationError{Conversation: fmt.Sprintf("%T", conv)}
		}
	}

	var sys *systemPrompt
	if system != "" {
		sys = &systemPrompt{Text: system}
	}

	r := messagesRequest{
		Model:         req.Model.Name,
		System:        sys,
		Messages:      messages,
		MaxTokens:     effectiveMaxTokens(sampling.MaxTokens),
		StopSequences: sampling.Stop,
		Stream:        stream,
	}

	for _, t := range req.Tools {
		r.Tools = append(r.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schemaOrDefault(t.Schema),
		})
	}
	if req.ToolChoice == inference.ToolChoiceRequired {
		r.ToolChoice = &toolChoice{Type: toolChoiceAny}
	}

	// effort → thinking, gated by the model's advertised Thinking capability. A
	// non-none Effort enables adaptive thinking and maps the level to
	// output_config.effort; EffortNone (or a model that can't think) emits neither.
	thinkingEnabled := false
	if req.Model.Caps.Thinking {
		if ev := effortValue(sampling.Effort); ev != "" {
			r.Thinking = &thinkingConfig{Type: thinkingTypeAdaptive}
			r.OutputConfig = &outputConfig{Effort: ev}
			thinkingEnabled = true
		}
	}
	if req.Output != nil {
		if r.OutputConfig == nil {
			r.OutputConfig = &outputConfig{}
		}
		r.OutputConfig.Format = &outputFormat{
			Type:   outputFormatJSONSchema,
			Schema: req.Output.Schema,
		}
	}

	// temperature/top_p reconciliation: current adaptive-thinking Anthropic models
	// reject temperature or top_p sent alongside thinking with an HTTP 400, so when
	// thinking is enabled for this request the codec OMITS both. Otherwise they pass
	// through only when set (omitempty on the wire struct). This is the codec's job
	// per the sampling design — the dialect-validity rule lives here, not on Sampling.
	if !thinkingEnabled {
		r.Temperature = sampling.Temperature
		r.TopP = sampling.TopP
	}

	// A non-empty transient SystemMessage folds into the top-level system
	// prefix, so caching either breakpoint would include transient context.
	if req.Model.Caps.PromptCaching && !transientSystemMessageNonEmpty {
		applyCacheBreakpoints(&r, committedEncodedMessages)
	}

	return r, nil
}

// applyCacheBreakpoints marks the codec's two ephemeral cache_control
// breakpoints: one on the system prompt (which caches tools + system, since
// tools render before system in Anthropic's prefix order) and one on the last
// cacheable block of the committed message history, so multi-turn requests
// accrue incremental cache hits without marking transient runtime messages.
// A thinking block cannot carry cache_control, so the message breakpoint walks
// back to the nearest non-thinking block. At most two breakpoints are emitted,
// well under the wire limit of four.
func applyCacheBreakpoints(r *messagesRequest, committedMessages int) {
	if r.System != nil {
		r.System.Cache = true
	}
	committed := r.Messages[:committedMessages]
	for i := len(committed) - 1; i >= 0; i-- {
		blocks := committed[i].Content
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j].Type == blockTypeThinking {
				continue
			}
			blocks[j].CacheControl = &cacheControl{Type: cacheControlEphemeral}
			return
		}
	}
}

// encodeBlocks maps a slice of content blocks to their Anthropic wire form.
func encodeBlocks(blocks []content.Block) ([]anthropicBlock, error) {
	out := make([]anthropicBlock, 0, len(blocks))
	for _, b := range blocks {
		eb, err := encodeBlock(b)
		if err != nil {
			return nil, err
		}
		out = append(out, eb)
	}
	return out, nil
}

// encodeBlock maps one content.Block to its Anthropic wire block. Text, image,
// thinking, and tool_use are supported; any other concrete type (audio,
// document) yields a typed UnsupportedBlockError — fail-secure, not silent.
func encodeBlock(b content.Block) (anthropicBlock, error) {
	switch b := b.(type) {
	case *content.TextBlock:
		return anthropicBlock{Type: blockTypeText, Text: b.Text}, nil
	case *content.ImageBlock:
		return anthropicBlock{Type: blockTypeImage, Source: imageSourceOf(b)}, nil
	case *content.ThinkingBlock:
		return anthropicBlock{Type: blockTypeThinking, Thinking: b.Thinking, Signature: b.Signature}, nil
	case *content.ToolUseBlock:
		return anthropicBlock{Type: blockTypeToolUse, ID: b.ID, Name: b.Name, Input: inputOrEmpty(b.Input)}, nil
	default:
		return anthropicBlock{}, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
	}
}

// encodeToolResult builds the tool_result block from a ToolResultMessage. The
// result content (typically text) is encoded through the same block encoder.
func encodeToolResult(m *content.ToolResultMessage) (anthropicBlock, error) {
	inner, err := encodeBlocks(m.Blocks)
	if err != nil {
		return anthropicBlock{}, err
	}
	return anthropicBlock{
		Type:      blockTypeToolResult,
		ToolUseID: m.ToolUseID,
		Content:   inner,
		IsError:   m.IsError,
	}, nil
}

// imageSourceOf builds the `source` for an image block. A URL takes precedence
// over inline Data; Data is base64-encoded with its media type.
func imageSourceOf(img *content.ImageBlock) *imageSource {
	if img.Source.URL != "" {
		return &imageSource{Type: imageSourceURL, URL: img.Source.URL}
	}
	return &imageSource{
		Type:      imageSourceBase64,
		MediaType: string(img.MediaType),
		Data:      base64.StdEncoding.EncodeToString(img.Source.Data),
	}
}

// inputOrEmpty guarantees a tool_use `input` is a JSON object: an empty raw
// value becomes "{}", which Anthropic requires.
func inputOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(emptyObject)
	}
	return raw
}

// schemaOrDefault guarantees a tool `input_schema` is a JSON object: an empty
// schema becomes {"type":"object"}, which Anthropic requires.
func schemaOrDefault(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(defaultSchema)
	}
	return schema
}

// effectiveMaxTokens returns the caller's MaxTokens when set to a positive value,
// else the mandatory codec default.
func effectiveMaxTokens(p *int) int {
	if p != nil && *p > 0 {
		return *p
	}
	return defaultMaxTokens
}

// effortValue maps the dialect-neutral model.Effort to Anthropic's
// output_config.effort wire value. EffortNone (and any unknown value, fail-safe)
// yields "", which suppresses both the thinking and output_config fields.
func effortValue(e model.Effort) string {
	switch e {
	case model.EffortLow:
		return "low"
	case model.EffortMedium:
		return "medium"
	case model.EffortHigh:
		return "high"
	case model.EffortMax:
		return "max"
	default: // EffortNone or unknown → omit
		return ""
	}
}

// appendSystem joins two system-prompt fragments, inserting a blank-line
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
