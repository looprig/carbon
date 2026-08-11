package openairesponses

// UnsupportedBlockError is returned by an encoder when a content block has a
// concrete type this dialect does not model in that position (e.g. audio or
// document blocks). Block holds the Go type name for diagnosis.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "openairesponses: unsupported content block type " + e.Block
}

// UnsupportedConversationError is returned by the encoder when a
// conversation turn has a concrete type outside the closed
// content.Conversation union the dialect maps (user / assistant /
// tool-result / system). Conversation holds the Go type name for diagnosis.
type UnsupportedConversationError struct {
	Conversation string
}

func (e *UnsupportedConversationError) Error() string {
	return "openairesponses: unsupported conversation type " + e.Conversation
}

// ServerDecodeError reports a native Responses request body this codec
// cannot decode into the provider-neutral vocabulary: malformed shape, a
// missing required field, or a recognized-but-unsupported feature. Reason is
// a short machine-checkable diagnostic code; Detail elaborates for
// logs/messages.
type ServerDecodeError struct {
	Reason string
	Detail string
}

func (e *ServerDecodeError) Error() string {
	msg := "openairesponses: invalid request: " + e.Reason
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
	return "openairesponses: duplicate JSON object key " + e.Key
}

// StreamTerminatedError is returned by StreamEncoder.WriteChunk, Finish, or
// Fail once the stream has already been terminated by a prior Finish or Fail
// call, per the single-termination-ownership rule in codec.StreamEncoder.
type StreamTerminatedError struct{}

func (e *StreamTerminatedError) Error() string {
	return "openairesponses: stream already terminated"
}

// UnsupportedChunkError is returned when a content.Chunk has a concrete type
// this dialect's stream encoder does not model. content.Chunk is a sealed
// interface, so this only guards against future variants added to the
// vocabulary.
type UnsupportedChunkError struct {
	Chunk string
}

func (e *UnsupportedChunkError) Error() string {
	return "openairesponses: unsupported stream chunk type " + e.Chunk
}

// StreamAPIError reports a native `response.failed` event received after a
// streaming request crossed the successful HTTP-status boundary. It retains
// only the provider's structured error code and message, never the raw
// response frame.
type StreamAPIError struct {
	Code    string
	Message string
}

func (e *StreamAPIError) Error() string {
	message := "openairesponses: stream error"
	if e.Code != "" {
		message += " (" + e.Code + ")"
	}
	if e.Message != "" {
		message += ": " + e.Message
	}
	return message
}
