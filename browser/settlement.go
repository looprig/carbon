package browser

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/sessionstore"
)

const pendingPageLimit = 128

// PendingObservationError means the due view could not establish a complete
// read. An unreadable row or truncated scan must never be reported as empty.
type PendingObservationError struct{ Cause error }

func (e *PendingObservationError) Error() string {
	return fmt.Sprintf("carbon: pending-command observation incomplete: %v", e.Cause)
}
func (e *PendingObservationError) Unwrap() error { return e.Cause }

// PendingCommandsError means shutdown's settlement budget ended while a
// disposition command remained nonterminal. It leaves durable rows unchanged.
type PendingCommandsError struct {
	Count int
	Cause error
}

func (e *PendingCommandsError) Error() string {
	return fmt.Sprintf("carbon: %d command(s) remain nonterminal at shutdown: %v", e.Count, e.Cause)
}
func (e *PendingCommandsError) Unwrap() error { return e.Cause }

type outstandingReader interface {
	ControlShards() int
	ListDueDispositionCommands(context.Context, sessionstore.ListDueDispositionCommandsRequest) (sessionstore.DispositionDueCommandPage, error)
}

// scanOutstanding reads the complete nonterminal due view. A deadline far in
// the future includes applying and claimed rows as well as pending commands.
func scanOutstanding(ctx context.Context, reader outstandingReader, tenant sessionwire.TenantID, pageBudget int) (int, error) {
	if reader == nil || pageBudget < 1 || reader.ControlShards() < 1 {
		return 0, &PendingObservationError{Cause: errors.New("invalid scan source or page budget")}
	}
	count, pages := 0, 0
	for shard := 0; shard < reader.ControlShards(); shard++ {
		cursor := sessionwire.Cursor("")
		for {
			if pages >= pageBudget {
				return count, &PendingObservationError{Cause: errors.New("page budget exhausted")}
			}
			pages++
			req := sessionstore.ListDueDispositionCommandsRequest{Shard: shard, Limit: pendingPageLimit, Cursor: cursor}
			if cursor == "" {
				req.DueAtOrBefore = time.Unix(0, math.MaxInt64).UTC()
			}
			page, err := reader.ListDueDispositionCommands(ctx, req)
			if err != nil {
				return count, &PendingObservationError{Cause: err}
			}
			if page.Unreadable != 0 {
				return count, &PendingObservationError{Cause: fmt.Errorf("shard %d has %d unreadable command row(s)", shard, page.Unreadable)}
			}
			for _, command := range page.Commands {
				if command.Record.Descriptor.TenantID == tenant {
					count++
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return count, nil
}

// waitOutstanding only observes. The Factory and Host remain live while the
// caller's settlement budget permits durable processing to complete.
func waitOutstanding(ctx context.Context, reader outstandingReader, tenant sessionwire.TenantID, pageBudget int, wait func(context.Context) error) error {
	for {
		count, err := scanOutstanding(ctx, reader, tenant, pageBudget)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if err := wait(ctx); err != nil {
			return &PendingCommandsError{Count: count, Cause: err}
		}
	}
}

func waitPendingPoll(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
