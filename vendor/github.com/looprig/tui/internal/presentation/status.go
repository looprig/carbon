package presentation

// Status is the TUI turn-lifecycle state.
type Status uint8

const (
	StatusIdle         Status = iota // no turn; Enter sends immediately
	StatusRunning                    // turn in flight; Enter queues
	StatusInterrupting               // Interrupt issued; awaiting TurnInterrupted
	StatusResetting                  // /clear reopen in flight; Enter/queue blocked
)
