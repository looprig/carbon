package hook

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

var (
	errGuardPanicked                = errors.New("hook: guard callback panicked")
	errDenialClassificationPanicked = errors.New("hook: denial classification panicked")
)

// Runner is an immutable, compiled hook set safe for concurrent dispatch.
type Runner struct {
	guards []Guard
	around []Around
}

type registeredFinish struct {
	index  int
	finish FinishFunc
}

type isolatedObserverContext struct {
	derived  context.Context
	fallback context.Context
}

func (c *isolatedObserverContext) Deadline() (time.Time, bool) {
	if deadline, exists, ok := contextDeadline(c.derived); ok {
		return deadline, exists
	}
	deadline, exists, _ := contextDeadline(c.fallback)
	return deadline, exists
}

func (c *isolatedObserverContext) Done() <-chan struct{} {
	if done, ok := contextDone(c.derived); ok {
		return done
	}
	done, _ := contextDone(c.fallback)
	return done
}

func (c *isolatedObserverContext) Err() error {
	if err, ok := contextErr(c.derived); ok {
		return err
	}
	err, _ := contextErr(c.fallback)
	return err
}

func (c *isolatedObserverContext) Value(key any) any {
	if value, ok := contextValue(c.derived, key); ok {
		return value
	}
	value, _ := contextValue(c.fallback, key)
	return value
}

type parentPreservingContext struct {
	context.Context
	parent context.Context
	cancel context.CancelCauseFunc
}

func (c *parentPreservingContext) Done() <-chan struct{} {
	c.syncParentCancellation()
	return c.Context.Done()
}

func (c *parentPreservingContext) Err() error {
	c.syncParentCancellation()
	return c.Context.Err()
}

func (c *parentPreservingContext) syncParentCancellation() {
	if c.parent.Err() != nil {
		c.cancel(context.Cause(c.parent))
	}
}

// Compile validates a hook set and takes independent ownership of its
// registration slices.
func Compile(set Set) (*Runner, error) {
	if err := ValidateSet(set); err != nil {
		return nil, err
	}
	return &Runner{
		guards: append([]Guard(nil), set.Guards...),
		around: append([]Around(nil), set.Around...),
	}, nil
}

// Start begins observation and evaluates policy for one valid operation call.
// Matching begin callbacks run in registration order with chained contexts,
// followed by matching guards in registration order. Every callback receives
// an independent snapshot.
//
// Observer panics are logged without callback-owned details and fail open. A
// guard or denial-classification panic fails closed as *GuardError. A validated
// intentional denial is returned as *Denial; every other guard failure is
// returned as *GuardError.
//
// The returned FinishFunc runs completed observers in reverse registration
// order exactly once, including when a guard blocks. Every non-nil context
// returned by Begin keeps its values and tighter cancellation while also
// preserving cancellation and deadlines from the previous context. Calling
// Finish releases the resources used to bridge detached contexts, even when no
// observer returned its own finish callback. The caller must therefore always
// invoke Finish and supply a valid Result for the same operation with a valid
// terminal Outcome.
func (r *Runner) Start(
	ctx context.Context,
	call Call,
) (context.Context, FinishFunc, error) {
	if r == nil {
		return ctx, func(Result) {}, nil
	}
	if err := ValidateCall(call); err != nil {
		return ctx, nil, err
	}
	if !r.Handles(call.Operation) {
		return ctx, func(Result) {}, nil
	}
	snapshot := CloneCall(call)

	finishes := make([]registeredFinish, 0, len(r.around))
	releases := make([]func(), 0, len(r.around))
	for index, around := range r.around {
		if around.Operation != snapshot.Operation {
			continue
		}
		next, finish, panicked := beginAround(ctx, snapshot, around.Begin)
		if panicked {
			logObservationFailure("hook: begin callback panicked", snapshot.Operation, index)
			continue
		}
		if next == nil {
			logObservationFailure("hook: begin callback returned nil context", snapshot.Operation, index)
		} else if !sameContext(ctx, next) {
			next = &isolatedObserverContext{derived: next, fallback: ctx}
			var release func()
			next, release = preserveParentCancellation(ctx, next)
			if release != nil {
				releases = append(releases, release)
			}
			ctx = next
		}
		if finish != nil {
			finishes = append(finishes, registeredFinish{index: index, finish: finish})
		}
	}

	finish := aggregateFinish(snapshot.Operation, finishes, releases)
	for index, guard := range r.guards {
		if guard.Operation != snapshot.Operation {
			continue
		}
		syncParentCancellation(ctx)
		err, panicked := checkGuard(ctx, snapshot, guard.Check)
		if panicked {
			return ctx, finish, &GuardError{
				Operation: snapshot.Operation,
				Index:     index,
				Cause:     errGuardPanicked,
			}
		}
		if err == nil {
			continue
		}
		denial, denied, classificationPanicked := classifyDenial(err)
		if classificationPanicked {
			return ctx, finish, &GuardError{
				Operation: snapshot.Operation,
				Index:     index,
				Cause:     errDenialClassificationPanicked,
			}
		}
		if denied {
			return ctx, finish, denial
		}
		return ctx, finish, &GuardError{
			Operation: snapshot.Operation,
			Index:     index,
			Cause:     err,
		}
	}
	return ctx, finish, nil
}

// Handles reports whether the compiled runner has a guard or observer for
// operation. It is nil-safe and lets operation boundaries skip snapshot and
// clock work when no callback can run.
func (r *Runner) Handles(operation Operation) bool {
	if r == nil {
		return false
	}
	for _, around := range r.around {
		if around.Operation == operation {
			return true
		}
	}
	for _, guard := range r.guards {
		if guard.Operation == operation {
			return true
		}
	}
	return false
}

func classifyDenial(err error) (denial *Denial, denied bool, panicked bool) {
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = recover()
		denial = nil
		denied = false
		panicked = true
	}()
	denial, denied = AsDenial(err)
	completed = true
	return denial, denied, false
}

func beginAround(
	ctx context.Context,
	call Call,
	begin BeginFunc,
) (next context.Context, finish FinishFunc, panicked bool) {
	defer func() {
		if recover() != nil {
			next = ctx
			finish = nil
			panicked = true
		}
	}()
	next, finish = begin(ctx, CloneCall(call))
	return next, finish, false
}

func checkGuard(
	ctx context.Context,
	call Call,
	check GuardFunc,
) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			err = errGuardPanicked
			panicked = true
		}
	}()
	return check(ctx, CloneCall(call)), false
}

func aggregateFinish(
	operation Operation,
	finishes []registeredFinish,
	releases []func(),
) FinishFunc {
	var once sync.Once
	return func(result Result) {
		once.Do(func() {
			for index := len(finishes) - 1; index >= 0; index-- {
				registered := finishes[index]
				if finishAround(registered.finish, result) {
					logObservationFailure(
						"hook: finish callback panicked",
						operation,
						registered.index,
					)
				}
			}
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		})
	}
}

func preserveParentCancellation(
	parent context.Context,
	next context.Context,
) (context.Context, func()) {
	syncParentCancellation(parent)
	if parent.Done() == nil || parent.Done() == next.Done() {
		return next, nil
	}

	cancelContext, cancel := context.WithCancelCause(next)
	linked := context.Context(cancelContext)
	deadlineCancel := func() {}
	if deadline, ok := parent.Deadline(); ok {
		linked, deadlineCancel = context.WithDeadlineCause(
			linked,
			deadline,
			context.DeadlineExceeded,
		)
	}
	linked = &parentPreservingContext{
		Context: linked,
		parent:  parent,
		cancel:  cancel,
	}

	var stopParent func() bool
	if parent.Err() != nil {
		cancel(context.Cause(parent))
	} else {
		stopParent = context.AfterFunc(parent, func() {
			cancel(context.Cause(parent))
		})
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			if stopParent != nil {
				stopParent()
			}
			deadlineCancel()
			cancel(context.Canceled)
		})
	}
	return linked, release
}

func syncParentCancellation(ctx context.Context) {
	if linked, ok := ctx.(*parentPreservingContext); ok {
		linked.syncParentCancellation()
	}
}

func finishAround(finish FinishFunc, result Result) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	finish(CloneResult(result))
	return false
}

func contextDeadline(ctx context.Context) (deadline time.Time, exists bool, ok bool) {
	defer func() {
		if recover() != nil {
			deadline = time.Time{}
			exists = false
			ok = false
		}
	}()
	deadline, exists = ctx.Deadline()
	return deadline, exists, true
}

func contextDone(ctx context.Context) (done <-chan struct{}, ok bool) {
	defer func() {
		if recover() != nil {
			done = nil
			ok = false
		}
	}()
	return ctx.Done(), true
}

func contextErr(ctx context.Context) (err error, ok bool) {
	defer func() {
		if recover() != nil {
			err = nil
			ok = false
		}
	}()
	return ctx.Err(), true
}

func contextValue(ctx context.Context, key any) (value any, ok bool) {
	defer func() {
		if recover() != nil {
			value = nil
			ok = false
		}
	}()
	return ctx.Value(key), true
}

func sameContext(left context.Context, right context.Context) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return left == right
}

func logObservationFailure(message string, operation Operation, callbackIndex int) {
	slog.Default().Error(
		message,
		slog.Uint64("operation", uint64(operation)),
		slog.Int("callback_index", callbackIndex),
	)
}
