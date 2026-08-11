package sessionruntime

import (
	"context"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

// SetMode selects one predeclared loop mode for this loop, effective at the NEXT turn
// boundary. It is the session-owned capability wrapper: it never mutates actor fields —
// it sends a command.SetLoopMode through the loop actor, which validates the mode name
// against the bound definition, durably emits event.LoopModeChanged, and applies the
// change at the next turn boundary. The actor replies the committed mode/model/effort,
// from which the live Handle view is updated so Handle.Mode()/Model() reflect the
// selection. Refusals (a faulted session, an unknown mode, a shutting-down/exited loop, a
// durable-append fault) return a typed error and change nothing.
func (h *loopHandle) SetMode(ctx context.Context, mode loop.ModeName) error {
	if err := h.owner.faultIfFaulted(); err != nil {
		return err
	}
	id, err := h.owner.newCommandID()
	if err != nil {
		return err
	}
	ack := make(chan command.LoopChangeResult, 1)
	cmd := command.SetLoopMode{
		Header: command.Header{CommandID: id, CreatedAt: h.owner.stampNow()},
		Mode:   string(mode),
		Ack:    ack,
	}
	return h.deliverChange(ctx, cmd, ack)
}

// Change alters ONLY the secret-free model descriptor and/or the inference effort for this
// loop, effective at the NEXT turn boundary. Every change in the batch is folded into one
// command.ChangeLoopInference and validated+committed atomically by the actor (it emits
// event.LoopInferenceChanged and applies model+effort at the next turn boundary). An empty
// batch is refused up front with ChangeNoChanges (no command sent). Like SetMode it never
// mutates actor fields and returns a typed error on any refusal, changing nothing.
func (h *loopHandle) Change(ctx context.Context, changes ...loop.Change) error {
	if err := h.owner.faultIfFaulted(); err != nil {
		return err
	}
	cmd := foldChanges(changes)
	if !cmd.SetModel && !cmd.SetEffort {
		return &loop.ChangeError{Kind: loop.ChangeNoChanges}
	}
	id, err := h.owner.newCommandID()
	if err != nil {
		return err
	}
	ack := make(chan command.LoopChangeResult, 1)
	cmd.Header = command.Header{CommandID: id, CreatedAt: h.owner.stampNow()}
	cmd.Ack = ack
	return h.deliverChange(ctx, cmd, ack)
}

// foldChanges folds a batch of loop.Change into a single command.ChangeLoopInference,
// last-write-wins per field: the final ChangeModel sets Model+SetModel, the final
// ChangeEffort sets Effort+SetEffort. A nil element is skipped defensively (a nil
// sealed-interface value would panic on projection). The batch's atomicity is enforced by
// the actor, which validates the resulting model+effort before applying anything.
func foldChanges(changes []loop.Change) command.ChangeLoopInference {
	var cmd command.ChangeLoopInference
	for _, c := range changes {
		if c == nil {
			continue
		}
		if model, ok := c.InferenceModel(); ok {
			cmd.Model = model
			cmd.SetModel = true
		}
		if effort, ok := c.InferenceEffort(); ok {
			cmd.Effort = effort
			cmd.SetEffort = true
		}
	}
	return cmd
}

// deliverChange sends a change command to the loop actor and waits for its committed reply,
// updating the live Handle view on success. Transport escapes map to typed *loop.ChangeError
// (the loop exited, or ctx cancelled); a validation/persistence refusal is the actor's typed
// error carried on the reply. On success it records the committed mode/model so
// Handle.Mode()/Model() reflect the new selection.
func (h *loopHandle) deliverChange(ctx context.Context, cmd command.Command, ack <-chan command.LoopChangeResult) error {
	select {
	case h.backend.CommandSink() <- cmd:
	case <-h.backend.DoneChan():
		return &loop.ChangeError{Kind: loop.ChangeLoopExited}
	case <-ctx.Done():
		return &loop.ChangeError{Kind: loop.ChangeContextDone, Cause: ctx.Err()}
	}
	select {
	case res := <-ack:
		if res.Err != nil {
			return res.Err
		}
		h.setLiveView(loop.ModeName(res.Mode), res.Model)
		return nil
	case <-h.backend.DoneChan():
		return &loop.ChangeError{Kind: loop.ChangeLoopExited}
	case <-ctx.Done():
		return &loop.ChangeError{Kind: loop.ChangeContextDone, Cause: ctx.Err()}
	}
}

// liveViewFor computes the initial live Handle view (mode + model) for a loop coming up
// under a restore fold: the restored mode (else the definition's initial mode), and the
// restored direct-inference model (else that mode's resolved model). It mirrors the actor's
// effective-config precedence — a mode change resets the model, a later inference change
// overrides it — so a restored loop's Handle reports what its next turn will run under.
func liveViewFor(bound loop.BoundDefinition, ri restoredInference) (loop.ModeName, model.Model) {
	mode := bound.InitialMode()
	if ri.HasMode {
		mode = ri.Mode
	}
	model := bound.Model()
	if bm, ok := bound.Mode(mode); ok {
		model = bm.Model
	}
	if ri.HasRuntime {
		model = applyModelRuntime(model, ri.Runtime)
	}
	return mode, model
}

// applyModelRuntime folds a durable ModelRuntime onto a base model to compute the
// lightweight reported Handle.Mode()/Model() view. Kept in lockstep with
// loopruntime.NewRestoredWithRuntime (internal/loopruntime/restored.go), which grafts the
// SAME fields onto the actor's REAL running model — see the design doc's "Both grafting
// sites, kept in lockstep". A divergent edit here would leave the reported Handle view out
// of sync with what the actor actually runs.
func applyModelRuntime(model model.Model, runtime event.ModelRuntime) model.Model {
	if runtime.APIFormat != "" {
		model.APIFormat = runtime.APIFormat
	}
	if runtime.BaseURL != "" {
		model.BaseURL = runtime.BaseURL
	}
	model.Provider = runtime.Key.Provider
	model.Name = runtime.Key.Model
	model.Limits = runtime.Limits
	model.Sampling = model.Sampling.Clone()
	model.Sampling.Effort = runtime.Effort
	return model
}
