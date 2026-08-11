package loopruntime

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// cloneCompactionTerminal gives one terminal graph a new owner. It copies the
// mutable waiter backing array and, for a commit, the complete summary message
// graph; scalar identity and measurement values copy by value.
func cloneCompactionTerminal(value event.Event, attemptID event.CompactAttemptID) (event.Event, error) {
	switch terminal := value.(type) {
	case event.CompactionCommitted:
		terminal.WaiterCommandIDs = append([]uuid.UUID(nil), terminal.WaiterCommandIDs...)
		terminal.Summary = cloneUserMessage(terminal.Summary)
		return terminal, nil
	case event.CompactionRejected:
		terminal.WaiterCommandIDs = append([]uuid.UUID(nil), terminal.WaiterCommandIDs...)
		return terminal, nil
	default:
		return nil, &CompactionFinalizationError{
			Kind: CompactionFinalizationTerminalClone, AttemptID: attemptID,
		}
	}
}
