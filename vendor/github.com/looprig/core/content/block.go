// Package content defines the unified content vocabulary shared across all
// internal packages. Block is a sealed interface; the concrete payload type is
// the discriminator. Only this package can add variants (unexported marker).
package content

import "encoding/json"

type BlockType string

const (
	TypeText       BlockType = "text"
	TypeImage      BlockType = "image"
	TypeAudio      BlockType = "audio"
	TypeDocument   BlockType = "document"
	TypeThinking   BlockType = "thinking"
	TypeToolUse    BlockType = "tool_use"
	TypeToolResult BlockType = "tool_result"
)

// Block is the sealed interface over all content block payloads. The concrete
// type is the discriminator; there is no Type field and no nil-able payload
// pointers. BlockType is retained only as the wire tag for the JSON codec
// (block_json.go, added in a later task), not as a field on any in-memory value.
type Block interface{ isBlock() }

func (*TextBlock) isBlock()       {}
func (*ImageBlock) isBlock()      {}
func (*AudioBlock) isBlock()      {}
func (*DocumentBlock) isBlock()   {}
func (*ThinkingBlock) isBlock()   {}
func (*ToolUseBlock) isBlock()    {}
func (*ToolResultBlock) isBlock() {}

type TextBlock struct {
	Text string
}

// ImageSource is a sum type for the origin of image data.
// Set exactly one of URL (remote) or Data (inline bytes).
type ImageSource struct {
	URL  string
	Data []byte
}

type ImageBlock struct {
	MediaType MediaType
	Source    ImageSource
}

type AudioBlock struct {
	MediaType MediaType
	Data      []byte
}

// DocumentBlock carries document data. Either Data (binary) or Text (extracted
// text) may be populated depending on how the document was provided.
type DocumentBlock struct {
	MediaType MediaType
	Name      string
	Data      []byte
	Text      string
}

// ThinkingBlock carries model reasoning text.
// Signature is empty during streaming and non-empty only on a complete block.
//
// ProviderState carries provider-private opaque reasoning state (for example
// an Anthropic thinking signature or a Gemini thoughtSignature) that this
// package never interprets. It is meaningful only for a same-dialect replay
// against the provider that issued it. Construct via NewThinkingBlock so the
// bytes are defensively copied; a bare struct literal aliases the caller's
// slice.
//
// ProviderStateFormat is an opaque, codec-chosen label identifying which
// dialect encoded ProviderState (for example "gemini" or "openai-responses").
// It is meaningless and unset whenever ProviderState is empty. This field
// exists to satisfy the inference gateway's first-milestone requirement that
// opaque replay state is never translated across provider dialects (see
// docs/plans/2026-07-31-inference-gateway-design.md, "Thinking" section):
// a codec MUST NEVER replay ProviderState toward a wire field it owns unless
// ProviderStateFormat equals that codec's own label; otherwise it MUST treat
// ProviderState as absent. This is the load-bearing invariant that prevents
// one provider's opaque bytes (e.g. a Gemini thoughtSignature) from being
// forwarded to a different provider (e.g. as an OpenAI Responses
// encrypted_content) as if it were that provider's own native state.
type ThinkingBlock struct {
	Thinking            string
	Signature           string
	ProviderState       json.RawMessage `json:"ProviderState,omitempty"`
	ProviderStateFormat string          `json:"ProviderStateFormat,omitempty"`
}

// NewThinkingBlock builds a ThinkingBlock, defensively copying providerState so
// the caller cannot mutate the retained block through its input slice.
// providerStateFormat tags which dialect encoded providerState; see the
// ThinkingBlock doc comment for the invariant this enforces.
func NewThinkingBlock(thinking, signature string, providerState json.RawMessage, providerStateFormat string) *ThinkingBlock {
	var state json.RawMessage
	if providerState != nil {
		state = append(json.RawMessage(nil), providerState...)
	}
	return &ThinkingBlock{
		Thinking:            thinking,
		Signature:           signature,
		ProviderState:       state,
		ProviderStateFormat: providerStateFormat,
	}
}

// ReplayableAs reports whether b carries provider-opaque state safe to
// replay toward a wire field owned by the dialect labeled format. False for
// a nil receiver, an empty ProviderState, or a ProviderStateFormat that does
// not exactly match format — the same "treat as absent" degrade every caller
// of this method must already apply on a false result. See the
// ProviderStateFormat field doc for the cross-dialect-replay invariant this
// method exists to let every call site enforce identically.
func (b *ThinkingBlock) ReplayableAs(format string) bool {
	return b != nil && len(b.ProviderState) > 0 && b.ProviderStateFormat == format
}

type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultBlock nests its own []Block, so it implements json.Marshaler /
// json.Unmarshaler in block_json.go (a later task). Do not add a Type field.
type ToolResultBlock struct {
	ToolUseID string
	Content   []Block
	IsError   bool
}
