package event

import "fmt"

// EmptyResponseError is the TurnFailed.Err cause when a provider returns a
// successful stream that contains no text or thinking content.
type EmptyResponseError struct{}

func (EmptyResponseError) Error() string { return "loop: empty response from provider" }

// ToolLimitError is the TurnFailed.Err cause when the agentic loop's runaway
// guard fires: the model requested another tool batch after either the
// per-turn iteration cap (LLM<->tool round-trips) or the total-call cap was
// exceeded. It is typed and secret-free (it carries only the counts), so it is
// safe to surface un-redacted in TurnFailed.Err — it never embeds raw
// messages or tool arguments. Callers may errors.As it to distinguish a runaway
// stop from a provider/network failure.
type ToolLimitError struct {
	Iterations    int
	MaxIterations int
	Calls         int
	MaxCalls      int
}

func (e *ToolLimitError) Error() string {
	return fmt.Sprintf("tool limit reached: %d/%d iterations, %d/%d calls",
		e.Iterations, e.MaxIterations, e.Calls, e.MaxCalls)
}

// TurnPanicError is the TurnFailed.Err cause when the turn goroutine panics.
// Detail is the recovered value rendered as a string.
type TurnPanicError struct{ Detail string }

func (e *TurnPanicError) Error() string {
	return "loop: panic in turn goroutine: " + e.Detail
}
