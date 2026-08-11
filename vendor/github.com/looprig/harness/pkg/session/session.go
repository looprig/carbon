// Package session exposes the live session data-plane and control-plane contracts.
// Session construction and restoration are owned exclusively by package rig.
package session

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/workspacestore"
)

// Session is the ordinary data-plane view of one live rig execution.
type Session interface {
	SessionID() uuid.UUID
	ActiveLoop() loop.Handle
	Loop(uuid.UUID) (loop.Handle, bool)
	Submit(context.Context, []content.Block) (uuid.UUID, error)
	SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error)
	Compact(context.Context) (uuid.UUID, error)
	CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error)
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
	RespondGate(context.Context, gate.GateResponse) error
	Interrupt(context.Context) (bool, error)
}

// GateHost is the capability to raise a HOST-OWNED gate: to put a structured
// question or an out-of-band action to a human and receive the answer directly.
//
// It is a SEPARATE contract rather than three more methods on SessionController,
// for two independent reasons.
//
// The first is segregation. Opening a gate is not part of running a session:
// almost every consumer of a SessionController — the TUI, the CLI, a test —
// submits work, watches events, and answers gates, and none of them raise one.
// Widening SessionController would force every implementation to grow three
// methods that only an integration host calls, which is the exact coupling the
// interface rules forbid.
//
// The second is that the two contracts have different holders. A SessionController
// is the session's operator. A GateHost is whatever opened a particular gate and
// is blocked on its answer — an MCP binding servicing an elicitation, say. Those
// are the two ends of the same gate, and RespondGate (on Session) is the other
// end: a client answers, the host receives. Keeping them separate keeps that
// asymmetry visible instead of collapsing both roles into one god-interface.
//
// A live session implements it, so a host obtains one by asserting on the
// controller rig returns:
//
//	host, ok := controller.(session.GateHost)
//
// The contract is host-owned gates ONLY (gate.KindForm and gate.KindOpenURL with
// gate.ResolverSession). There is deliberately no way to open a permission or
// ask-user gate through it: those are answered by resuming a parked loop, and a
// host that could mint one could park — or forge an approval against — a loop
// that is not its own. An implementation MUST refuse anything else at open time
// rather than at answer time, so a caller learns its request was invalid before a
// human is shown a prompt that can never be delivered.
type GateHost interface {
	// OpenHostGate opens g and returns its id. The gate is public and answerable
	// when it returns. The caller MUST then either AwaitGateAnswer or CloseGate;
	// abandoning it without either leaks the answer slot for the session's life.
	OpenHostGate(context.Context, uuid.UUID, gate.Gate, gate.Payload) (gate.ID, error)
	// AwaitGateAnswer blocks until the gate is answered and returns the validated
	// answer, including the form values that are absent from every durable record.
	// An answer is delivered exactly once. Cancelling the context abandons the
	// wait and frees the slot but does NOT close the gate — the gate is durable
	// state and the context is the caller's, so an opener that gives up must
	// CloseGate.
	AwaitGateAnswer(context.Context, gate.ID) (gate.Answer, error)
	// CloseGate withdraws a gate without answering it, waking any awaiter with a
	// *GateError{GateNotFound}. It is how an opener cleans up after a cancelled or
	// timed-out request.
	CloseGate(context.Context, gate.ID, gate.CloseReason) error
}

// SessionController is the trusted policy and lifecycle view of a Session.
type SessionController interface {
	Session
	SetActiveLoop(context.Context, uuid.UUID) error
	LoopController(uuid.UUID) (loop.Controller, bool)
	CheckpointWorkspace(context.Context) (workspacestore.Ref, error)
	RestoreWorkspace(context.Context, workspacestore.Ref) error
	Shutdown(context.Context) error
}
