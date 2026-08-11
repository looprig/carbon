package loop

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
)

// Backend is the narrow turn-engine contract Session drives. Both native and
// *foreignloop.Loop satisfy it. It is deliberately the minimal subset Session uses
// (command submission, completion signalling, committed-state snapshot) — it does
// NOT expose the native loop's internal gate/commit/drain seams, which a foreign
// loop has no analogue for. Snapshot's signature is exactly *Loop.Snapshot's.
type Backend interface {
	CommandSink() chan<- command.Command
	DoneChan() <-chan struct{}
	Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error)
}
