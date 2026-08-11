package inference

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"math"
	"reflect"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/inference/stream"
)

// StructuredResult extracts one structured JSON object from a complete
// response and verifies that the finish reason agrees with its representation.
func StructuredResult(resp *Response) (json.RawMessage, error) {
	if resp == nil {
		return nil, malformedError(MalformedReasonNilResponse, nil)
	}

	switch resp.FinishReason {
	case stream.FinishReasonLength, stream.FinishReasonContentFilter:
		return nil, &StructuredOutputFinishError{Reason: resp.FinishReason}
	case stream.FinishReasonStop:
		if containsToolCall(resp.Message) {
			return nil, &StructuredOutputFinishError{Reason: resp.FinishReason}
		}
	case stream.FinishReasonToolUse:
		if !isTerminalToolRepresentation(resp.Message) {
			return nil, &StructuredOutputFinishError{Reason: resp.FinishReason}
		}
	case stream.FinishReasonUnknown:
	default:
		return nil, &StructuredOutputFinishError{Reason: StructuredOutputFinishReasonOther}
	}

	return StructuredMessageResult(resp.Message)
}

// StructuredMessageResult extracts exactly one JSON-object representation
// from assistant text fragments or one reserved terminal-tool input. Thinking
// blocks are ignored. The returned bytes are compacted and independently owned.
func StructuredMessageResult(msg *content.AIMessage) (json.RawMessage, error) {
	if msg == nil {
		return nil, malformedError(MalformedReasonNilMessage, nil)
	}
	if msg.Role != content.RoleAssistant {
		return nil, malformedError(MalformedReasonWrongRole, nil)
	}

	text := newStructuredTextCollector()
	var terminalInput json.RawMessage
	textSeen := false
	terminalCount := 0
	ordinaryCount := 0

	for _, block := range msg.Blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			textSeen = true
			text.add(typed.Text)
		case *content.ThinkingBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			if typed.Name == StructuredOutputToolName {
				terminalCount++
				terminalInput = typed.Input
			} else {
				ordinaryCount++
			}
		case *content.ImageBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			return nil, malformedError(MalformedReasonInvalidBlock, nil)
		case *content.AudioBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			return nil, malformedError(MalformedReasonInvalidBlock, nil)
		case *content.DocumentBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			return nil, malformedError(MalformedReasonInvalidBlock, nil)
		case *content.ToolResultBlock:
			if typed == nil {
				return nil, malformedError(MalformedReasonNilBlock, nil)
			}
			return nil, malformedError(MalformedReasonInvalidBlock, nil)
		default:
			return nil, malformedError(MalformedReasonNilBlock, nil)
		}
	}

	if textSeen && terminalCount+ordinaryCount > 0 {
		return nil, malformedError(MalformedReasonAmbiguous, nil)
	}
	if terminalCount > 1 || terminalCount == 1 && ordinaryCount > 0 {
		return nil, malformedError(MalformedReasonAmbiguous, nil)
	}
	if ordinaryCount > 0 {
		return nil, malformedError(MalformedReasonInvalidRepresentation, nil)
	}
	if textSeen {
		if text.tooLarge {
			return nil, text.malformedError(MalformedReasonTooLarge)
		}
		return parseStructuredObject(text.buffer.Bytes())
	}
	if terminalCount == 1 {
		if len(terminalInput) > MaxStructuredResultBytes {
			return nil, malformedError(MalformedReasonTooLarge, terminalInput)
		}
		return parseStructuredObject(terminalInput)
	}
	return nil, malformedError(MalformedReasonEmpty, nil)
}

// DecodeOutput extracts and strictly decodes a response into a non-nil concrete
// pointer. Required and other domain invariants remain the caller's concern.
func DecodeOutput(resp *Response, out any) error {
	raw, err := StructuredResult(resp)
	if err != nil {
		return err
	}
	return decodeStructuredOutput(raw, out)
}

// DecodeMessageOutput extracts and strictly decodes a message into a non-nil
// concrete pointer. Required and other domain invariants remain caller-owned.
func DecodeMessageOutput(msg *content.AIMessage, out any) error {
	raw, err := StructuredMessageResult(msg)
	if err != nil {
		return err
	}
	return decodeStructuredOutput(raw, out)
}

func parseStructuredObject(raw []byte) (json.RawMessage, error) {
	if len(raw) > MaxStructuredResultBytes {
		return nil, malformedError(MalformedReasonTooLarge, raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, malformedError(MalformedReasonEmpty, raw)
	}
	if !utf8.Valid(trimmed) || !json.Valid(trimmed) {
		return nil, malformedError(MalformedReasonMalformedJSON, raw)
	}
	if trimmed[0] != '{' {
		return nil, malformedError(MalformedReasonRootNotObject, raw)
	}
	_, duplicate, err := findDuplicateObjectMember(trimmed, false)
	if err != nil || duplicate {
		return nil, malformedError(MalformedReasonMalformedJSON, raw)
	}

	buffer := bytes.NewBuffer(make([]byte, 0, len(trimmed)))
	if err := json.Compact(buffer, trimmed); err != nil {
		return nil, malformedError(MalformedReasonMalformedJSON, raw)
	}
	return json.RawMessage(buffer.Bytes()), nil
}

func malformedError(reason MalformedStructuredOutputReason, raw []byte) error {
	return &MalformedStructuredOutputError{
		ReasonCode: reason,
		Length:     len(raw),
		SHA256:     sha256.Sum256(raw),
	}
}

func containsToolCall(msg *content.AIMessage) bool {
	if msg == nil {
		return false
	}
	for _, block := range msg.Blocks {
		if tool, ok := block.(*content.ToolUseBlock); ok && tool != nil {
			return true
		}
	}
	return false
}

func isTerminalToolRepresentation(msg *content.AIMessage) bool {
	if msg == nil || msg.Role != content.RoleAssistant {
		return false
	}
	terminalCount := 0
	for _, block := range msg.Blocks {
		switch typed := block.(type) {
		case *content.ThinkingBlock:
			if typed == nil {
				return false
			}
		case *content.ToolUseBlock:
			if typed == nil || typed.Name != StructuredOutputToolName {
				return false
			}
			terminalCount++
		default:
			return false
		}
	}
	return terminalCount == 1
}

func decodeStructuredOutput(raw json.RawMessage, out any) error {
	if out == nil {
		return &SchemaValidationError{Field: SchemaFieldOutput, ReasonCode: SchemaReasonInvalidTarget}
	}
	target := reflect.ValueOf(out)
	if target.Kind() != reflect.Pointer || target.IsNil() || target.Elem().Kind() != reflect.Struct {
		return &SchemaValidationError{Field: SchemaFieldOutput, ReasonCode: SchemaReasonInvalidTarget}
	}

	scratch := reflect.New(target.Elem().Type())
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(scratch.Interface()); err != nil {
		return &SchemaValidationError{Field: SchemaFieldOutput, ReasonCode: SchemaReasonDecodeFailed}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &SchemaValidationError{Field: SchemaFieldOutput, ReasonCode: SchemaReasonDecodeFailed}
	}
	target.Elem().Set(scratch.Elem())
	return nil
}

type structuredTextCollector struct {
	buffer      bytes.Buffer
	digest      hash.Hash
	length      int
	tooLarge    bool
	digestValid bool
}

func newStructuredTextCollector() structuredTextCollector {
	return structuredTextCollector{}
}

func (c *structuredTextCollector) add(fragment string) {
	if c.tooLarge {
		c.length = saturatedLengthAdd(c.length, len(fragment))
		c.digestValid = writeHashString(c.digest, fragment) && c.digestValid
		return
	}

	if len(fragment) <= MaxStructuredResultBytes-c.buffer.Len() {
		c.buffer.WriteString(fragment)
		c.length += len(fragment)
		return
	}

	c.tooLarge = true
	c.length = saturatedLengthAdd(c.buffer.Len(), len(fragment))
	c.digest = sha256.New()
	c.digestValid = writeHashBytes(c.digest, c.buffer.Bytes()) && writeHashString(c.digest, fragment)
	c.buffer = bytes.Buffer{}
}

func (c *structuredTextCollector) malformedError(reason MalformedStructuredOutputReason) error {
	var sum [sha256.Size]byte
	if c.digestValid {
		copy(sum[:], c.digest.Sum(nil))
	}
	return &MalformedStructuredOutputError{ReasonCode: reason, Length: c.length, SHA256: sum}
}

func saturatedLengthAdd(current, added int) int {
	if added > math.MaxInt-current {
		return math.MaxInt
	}
	return current + added
}

func writeHashBytes(digest hash.Hash, value []byte) bool {
	written, err := digest.Write(value)
	return err == nil && written == len(value)
}

func writeHashString(digest hash.Hash, value string) bool {
	const chunkBytes = 32 << 10
	for len(value) > 0 {
		length := min(len(value), chunkBytes)
		if !writeHashBytes(digest, []byte(value[:length])) {
			return false
		}
		value = value[length:]
	}
	return true
}
