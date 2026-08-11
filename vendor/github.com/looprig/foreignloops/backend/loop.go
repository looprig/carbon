package backend

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

type preparedInput struct {
	command command.UserInput
	turnID  uuid.UUID
	stepID  uuid.UUID
}

var _ loop.Backend = (*Loop)(nil)

// New validates the composition wiring, binds an initial session ID when
// requested, and starts the single-owner backend actor.
func New(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
	pub foreign.EventPublisher, loopCfg loop.BoundDefinition, backendCfg Config,
	idGen func() (uuid.UUID, error), fac *event.Factory,
) (*Loop, string, error) {
	return newWithServices(loopCtx, sessionID, loopID, parent, pub, loopCfg, backendCfg, idGen, fac, foreign.Services{})
}

func newWithServices(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
	pub foreign.EventPublisher, loopCfg loop.BoundDefinition, backendCfg Config,
	idGen func() (uuid.UUID, error), fac *event.Factory, services foreign.Services,
) (*Loop, string, error) {
	if err := validateConfig(backendCfg); err != nil {
		return nil, "", err
	}
	if err := validateRuntimeWiring(loopCfg, idGen, fac, pub); err != nil {
		return nil, "", err
	}
	sid := ""
	sidBound := false
	if backendCfg.SIDMode == SIDPrebound {
		minted, err := idGen()
		if err != nil {
			return nil, "", &driver.SpawnError{Cause: err}
		}
		sid = minted.String()
		sidBound = true
	}
	state := &Loop{
		Commands:   make(chan command.Command),
		Done:       make(chan struct{}),
		snapshots:  make(chan snapshotReq),
		sessionID:  sessionID,
		loopID:     loopID,
		sid:        sid,
		sidBound:   sidBound,
		parent:     parent,
		pub:        pub,
		cfg:        loopCfg,
		backendCfg: backendCfg,
		services:   cloneServices(services),
		idGen:      idGen,
		fac:        fac,
	}
	go state.run(loopCtx)
	return state, sid, nil
}

func (l *Loop) CommandSink() chan<- command.Command { return l.Commands }

func (l *Loop) DoneChan() <-chan struct{} { return l.Done }

func (l *Loop) run(loopCtx context.Context) {
	defer close(l.Done)
	defer l.closeAgent()
	for {
		if len(l.pending) > 0 {
			next := l.pending[0]
			l.pending = l.pending[1:]
			if l.runTurn(loopCtx, next) {
				return
			}
			continue
		}
		select {
		case <-loopCtx.Done():
			return
		case req := <-l.snapshots:
			req.reply <- snapshotResult{msgs: cloneMessages(l.msgs), turnIndex: l.turnIndex}
		case input := <-l.Commands:
			switch typed := input.(type) {
			case command.UserInput:
				prepared, ok := l.prepareInput(loopCtx, typed)
				if !ok {
					continue
				}
				if typed.Accepted != nil {
					typed.Accepted <- nil
				}
				if l.runTurn(loopCtx, prepared) {
					return
				}
			case command.Shutdown:
				l.closeAgent()
				typed.Ack <- nil
				return
			case command.Interrupt:
				typed.Ack <- false
			case command.CancelDelegateRequest:
				if typed.Ack != nil {
					typed.Ack <- command.DelegateCancelNoop
				}
			default:
				slog.Warn("foreignloop: dropping un-honorable command while idle", "type", fmt.Sprintf("%T", input))
			}
		}
	}
}

func (l *Loop) closeAgent() {
	l.closeOnce.Do(func() {
		closer, ok := l.backendCfg.Agent.(driver.Closer)
		if !ok {
			return
		}
		if err := closer.Close(); err != nil {
			slog.Warn("foreignloop: agent close failed", "error", err)
		}
	})
}

func (l *Loop) prepareInput(ctx context.Context, input command.UserInput) (preparedInput, bool) {
	if err := command.ValidateCommand(input); err != nil {
		if input.Accepted != nil {
			input.Accepted <- err
		}
		return preparedInput{}, false
	}
	turnID, err := l.idGen()
	if err != nil {
		if input.Accepted != nil {
			input.Accepted <- err
		} else {
			slog.Error("foreignloop: turn id mint failed; dropping submit (fail-secure)", "error", err)
		}
		return preparedInput{}, false
	}
	stepID, err := l.idGen()
	if err != nil {
		if input.Accepted != nil {
			input.Accepted <- err
		} else {
			slog.Error("foreignloop: step id mint failed; dropping submit (fail-secure)", "error", err)
		}
		return preparedInput{}, false
	}
	if input.Accepted != nil {
		if err := l.publishAcceptance(ctx, input.CommandID); err != nil {
			input.Accepted <- err
			return preparedInput{}, false
		}
	}
	return preparedInput{command: input, turnID: turnID, stepID: stepID}, true
}
