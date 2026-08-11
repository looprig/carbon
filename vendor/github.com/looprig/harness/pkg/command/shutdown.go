package command

const (
	CommandShutdown CommandName  = "Shutdown"
	ShutdownAck     CommandField = "Ack"
)

// Shutdown cancels the running turn (if any), delivers its terminal event, and
// exits the actor. Ack receives nil after clean exit, or *LoopTerminatedError
// if the loop's root context was cancelled before cleanup completed.
// Ack is required and must be non-nil.
type Shutdown struct {
	Header
	Ack chan<- error `json:"-"` // live reply channel; no JSON representation
}

func (Shutdown) isCommand() {}

// Validate checks that all required fields are non-nil.
func (c Shutdown) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandShutdown, Field: ShutdownAck}
	}
	return nil
}

// LoopTerminatedError is sent on Shutdown.Ack when the loop's root context was
// cancelled before the actor finished cleanup.
type LoopTerminatedError struct{ Cause error }

func (e *LoopTerminatedError) Error() string {
	return "loop: terminated by context: " + e.Cause.Error()
}
func (e *LoopTerminatedError) Unwrap() error { return e.Cause }
