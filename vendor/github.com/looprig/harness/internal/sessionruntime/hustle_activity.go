package sessionruntime

import (
	"context"

	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
)

// hustleActivityLease is the narrow return contract the hustle controller will
// consume. It is intentionally an interface here rather than the hub's concrete
// lease type.
type hustleActivityLease = hustleruntime.ActivityLease

// hustleActivityTracker is the controller-facing acquisition contract.
// Go return types are invariant, so *hub.Hub does not and should not satisfy it
// directly even though *hub.HustleActivityLease satisfies hustleActivityLease.
type hustleActivityTracker = hustleruntime.ActivityTracker

type hubHustleActivityTracker struct{ hub *hub.Hub }

func newHubHustleActivityTracker(h *hub.Hub) hustleActivityTracker {
	return hubHustleActivityTracker{hub: h}
}

func (t hubHustleActivityTracker) AcquireHustleActivity(ctx context.Context, runID hustle.RunID) (hustleActivityLease, error) {
	lease, err := t.hub.AcquireHustleActivity(ctx, runID)
	if lease == nil {
		return nil, err
	}
	return lease, err
}
