package loop

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// Handle is the read-only identity and inference view of a live loop.
type Handle interface {
	ID() uuid.UUID
	Mode() ModeName
	Model() model.Model
}

// ModeCatalog is the optional read-only selectable-mode view of a live loop.
// The empty ModeName identifies the base mode. Implementations return a
// defensive copy so callers cannot mutate the bound definition.
type ModeCatalog interface {
	Modes() []ModeName
}

// Controller is the trusted mutation surface of a live loop. Changes are applied
// by the actor at a turn boundary; Task 9 provides that behavior.
type Controller interface {
	Handle
	SetMode(context.Context, ModeName) error
	Change(context.Context, ...Change) error
	// Interrupt cancels this loop's current turn AND every loop below it in the delegate
	// subtree, marking the whole subtree interrupt-pending so a parent whose interrupted
	// delegate wait resolves cannot open a fresh delegate step. It is the subtree-scoped
	// counterpart to the session-wide Session.Interrupt and the single-child agent
	// interrupt. The runtime holds an admission barrier over the subtree until it is idle.
	Interrupt(context.Context) error
}

// ExternalToolset is one atomic replacement of a loop's external tool slot.
// Source namespaces the slot (e.g. "mcp"): a replacement REPLACES that source's
// whole generation and never touches another source's, nor the loop definition's
// immutable declared tools. Generation is an opaque caller-computed identity digest
// recorded durably so an operator can tell which catalog a turn ran under; it is
// never interpreted by the runtime. Definitions are live factories built with the
// loop's own bindings — they are never serialized.
//
// An empty Definitions is legal and meaningful: it clears the source's slot.
type ExternalToolset struct {
	Source      string
	Generation  string
	Definitions []tool.Definition
}

// ExternalToolInstaller is the OPTIONAL trusted surface for replacing a loop's
// external toolset at a turn boundary. It is deliberately separate from Controller
// (like ModeCatalog) rather than folded into it: only a composition root wiring an
// external tool source needs it, and every existing Controller implementation must
// keep compiling. Callers type-assert for it and fail closed when it is absent.
//
// The replacement is atomic and applies at the loop's NEXT turn boundary — a turn in
// flight keeps the toolset it started under. Every refusal (an unbuildable
// definition, a name colliding with a declared tool, a shutting-down loop) leaves
// the prior generation installed and changes nothing.
type ExternalToolInstaller interface {
	ReplaceExternalTools(context.Context, ExternalToolset) error
}

// Change is a sealed immutable loop-inference change. Runtime implementations
// inspect the two read-only projections and validate a whole batch atomically.
type Change interface {
	InferenceModel() (model.Model, bool)
	InferenceEffort() (model.Effort, bool)
	change()
}

type modelChange struct{ model model.Model }

func (modelChange) change()                               {}
func (c modelChange) InferenceModel() (model.Model, bool) { return cloneModel(c.model), true }
func (modelChange) InferenceEffort() (model.Effort, bool) { return model.EffortNone, false }

type effortChange struct{ effort model.Effort }

func (effortChange) change()                                 {}
func (effortChange) InferenceModel() (model.Model, bool)     { return model.Model{}, false }
func (c effortChange) InferenceEffort() (model.Effort, bool) { return c.effort, true }

// ChangeModel selects a new secret-free model descriptor. Controllers validate it
// atomically with the other changes before applying anything.
func ChangeModel(model model.Model) Change { return modelChange{model: cloneModel(model)} }

// ChangeEffort selects a new inference effort. Controllers validate it atomically.
func ChangeEffort(effort model.Effort) Change { return effortChange{effort: effort} }

// ChangeErrorKind classifies why a SetMode or Change was refused. Every kind is a
// fail-secure refusal: the change is NOT applied (no partial apply) and no enduring
// event is emitted (except when the durable append itself is the failure, in which case
// the append faulted the session and the change is not applied).
type ChangeErrorKind string

const (
	// ChangeInvalidMode: the requested mode name is not a predeclared mode of the loop
	// definition (nor the base mode).
	ChangeInvalidMode ChangeErrorKind = "invalid_mode"
	// ChangeInvalidModel: the requested model descriptor failed structural validation.
	ChangeInvalidModel ChangeErrorKind = "invalid_model"
	// ChangeInvalidEffort: the requested effort is not a known effort level.
	ChangeInvalidEffort ChangeErrorKind = "invalid_effort"
	// ChangeNoChanges: a Change batch selected neither a model nor an effort.
	ChangeNoChanges ChangeErrorKind = "no_changes"
	// ChangeLoopShuttingDown: the loop is shutting down and admits no configuration change.
	ChangeLoopShuttingDown ChangeErrorKind = "loop_shutting_down"
	// ChangeLoopExited: the loop's actor has exited, so no change can be delivered.
	ChangeLoopExited ChangeErrorKind = "loop_exited"
	// ChangeContextDone: the caller's context was cancelled before the change committed.
	ChangeContextDone ChangeErrorKind = "context_done"
	// ChangeDurableAppendFailed: the enduring change event's required durable append
	// failed (the session faulted), so the change was NOT applied.
	ChangeDurableAppendFailed ChangeErrorKind = "durable_append_failed"
	// ChangeInvalidExternalSource: the external toolset's Source is empty, over-long,
	// or not a valid slot name.
	ChangeInvalidExternalSource ChangeErrorKind = "invalid_external_source"
	// ChangeInvalidExternalGeneration: the external toolset's Generation is empty or
	// over-long. A generation is required — an unidentified toolset cannot be audited.
	ChangeInvalidExternalGeneration ChangeErrorKind = "invalid_external_generation"
	// ChangeExternalBuildFailed: at least one external definition failed to Build (or
	// to describe itself). NOTHING was installed — the prior generation stays.
	ChangeExternalBuildFailed ChangeErrorKind = "external_build_failed"
	// ChangeExternalToolCollision: an external tool's model-facing name collides with a
	// declared tool of the loop definition, with another tool in the same replacement,
	// or with a tool installed by a different source. The whole replacement is refused
	// so an external tool can never shadow a declared one.
	ChangeExternalToolCollision ChangeErrorKind = "external_tool_collision"
	// ChangeExternalToolsUnsupported: this loop cannot host external tools (it is a
	// foreign loop whose toolset is owned by the foreign agent, so harness holds no
	// tool bindings for it).
	ChangeExternalToolsUnsupported ChangeErrorKind = "external_tools_unsupported"
)

// ChangeError is the typed refusal returned by Controller.SetMode / Controller.Change.
// Mode carries the offending mode name for ChangeInvalidMode; Cause chains an underlying
// validation or persistence error where one exists. Callers errors.As it to distinguish
// a user error (invalid mode/model/effort) from a lifecycle refusal (shutting down /
// exited) from a persistence fault.
// Tool carries the offending model-facing tool name for ChangeExternalToolCollision
// and ChangeExternalBuildFailed, so a caller can report which tool refused the batch
// without parsing the message.
type ChangeError struct {
	Kind  ChangeErrorKind
	Mode  ModeName
	Tool  string
	Cause error
}

func (e *ChangeError) Error() string {
	msg := "loop: change refused (" + string(e.Kind) + ")"
	if e.Mode != "" {
		msg += ": mode=" + string(e.Mode)
	}
	if e.Tool != "" {
		msg += ": tool=" + e.Tool
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *ChangeError) Unwrap() error { return e.Cause }
