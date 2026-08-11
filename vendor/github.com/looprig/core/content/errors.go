package content

import "fmt"

// UnknownBlockTypeError is returned by the codec when serialized bytes carry a
// tag with no concrete type (including the empty tag). The restore path is an
// untrusted boundary; callers fail secure on this error.
type UnknownBlockTypeError struct{ Type BlockType }

func (e *UnknownBlockTypeError) Error() string {
	return fmt.Sprintf("content: unknown block type %q", string(e.Type))
}

// NilBlockError is returned by MarshalBlock when a Block holds a typed-nil
// payload pointer (e.g. (*TextBlock)(nil)). Construction always uses non-nil
// &content.X{} literals, so this indicates a caller bug; the codec fails secure
// rather than emit an empty typed block that could mask data loss on restore.
type NilBlockError struct{ Type BlockType }

func (e *NilBlockError) Error() string {
	return fmt.Sprintf("content: nil %q block payload", string(e.Type))
}

// BlockEncodeError wraps a failure to marshal a concrete block payload.
type BlockEncodeError struct {
	Type  BlockType
	Cause error
}

func (e *BlockEncodeError) Error() string {
	return fmt.Sprintf("content: encode block %q: %v", string(e.Type), e.Cause)
}
func (e *BlockEncodeError) Unwrap() error { return e.Cause }

// BlockDecodeError wraps a failure to unmarshal serialized block bytes.
type BlockDecodeError struct{ Cause error }

func (e *BlockDecodeError) Error() string { return "content: decode block: " + e.Cause.Error() }
func (e *BlockDecodeError) Unwrap() error { return e.Cause }

// BlockLimitError is returned when serialized input exceeds a codec safety cap.
type BlockLimitError struct {
	Limit string // "block_bytes" | "slice_count"
	Got   int
	Max   int
}

func (e *BlockLimitError) Error() string {
	return fmt.Sprintf("content: block input exceeds %s cap (%d > %d)", e.Limit, e.Got, e.Max)
}
