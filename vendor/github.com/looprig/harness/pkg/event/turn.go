package event

import (
	"github.com/looprig/core/content"
	model "github.com/looprig/inference/model"
)

// ModelRuntime is the single durable, secret-free description of the model
// runtime selected for a loop. It deliberately excludes endpoint and catalog
// data so journal replay and catalog repair do not depend on mutable config.
//
// APIFormat and BaseURL identify the declared transport a selection resolved
// against at the moment it was accepted. A zero/absent value on either field
// means "use the definition's declared base transport" — this is both the
// interpretation for every pre-existing journal record written before these
// fields existed, and the default for any caller that does not yet populate
// them. Both fields are unconstrained by validateModelRuntime: they always
// originate from a model.Model that already passed model.Validate() at the
// point a live change accepted it, so re-validating them here would be
// redundant, not additional safety.
type ModelRuntime struct {
	Key       model.ModelKey      `json:"key"`
	Limits    model.ContextLimits `json:"limits"`
	Effort    model.Effort        `json:"effort,omitzero"`
	APIFormat model.APIFormat     `json:"api_format,omitzero"`
	BaseURL   string              `json:"base_url,omitzero"`
}

// LoopInferenceChanged durably selects the resolved secret-free runtime for
// this loop at the next turn boundary.
type LoopInferenceChanged struct {
	enduring
	loopScoped
	Header
	Runtime ModelRuntime `json:"runtime,omitzero"`
}

// LoopModeChanged durably selects one predeclared loop mode.
type LoopModeChanged struct {
	enduring
	loopScoped
	Header
	PreviousMode string       `json:"previous_mode,omitzero"`
	Mode         string       `json:"mode,omitzero"`
	Runtime      ModelRuntime `json:"runtime,omitzero"`
}

// ExternalToolIdentity is the durable, secret-free identity of ONE external tool
// installed into a loop's external slot. Name is the model-facing tool name;
// SchemaDigest is the hex SHA-256 of the tool's compacted argument JSON Schema.
// The schema itself is deliberately NOT recorded: an external schema is attacker-
// or third-party-supplied and may carry descriptions, defaults, or examples that
// embed secrets, so only its digest crosses into the journal.
type ExternalToolIdentity struct {
	Name         string `json:"name"`
	SchemaDigest string `json:"schema_digest"`
}

// LoopExternalToolsetChanged durably records that a loop's external tool slot for
// one Source was REPLACED, effective at the loop's next turn boundary. It records
// identity only (Source + Generation + per-tool name/schema digest) — never the
// tool factories and never the schemas themselves.
//
// It is deliberately NOT a restore input: external tools are live resources (an MCP
// connection cannot be rebuilt from journal bytes), so a restored loop comes up with
// an EMPTY external slot and the composing application re-installs. The event exists
// for audit, drift detection, and catalog projection — replay folds it for its
// runtime identity only, never to reconstruct tools.
type LoopExternalToolsetChanged struct {
	enduring
	loopScoped
	Header
	Source     string                 `json:"source"`
	Generation string                 `json:"generation"`
	Tools      []ExternalToolIdentity `json:"tools,omitempty"`
}

// TurnStarted is emitted when runLoop commits a turn's initial UserMessage. It is
// the first enduring turn event. Header.Cause.CommandID is the submit command id.
// Message is the exact UserMessage committed as the first message of the turn.
type TurnStarted struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex            `json:"turn_index,omitzero"`
	Message   *content.UserMessage `json:"message,omitzero"`
}

// StepDone is the enduring event emitted when a completed step's finalized group
// is committed: the step's single AIMessage followed by its ToolResultMessages.
// It is emitted at step completion (the actor-owned commit point once the commit
// handshake lands), so it is never a lie.
type StepDone struct {
	enduring
	loopScoped
	Header
	Messages content.AgenticMessages `json:"messages,omitempty"`
}

// TurnFoldedInto is emitted when queued input folds into a mandatory
// tool-continuation request. Header.Cause.CommandID is the submit command id;
// Header.Cause.LoopID is set for a SubagentResult hand-back. Message is the folded
// user message.
type TurnFoldedInto struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex            `json:"turn_index,omitzero"`
	Message   *content.UserMessage `json:"message,omitzero"`
}

// InputCancelled is emitted when a queued input leaves the loop queue without
// committing — client retract, or a return after an abnormal turn end.
// Header.Cause.CommandID is the submit command id; Header.TurnID is the active turn
// that caused a return, or zero for a pure client retract outside a turn. Message is
// the returned/retracted user message.
type InputCancelled struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex            `json:"turn_index,omitzero"`
	Reason    CancelReason         `json:"reason,omitzero"`
	Message   *content.UserMessage `json:"message,omitzero"`
}

// RejectReason explains why a UserInput submit was refused (carried by TurnRejected).
// It is the single source of truth for submit-rejection reasons — the loop publishes
// it on the event stream; there is no command-side copy (the former
// command.Disposition reply, with its command.RejectReason, was removed).
type RejectReason uint8

const (
	// RejectUnspecified is the zero-value sentinel: NOT a reason the loop ever
	// produces. It exists so a zero-valued TurnRejected{} does not masquerade as a
	// real reason and so the json:"reason,omitzero" tag only drops a genuinely-empty
	// reason (every real reason below is non-zero and always serializes). A
	// RejectReason that compares equal to this came from a zero value, not a decision.
	RejectUnspecified RejectReason = iota
	// RejectQueueFull: the loop's inbox is at capacity.
	RejectQueueFull
	// RejectShuttingDown: the loop is shutting down and accepts no new input.
	RejectShuttingDown
	// RejectInternal: a transient internal failure (e.g. id generation); the loop is
	// healthy and the caller MAY retry.
	RejectInternal
)

// InputQueued is the Ephemeral Reply event for a UserInput accepted into the loop
// inbox but not yet assigned to a turn (it later resolves to TurnStarted,
// TurnFoldedInto, or InputCancelled). Header.Cause.CommandID is the submit command
// id. Ephemeral: it self-heals — the authoritative resolution event still follows
// if this is dropped.
type InputQueued struct {
	ephemeral
	loopScoped
	Header
}

// TurnRejected is the Enduring Reply event for a UserInput the loop refused
// (queue-full, shutting-down, or a transient internal failure). Enduring: a rejected
// user message must never silently vanish. Header.Cause.CommandID is the submit
// command id.
type TurnRejected struct {
	enduring
	loopScoped
	Header
	Reason RejectReason `json:"reason,omitzero"`
}

func (InputQueued) isEvent()  {}
func (TurnRejected) isEvent() {}

func (InputQueued) isReply()  {}
func (TurnRejected) isReply() {}

// TokenDelta is emitted for each streaming chunk from the LLM. TokenDelta and
// the ToolCallStarted/ToolCallCompleted events (in tool.go) are the Ephemeral
// events.
type TokenDelta struct {
	ephemeral
	loopScoped
	Header
	TurnIndex TurnIndex `json:"turn_index,omitzero"`
	// Chunk is content.Chunk, a sealed interface that is never serialized (chunks
	// have no wire codec — see content.Chunk); TokenDelta is an Ephemeral streaming
	// delta that never reaches the journal, so the field is tagged json:"-".
	Chunk content.Chunk `json:"-"`
}

// TurnDone is the terminal success event for a turn.
type TurnDone struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex `json:"turn_index,omitzero"`
	// Message is the complete AI response.
	Message *content.AIMessage `json:"message,omitzero"`
	// Usage is the checked sum of every completed request in this turn. Loop
	// cumulative accounting folds StepDone only, so this projection is never
	// added a second time.
	Usage content.Usage `json:"usage,omitzero"`
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors. Err
// carries the typed cause; callers may errors.As it to inspect and retry.
type TurnFailed struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex `json:"turn_index,omitzero"`
	// Err is the typed cause; an error value cannot round-trip through
	// encoding/json (no codec, no stable shape), so it is tagged json:"-" to keep
	// the journal output clean rather than emitting garbage. Callers read it
	// in-memory via errors.As; a journal records the failure via the event itself.
	Err error `json:"-"`
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
type TurnInterrupted struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex `json:"turn_index,omitzero"`
}

func (TurnStarted) isEvent()          {}
func (StepDone) isEvent()             {}
func (TurnFoldedInto) isEvent()       {}
func (InputCancelled) isEvent()       {}
func (TokenDelta) isEvent()           {}
func (TurnDone) isEvent()             {}
func (TurnFailed) isEvent()           {}
func (TurnInterrupted) isEvent()      {}
func (LoopInferenceChanged) isEvent() {}
func (LoopModeChanged) isEvent()      {}

func (LoopExternalToolsetChanged) isEvent() {}

// isReply marks the command-outcome events. Together with InputQueued/TurnRejected
// (declared above) these are exactly the five Reply events: a command issuer
// recognises its answer via ReplyTo() == its command id.
func (TurnStarted) isReply()    {}
func (TurnFoldedInto) isReply() {}
func (InputCancelled) isReply() {}
