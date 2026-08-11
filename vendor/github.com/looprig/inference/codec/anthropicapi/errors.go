package anthropicapi

// UnsupportedBlockError is returned by the encoder when a content block has a
// concrete type the Anthropic Messages API dialect does not model (e.g. audio or
// document blocks). Block holds the Go type name for diagnosis. Callers may
// errors.As to detect an unencodable block rather than string-matching.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "anthropicapi: unsupported content block type " + e.Block
}

// UnsupportedConversationError is returned by the encoder when a conversation
// turn has a concrete type outside the closed content.Conversation union the
// dialect maps (user / assistant / tool-result / system). Conversation holds the
// Go type name for diagnosis.
type UnsupportedConversationError struct {
	Conversation string
}

func (e *UnsupportedConversationError) Error() string {
	return "anthropicapi: unsupported conversation type " + e.Conversation
}

// StreamAPIError reports an Anthropic error event received after a streaming
// request crossed the successful HTTP-status boundary. It retains only the
// provider's structured error type and message, never the raw response frame.
type StreamAPIError struct {
	Type    string
	Message string
}

func (e *StreamAPIError) Error() string {
	message := "anthropicapi: stream error"
	if e.Type != "" {
		message += " (" + e.Type + ")"
	}
	if e.Message != "" {
		message += ": " + e.Message
	}
	return message
}
