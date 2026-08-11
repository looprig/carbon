package stream

// StreamFrame is a raw stream event: an optional event name, optional metadata, and
// the data payload. It is wire-level rather than semantic.
type StreamFrame struct {
	Name     string
	Metadata map[string]string
	Data     []byte
}
