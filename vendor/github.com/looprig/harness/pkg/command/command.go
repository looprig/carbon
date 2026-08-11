package command

// Command is a sealed interface for all loop commands.
// Only types in this package can implement it.
type Command interface {
	isCommand()
	CommandHeader() Header
}

type CommandName string
type CommandField string

// InvalidCommandError is returned when an internal caller violates a command contract.
type InvalidCommandError struct {
	Command CommandName
	Field   CommandField
}

func (e *InvalidCommandError) Error() string {
	return "loop: invalid command: " + string(e.Command) + "." + string(e.Field) + " is required"
}
