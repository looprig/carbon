package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

type turnOutcome struct {
	committed   content.AgenticMessages
	success     bool
	interrupted bool
	spawned     bool
	boundSID    string
}

// turnObservation is the actor mailbox unit. The producer owns the raw ACP
// read and translates it into zero or more Harness events, but it never calls
// the session publisher. Raw ordered observations remain attached so the
// steering state machine can adjudicate acknowledgements against prompt
// completion without consulting a second stream or competing goroutine.
type turnObservation struct {
	raw   driver.Observation
	event event.Event
}

type drainedTurn struct {
	assistant []*content.AIMessage
	boundSID  string
	termErr   error
	bindErr   error
	terminal  bool
	stopped   bool
}

type turnLock interface{ release() }

type turnLockOps struct {
	acquireTemporary func(loopID, cwd string) (turnLock, error)
	acquireDurable   func(sid, cwd string) (turnLock, error)
}

func productionTurnLockOps() turnLockOps {
	return turnLockOps{
		acquireTemporary: func(loopID, cwd string) (turnLock, error) { return acquireTemporaryForeignLock(loopID, cwd) },
		acquireDurable:   func(sid, cwd string) (turnLock, error) { return acquireForeignLock(sid, cwd) },
	}
}

func (l *Loop) runTurn(loopCtx context.Context, prepared preparedInput) bool {
	input := prepared.command
	cur := l.turnIndex + 1
	pub := l.publisher(loopCtx, prepared.turnID, prepared.stepID)
	user := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: input.Blocks}}
	started := event.TurnStarted{
		Header:    event.Header{Cause: identity.Cause{CommandID: input.Header.CommandID, Agency: input.Header.Agency}},
		TurnIndex: cur,
		Message:   user,
	}
	if err := l.publishActor(loopCtx, prepared.turnID, prepared.stepID, started); err != nil {
		slog.Error("foreignloop: failed to publish TurnStarted", "error", err)
		return true
	}
	turn := driver.Turn{
		SystemPrompt: l.cfg.EffectiveSystem(),
		ForeignSID:   l.sid,
		StartNew:     !l.hasSpawned,
		Input:        input.Blocks,
		Cwd:          l.backendCfg.Cwd,
		Posture:      l.backendCfg.Posture,
	}
	turnCtx, cancel := context.WithCancel(loopCtx)
	mailbox := make(chan turnObservation, 64)
	result := make(chan turnOutcome, 1)
	streamReady := make(chan driver.Stream, 1)
	machine := newSteeringMachine(turnCtx, l, pub, cur, prepared.turnID, prepared.stepID, nil)
	go l.driveTurnToMailbox(turnCtx, cancel, turn, cur, l.sidBound, mailbox, result, streamReady)
	return l.awaitTurn(loopCtx, cur, input.CommandID, prepared.turnID, prepared.stepID, cancel, pub, mailbox, result, streamReady, machine)
}

func (l *Loop) awaitTurn(loopCtx context.Context, cur event.TurnIndex, activeCommandID, turnID, stepID uuid.UUID,
	cancel context.CancelFunc, pub func(event.Event), mailbox <-chan turnObservation, result chan turnOutcome,
	streamReady <-chan driver.Stream, machine *steeringMachine,
) bool {
	if cancel != nil {
		defer cancel()
	}
	defer func() {
		if machine != nil {
			if err := machine.shutdown(); err != nil {
				machine.logFault()
			}
			machine.cleanup()
		}
	}()
	var (
		terminalHold *turnObservation
		outcome      *turnOutcome
	)
	for {
		if terminalHold != nil && (machine == nil || machine.terminalReady()) {
			if err := l.publishTurnObservation(loopCtx, cur, turnID, stepID, *terminalHold); err != nil {
				slog.Error("foreignloop: held terminal publication failed", "error", err)
				return true
			}
			terminalHold = nil
		}
		if outcome != nil && (machine == nil || machine.terminalReady()) {
			if terminalHold != nil {
				if err := l.publishTurnObservation(loopCtx, cur, turnID, stepID, *terminalHold); err != nil {
					slog.Error("foreignloop: held terminal publication failed", "error", err)
					return true
				}
				terminalHold = nil
			}
			if err := l.applyOutcome(loopCtx, cur, turnID, stepID, *outcome); err != nil {
				slog.Error("foreignloop: turn lifecycle publication failed", "error", err)
				return true
			}
			if !outcome.success && !outcome.interrupted {
				if err := l.cancelPending(loopCtx, turnID, stepID, event.CancelTurnFailed); err != nil {
					slog.Error("foreignloop: queued cancellation publication failed", "error", err)
					_ = machine.shutdown()
					return true
				}
			}
			if err := machine.shutdown(); err != nil {
				machine.logFault()
				return true
			}
			return false
		}
		commands := l.Commands
		// Once the producer has delivered its outcome, only steering
		// adjudication and the held terminal remain before this turn can
		// return to the actor pump. Do not hand a command to the legacy
		// cancellation path while its result channel has already been
		// consumed; the unbuffered command sender will be serviced next.
		if outcome != nil {
			commands = nil
		}
		timerAttempt, timerRef := (*steeringAttempt)(nil), (steeringTimer)(nil)
		deadlineAttempt, deadlineRef := (*steeringAttempt)(nil), (steeringTimer)(nil)
		if machine != nil {
			timerAttempt, timerRef = machine.active, machine.timer
			deadlineAttempt, deadlineRef = machine.active, machine.deadlineTimer
		}
		select {
		case turnOutcomeValue := <-result:
			outcome = &turnOutcomeValue
			result = nil
			if err := l.drainTurnObservations(loopCtx, cur, turnID, stepID, mailbox, machine, &terminalHold); err != nil {
				slog.Error("foreignloop: ordered observation publication failed", "error", err)
				return true
			}
			mailbox = nil
		case stream := <-streamReady:
			if machine != nil {
				if err := machine.setStream(stream); err != nil {
					machine.logFault()
					cancel()
					return true
				}
			}
			streamReady = nil
		case observation, ok := <-mailbox:
			if !ok {
				mailbox = nil
				continue
			}
			if err := l.processTurnObservation(loopCtx, cur, turnID, stepID, observation, machine, &terminalHold); err != nil {
				cancel()
				slog.Error("foreignloop: ordered observation publication failed", "error", err)
				return true
			}
		case completion := <-machine.completionsChan():
			if err := machine.complete(completion); err != nil {
				machine.logFault()
				cancel()
				return true
			}
		case <-machine.timerChan():
			if err := l.drainReadyTurnEvidence(loopCtx, cur, turnID, stepID, &mailbox, &result, &outcome, machine, &terminalHold); err != nil {
				machine.logFault()
				cancel()
				return true
			}
			if machine.active != timerAttempt || machine.timer != timerRef {
				continue
			}
			if err := machine.timeout(); err != nil {
				machine.logFault()
				cancel()
				return true
			}
		case <-machine.deadlineTimerChan():
			if err := l.drainReadyTurnEvidence(loopCtx, cur, turnID, stepID, &mailbox, &result, &outcome, machine, &terminalHold); err != nil {
				machine.logFault()
				cancel()
				return true
			}
			if machine.active != deadlineAttempt || machine.deadlineTimer != deadlineRef {
				continue
			}
			if err := machine.deadlineTimeout(); err != nil {
				machine.logFault()
				cancel()
				return true
			}
		case req := <-l.snapshots:
			req.reply <- snapshotResult{msgs: cloneMessages(l.msgs), turnIndex: l.turnIndex}
		case input := <-commands:
			if done, exit := l.handleTurnCommand(loopCtx, input, cur, activeCommandID, turnID, stepID, cancel, pub, mailbox, result, machine); done {
				return exit
			}
		case <-loopCtx.Done():
			cancel()
			if err := l.drainSteeringShutdown(loopCtx, cur, turnID, stepID, mailbox, result, machine); err != nil {
				machine.logFault()
				return true
			}
			if err := machine.shutdown(); err != nil {
				machine.logFault()
				return true
			}
			return true
		}
	}
}

func (l *Loop) handleTurnCommand(loopCtx context.Context, input command.Command, cur event.TurnIndex,
	activeCommandID, turnID, stepID uuid.UUID, cancel context.CancelFunc, pub func(event.Event),
	mailbox <-chan turnObservation, result chan turnOutcome, machine *steeringMachine,
) (bool, bool) {
	switch typed := input.(type) {
	case command.UserInput:
		if len(l.pending)+machine.pendingCount() >= loop.ManagedInputQueueCapacity {
			rejected := event.TurnRejected{Header: event.Header{Cause: identity.Cause{CommandID: typed.CommandID}}, Reason: event.RejectQueueFull}
			if err := l.publishActor(loopCtx, turnID, stepID, rejected); err != nil {
				slog.Error("foreignloop: queue rejection publication failed", "error", err)
				if typed.Accepted != nil {
					typed.Accepted <- err
				}
				cancel()
				return true, true
			}
			if typed.Accepted != nil {
				typed.Accepted <- &loop.InputRejectedError{Reason: event.RejectQueueFull}
			}
			return false, false
		}
		prepared, ok := l.prepareInput(loopCtx, typed)
		if !ok {
			return false, false
		}
		if machine != nil {
			handled, err := machine.offer(prepared)
			if handled {
				if err != nil {
					machine.logFault()
					cancel()
					return true, true
				}
				if typed.Accepted != nil {
					typed.Accepted <- nil
				}
				return false, false
			}
		}
		l.pending = append(l.pending, prepared)
		pub(event.InputQueued{Header: event.Header{Cause: identity.Cause{CommandID: typed.CommandID}}})
		if typed.Accepted != nil {
			typed.Accepted <- nil
		}
		return false, false
	case command.CancelDelegateRequest:
		if err := typed.Validate(); err != nil {
			slog.Warn("foreignloop: invalid CancelDelegateRequest", "error", err)
			return false, false
		}
		if err := command.ValidateCommand(typed); err != nil {
			typed.Ack <- command.DelegateCancelNoop
			return false, false
		}
		for i, pending := range l.pending {
			if pending.command.CommandID != typed.TargetCommandID {
				continue
			}
			queued := pending.command
			if err := l.publishActor(loopCtx, uuid.UUID{}, uuid.UUID{}, event.InputCancelled{
				Header:  event.Header{Cause: identity.Cause{CommandID: queued.CommandID, Agency: queued.Agency}},
				Reason:  event.CancelClientRetracted,
				Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: queued.Blocks}},
			}); err != nil {
				slog.Error("foreignloop: queued cancellation publication failed", "error", err)
				typed.Ack <- command.DelegateCancelNoop
				return true, true
			}
			l.pending = append(l.pending[:i], l.pending[i+1:]...)
			typed.Ack <- command.DelegateCancelQueued
			return false, false
		}
		if typed.TargetCommandID != activeCommandID {
			typed.Ack <- command.DelegateCancelNoop
			return false, false
		}
		select {
		case completed := <-result:
			ready := make(chan turnOutcome, 1)
			ready <- completed
			var captured turnOutcome
			haveOutcome := false
			err := l.drainSteeringShutdownWithOutcome(loopCtx, cur, turnID, stepID, mailbox, ready, machine, &captured, &haveOutcome)
			if err != nil {
				slog.Error("foreignloop: ordered cancellation adjudication failed", "error", err)
				typed.Ack <- command.DelegateCancelNoop
				return true, true
			}
			outcome := interruptedTurnOutcome(captured, haveOutcome)
			if err := l.applyOutcome(loopCtx, cur, turnID, stepID, outcome); err != nil {
				slog.Error("foreignloop: turn lifecycle publication failed", "error", err)
				typed.Ack <- command.DelegateCancelNoop
				return true, true
			}
			if !outcome.success && !outcome.interrupted {
				if err := l.cancelPending(loopCtx, turnID, stepID, event.CancelTurnFailed); err != nil {
					slog.Error("foreignloop: queued cancellation publication failed", "error", err)
					typed.Ack <- command.DelegateCancelNoop
					return true, true
				}
			}
			typed.Ack <- command.DelegateCancelNoop
			return true, false
		default:
		}
		cancel()
		var captured turnOutcome
		haveOutcome := false
		err := l.drainSteeringShutdownWithOutcome(loopCtx, cur, turnID, stepID, mailbox, result, machine, &captured, &haveOutcome)
		if err != nil {
			typed.Ack <- command.DelegateCancelNoop
			return true, true
		}
		outcome := interruptedTurnOutcome(captured, haveOutcome)
		if err := l.applyOutcome(loopCtx, cur, turnID, stepID, outcome); err != nil {
			typed.Ack <- command.DelegateCancelNoop
			return true, true
		}
		typed.Ack <- command.DelegateCancelActive
		return true, false
	case command.Interrupt:
		cancel()
		var captured turnOutcome
		haveOutcome := false
		err := l.drainSteeringShutdownWithOutcome(loopCtx, cur, turnID, stepID, mailbox, result, machine, &captured, &haveOutcome)
		if err != nil {
			typed.Ack <- true
			return true, true
		}
		outcome := interruptedTurnOutcome(captured, haveOutcome)
		if err := l.applyOutcome(loopCtx, cur, turnID, stepID, outcome); err != nil {
			typed.Ack <- true
			return true, true
		}
		if err := l.cancelPending(loopCtx, turnID, stepID, event.CancelTurnInterrupted); err != nil {
			slog.Error("foreignloop: queued cancellation publication failed", "error", err)
			typed.Ack <- true
			return true, true
		}
		typed.Ack <- true
		return true, false
	case command.Shutdown:
		cancel()
		if err := l.drainSteeringShutdown(loopCtx, cur, turnID, stepID, mailbox, result, machine); err != nil {
			l.closeAgent()
			typed.Ack <- err
			return true, true
		}
		if err := machine.shutdown(); err != nil {
			l.closeAgent()
			typed.Ack <- err
			return true, true
		}
		if err := l.cancelPending(loopCtx, turnID, stepID, event.CancelTurnInterrupted); err != nil {
			slog.Error("foreignloop: queued cancellation publication failed", "error", err)
			l.closeAgent()
			typed.Ack <- err
			return true, true
		}
		l.closeAgent()
		typed.Ack <- nil
		return true, true
	default:
		slog.Warn("foreignloop: dropping un-honorable command during turn", "type", fmt.Sprintf("%T", input))
		return false, false
	}
}

func (l *Loop) cancelPending(ctx context.Context, turnID, stepID uuid.UUID, reason event.CancelReason) error {
	for len(l.pending) > 0 {
		pending := l.pending[0]
		input := pending.command
		if err := l.publishActor(ctx, turnID, stepID, event.InputCancelled{
			Header:  event.Header{Cause: identity.Cause{CommandID: input.CommandID, Agency: input.Agency}},
			Reason:  reason,
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: input.Blocks}},
		}); err != nil {
			return err
		}
		l.pending = l.pending[1:]
	}
	return nil
}

func (l *Loop) applyOutcome(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID, outcome turnOutcome) error {
	l.applyBoundSID(outcome.boundSID)
	if outcome.interrupted {
		return l.publishActor(ctx, turnID, stepID, event.TurnInterrupted{TurnIndex: cur})
	}
	l.msgs = append(l.msgs, outcome.committed...)
	if outcome.spawned {
		l.hasSpawned = true
	}
	if outcome.success {
		l.turnIndex = cur
	}
	return nil
}

func interruptedTurnOutcome(captured turnOutcome, haveOutcome bool) turnOutcome {
	if !haveOutcome {
		return turnOutcome{interrupted: true}
	}
	// Cancellation is the actor command's lifecycle decision even when a
	// provider outcome raced into the mailbox. Retain only a bound SID; a
	// canceled turn must not commit provider messages as a successful turn.
	return turnOutcome{interrupted: true, boundSID: captured.boundSID}
}

func (l *Loop) applyBoundSID(sid string) {
	if sid == "" {
		return
	}
	l.sid = sid
	l.sidBound = true
	l.hasSpawned = true
}

func (l *Loop) driveTurnToMailbox(turnCtx context.Context, cancel context.CancelFunc, turn driver.Turn,
	cur event.TurnIndex, sidBound bool, mailbox chan<- turnObservation, result chan turnOutcome,
	streamReady chan<- driver.Stream,
) {
	defer close(mailbox)
	sink := func(observation turnObservation) bool {
		select {
		case mailbox <- observation:
			return true
		case <-turnCtx.Done():
			return false
		}
	}
	l.driveTurnToSink(turnCtx, cancel, turn, cur, sidBound, sink, result, productionTurnLockOps(), streamReady)
}

// driveTurnWithLocks is retained as a small compatibility seam for the lock
// lifecycle tests. It routes non-terminal observations to the supplied trace
// callback but deliberately never publishes a terminal from the producer.
func (l *Loop) driveTurnWithLocks(turnCtx context.Context, cancel context.CancelFunc, turn driver.Turn,
	cur event.TurnIndex, sidBound bool, pub func(event.Event), result chan turnOutcome, locks turnLockOps,
) {
	sink := func(observation turnObservation) bool {
		if observation.event == nil || isTurnTerminal(observation.event) {
			return true
		}
		if pub != nil {
			pub(observation.event)
		}
		return true
	}
	l.driveTurnToSink(turnCtx, cancel, turn, cur, sidBound, sink, result, locks, nil)
}

func (l *Loop) driveTurnToSink(turnCtx context.Context, cancel context.CancelFunc, turn driver.Turn,
	cur event.TurnIndex, sidBound bool, sink func(turnObservation) bool, result chan turnOutcome, locks turnLockOps,
	streamReady chan<- driver.Stream,
) {
	defer cancel()
	var (
		lock turnLock
		err  error
	)
	if sidBound {
		lock, err = locks.acquireDurable(turn.ForeignSID, l.backendCfg.Cwd)
	} else {
		lock, err = locks.acquireTemporary(l.loopID.String(), l.backendCfg.Cwd)
	}
	if err != nil {
		sink(turnObservation{event: event.TurnFailed{TurnIndex: cur, Err: err}})
		result <- turnOutcome{}
		return
	}
	var outcome turnOutcome
	defer func() {
		lock.release()
		result <- outcome
	}()
	stream, err := l.backendCfg.Agent.Spawn(turnCtx, turn)
	if err != nil {
		sink(turnObservation{event: event.TurnFailed{TurnIndex: cur, Err: &driver.SpawnError{Cause: err}}})
		return
	}
	if streamReady != nil {
		select {
		case streamReady <- stream:
		case <-turnCtx.Done():
		}
	}
	bindSID := func(sid string) error {
		boundLock, err := locks.acquireDurable(sid, l.backendCfg.Cwd)
		if err != nil {
			return err
		}
		lock.release()
		lock = boundLock
		return nil
	}
	drained := l.drainStreamToSink(stream, cur, sidBound, turn.ForeignSID, bindSID, sink)
	closeErr := stream.Close()
	spawned := sidBound || drained.boundSID != ""
	if drained.bindErr != nil {
		sink(turnObservation{event: event.TurnFailed{TurnIndex: cur, Err: errors.Join(drained.bindErr, closeErr)}})
		outcome = turnOutcome{spawned: spawned, boundSID: drained.boundSID}
		return
	}
	if turnCtx.Err() != nil {
		outcome = turnOutcome{interrupted: true, spawned: spawned, boundSID: drained.boundSID}
		return
	}
	committed := l.commitTurnToSink(stream, drained.assistant, sink)
	if turnErr := joinTurnErrors(drained.termErr, closeErr); turnErr != nil {
		sink(turnObservation{event: event.TurnFailed{TurnIndex: cur, Err: turnErr}})
		outcome = turnOutcome{committed: committed, spawned: spawned, boundSID: drained.boundSID}
		return
	}
	sink(turnObservation{event: event.TurnDone{TurnIndex: cur, Message: lastOf(drained.assistant)}})
	outcome = turnOutcome{committed: committed, success: true, spawned: spawned, boundSID: drained.boundSID}
}

func (l *Loop) drainStreamToSink(stream driver.Stream, cur event.TurnIndex, sidBound bool,
	expectedSID string, bindSID func(string) error, sink func(turnObservation) bool,
) drainedTurn {
	if ordered, ok := stream.(driver.OrderedStream); ok {
		observations := ordered.Observations()
		if observations != nil {
			return l.drainOrderedStream(observations, cur, sidBound, expectedSID, bindSID, sink)
		}
	}
	return l.drainLegacyStream(stream.Events(), cur, sidBound, expectedSID, bindSID, sink)
}

func (l *Loop) drainLegacyStream(inputs <-chan driver.Event, cur event.TurnIndex, sidBound bool,
	expectedSID string, bindSID func(string) error, sink func(turnObservation) bool,
) drainedTurn {
	mapper := newMapper(cur, l.idGen)
	var output drainedTurn
	for input := range inputs {
		if !l.consumeDriverEvent(&output, input, sidBound, &expectedSID, bindSID, mapper, sink, nil) {
			output.stopped = true
			return output
		}
	}
	if output.bindErr == nil {
		validateDrainedTurn(&output, sidBound)
	}
	return output
}

func (l *Loop) drainOrderedStream(inputs <-chan driver.Observation, cur event.TurnIndex, sidBound bool,
	expectedSID string, bindSID func(string) error, sink func(turnObservation) bool,
) drainedTurn {
	mapper := newMapper(cur, l.idGen)
	var output drainedTurn
	for observation := range inputs {
		switch typed := observation.(type) {
		case driver.UpdateObservation:
			emitted := false
			orderedSink := func(item turnObservation) bool {
				emitted = true
				return sink(item)
			}
			if !l.consumeDriverEvent(&output, typed.Event, sidBound, &expectedSID, bindSID, mapper, orderedSink, observation) {
				output.stopped = true
				return output
			}
			if !emitted && !sink(turnObservation{raw: observation}) {
				output.stopped = true
				return output
			}
		case driver.PromptObservation:
			output.terminal = true
			output.termErr = orderedPromptError(typed)
			if typed.Message != nil {
				output.assistant = append(output.assistant, typed.Message)
			}
			if !sink(turnObservation{raw: observation}) {
				output.stopped = true
				return output
			}
		case driver.SteerObservation:
			if !sink(turnObservation{raw: observation}) {
				output.stopped = true
				return output
			}
		default:
			slog.Warn("foreignloop: ignoring unknown ordered observation", "type", fmt.Sprintf("%T", observation))
		}
	}
	if output.bindErr == nil {
		validateDrainedTurn(&output, sidBound)
	}
	return output
}

func (l *Loop) consumeDriverEvent(output *drainedTurn, input driver.Event, sidBound bool,
	expectedSID *string, bindSID func(string) error, mapper *mapper, sink func(turnObservation) bool,
	raw driver.Observation,
) bool {
	if output.terminal && input.Kind == driver.KindInit {
		return true
	}
	switch input.Kind {
	case driver.KindInit:
		if input.SessionID != "" && !sidBound && output.boundSID == "" {
			output.boundSID = input.SessionID
			*expectedSID = input.SessionID
			if err := bindSID(input.SessionID); err != nil {
				if !sink(turnObservation{raw: raw, event: event.ForeignSessionBound{ForeignSID: input.SessionID}}) {
					return false
				}
				output.bindErr = err
				return true
			}
			if !sink(turnObservation{raw: raw, event: event.ForeignSessionBound{ForeignSID: input.SessionID}}) {
				return false
			}
		} else if input.SessionID != "" && input.SessionID != *expectedSID {
			slog.Warn("foreignloop: foreign session id mismatch", "want", *expectedSID, "got", input.SessionID)
		}
	case driver.KindStepComplete:
		if input.Message != nil {
			output.assistant = append(output.assistant, input.Message)
		}
	case driver.KindTerminalOK:
		output.terminal = true
		if input.Message != nil {
			output.assistant = append(output.assistant, input.Message)
		}
	case driver.KindTerminalError, driver.KindModelFacingError:
		output.terminal = true
		output.termErr = resultError(input)
	default:
		events, err := mapper.toEvents(input)
		if err != nil {
			slog.Error("foreignloop: mapping foreign event failed; skipping", "error", err)
			return true
		}
		for _, mapped := range events {
			if !sink(turnObservation{raw: raw, event: mapped}) {
				return false
			}
		}
	}
	return true
}

func validateDrainedTurn(output *drainedTurn, sidBound bool) {
	switch {
	case !sidBound && output.boundSID == "":
		output.termErr = errors.Join(output.termErr, &ForeignProtocolError{Reason: "late-bound stream ended without init event"})
	case !output.terminal:
		output.termErr = errors.Join(output.termErr, &ForeignProtocolError{Reason: "stream ended without terminal event"})
	}
}

func orderedPromptError(observation driver.PromptObservation) error {
	if observation.Err != nil {
		return observation.Err
	}
	switch strings.ToLower(observation.StopReason) {
	case "end_turn", "cancelled", "cancelled_by_user":
		return nil
	case "max_tokens":
		return &ForeignResultError{Detail: "acp prompt reached its token limit"}
	case "max_turn_requests":
		return &ForeignResultError{Detail: "acp prompt reached its turn limit"}
	case "refusal":
		return &ForeignResultError{Detail: "acp prompt was refused"}
	default:
		return &ForeignResultError{Detail: "acp prompt ended with an unknown stop reason"}
	}
}

func isTurnTerminal(input event.Event) bool {
	switch input.(type) {
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		return true
	default:
		return false
	}
}

// publishTurnObservation is intentionally actor-only. Producers put ordered
// observations on the mailbox; this method is called by the actor goroutine
// and is the sole path that can publish an event derived from a live turn.
func (l *Loop) publishTurnObservation(ctx context.Context, _ event.TurnIndex, turnID, stepID uuid.UUID, observation turnObservation) error {
	if observation.event == nil {
		return nil
	}
	if err := l.publishActor(ctx, turnID, stepID, observation.event); err != nil {
		return &ForeignPublicationError{Event: fmt.Sprintf("%T", observation.event), Cause: err}
	}
	l.applyBoundObservation(observation.event)
	return nil
}

func (l *Loop) applyBoundObservation(input event.Event) {
	bound, ok := input.(event.ForeignSessionBound)
	if !ok || bound.ForeignSID == "" {
		return
	}
	l.applyBoundSID(bound.ForeignSID)
}

func (l *Loop) processTurnObservation(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	observation turnObservation, machine *steeringMachine, terminalHold **turnObservation,
) error {
	return l.processTurnOutcomeObservation(ctx, cur, turnID, stepID, observation, machine, terminalHold, true)
}

// processTurnOutcomeObservation is the actor-owned path used while a turn is
// being canceled or interrupted. It keeps consuming raw ordered facts so the
// steering machine can adjudicate the reserved request before the lifecycle
// outcome is applied. publish=false is used for shutdown: facts still resolve
// delivery, while no late turn events are emitted.
func (l *Loop) processTurnOutcomeObservation(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	observation turnObservation, machine *steeringMachine, terminalHold **turnObservation, publish bool,
) error {
	if machine != nil && observation.raw != nil {
		if err := machine.observe(observation.raw); err != nil {
			return err
		}
	}
	if observation.event == nil {
		return nil
	}
	if machine != nil && isTurnTerminal(observation.event) {
		hold, err := machine.beforeTerminal()
		if err != nil {
			return err
		}
		if hold {
			copyOf := observation
			if terminalHold != nil {
				*terminalHold = &copyOf
			}
			return nil
		}
	}
	if !publish {
		// Cancellation may consume the authoritative binding after the normal
		// checked publication path has stopped. The binding is still durable
		// lifecycle state: publish it through the checked path before applying it
		// locally so restore can recover the same SID. Other late lifecycle facts
		// remain suppressed during cancellation.
		if _, ok := observation.event.(event.ForeignSessionBound); ok {
			return l.publishTurnObservation(ctx, cur, turnID, stepID, observation)
		}
		return nil
	}
	return l.publishTurnObservation(ctx, cur, turnID, stepID, observation)
}

func (l *Loop) drainTurnObservations(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	mailbox <-chan turnObservation, machine *steeringMachine, terminalHold **turnObservation,
) error {
	if mailbox == nil {
		return nil
	}
	for observation := range mailbox {
		if err := l.processTurnObservation(ctx, cur, turnID, stepID, observation, machine, terminalHold); err != nil {
			return err
		}
	}
	return nil
}

// drainReadyTurnEvidence gives buffered provider facts precedence over a timer
// selected by the actor. It is deliberately nonblocking: only evidence already
// available at the adjudication boundary is consumed, so a deadline remains a
// bounded fallback when the provider is silent.
func (l *Loop) drainReadyTurnEvidence(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	mailbox *<-chan turnObservation, result *chan turnOutcome, outcome **turnOutcome,
	machine *steeringMachine, terminalHold **turnObservation,
) error {
	for {
		select {
		case value, ok := <-*result:
			if !ok {
				*result = nil
				continue
			}
			if *outcome == nil {
				copyOf := value
				*outcome = &copyOf
			}
			*result = nil
		case observation, ok := <-*mailbox:
			if !ok {
				*mailbox = nil
				continue
			}
			if err := l.processTurnObservation(ctx, cur, turnID, stepID, observation, machine, terminalHold); err != nil {
				return err
			}
		case completion := <-machine.completionsChan():
			if err := machine.complete(completion); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// drainSteeringShutdown adjudicates only the actor-owned steering state during
// clean shutdown. A turn provider may ignore cancellation and leave both its
// outcome and observation mailbox open; neither is needed to finalize a
// reserved delivery. Already-buffered completions/observations are consumed
// first, while the machine's bounded terminal/deadline timers provide the only
// wait when no evidence is available.
func (l *Loop) drainSteeringShutdown(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	mailbox <-chan turnObservation, result <-chan turnOutcome, machine *steeringMachine,
) error {
	return l.drainSteeringShutdownWithOutcome(ctx, cur, turnID, stepID, mailbox, result, machine, nil, nil)
}

func (l *Loop) drainSteeringShutdownWithOutcome(ctx context.Context, cur event.TurnIndex, turnID, stepID uuid.UUID,
	mailbox <-chan turnObservation, result <-chan turnOutcome, machine *steeringMachine,
	captured *turnOutcome, haveOutcome *bool,
) error {
	if machine == nil {
		return nil
	}
	if _, err := machine.beforeTerminal(); err != nil {
		return err
	}
	// A turn without an active steering attempt still has to finish its
	// provider lifecycle before the actor starts another turn: the outcome
	// carries the learned late-bound SID, and the provider releases its durable
	// session lock before sending it. Once steering is active, however, the
	// actor-owned steering deadline is the bounded adjudication point and must
	// not wait for a context-ignoring provider turn outcome.
	waitForTurnOutcome := machine.active == nil && !machine.disabled
	var turnWaitTimer steeringTimer
	var turnWait <-chan time.Time
	if waitForTurnOutcome {
		turnWaitTimer = machine.newTimer(steeringCallTimeout)
		turnWait = turnWaitTimer.Chan()
		defer stopSteeringTimer(turnWaitTimer)
	}
	var terminalHold *turnObservation
	drainReady := func() error {
		for {
			select {
			case value, ok := <-result:
				if !ok {
					result = nil
				} else if captured != nil && haveOutcome != nil {
					*captured = value
					*haveOutcome = true
					result = nil
				}
			case completion := <-machine.completionsChan():
				if err := machine.complete(completion); err != nil {
					return err
				}
			case observation, ok := <-mailbox:
				if !ok {
					mailbox = nil
					continue
				}
				if err := l.processTurnOutcomeObservation(ctx, cur, turnID, stepID, observation, machine, &terminalHold, false); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}
	for {
		if err := drainReady(); err != nil {
			return err
		}
		if machine.terminalReady() && (!waitForTurnOutcome || (result == nil && mailbox == nil)) {
			return nil
		}
		select {
		case value, ok := <-result:
			if !ok {
				result = nil
			} else if captured != nil && haveOutcome != nil {
				*captured = value
				*haveOutcome = true
				result = nil
			}
		case completion := <-machine.completionsChan():
			if err := machine.complete(completion); err != nil {
				return err
			}
		case observation, ok := <-mailbox:
			if !ok {
				mailbox = nil
				continue
			}
			if err := l.processTurnOutcomeObservation(ctx, cur, turnID, stepID, observation, machine, &terminalHold, false); err != nil {
				return err
			}
		case <-turnWait:
			return nil
		case <-machine.timerChan():
			if err := drainReady(); err != nil {
				return err
			}
			if machine.active != nil {
				if err := machine.timeout(); err != nil {
					return err
				}
			}
		case <-machine.deadlineTimerChan():
			if err := drainReady(); err != nil {
				return err
			}
			if machine.active != nil {
				if err := machine.deadlineTimeout(); err != nil {
					return err
				}
			}
		}
	}
}

// commitTurn asks for provider-neutral authoritative history only after the
// caller has closed the stream. Deliberately unavailable or failed history
// degrades to the complete assistant messages observed on the live stream.
func (l *Loop) commitTurn(stream driver.Stream, assistant []*content.AIMessage, pub func(event.Event)) content.AgenticMessages {
	return l.commitTurnToSink(stream, assistant, func(observation turnObservation) bool {
		if observation.event != nil && pub != nil {
			pub(observation.event)
		}
		return true
	})
}

func (l *Loop) commitTurnToSink(stream driver.Stream, assistant []*content.AIMessage, sink func(turnObservation) bool) content.AgenticMessages {
	history, err := stream.History()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("foreignloop: transcript decode failed; degrading to stream assistant", "error", err)
		}
		return commitFromAssistantToSink(assistant, sink)
	}
	if !history.Available {
		return commitFromAssistantToSink(assistant, sink)
	}
	var committed content.AgenticMessages
	for _, group := range history.Steps {
		if !sink(turnObservation{event: event.StepDone{Messages: group}}) {
			break
		}
		committed = append(committed, group...)
	}
	return committed
}

func commitFromAssistantToSink(assistant []*content.AIMessage, sink func(turnObservation) bool) content.AgenticMessages {
	var committed content.AgenticMessages
	for _, message := range assistant {
		if !sink(turnObservation{event: event.StepDone{Messages: content.AgenticMessages{message}}}) {
			break
		}
		committed = append(committed, message)
	}
	return committed
}

func lastOf(assistant []*content.AIMessage) *content.AIMessage {
	if len(assistant) == 0 {
		return nil
	}
	return assistant[len(assistant)-1]
}
