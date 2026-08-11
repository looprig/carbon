package loopruntime

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

type compactionInference func(context.Context) error

// publishCompactionStartedBeforeInference preserves the progress-before-work
// boundary: the start is fully stamped, validated, and checked-published before
// the inference callback can run.
func publishCompactionStartedBeforeInference(
	ctx context.Context,
	publisher eventPublisher,
	factory *event.Factory,
	sessionID, loopID uuid.UUID,
	started event.CompactionStarted,
	infer compactionInference,
) error {
	stamped, err := stampLoopEvent(started, factory, sessionID, loopID, uuid.UUID{})
	if err != nil {
		return err
	}
	progress := stamped.(event.CompactionStarted)
	if err := event.ValidateEvent(progress); err != nil {
		return err
	}
	if err := publisher.PublishEventChecked(ctx, progress); err != nil {
		return err
	}
	return infer(ctx)
}
