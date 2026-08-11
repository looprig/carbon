package process

import (
	"context"
	"math"

	"github.com/looprig/harness/pkg/tool"
)

// lifecycle_bridge.go adapts this package's own private lifecycleSink/
// completionNotifier capabilities (supervisor.go/entry.go) to Harness's
// public tool.ProcessLifecyclePublisher/tool.ProcessCompletionNotifier
// contracts, and back. It is the one place this package constructs a real
// tool.ProcessLifecycleMetadata or tool.ProcessCompletionNotification DTO;
// session_resource.go's SupervisorResource.Activate is this file's only
// caller. No supervision policy lives here -- only field-for-field mapping,
// mirroring how the product composition root's own process_adapter.go stays
// purely mechanical (see that file's package doc comment for the identical
// justification one module up the stack).

// lifecyclePublisherAdapter adapts a validated tool.ProcessLifecyclePublisher
// to this package's own lifecycleSink.
type lifecyclePublisherAdapter struct {
	publisher tool.ProcessLifecyclePublisher
}

func (a lifecyclePublisherAdapter) publishStart(ctx context.Context, event lifecycleStartEvent) error {
	return a.publisher.PublishProcessLifecycle(ctx, tool.ProcessLifecycleMetadata{
		EventID:           event.EventID,
		Kind:              event.Kind,
		SessionID:         event.Identity.Owner.SessionID,
		LoopID:            event.Identity.Owner.LoopID,
		ProcessHandle:     string(event.Identity.Handle),
		OriginExecutionID: event.Identity.Origin.ToolExecutionID,
		State:             tool.ProcessLifecycleRunning,
		ProcessCreatedAt:  event.CreatedAt,
		ProcessStartedAt:  event.StartedAt,
	})
}

func (a lifecyclePublisherAdapter) publish(ctx context.Context, event lifecycleTerminalEvent) error {
	metadata := tool.ProcessLifecycleMetadata{
		EventID:           event.EventID,
		Kind:              event.Kind,
		SessionID:         event.Identity.Owner.SessionID,
		LoopID:            event.Identity.Owner.LoopID,
		ProcessHandle:     string(event.Identity.Handle),
		OriginExecutionID: event.Identity.Origin.ToolExecutionID,
		State:             mapLifecycleState(event.State),
		ProcessCreatedAt:  event.CreatedAt,
		ProcessStartedAt:  event.StartedAt,
		ProcessFinishedAt: event.FinishedAt,
		Reason:            mapTerminalReason(event.Result.Reason),
	}
	if event.Result.ExitCode != nil {
		metadata.HasExitCode = true
		metadata.ExitCode = clampExitCode(*event.Result.ExitCode)
	}
	return a.publisher.PublishProcessLifecycle(ctx, metadata)
}

var _ lifecycleSink = lifecyclePublisherAdapter{}

// completionNotifierAdapter adapts a validated tool.ProcessCompletionNotifier
// to this package's own completionNotifier.
type completionNotifierAdapter struct {
	notifier tool.ProcessCompletionNotifier
}

func (a completionNotifierAdapter) notify(ctx context.Context, event completionEvent) error {
	return a.notifier.NotifyProcessCompletion(ctx, tool.ProcessCompletionNotification{
		CommandID:     event.CommandID,
		SessionID:     event.Owner.SessionID,
		LoopID:        event.Owner.LoopID,
		ProcessHandle: string(event.Handle),
		State:         mapLifecycleState(event.State),
		Reason:        mapTerminalReason(event.Result.Reason),
	})
}

var _ completionNotifier = completionNotifierAdapter{}

// clampExitCode narrows a process exit code (Go's os/exec convention: a
// plain int, in practice always well within int32 range on every platform
// this module supports -- POSIX exit statuses are 8-bit, and Windows exit
// codes are a 32-bit DWORD) into tool.ProcessLifecycleMetadata.ExitCode's
// int32 field without risking an integer-overflow conversion. It clamps
// rather than truncates, so a genuinely out-of-range value (never expected
// in practice) reports a saturated boundary value instead of a silently
// wrapped, misleading different one.
func clampExitCode(code int) int32 {
	switch {
	case code > math.MaxInt32:
		return math.MaxInt32
	case code < math.MinInt32:
		return math.MinInt32
	default:
		return int32(code)
	}
}

// mapLifecycleState translates this package's own closed State domain
// (state.go) to Harness's tool.ProcessLifecycleState one-for-one by name.
// State.Valid() is what admits a value into a Manifest in the first place,
// so every value this function ever actually receives from a live Start/
// terminalize/restore path is one of these nine; an unrecognized value
// (never produced by this package today) conservatively maps to the zero
// tool.ProcessLifecycleState, which tool.ProcessLifecycleMetadata.Validate
// rejects outright rather than silently misreporting a state.
func mapLifecycleState(s State) tool.ProcessLifecycleState {
	switch s {
	case StateStarting:
		return tool.ProcessLifecycleStarting
	case StateRunning:
		return tool.ProcessLifecycleRunning
	case StateExited:
		return tool.ProcessLifecycleExited
	case StateFailed:
		return tool.ProcessLifecycleFailed
	case StateTimedOut:
		return tool.ProcessLifecycleTimedOut
	case StateInterrupted:
		return tool.ProcessLifecycleInterrupted
	case StateTerminated:
		return tool.ProcessLifecycleTerminated
	case StateKilled:
		return tool.ProcessLifecycleKilled
	case StateLostOnRestore:
		return tool.ProcessLifecycleLostOnRestore
	default:
		return 0
	}
}

// mapTerminalReason translates Result.Reason -- this package's own closed
// string enum (entry.go's reasonString is the exact, one-directional forward
// mapping this function inverts) -- to Harness's tool.ProcessTerminalReason.
// The inversion is deliberately lossy in exactly the two places
// reasonString itself already collapses distinct Harness reasons onto one
// string: "terminated" is reasonString's output for
// tool.ProcessTerminalTerminated, tool.ProcessTerminalRunnerShutdown, AND
// tool.ProcessTerminalOutputLimit alike, and "killed" likewise collapses
// tool.ProcessTerminalKilled/RunnerShutdown/OutputLimit. This function maps
// both back to the single reason matching the terminal State a completion
// record actually carries (Terminated/Killed respectively) rather than
// guessing at a specific one of the three -- Harness's own
// validProcessCompletionTuple accepts any of the three for that State, so
// this is always a valid, if narrower, reason. An unrecognized string
// (never produced by reasonString today) conservatively maps to
// tool.ProcessTerminalFailed, mirroring reasonString's own "unrecognized
// reason maps to failed" fallback in the opposite direction.
func mapTerminalReason(reason string) tool.ProcessTerminalReason {
	switch reason {
	case "exited":
		return tool.ProcessTerminalExited
	case "timed-out":
		return tool.ProcessTerminalTimedOut
	case "interrupted":
		return tool.ProcessTerminalInterrupted
	case "terminated":
		return tool.ProcessTerminalTerminated
	case "killed":
		return tool.ProcessTerminalKilled
	case "lost-on-restore":
		return tool.ProcessTerminalLostOnRestore
	default:
		return tool.ProcessTerminalFailed
	}
}
