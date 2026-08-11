// Package driver defines provider-neutral contracts for foreign agents.
//
// A concrete provider constructor returns an Agent. The product composition
// root combines that agent with backend configuration and installs the resulting
// builders through Harness. Per-turn prompts, workspace, permission posture,
// session selection, normalized events, and authoritative history cross this
// package boundary; provider wire formats and transcript paths do not.
package driver

import (
	"context"

	"github.com/looprig/core/content"
)

// Agent starts one turn with a foreign agent.
type Agent interface {
	Spawn(context.Context, Turn) (Stream, error)
}

// Steerer is an optional capability for agents that can inject a message into
// an active turn while retaining a host-owned fallback path. Agent
// implementations that do not support steering remain valid: callers must
// discover this interface with a type assertion and queue a normal turn when
// it is absent.
//
// The context is runtime-owned. In particular, any bounded acknowledgement
// deadline belongs in that context (or the enclosing runtime policy), never in
// SteerRequest, so model-facing request values cannot control it.
type Steerer interface {
	Steer(context.Context, SteerRequest) (SteerResult, error)
}

// Closer is optionally implemented by agents that own long-lived resources
// spanning turns. The backend invokes Close exactly once after the command
// pump exits, whether it exits from command.Shutdown or loop-context
// cancellation. Drivers that spawn a new CLI process per turn do not need to
// implement Closer.
type Closer interface {
	Close() error
}

// Turn is one turn's input to a foreign agent.
type Turn struct {
	SystemPrompt string
	ForeignSID   string
	StartNew     bool
	Input        []content.Block
	Cwd          string
	Posture      PermissionPosture
}

// Stream is the live normalized event stream and its authoritative history.
type Stream interface {
	Events() <-chan Event
	History() (History, error)
	Close() error
}

// Kind identifies a normalized foreign-agent event.
type Kind uint8

const (
	KindInit Kind = iota
	KindTextDelta
	KindThinkingDelta
	KindToolUse
	KindToolResult
	KindStepComplete
	KindTerminalOK
	KindTerminalError
	// KindModelFacingError carries a bounded, sanitized protocol failure that
	// is safe to expose to the model. Keeping it distinct from KindTerminalError
	// prevents ordinary provider failures from becoming model-facing.
	KindModelFacingError
)

// Event is the normalized event union emitted by a Stream.
type Event struct {
	Kind          Kind
	SessionID     string
	Text          string
	ToolUseID     string
	ToolName      string
	IsError       bool
	ResultPreview string
	Message       *content.AIMessage
	ErrText       string
}

// PermissionPosture is the typed, non-interactive permission mode passed to an
// agent.
type PermissionPosture uint8

const (
	PostureDefault PermissionPosture = iota
	PostureAcceptEdits
)
