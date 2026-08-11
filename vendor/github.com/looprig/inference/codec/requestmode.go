package codec

// RequestMode is the typed encode mode a Codec receives: the wire body differs because
// streaming sets "stream": true. Typed, not a bool, per CLAUDE.md.
type RequestMode uint8

const (
	RequestModeInvoke RequestMode = iota
	RequestModeStream
)
