package app

import (
	"context"
	"errors"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
)

const carbonRestoreAdoptionMessage = "Carbon adopted current ephemeral runtime configuration"

type carbonRestoreDecider struct{}

func (carbonRestoreDecider) DecideRestore(_ context.Context, assessment event.DriftAssessment) (session.RestoreDecision, error) {
	for _, change := range assessment.Changes {
		if change.Severity == event.DriftWarn && !carbonEphemeralRestoreChange(change) {
			return session.RestoreDecision{Source: event.DecisionSourcePolicy}, nil
		}
	}
	return session.RestoreDecision{
		Accept:  true,
		Source:  event.DecisionSourcePolicy,
		Message: carbonRestoreAdoptionMessage,
	}, nil
}

func carbonEphemeralRestoreChange(change event.DriftChange) bool {
	switch change.Category {
	case event.DriftExternal, event.DriftRuntimeSkills, event.DriftConfinement:
		return true
	case event.DriftRuntime:
		return change.Field == "catalog_rev"
	case event.DriftPermission:
		return change.Field == "" || change.Field == "posture"
	case event.DriftApp:
		return change.Field == "access_profile"
	default:
		return false
	}
}

type carbonRuntimeRestoreResolver struct{}

func (carbonRuntimeRestoreResolver) ResolveRuntimeRestore(ctx context.Context, request session.RuntimeRestoreRequest) (loop.Resolved, error) {
	if err := ctx.Err(); err != nil {
		return loop.Resolved{}, err
	}
	resolved, err := request.Catalog.Resolve(request.AgentName, request.Harness, "", model.EffortNone)
	if err != nil || resolved.AgentHarness != request.Harness {
		return loop.Resolved{}, errors.New("carbon: current ACP harness default unavailable")
	}
	return resolved, nil
}
