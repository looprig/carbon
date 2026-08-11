package journal

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
)

// AppendFunc is one journal append operation.
type AppendFunc func(context.Context, JournalRecord) (uint64, error)

// AppendMiddleware decorates one AppendFunc. Implementations must invoke next
// synchronously exactly once with the supplied record and return its exact
// sequence and error.
type AppendMiddleware func(next AppendFunc) AppendFunc

type appendFuncJournal struct {
	append AppendFunc
}

type appendPanicError struct{}

func (*appendPanicError) Error() string { return "journal: append panicked" }

func (j *appendFuncJournal) Append(ctx context.Context, record JournalRecord) (uint64, error) {
	return j.append(ctx, record)
}

// WithHooks observes each durable append while preserving the journal's result.
func WithHooks(j SessionJournal, runner *hook.Runner, sessionID uuid.UUID) SessionJournal {
	if j == nil {
		return nil
	}
	middleware := HookMiddleware(runner, sessionID)
	if middleware == nil {
		return j
	}
	return &appendFuncJournal{append: middleware(j.Append)}
}

// HookMiddleware observes safe, classifiable journal records with runner.
// Records whose metadata cannot be derived without panicking bypass observation
// and delegate unchanged.
func HookMiddleware(runner *hook.Runner, sessionID uuid.UUID) AppendMiddleware {
	if !runner.Handles(hook.OperationJournalAppend) {
		return nil
	}
	return func(next AppendFunc) AppendFunc {
		return func(ctx context.Context, record JournalRecord) (uint64, error) {
			family, recordID, observable := describeRecord(record)
			if !observable {
				return next(ctx, record)
			}
			call := hook.Call{
				Operation:   hook.OperationJournalAppend,
				StartedAt:   time.Now(),
				Coordinates: identity.Coordinates{SessionID: sessionID},
				JournalAppend: &hook.JournalAppendData{
					Family:   family,
					RecordID: recordID,
				},
			}
			hookCtx, finish, startErr := runner.Start(ctx, call)
			if startErr != nil {
				return next(ctx, record)
			}
			defer func() {
				if recovered := recover(); recovered != nil {
					finish(hook.Result{
						Call:    call,
						EndedAt: time.Now(),
						Outcome: hook.OutcomeFailed,
						Err:     &appendPanicError{},
					})
					panic(recovered)
				}
			}()
			seq, err := next(hookCtx, record)
			outcome := hook.OutcomeCompleted
			switch {
			case err == nil:
				outcome = hook.OutcomeCompleted
			case ctx.Err() != nil || hookCtx.Err() != nil:
				outcome = hook.OutcomeCanceled
			default:
				outcome = hook.OutcomeFailed
			}
			finish(hook.Result{
				Call:    call,
				EndedAt: time.Now(),
				Outcome: outcome,
				Err:     err,
			})
			return seq, err
		}
	}
}

func describeRecord(record JournalRecord) (family hook.RecordFamily, recordID string, ok bool) {
	if record == nil {
		return "", "", false
	}
	defer func() {
		if recover() != nil {
			family = ""
			recordID = ""
			ok = false
		}
	}()
	switch record.(type) {
	case EventRecord, *EventRecord:
		family = hook.RecordEvent
	case CommandRecord, *CommandRecord:
		family = hook.RecordCommand
	case GatePreparedRecord, *GatePreparedRecord:
		family = hook.RecordGatePrepared
	case FenceRecord, *FenceRecord:
		family = hook.RecordFence
	default:
		return "", "", false
	}
	return family, record.IdempotencyID(), true
}
