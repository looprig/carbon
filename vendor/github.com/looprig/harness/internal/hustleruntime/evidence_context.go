package hustleruntime

import (
	"context"
	"time"
)

// evidenceAttemptContext exposes cancellation from the owned execution while
// deliberately retaining no values from its caller.
type evidenceAttemptContext struct {
	context.Context
	deadline    time.Time
	hasDeadline bool
}

func (c evidenceAttemptContext) Deadline() (time.Time, bool) {
	return c.deadline, c.hasDeadline
}

func (c evidenceAttemptContext) Err() error {
	if c.Context.Err() == nil {
		return nil
	}
	if context.Cause(c.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func newEvidenceAttemptContext(parent context.Context) (context.Context, func()) {
	deadline, hasDeadline := parent.Deadline()
	cancellable, cancel := context.WithCancelCause(context.Background())
	ctx := evidenceAttemptContext{
		Context: cancellable, deadline: deadline, hasDeadline: hasDeadline,
	}
	stop := context.AfterFunc(parent, func() {
		cancel(context.Cause(parent))
	})
	if parent.Err() != nil {
		cancel(context.Cause(parent))
	}
	cleanup := func() {
		stop()
		cancel(context.Canceled)
	}
	return ctx, cleanup
}
