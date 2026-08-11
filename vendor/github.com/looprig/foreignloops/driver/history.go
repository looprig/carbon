package driver

import "github.com/looprig/core/content"

// History is the complete authoritative history available from a Stream.
type History struct {
	Available bool
	Steps     []content.AgenticMessages
}
