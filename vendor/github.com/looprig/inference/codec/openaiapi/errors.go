package openaiapi

import "strconv"

// UnsupportedBlockError is returned by the encoder when a content block has a
// concrete type the OpenAI chat completions dialect does not model in that
// position (e.g. audio or document blocks anywhere, or any non-text block in a
// text-only tool message). Block holds the Go type name for diagnosis.
// Fail-secure per CLAUDE.md and consistent with the sibling anthropicapi and
// geminiapi codecs: an unmodeled block is refused, never silently dropped, so
// the model never receives less than the caller sent. Callers may errors.As to
// detect it.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "openai: unsupported content block type " + e.Block
}

// ServerDecodeError reports a native Chat Completions request body this codec
// cannot decode into the provider-neutral vocabulary: malformed shape, a
// missing required field, or a recognized-but-unsupported feature. Reason is
// a short machine-checkable diagnostic code; Detail elaborates for
// logs/messages.
type ServerDecodeError struct {
	Reason string
	Detail string
}

func (e *ServerDecodeError) Error() string {
	msg := "openaiapi: invalid request: " + e.Reason
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// DuplicateKeyError reports a request body with a duplicate JSON object
// member name. encoding/json silently takes the last occurrence; this codec
// rejects the request instead so a client cannot smuggle a semantically
// different value past a naive review of the first occurrence.
type DuplicateKeyError struct {
	Key string
}

func (e *DuplicateKeyError) Error() string {
	return "openaiapi: duplicate JSON object key " + e.Key
}

// UnsupportedChoiceCountError reports a request that asked for more than one
// completion choice ("n" > 1). The neutral one-response contract has no
// concept of multiple parallel choices, so this fails closed rather than
// silently returning only the first choice a harness may have expected N of.
type UnsupportedChoiceCountError struct {
	N int
}

func (e *UnsupportedChoiceCountError) Error() string {
	return "openaiapi: unsupported choice count: n=" + strconv.Itoa(e.N) + " (only n=1 is supported)"
}

// StreamTerminatedError is returned by StreamEncoder.WriteChunk, Finish, or
// Fail once the stream has already been terminated by a prior Finish or Fail
// call, per the single-termination-ownership rule in codec.StreamEncoder.
type StreamTerminatedError struct{}

func (e *StreamTerminatedError) Error() string {
	return "openaiapi: stream already terminated"
}

// UnsupportedChunkError is returned when a content.Chunk has a concrete type
// this dialect's stream encoder does not model. content.Chunk is a sealed
// interface, so this only guards against future variants added to the
// vocabulary.
type UnsupportedChunkError struct {
	Chunk string
}

func (e *UnsupportedChunkError) Error() string {
	return "openaiapi: unsupported stream chunk type " + e.Chunk
}
