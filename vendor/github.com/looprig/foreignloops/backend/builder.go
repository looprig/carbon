package backend

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

// BuildWith adapts actor construction to the legacy Harness-owned seam. It
// withholds all scoped capabilities by invoking the additive builder with
// zero Services.
func BuildWith(backendCfg Config) foreign.Builder {
	build := BuildWithServices(backendCfg)
	return func(
		loopCtx context.Context,
		sessionID, loopID uuid.UUID,
		parent loop.Provenance,
		pub foreign.EventPublisher,
		loopCfg loop.BoundDefinition,
		idGen func() (uuid.UUID, error),
		fac *event.Factory,
	) (loop.Backend, string, error) {
		return build(loopCtx, sessionID, loopID, parent, pub, loopCfg, idGen, fac, foreign.Services{})
	}
}

// BuildWithServices adapts actor construction to the additive Harness seam.
// The supplied services are copied into the actor before it starts.
func BuildWithServices(backendCfg Config) foreign.ServicesBuilder {
	return func(
		loopCtx context.Context,
		sessionID, loopID uuid.UUID,
		parent loop.Provenance,
		pub foreign.EventPublisher,
		loopCfg loop.BoundDefinition,
		idGen func() (uuid.UUID, error),
		fac *event.Factory,
		services foreign.Services,
	) (loop.Backend, string, error) {
		state, sid, err := newWithServices(loopCtx, sessionID, loopID, parent, pub, loopCfg, backendCfg, idGen, fac, services)
		if err != nil {
			return nil, "", err
		}
		return state, sid, nil
	}
}

func validateRuntimeWiring(
	loopCfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	pub foreign.EventPublisher,
) error {
	switch {
	case loopCfg == nil || loopCfg.EffectiveSystem() == "":
		return &ConfigError{Field: "System", Reason: "required"}
	case idGen == nil:
		return &ConfigError{Field: "idGen", Reason: "required"}
	case fac == nil:
		return &ConfigError{Field: "fac", Reason: "required"}
	case nilLike(pub):
		return &ConfigError{Field: "pub", Reason: "required"}
	default:
		return nil
	}
}
