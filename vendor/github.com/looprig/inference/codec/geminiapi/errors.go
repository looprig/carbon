package geminiapi

// EncodeError is a failure while translating an inference.Request into the Gemini wire
// body — an unknown conversation type or a JSON marshal failure. Typed per
// CLAUDE.md so callers can errors.As it to distinguish an encode fault from a
// transport or API error.
type EncodeError struct {
	Reason string
	Err    error
}

func (e *EncodeError) Error() string {
	if e.Err != nil {
		return "gemini: encode: " + e.Reason + ": " + e.Err.Error()
	}
	return "gemini: encode: " + e.Reason
}

func (e *EncodeError) Unwrap() error { return e.Err }

// UnsupportedBlockError is returned by the encoder when a user or model content
// block has a concrete type the Gemini generateContent dialect does not model
// (e.g. audio or document blocks). Block holds the Go type name for diagnosis.
// Fail-secure per CLAUDE.md and consistent with the sibling anthropicapi codec:
// an unmodeled block is refused, never silently dropped, so the model never
// receives less than the caller sent. Callers may errors.As to detect it.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "gemini: unsupported content block type " + e.Block
}

// DecodeError is a failure while parsing a Gemini response body into a
// provider-neutral Response (a JSON unmarshal failure). The distinct
// "no candidates" case returns *failure.APIError instead, matching the sibling
// OpenAI codec so the transport and callers treat every dialect uniformly.
type DecodeError struct {
	Reason string
	Err    error
}

func (e *DecodeError) Error() string {
	if e.Err != nil {
		return "gemini: decode: " + e.Reason + ": " + e.Err.Error()
	}
	return "gemini: decode: " + e.Reason
}

func (e *DecodeError) Unwrap() error { return e.Err }

// ServerDecodeError reports a native generateContent/streamGenerateContent
// request this codec cannot decode into the provider-neutral vocabulary:
// malformed shape, an unrecognized route, or a recognized-but-unsupported
// feature. Reason is a short machine-checkable diagnostic code; Detail
// elaborates for logs/messages.
type ServerDecodeError struct {
	Reason string
	Detail string
}

func (e *ServerDecodeError) Error() string {
	msg := "gemini: invalid request: " + e.Reason
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
	return "gemini: duplicate JSON object key " + e.Key
}

// StreamTerminatedError is returned by StreamEncoder.WriteChunk, Finish, or
// Fail once the stream has already been terminated by a prior Finish or Fail
// call, per the single-termination-ownership rule in codec.StreamEncoder.
type StreamTerminatedError struct{}

func (e *StreamTerminatedError) Error() string {
	return "gemini: stream already terminated"
}

// UnsupportedChunkError is returned when a content.Chunk has a concrete type
// this dialect's stream encoder does not model. content.Chunk is a sealed
// interface, so this only guards against future variants added to the
// vocabulary.
type UnsupportedChunkError struct {
	Chunk string
}

func (e *UnsupportedChunkError) Error() string {
	return "gemini: unsupported stream chunk type " + e.Chunk
}
