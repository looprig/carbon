package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

// StepIndex is the turn-local index of a step. Each turn numbers its own steps
// from 0; it is not unique across turns.
type StepIndex uint64

// stepStatus is the lifecycle of one step, mirroring loopStatus style.
// The zero value is stepStreaming: a fresh step is mid-stream until its single
// AIMessage is finalized (stepDone) or the stream produced a terminal (stepFailed).
type stepStatus int

const (
	// stepStreaming is the initial state: the step is consuming LLM chunks and has
	// not yet finalized its AIMessage.
	stepStreaming stepStatus = iota
	// stepDone marks a step whose single AIMessage was finalized into msgs[0].
	// Tool-result appends (owned by runTurn) happen after this transition.
	stepDone
	// stepFailed marks a step whose LLM cycle produced a terminal (provider error,
	// empty response, or interrupt); no AIMessage was stored.
	stepFailed
)

// stepConfig carries the dependencies of one LLM request/response cycle: the
// request to stream, the client to stream it through, and the event sink for live
// TokenDeltas. runtimeConfig/dependencies stay at this boundary; stepState owns one
// step's messages and block state.
type stepConfig struct {
	req       inference.Request
	client    inference.Client
	emit      func(event.Event)
	hooks     *hook.Runner
	agentName identity.AgentName
	cause     identity.Cause
	now       func() time.Time
}

// stepResult is the outcome of runStep: the updated step state, an independently
// owned authoritative terminal stream result when the provider supplied one,
// and, on an abnormal LLM cycle, a turn-terminal event. terminal is nil on
// success (the step's single AIMessage was finalized into state.msgs[0]); it is
// non-nil (TurnFailed / TurnInterrupted) when runTurn should stop and roll back.
type stepResult struct {
	state        stepState
	streamResult *stream.StreamResult
	terminal     event.Event
}

// stepState is the state of one step: its identity (copied from the turn), its
// turn-local index, the one-step conversation (exactly one AIMessage followed by
// zero or more ToolResultMessages), the block accumulator for the in-progress
// assistant blocks, and its lifecycle status. The zero blockState is ready to
// use.
//
// Phase 10 (Open Items A) collapsed the thin `step{state stepState}` wrapper:
// like block, it was a one-field struct with no methods and no runtime role, so
// runStep now takes stepState directly (YAGNI). The turn runtime state stays the
// real one, owned by the actor.
type stepState struct {
	// sessionID is copied from the turn's identity (shared across the session).
	sessionID uuid.UUID
	// loopID is copied from the turn's identity (the parent loop).
	loopID uuid.UUID
	// turnID is copied from the turn's identity (the parent turn).
	turnID uuid.UUID
	// id is this step's id, minted by the turn before the step runs.
	id uuid.UUID

	index StepIndex

	// msgs is one step's conversation: exactly one AIMessage followed by zero or
	// more ToolResultMessages. runStep finalizes the AIMessage into msgs[0]; runTurn
	// appends the ToolResultMessages after it.
	msgs   content.AgenticMessages
	blocks blockState
	status stepStatus
}

// newStepState builds a fresh stepState with its identity (copied from the turn)
// and turn-local index. msgs is empty and status is stepStreaming until runStep
// finalizes the AIMessage.
func newStepState(sessionID, loopID, turnID, stepID uuid.UUID, index StepIndex) stepState {
	return stepState{
		sessionID: sessionID,
		loopID:    loopID,
		turnID:    turnID,
		id:        stepID,
		index:     index,
	}
}

// runStep owns one LLM request/response cycle: it streams the request's chunks
// through a chunkProcessor (emit a live TokenDelta THEN fold into the step's
// blockState) and, after EOF, materializes EXACTLY ONE AIMessage into
// stepState.msgs[0]. Tool EXECUTION is NOT runStep's job; runTurn owns the
// ToolSet/gate registry and appends ToolResultMessages to the same step after
// runStep returns.
//
// turnIndex is the parent turn's loop-local index, carried only to stamp the
// legacy TurnIndex field on the TokenDelta and terminal events runStep emits (a
// turn-level concern; the step's own index lives in stepState.index). It is NOT
// derived from the step index.
//
// A terminal in the result (TurnFailed / TurnInterrupted) means the LLM cycle
// could not produce a usable AIMessage — a provider/stream error, an empty
// response (TurnFailed{*EmptyResponseError}), or an interrupt — and runTurn must
// stop and roll back. On terminal, state.msgs is left empty (no message stored).
//
// Each assembled tool-call block is validated (ID & Name non-empty, valid JSON
// Input). An invalid block is sanitized to "{}" IN THE STORED MESSAGE so history
// re-encodes cleanly; the raw executable view (state.blocks.ToolUses) keeps the
// RAW folded Input so RunBatch detects the invalid JSON and reports it as a
// pre-execution, model-visible tool-result error.
func runStep(ctx context.Context, cfg stepConfig, turnIndex event.TurnIndex, st stepState) (result stepResult) {
	call := hook.Call{
		Operation: hook.OperationInference,
		StartedAt: hookNow(cfg.now),
		Coordinates: identity.Coordinates{
			SessionID: st.sessionID,
			LoopID:    st.loopID,
			TurnID:    st.turnID,
			StepID:    st.id,
		},
		AgentName: cfg.agentName,
		Cause:     cfg.cause,
		Inference: &hook.InferenceData{Request: &cfg.req},
	}
	if cfg.hooks.Handles(hook.OperationInference) {
		call = hook.CloneCall(call)
	}
	hookCtx, finish, startErr := cfg.hooks.Start(ctx, call)
	if startErr != nil {
		st.status = stepFailed
		finishHook(finish, call, hookOutcome(ctx, startErr), startErr)
		return stepResult{
			state: st,
			terminal: event.TurnFailed{
				TurnIndex: turnIndex,
				Err:       safeHookError(hook.OperationInference, startErr),
			},
		}
	}
	ctx = hookCtx
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := &operationHookPanicError{Operation: hook.OperationInference}
			finishHook(finish, call, hook.OutcomeFailed, panicErr)
			st.status = stepFailed
			result = stepResult{
				state: st,
				terminal: event.TurnFailed{
					TurnIndex: turnIndex,
					Err: &event.TurnPanicError{
						Detail: fmt.Sprintf("%v", recovered),
					},
				},
			}
			return
		}
		call.Inference.AIMessage = inferenceResultMessage(result)
		call.Inference.StreamResult = result.streamResult
		err := terminalHookError(result.terminal)
		finishHook(finish, call, hookOutcome(ctx, err), err)
	}()

	sr, err := cfg.client.Stream(ctx, cfg.req)
	if err != nil {
		st.status = stepFailed
		return stepResult{state: st, terminal: streamFailure(ctx, turnIndex, err)}
	}
	defer closeStream(sr)

	// The chunkProcessor owns the per-chunk "emit TokenDelta THEN accumulate" order,
	// folding into the step's blockState.
	proc := newChunkProcessor(cfg.emit, chunkState{blocks: &st.blocks})
	for {
		chunk, nextErr := sr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			st.status = stepFailed
			return stepResult{state: st, terminal: streamFailure(ctx, turnIndex, nextErr)}
		}
		proc.process(chunk, turnIndex)
	}
	streamResult := terminalStreamResult(sr)

	// Materialize the single assistant message (thinking?, text?, then tool_use
	// blocks in ascending Index order) and the raw executable tool-use view. The
	// AIMessage's tool-use blocks are a DISTINCT allocation from rawCalls, so
	// sanitizing the stored message never mutates the raw executable Input.
	aiMsg := st.blocks.AIMessage()
	if streamResult != nil {
		aiMsg.Usage = cloneUsage(streamResult.Usage)
	}
	rawCalls := st.blocks.ToolUses()

	// A successful stream with no usable content at all (no non-empty text, no
	// non-empty thinking, no tool calls) is a failure: emit no AIMessage, return the
	// controlled TurnFailed{*EmptyResponseError} terminal instead of finalizing an
	// empty assistant response.
	if isEmptyAssistantMessage(aiMsg, rawCalls) {
		st.status = stepFailed
		return stepResult{
			state:        st,
			streamResult: streamResult,
			terminal:     event.TurnFailed{TurnIndex: turnIndex, Err: &event.EmptyResponseError{}},
		}
	}

	// Sanitize the STORED assistant message (drop zero-length text/thinking, rewrite
	// malformed tool Input to "{}"); the raw view in st.blocks keeps the unsanitized
	// Input for execution. Finalize it as the step's single AIMessage.
	aiMsg.Blocks = sanitizeAssistantBlocks(aiMsg.Blocks)
	st.msgs = content.AgenticMessages{aiMsg}
	st.status = stepDone
	return stepResult{state: st, streamResult: streamResult, terminal: nil}
}

func inferenceResultMessage(result stepResult) *content.AIMessage {
	if len(result.state.msgs) == 0 {
		return nil
	}
	message, _ := result.state.msgs[0].(*content.AIMessage)
	return message
}

// terminalStreamResult retains an independently owned snapshot of all
// authoritative provider metadata captured at clean EOF. A stream without
// terminal metadata remains unknown (nil). Usage is cloned independently from
// both the reader's snapshot and the finalized AIMessage so no producer,
// runtime, or event graph can race through a shared pointer.
func terminalStreamResult(sr *stream.StreamReader[content.Chunk]) *stream.StreamResult {
	result, ok := sr.Result()
	if !ok {
		return nil
	}
	result.Usage = cloneUsage(result.Usage)
	return &result
}

func cloneUsage(usage *content.Usage) *content.Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}
