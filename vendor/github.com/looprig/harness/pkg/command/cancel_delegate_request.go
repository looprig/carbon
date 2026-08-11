package command

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

const (
	CommandCancelDelegateRequest CommandName  = "CancelDelegateRequest"
	CancelDelegateRequestAck     CommandField = "Ack"
)

// DelegateCancelResult is the actor-authoritative outcome of a targeted request
// cancellation. It is transient control-plane state and is never serialized.
type DelegateCancelResult uint8

const (
	DelegateCancelNoop DelegateCancelResult = iota
	DelegateCancelQueued
	DelegateCancelActive
)

// CancelDelegateRequest atomically cancels one managed request on one loop.
type CancelDelegateRequest struct {
	Header
	identity.Coordinates
	TargetCommandID uuid.UUID                   `json:"target_command_id,omitzero"`
	Ack             chan<- DelegateCancelResult `json:"-"`
}

func (CancelDelegateRequest) isCommand() {}

func (c CancelDelegateRequest) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandCancelDelegateRequest, Field: CancelDelegateRequestAck}
	}
	return nil
}
