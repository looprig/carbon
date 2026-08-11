package acp

import (
	"context"
	"errors"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/backend"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

// BuildWith adapts an ACP configuration to the legacy Harness foreign-loop
// builder. It withholds all scoped capabilities by invoking the additive
// builder with zero Services.
func BuildWith(cfg Config) foreign.Builder {
	build := BuildWithServices(cfg)
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

// BuildWithServices adapts an ACP configuration to the additive Harness
// services-aware builder. Each invocation creates one ACP driver and one
// backend Loop. The returned identity is the agent-assigned ACP session id,
// which the backend binds on the first live turn through a synthetic KindInit
// event.
func BuildWithServices(cfg Config) foreign.ServicesBuilder {
	cfg.McpServers = cloneMcpServers(cfg.McpServers)
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
		d, err := New(loopCtx, cfg)
		if err != nil {
			return nil, "", err
		}

		agent := &initAgent{agent: d, sessionID: d.AgentSessionID()}
		state, _, err := backend.BuildWithServices(backend.Config{
			Agent:   agent,
			Cwd:     cfg.WorkspaceRoot,
			Posture: legacyPosture(cfg.Posture),
			SIDMode: backend.SIDLateBound,
		})(loopCtx, sessionID, loopID, parent, pub, loopCfg, idGen, fac, services)
		if err != nil {
			return nil, "", closeAfterBackendFailure(d, err)
		}
		return state, d.AgentSessionID(), nil
	}
}

// BuildRestoredWith adapts ACP resume construction to the legacy Harness
// restored builder. It withholds all scoped capabilities by invoking the
// additive builder with zero Services.
func BuildRestoredWith(cfg Config) foreign.RestoredBuilder {
	build := BuildRestoredWithServices(cfg)
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

// BuildRestoredWithServices adapts ACP resume construction to the additive
// Harness restored builder. The journal's AgentSessionID is authoritative for
// ACP session/load; ForeignSID remains the backend's recovered routing
// identity. An empty AgentSessionID preserves legacy session/new behavior.
func BuildRestoredWithServices(cfg Config) foreign.ServicesRestoredBuilder {
	cfg.McpServers = cloneMcpServers(cfg.McpServers)
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
		if err := validateRestoredSeed(seed); err != nil {
			return nil, err
		}
		resumeCfg := cfg
		resumeCfg.AgentSessionID = seed.AgentSessionID
		d, err := New(loopCtx, resumeCfg)
		if err != nil {
			return nil, err
		}

		state, err := backend.BuildRestoredWithServices(backend.Config{
			Agent:   d,
			Cwd:     resumeCfg.WorkspaceRoot,
			Posture: legacyPosture(resumeCfg.Posture),
			SIDMode: backend.SIDLateBound,
		})(
			loopCtx,
			sessionID,
			loopID,
			parent,
			pub,
			loopCfg,
			idGen,
			fac,
			seed,
			services,
		)
		if err != nil {
			return nil, closeAfterBackendFailure(d, err)
		}
		return state, nil
	}
}

func validateRestoredSeed(seed foreign.RestoredForeign) error {
	if seed.ForeignSID == "" {
		return &backend.ConfigError{Field: "RestoredForeign.ForeignSID", Reason: "required"}
	}
	return nil
}

func legacyPosture(posture driver.Posture) driver.PermissionPosture {
	switch posture {
	case driver.PostureWorkspaceWrite:
		return driver.PostureAcceptEdits
	case driver.PostureReadOnly:
		return driver.PostureDefault
	default:
		// New validates the neutral posture before this boundary is reached.
		// Keep an invalid direct call restrictive rather than widening access.
		return driver.PostureDefault
	}
}

func closeAfterBackendFailure(d *Driver, cause error) error {
	if closeErr := d.Close(); closeErr != nil {
		return errors.Join(cause, closeErr)
	}
	return cause
}

// initAgent gives the existing backend its late-bind event without changing
// ACP's direct driver stream contract. Only the first successful live Spawn
// is prefixed; restored loops already carry a journaled SID.
type initAgent struct {
	agent     *Driver
	sessionID string

	mu          sync.Mutex
	initialized bool
}

func (a *initAgent) Spawn(ctx context.Context, turn driver.Turn) (driver.Stream, error) {
	stream, err := a.agent.Spawn(ctx, turn)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	first := !a.initialized
	if first {
		a.initialized = true
	}
	a.mu.Unlock()
	if !first {
		return stream, nil
	}
	if _, ok := stream.(driver.OrderedStream); ok {
		return newOrderedInitStream(stream, a.sessionID), nil
	}
	return newLegacyInitStream(stream, a.sessionID), nil
}

func (a *initAgent) Close() error { return a.agent.Close() }

func (a *initAgent) Steer(ctx context.Context, request driver.SteerRequest) (driver.SteerResult, error) {
	return a.agent.Steer(ctx, request)
}

// initStream prefixes exactly one synthetic init value for a legacy Events
// stream. It has its own projection owner so no wrapper goroutine sends to or
// closes a public channel directly.
type initStream struct {
	inner      driver.Stream
	projection *projection
	done       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	sessionID  string

	startOnce sync.Once
	selectMu  sync.Mutex
	selected  streamView
	doneOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// orderedInitStream exposes the ordered projection of the wrapped first-turn
// stream. Keeping this as a distinct concrete type is important: an
// unordered/legacy stream must not satisfy driver.OrderedStream merely because
// the wrapper has an inactive Observations method.
type orderedInitStream struct{ inner *initStream }

// legacyInitStream deliberately does not expose Observations. The wrapped
// helper retains that method for its focused projection tests, but the stream
// crossing the builder boundary must advertise only the selected legacy view.
type legacyInitStream struct{ inner *initStream }

func newOrderedInitStream(inner driver.Stream, sessionID string) driver.Stream {
	if _, ok := inner.(driver.OrderedStream); !ok {
		return newInitStream(inner, sessionID)
	}
	wrapped := newInitStream(inner, sessionID).(*initStream)
	wrapped.start(viewObservations)
	return &orderedInitStream{inner: wrapped}
}

func newLegacyInitStream(inner driver.Stream, sessionID string) driver.Stream {
	wrapped := newInitStream(inner, sessionID).(*initStream)
	return &legacyInitStream{inner: wrapped}
}

func newInitStream(inner driver.Stream, sessionID string) driver.Stream {
	ctx, cancel := context.WithCancel(context.Background())
	projectionOwner := newProjection()
	projectionOwner.stopOn(ctx)
	return &initStream{
		inner:      inner,
		projection: projectionOwner,
		done:       make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		sessionID:  sessionID,
	}
}

func (s *initStream) Events() <-chan driver.Event {
	if s == nil {
		return nil
	}
	s.start(viewEvents)
	return s.projection.eventsView()
}

func (s *initStream) Observations() <-chan driver.Observation {
	if s == nil {
		return nil
	}
	s.start(viewObservations)
	return s.projection.observationsView()
}

func (s *initStream) start(view streamView) {
	s.selectMu.Lock()
	defer s.selectMu.Unlock()
	if s.selected == viewUnselected {
		s.selected = view
		s.projection.selectView(view)
		s.startOnce.Do(func() { go s.forward(view) })
	}
}

func (s *orderedInitStream) Events() <-chan driver.Event {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Events()
}

func (s *orderedInitStream) Observations() <-chan driver.Observation {
	if s == nil || s.inner == nil || s.inner.projection == nil {
		return nil
	}
	return s.inner.projection.observationsView()
}

func (s *orderedInitStream) History() (driver.History, error) {
	if s == nil || s.inner == nil {
		return driver.History{Available: false}, nil
	}
	return s.inner.History()
}

func (s *orderedInitStream) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Close()
}

func (s *legacyInitStream) Events() <-chan driver.Event {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Events()
}

func (s *legacyInitStream) History() (driver.History, error) {
	if s == nil || s.inner == nil {
		return driver.History{Available: false}, nil
	}
	return s.inner.History()
}

func (s *legacyInitStream) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Close()
}

func (s *initStream) forward(view streamView) {
	defer s.finishDone()
	defer s.projection.close()
	if view == viewObservations {
		ordered, ok := s.inner.(driver.OrderedStream)
		if !ok {
			return
		}
		s.projection.emitObservation(driver.UpdateObservation{Event: driver.Event{Kind: driver.KindInit, SessionID: s.sessionID}})
		innerObservations := ordered.Observations()
		for {
			select {
			case <-s.ctx.Done():
				return
			case observation, ok := <-innerObservations:
				if !ok {
					return
				}
				s.projection.emitObservation(observation)
			}
		}
	}
	s.projection.emitEvent(driver.Event{Kind: driver.KindInit, SessionID: s.sessionID})
	innerEvents := s.inner.Events()
	if innerEvents == nil {
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-innerEvents:
			if !ok {
				return
			}
			s.projection.emitEvent(event)
		}
	}
}

func (s *initStream) finishDone() {
	if s == nil {
		return
	}
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *initStream) History() (driver.History, error) {
	if s == nil || s.inner == nil {
		return driver.History{Available: false}, nil
	}
	return s.inner.History()
}

func (s *initStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.inner != nil {
			s.closeErr = s.inner.Close()
		}
		// Close-before-selection must still make the wrapper's lifecycle
		// observable and must not start a forwarding goroutine.
		s.projection.close()
		s.finishDone()
	})
	return s.closeErr
}

var (
	_ foreign.Builder                 = BuildWith(Config{})
	_ foreign.ServicesBuilder         = BuildWithServices(Config{})
	_ foreign.RestoredBuilder         = BuildRestoredWith(Config{})
	_ foreign.ServicesRestoredBuilder = BuildRestoredWithServices(Config{})
	_ driver.Agent                    = (*initAgent)(nil)
	_ driver.Steerer                  = (*initAgent)(nil)
	_ driver.Closer                   = (*initAgent)(nil)
	_ driver.Stream                   = (*initStream)(nil)
	_ driver.Stream                   = (*legacyInitStream)(nil)
	_ driver.OrderedStream            = (*orderedInitStream)(nil)
)
