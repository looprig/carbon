package sessionruntime

import (
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/loop"
)

// priorityCommandBackend is the optional native-loop capability for the bounded
// Interrupt/Shutdown lane. Foreign and test backends retain the ordinary sink.
type priorityCommandBackend interface {
	PriorityCommandSink() chan<- command.Command
}

func commandSinkFor(backend loop.Backend, cmd command.Command) chan<- command.Command {
	switch cmd.(type) {
	case command.Interrupt, command.Shutdown:
		if priority, ok := backend.(priorityCommandBackend); ok {
			return priority.PriorityCommandSink()
		}
	}
	return backend.CommandSink()
}
