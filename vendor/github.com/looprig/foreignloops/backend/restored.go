package backend

import (
	"context"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

// Loop holds the backend actor's immutable dependencies and actor-owned state.
// Only the run goroutine mutates messages, turn state, binding, and the queue.
type Loop struct {
	Commands  chan command.Command
	Done      chan struct{}
	snapshots chan snapshotReq

	sessionID  uuid.UUID
	loopID     uuid.UUID
	sid        string
	parent     loop.Provenance
	pub        foreign.EventPublisher
	cfg        loop.BoundDefinition
	backendCfg Config
	services   foreign.Services
	idGen      func() (uuid.UUID, error)
	fac        *event.Factory
	closeOnce  sync.Once

	msgs       content.AgenticMessages
	turnIndex  event.TurnIndex
	hasSpawned bool
	sidBound   bool
	pending    []preparedInput
}

func newRestoredState(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	loopCfg loop.BoundDefinition,
	backendCfg Config,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed foreign.RestoredForeign,
) (*Loop, error) {
	return newRestoredStateWithServices(loopCtx, sessionID, loopID, parent, pub, loopCfg, backendCfg, idGen, fac, seed, foreign.Services{})
}

func newRestoredStateWithServices(
	_ context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	loopCfg loop.BoundDefinition,
	backendCfg Config,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed foreign.RestoredForeign,
	services foreign.Services,
) (*Loop, error) {
	if err := validateConfig(backendCfg); err != nil {
		return nil, err
	}
	if err := validateRuntimeWiring(loopCfg, idGen, fac, pub); err != nil {
		return nil, err
	}
	if err := validateRestoredSeed(seed); err != nil {
		return nil, err
	}
	return &Loop{
		Commands:   make(chan command.Command),
		Done:       make(chan struct{}),
		snapshots:  make(chan snapshotReq),
		sessionID:  sessionID,
		loopID:     loopID,
		sid:        seed.ForeignSID,
		parent:     parent,
		pub:        pub,
		cfg:        loopCfg,
		backendCfg: backendCfg,
		idGen:      idGen,
		fac:        fac,
		services:   cloneServices(services),
		msgs:       cloneMessages(seed.Msgs),
		turnIndex:  seed.TurnIndex,
		hasSpawned: true,
		sidBound:   true,
	}, nil
}

// BuildRestoredWith constructs and starts an actor from Harness-folded state
// through the legacy seam. It withholds all scoped capabilities by invoking
// the additive builder with zero Services.
func BuildRestoredWith(backendCfg Config) foreign.RestoredBuilder {
	build := BuildRestoredWithServices(backendCfg)
	return func(
		loopCtx context.Context,
		sessionID, loopID uuid.UUID,
		parent loop.Provenance,
		pub foreign.EventPublisher,
		loopCfg loop.BoundDefinition,
		idGen func() (uuid.UUID, error),
		fac *event.Factory,
		seed foreign.RestoredForeign,
	) (loop.Backend, error) {
		return build(loopCtx, sessionID, loopID, parent, pub, loopCfg, idGen, fac, seed, foreign.Services{})
	}
}

// BuildRestoredWithServices constructs and starts an actor from Harness-folded
// state through the additive services-aware seam. The supplied services are
// copied into the restored actor before it starts.
func BuildRestoredWithServices(backendCfg Config) foreign.ServicesRestoredBuilder {
	return func(
		loopCtx context.Context,
		sessionID, loopID uuid.UUID,
		parent loop.Provenance,
		pub foreign.EventPublisher,
		loopCfg loop.BoundDefinition,
		idGen func() (uuid.UUID, error),
		fac *event.Factory,
		seed foreign.RestoredForeign,
		services foreign.Services,
	) (loop.Backend, error) {
		state, err := newRestoredStateWithServices(loopCtx, sessionID, loopID, parent, pub, loopCfg, backendCfg, idGen, fac, seed, services)
		if err != nil {
			return nil, err
		}
		go state.run(loopCtx)
		return state, nil
	}
}

func validateRestoredSeed(seed foreign.RestoredForeign) error {
	if seed.ForeignSID == "" {
		return &ConfigError{Field: "RestoredForeign.ForeignSID", Reason: "required"}
	}
	return nil
}
