package command

import "github.com/looprig/harness/pkg/identity"

// Compact requests conversation compaction for one exact loop. Agency records
// whether the trusted dispatcher admitted a manual or automatic request.
type Compact struct {
	Header
	identity.Coordinates
}

func (Compact) isCommand() {}
