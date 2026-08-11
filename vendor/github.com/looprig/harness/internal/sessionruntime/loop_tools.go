package sessionruntime

import (
	"context"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// The handle satisfies the optional installer interface, so an application holding the
// loop.Controller from Session.LoopController discovers the capability by type
// assertion — exactly how loop.ModeCatalog is discovered. Asserted at compile time so
// a signature drift breaks the build rather than silently disabling the capability at
// runtime (a failed assertion would look identical to "not supported").
var _ loop.ExternalToolInstaller = (*loopHandle)(nil)

// Bounds mirrored from the durable event's validator. They are enforced HERE too so a
// bad request is refused with a typed *loop.ChangeError before anything is built or
// sent, rather than surfacing as a durable-append validation failure that would fault
// the session.
const (
	maxExternalSourceLen     = 64
	maxExternalGenerationLen = 128
)

// ReplaceExternalTools atomically replaces one source's external toolset on this loop,
// effective at the NEXT turn boundary. It is the session-owned capability wrapper and
// the implementation of loop.ExternalToolInstaller: it never mutates actor fields — it
// builds the replacement, then sends a command.ReplaceLoopExternalTools through the
// loop actor, which durably emits event.LoopExternalToolsetChanged and swaps the slot.
//
// Everything expensive happens HERE, on the caller's goroutine, deliberately:
//
//   - Build: the session owns the loop's tool.Bindings (the actor only ever receives a
//     BoundDefinition), and an external factory may perform I/O. Building on the actor
//     goroutine would stall the loop — and stall the idle detection this feature's
//     boundary semantics depend on.
//   - Info: each built tool is asked to describe itself so its model-facing name and
//     schema digest can be computed. That is also potentially I/O.
//
// Atomicity: EVERY definition is built and described before anything is sent. On any
// failure the prior generation stays installed, nothing is journaled, and a typed
// *loop.ChangeError is returned — there is never a partial swap.
func (h *loopHandle) ReplaceExternalTools(ctx context.Context, set loop.ExternalToolset) error {
	if err := h.owner.faultIfFaulted(); err != nil {
		return err
	}
	if set.Source == "" || len(set.Source) > maxExternalSourceLen {
		return &loop.ChangeError{Kind: loop.ChangeInvalidExternalSource}
	}
	if set.Generation == "" || len(set.Generation) > maxExternalGenerationLen {
		return &loop.ChangeError{Kind: loop.ChangeInvalidExternalGeneration}
	}
	// A foreign loop's toolset belongs to the foreign agent, and its backend has no
	// ReplaceLoopExternalTools arm — the command would be silently dropped and this call
	// would block on an ack that never arrives. Refuse structurally, on the ENGINE.
	//
	// Do NOT test the bindings for emptiness: production builds full tool.Bindings for
	// every engine, foreign included, so a zero-bindings check never fires. Engine is the
	// real discriminator, mirroring the compaction guard in Session.Compact.
	if h.bound == nil || h.bound.Engine() != loop.EngineNative {
		return &loop.ChangeError{Kind: loop.ChangeExternalToolsUnsupported}
	}
	tools, identities, err := h.buildExternalTools(ctx, set)
	if err != nil {
		return err
	}
	if err := h.checkDeclaredCollision(ctx, identities); err != nil {
		return err
	}
	id, err := h.owner.newCommandID()
	if err != nil {
		return err
	}
	ack := make(chan command.LoopToolsResult, 1)
	cmd := command.ReplaceLoopExternalTools{
		Header:     command.Header{CommandID: id, CreatedAt: h.owner.stampNow()},
		Source:     set.Source,
		Generation: set.Generation,
		Tools:      tools,
		Identities: identities,
		Ack:        ack,
	}
	select {
	case h.backend.CommandSink() <- cmd:
	case <-h.backend.DoneChan():
		return &loop.ChangeError{Kind: loop.ChangeLoopExited}
	case <-ctx.Done():
		return &loop.ChangeError{Kind: loop.ChangeContextDone, Cause: ctx.Err()}
	}
	select {
	case res := <-ack:
		return res.Err
	case <-h.backend.DoneChan():
		return &loop.ChangeError{Kind: loop.ChangeLoopExited}
	case <-ctx.Done():
		return &loop.ChangeError{Kind: loop.ChangeContextDone, Cause: ctx.Err()}
	}
}

// buildExternalTools builds every definition and computes its durable identity. It is
// all-or-nothing: the FIRST failure aborts and returns a typed error, so a caller can
// never observe a half-built generation. Duplicate model-facing names WITHIN the
// replacement are rejected here — the registry must stay collision-free, and a durable
// record naming the same tool twice would describe a state that cannot exist.
//
// Requirements are enforced by tool.Definition.Build itself: it validates the loop's
// bindings against the definition's Requirements and attenuates them, so an external
// definition demanding a capability this loop was not provisioned with (e.g. a
// workspace) fails closed here instead of silently binding a nil capability. An
// external tool can therefore never ESCALATE a loop's privileges — it is offered
// exactly the bindings the declared tools were bound with, and nothing more.
func (h *loopHandle) buildExternalTools(ctx context.Context, set loop.ExternalToolset) ([]tool.InvokableTool, []event.ExternalToolIdentity, error) {
	// Validate the complete immutable name projection before invoking any factory.
	// That keeps rejection atomic even when a reserved definition follows valid
	// definitions whose factories have observable work.
	for _, def := range set.Definitions {
		if def == nil {
			return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed}
		}
		for _, name := range def.ProducedToolNames() {
			if loop.IsReservedToolName(name) {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalToolCollision, Tool: inference.StructuredOutputToolName}
			}
		}
	}

	var built []tool.InvokableTool
	var identities []event.ExternalToolIdentity
	seen := make(map[string]struct{})
	for _, def := range set.Definitions {
		if def == nil {
			return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed}
		}
		instances, err := def.Build(ctx, h.bindings)
		if err != nil {
			return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Tool: def.Name(), Cause: err}
		}
		for _, instance := range instances {
			if instance == nil {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Tool: def.Name()}
			}
			info, infoErr := instance.Info(ctx)
			if infoErr != nil {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Tool: def.Name(), Cause: infoErr}
			}
			if info == nil || info.Name == "" {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Tool: def.Name()}
			}
			if loop.IsReservedToolName(info.Name) {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalToolCollision, Tool: inference.StructuredOutputToolName}
			}
			digest, digestErr := tool.SchemaDigest(info.Schema)
			if digestErr != nil {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Tool: info.Name, Cause: digestErr}
			}
			if _, dup := seen[info.Name]; dup {
				return nil, nil, &loop.ChangeError{Kind: loop.ChangeExternalToolCollision, Tool: info.Name}
			}
			seen[info.Name] = struct{}{}
			built = append(built, instance)
			identities = append(identities, event.ExternalToolIdentity{Name: info.Name, SchemaDigest: digest})
		}
	}
	return built, identities, nil
}

// checkDeclaredCollision refuses a replacement whose any tool name collides with a
// DECLARED tool of the loop definition. It checks against the union of EVERY mode's
// declared tools, not just the current mode — deliberately. Modes are selectable at any
// later turn boundary, so a name that is free in the current mode but declared in
// another would let a subsequent SetMode silently produce a shadowed registry. Checking
// the union makes "external never shadows declared" an invariant that holds under every
// future mode change, at the cost of refusing some replacements that would be safe
// today. That is the fail-secure trade.
func (h *loopHandle) checkDeclaredCollision(ctx context.Context, identities []event.ExternalToolIdentity) error {
	if len(identities) == 0 {
		return nil
	}
	declared := make(map[string]struct{})
	for _, mode := range h.bound.Modes() {
		for _, t := range mode.Tools {
			info, err := t.Info(ctx)
			if err != nil {
				// A declared tool that cannot describe itself makes the collision check
				// unanswerable. Fail closed: refuse rather than risk installing a shadow.
				return &loop.ChangeError{Kind: loop.ChangeExternalBuildFailed, Cause: err}
			}
			if info != nil {
				declared[info.Name] = struct{}{}
			}
		}
	}
	for _, id := range identities {
		if _, exists := declared[id.Name]; exists {
			return &loop.ChangeError{Kind: loop.ChangeExternalToolCollision, Tool: id.Name}
		}
	}
	return nil
}
