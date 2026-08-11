package command

import (
	model "github.com/looprig/inference/model"
)

const (
	CommandSetLoopMode         CommandName  = "SetLoopMode"
	CommandChangeLoopInference CommandName  = "ChangeLoopInference"
	SetLoopModeAck             CommandField = "Ack"
	ChangeLoopInferenceAck     CommandField = "Ack"
)

// LoopChangeResult is the loop actor's synchronous reply to a SetLoopMode or
// ChangeLoopInference command. Err is the typed failure (nil on success); on success
// Mode/Model/Effort report the EFFECTIVE mode, secret-free model descriptor, and effort
// the actor committed — the values the NEXT turn will start under. The session
// controller updates its live Handle view from these committed values so
// Handle.Mode()/Model() reflect the current selection. It rides a live channel and is
// never serialized (the ack channel is json:"-").
type LoopChangeResult struct {
	Err    error
	Mode   string
	Model  model.Model
	Effort model.Effort
}

// SetLoopMode selects one predeclared loop mode. It is a CONTROL command carried on a
// live reply channel (Ack), not a journaled wire command: its durable, replayable record
// is the event.LoopModeChanged the actor emits, which restore folds — so it is
// deliberately absent from the intent-log codec. The actor validates the mode name
// against the loop's bound definition, emits the enduring event, and applies the change
// at the NEXT turn boundary; the running turn keeps the mode it started under. An empty
// Mode names the base mode. Ack is required and must be non-nil and buffered(1).
type SetLoopMode struct {
	Header
	Mode string                  `json:"mode,omitzero"`
	Ack  chan<- LoopChangeResult `json:"-"` // live reply channel; no JSON representation
}

func (SetLoopMode) isCommand() {}

// Validate checks the reply-channel contract: Ack must be present AND buffered (cap >= 1).
// The actor delivers the reply with a single non-blocking direct send, so an unbuffered Ack
// would wedge the actor; both violations are typed so a caller can errors.As them.
func (c SetLoopMode) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandSetLoopMode, Field: SetLoopModeAck}
	}
	if cap(c.Ack) < 1 {
		return &UnbufferedAckError{Command: CommandSetLoopMode, Field: SetLoopModeAck}
	}
	return nil
}

// ChangeLoopInference changes only the secret-free model descriptor and/or the inference
// effort. Like SetLoopMode it is a CONTROL command on a live reply channel — its durable
// record is the event.LoopInferenceChanged the actor emits — so it is not in the wire
// codec. SetModel/SetEffort select which of Model/Effort the batch changes; the whole
// batch is validated atomically by the actor before anything is applied. The change takes
// effect at the NEXT turn boundary; a running turn keeps the model/effort it started
// under. Ack is required and must be non-nil and buffered(1).
type ChangeLoopInference struct {
	Header
	Model     model.Model             `json:"model,omitzero"`
	Effort    model.Effort            `json:"effort,omitzero"`
	SetModel  bool                    `json:"set_model,omitzero"`
	SetEffort bool                    `json:"set_effort,omitzero"`
	Ack       chan<- LoopChangeResult `json:"-"` // live reply channel; no JSON representation
}

func (ChangeLoopInference) isCommand() {}

// Validate checks the reply-channel contract: Ack must be present AND buffered (cap >= 1),
// like SetLoopMode. The model/effort VALUES are validated by the actor against the loop
// definition (atomically), not here.
func (c ChangeLoopInference) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandChangeLoopInference, Field: ChangeLoopInferenceAck}
	}
	if cap(c.Ack) < 1 {
		return &UnbufferedAckError{Command: CommandChangeLoopInference, Field: ChangeLoopInferenceAck}
	}
	return nil
}

// UnbufferedAckError reports that a loop-change command's live reply channel is present but
// unbuffered (cap < 1). The loop actor replies with a single non-blocking direct send, so an
// unbuffered Ack would wedge it; the contract requires a buffered(1) channel. It is distinct
// from InvalidCommandError (a MISSING channel) so the two failure modes never alias.
type UnbufferedAckError struct {
	Command CommandName
	Field   CommandField
}

func (e *UnbufferedAckError) Error() string {
	return "loop: invalid command: " + string(e.Command) + "." + string(e.Field) + " must be buffered (cap >= 1)"
}
