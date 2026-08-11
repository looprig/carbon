package presentation

// AgentHolder is the narrow, read-only view of a presentation shell that the CLI
// composition root (runtime/run.go — a DIFFERENT package) type-asserts against at teardown:
// it exposes ONLY the live Agent, so Run can bound a best-effort Close of whichever agent
// a /clear may have swapped in, without depending on either concrete shell type. It returns
// nil after a failed /clear handoff because the prior agent was already closed and no
// replacement exists. Both
// Screen and Screen satisfy it through the Agent() method promoted from the embedded
// sessionCore (a value receiver), so Run asserts the final tea.Model against this one
// interface and teardown works whichever shell runtime.Run wires. It is exported because the
// cross-package assertion in runtime/run.go needs the name, and deliberately tiny — one method
// — so the composition root depends on nothing it does not use (interface segregation).
type AgentHolder interface {
	Agent() Agent
}

// TerminalErrorHolder is the minimal final-model surface runtime.Run uses to distinguish a
// clean Bubble Tea quit from a fatal /clear handoff. Bubble Tea itself returns nil when the
// model intentionally emits tea.Quit, so the model must retain the cause.
type TerminalErrorHolder interface {
	TerminalError() error
}

// HandoffFinalizer is the final-model lifecycle barrier runtime.Run crosses before returning to
// a composition root that may close stores used by an asynchronous /clear open or deferred
// replacement close.
type HandoffFinalizer interface {
	FinalizeHandoff() error
}

// Compile-time assertion that the presentation shell satisfies AgentHolder, so runtime/run.go's
// teardown assertion resolves for the wired shell (Screen). Agent() is a value receiver
// on the embedded sessionCore, so the zero-value struct literal satisfies the interface — no
// fields are needed to promote it.
var (
	_ AgentHolder         = Screen{}
	_ TerminalErrorHolder = Screen{}
	_ HandoffFinalizer    = Screen{}
)
