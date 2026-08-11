package content

import "encoding/json"

// Codec safety caps for the untrusted restore boundary. Conservative starting
// values; tune to real history sizes later.
const (
	maxBlockBytes     = 8 << 20 // 8 MiB per serialized block
	maxBlocksPerSlice = 10_000  // elements in one []Block
)

// blockTag returns the wire discriminator for a concrete Block. A nil or foreign
// value yields UnknownBlockTypeError (typed-nil / nil-interface fail closed here).
func blockTag(b Block) (BlockType, error) {
	switch b.(type) {
	case *TextBlock:
		return TypeText, nil
	case *ImageBlock:
		return TypeImage, nil
	case *AudioBlock:
		return TypeAudio, nil
	case *DocumentBlock:
		return TypeDocument, nil
	case *ThinkingBlock:
		return TypeThinking, nil
	case *ToolUseBlock:
		return TypeToolUse, nil
	case *ToolResultBlock:
		return TypeToolResult, nil
	default:
		return "", &UnknownBlockTypeError{}
	}
}

// MarshalBlock writes {"type": <tag>, ...payload}. The payload is marshaled first
// (so ToolResultBlock's custom MarshalJSON runs), then the tag is merged in as a
// sibling key — never via an embedding wrapper, which would let a Marshaler
// payload shadow the "type" key. Key order is not significant.
func MarshalBlock(b Block) ([]byte, error) {
	tag, err := blockTag(b)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	if string(payload) == "null" {
		// A non-nil block always marshals to a JSON object; "null" means the
		// concrete payload pointer was nil. Fail secure.
		return nil, &NilBlockError{Type: tag}
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	tagJSON, _ := json.Marshal(tag) // BlockType is a string; cannot fail
	fields["type"] = tagJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &BlockEncodeError{Type: tag, Cause: err}
	}
	return out, nil
}

// UnmarshalBlock reads the tag, allocates the concrete type, and decodes the same
// bytes into it (the extra "type" key is ignored by the struct decode).
func UnmarshalBlock(data []byte) (Block, error) {
	if len(data) > maxBlockBytes {
		return nil, &BlockLimitError{Limit: "block_bytes", Got: len(data), Max: maxBlockBytes}
	}
	var probe struct {
		Type BlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	switch probe.Type {
	case TypeText:
		return decodeInto[TextBlock](data)
	case TypeImage:
		return decodeInto[ImageBlock](data)
	case TypeAudio:
		return decodeInto[AudioBlock](data)
	case TypeDocument:
		return decodeInto[DocumentBlock](data)
	case TypeThinking:
		return decodeInto[ThinkingBlock](data)
	case TypeToolUse:
		return decodeInto[ToolUseBlock](data)
	case TypeToolResult:
		return decodeInto[ToolResultBlock](data)
	default:
		return nil, &UnknownBlockTypeError{Type: probe.Type}
	}
}

// decodeInto unmarshals data into a freshly allocated *T and returns it as Block.
func decodeInto[T any](data []byte) (Block, error) {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	return any(v).(Block), nil // each *T satisfies Block for the seven payload types
}

// MarshalBlocks encodes a []Block as a JSON array of tagged blocks.
func MarshalBlocks(bs []Block) ([]byte, error) {
	raws := make([]json.RawMessage, len(bs))
	for i, b := range bs {
		r, err := MarshalBlock(b)
		if err != nil {
			return nil, err
		}
		raws[i] = r
	}
	return json.Marshal(raws)
}

// UnmarshalBlocks decodes a JSON array of tagged blocks. It is the single
// recursion point for nested content (ToolResultBlock.Content) and enforces the
// element-count cap.
func UnmarshalBlocks(data []byte) ([]Block, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, &BlockDecodeError{Cause: err}
	}
	if len(raws) > maxBlocksPerSlice {
		return nil, &BlockLimitError{Limit: "slice_count", Got: len(raws), Max: maxBlocksPerSlice}
	}
	if len(raws) == 0 {
		// An empty array decodes to nil, matching the marshal path which omits
		// both nil and empty slices. This keeps the codec a stable fixed point:
		// a re-marshaled empty slice round-trips deep-equal instead of flipping
		// between []Block{} and nil.
		return nil, nil
	}
	blocks := make([]Block, 0, len(raws))
	for _, r := range raws {
		// Recursion point for nested content (ToolResultBlock.Content). Nesting
		// depth is bounded by encoding/json's internal max-depth, which surfaces
		// as a *BlockDecodeError, so no explicit depth guard is needed here — do
		// not "fix" this by swapping decoders.
		b, err := UnmarshalBlock(r)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// toolResultJSON is the wire form of ToolResultBlock; Content goes through the
// slice codec so nested blocks stay tagged.
type toolResultJSON struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (t *ToolResultBlock) MarshalJSON() ([]byte, error) {
	var content json.RawMessage
	if len(t.Content) > 0 {
		c, err := MarshalBlocks(t.Content)
		if err != nil {
			return nil, err
		}
		content = c
	}
	return json.Marshal(toolResultJSON{ToolUseID: t.ToolUseID, Content: content, IsError: t.IsError})
}

func (t *ToolResultBlock) UnmarshalJSON(data []byte) error {
	var j toolResultJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	t.ToolUseID = j.ToolUseID
	t.IsError = j.IsError
	if len(j.Content) > 0 {
		blocks, err := UnmarshalBlocks(j.Content)
		if err != nil {
			return err
		}
		t.Content = blocks
	}
	return nil
}
