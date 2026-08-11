package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
)

// operationHookError is the safe runtime projection of a guard failure. It
// deliberately omits the callback error: guard errors are trusted in-process
// values and may contain policy details or arbitrary user data.
type operationHookError struct {
	Operation hook.Operation
	Denied    bool
}

func (e *operationHookError) Error() string {
	if e.Denied {
		return fmt.Sprintf("loop: operation %d denied by hook policy", e.Operation)
	}
	return fmt.Sprintf("loop: operation %d blocked by hook failure", e.Operation)
}

type operationHookPanicError struct {
	Operation hook.Operation
}

func (e *operationHookPanicError) Error() string {
	return fmt.Sprintf("loop: operation %d panicked", e.Operation)
}

func safeHookError(operation hook.Operation, err error) error {
	_, denied := classifyHookError(err)
	return &operationHookError{Operation: operation, Denied: denied}
}

// hookOutcome maps a terminal error to the bounded hook result domain. A nil
// error is always a normalized success, even if an owner canceled its private
// context after producing that success. Cancellation is classified from err,
// because actor ownership cleanup may cancel ctx before a durable finish runs.
func hookOutcome(_ context.Context, err error) hook.Outcome {
	outcome, _ := classifyHookError(err)
	return outcome
}

func classifyHookError(err error) (outcome hook.Outcome, denied bool) {
	outcome = hook.OutcomeFailed
	defer func() {
		if recover() != nil {
			outcome = hook.OutcomeFailed
			denied = false
		}
	}()
	if err == nil {
		return hook.OutcomeCompleted, false
	}
	if _, denied := hook.AsDenial(err); denied {
		return hook.OutcomeDenied, true
	}
	var guardErr *hook.GuardError
	if errors.As(err, &guardErr) {
		return hook.OutcomeFailed, false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return hook.OutcomeCanceled, false
	}
	return hook.OutcomeFailed, false
}

func finishHook(
	finish hook.FinishFunc,
	call hook.Call,
	outcome hook.Outcome,
	err error,
) {
	if finish == nil {
		return
	}
	endedAt := time.Now()
	if endedAt.Before(call.StartedAt) {
		endedAt = call.StartedAt
	}
	finish(hook.Result{
		Call:    call,
		EndedAt: endedAt,
		Outcome: outcome,
		Err:     err,
	})
}

type operationHookScope struct {
	call     hook.Call
	finish   hook.FinishFunc
	finished bool
}

func (s *operationHookScope) Finish(outcome hook.Outcome, err error) {
	if s == nil || s.finished {
		return
	}
	s.finished = true
	finishHook(s.finish, s.call, outcome, err)
}

type turnStartHookFailure struct {
	err      error
	complete func()
}

func (e *turnStartHookFailure) Error() string { return e.err.Error() }

func completeTurnStartHook(err error) {
	failure, ok := err.(*turnStartHookFailure)
	if !ok || failure.complete == nil {
		return
	}
	failure.complete()
}

func hookNow(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}

func terminalHookError(terminal event.Event) error {
	if terminal == nil {
		return nil
	}
	switch value := terminal.(type) {
	case event.TurnDone:
		return nil
	case event.TurnFailed:
		return value.Err
	case event.TurnInterrupted:
		return context.Canceled
	default:
		return errors.New("loop: unknown turn terminal")
	}
}
