package presentation

import (
	"github.com/looprig/core/uuid"
)

// ToolStatus is the lifecycle state of a tool call rendered in the transcript.
type ToolStatus uint8

const (
	ToolRunning   ToolStatus = iota // started, no completion seen yet
	ToolOK                          // completed without error
	ToolError                       // completed with an error
	ToolCancelled                   // turn interrupted while the call was still running
)

// gateDecision is how the user resolved a tool call's permission gate. It annotates
// the card header with a verb ("Approved …" / "Denied …"). gateNone is a call that
// never prompted — ungated, or pre-approved by an existing session/workspace grant —
// and shows no verb. gatePending is a gate awaiting the user's keypress (it resolves
// to gateApproved/gateDenied before the step's StepDone, so it never reaches a
// committed card).
type gateDecision uint8

const (
	gateNone gateDecision = iota
	gatePending
	gateApproved
	gateDenied
)

// subStatus is the terminal state of a SUBAGENT loop, read from its child terminal
// event (TurnDone/TurnFailed/TurnInterrupted) — NOT from a tool result's IsError (a
// Subagent returns failures as text). It grades the nested card's done line (design
// §4). The zero value is subRunning: an outstanding child that has not yet handed back.
type subStatus uint8

const (
	subRunning     subStatus = iota // child loop still in flight (no terminal seen)
	subDone                         // child TurnDone
	subFailed                       // child TurnFailed
	subInterrupted                  // child TurnInterrupted
)

// ToolCallView is one tool call rendered as a child of its assistant segment. It
// is reconstructed from the turn event stream (ToolCallStarted / ToolCallCompleted),
// correlated by ToolExecutionID.
//
// A Subagent card carries the nested-subagent fields (Children/Steps/Agent/Task/
// SubStatus/Nested), populated at the orchestrator's StepDone from a detached
// accumulator built off the child's ENDURING events (design §1/§3). Every one of
// those fields is zero/empty for an ordinary (non-Subagent) card.
type ToolCallView struct {
	ToolExecutionID uuid.UUID
	ToolName        string       // ToolCallStarted.ToolName
	Summary         string       // ToolCallStarted.Summary (already redacted, one line)
	Permission      string       // PermissionRequest.Description for gated calls, if available
	Status          ToolStatus   // lifecycle state
	Result          []string     // capped preview lines from ToolCallCompleted; nil while running
	Decision        gateDecision // the user's permission decision, if this call prompted (else gateNone)
	// Children are the SUBAGENT's nested tool cards, reconstructed from the child's
	// StepDone groups via the PURE storedStepToolCard builder (design §3a). Empty for
	// an ordinary card and for a Subagent whose child loop never ran (spawn failure).
	Children []ToolCallView
	// Steps is the count of the child's StepDone events (its "N steps"). Zero for an
	// ordinary card.
	Steps int
	// Agent is the subagent's agent name (the child LoopStarted.AgentName). Non-empty
	// ONLY on a reconciled Subagent card; it is the field other code keys on to tell a
	// Subagent card apart from an ordinary one.
	Agent string
	// Task is the subagent's task message (its first TurnStarted.Message, truncated to
	// one line). Empty for an ordinary card.
	Task string
	// SubStatus is the child loop's terminal state (running/done/failed/interrupted),
	// read from the child terminal event — not from a tool result's IsError. subRunning
	// (the zero value) for an ordinary card.
	SubStatus subStatus
	// Nested is the depth-2 collapsed counter ("+N nested subagent steps", design §6):
	// the count of deeper StepDones attributed to this depth-1 card. Wired in Task 7;
	// zero here. Zero for an ordinary card.
	Nested int
	// spawn identifies the detached child accumulator backing a reconciled agent card.
	// It is presentation-internal metadata used to keep a card current when child events
	// arrive after the parent's StepDone; ordinary cards leave it zero.
	spawn spawnKey
}
