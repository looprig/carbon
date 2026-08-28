package main

// serve.go is the ONE place in this module that imports harness's generic HTTP
// layer. cmd/carbon/main_test.go's TestRigPackagesHaveNoServeAdapter forbids the
// import everywhere under internal/, and TestServeCompositionLivesInCommand
// requires it here — the composition is a process-root concern, exactly as the
// generic serve.Handler[S, O] shape intends.

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/session"
)

// serveHost is the narrow view serveRig needs of internal/app's *ServeHost. It is
// declared HERE, at the consumer, so the wrapper is unit-testable against a stub and
// so internal/app never has to name a serve type.
type serveHost interface {
	NewSession(ctx context.Context) (session.SessionController, error)
	RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error)
	HasSession(ctx context.Context, id uuid.UUID) (bool, error)
}

// serveRig adapts carbon's session host to serve.Rig. It exists for exactly one
// behaviour the host cannot provide itself: minting a serve.SessionNotFoundError for
// an id that was never persisted, so handleRestore answers 404 instead of the bare
// 500 every other RestoreSession failure maps to.
//
// It is NOT a "Runner" adapter of the kind harness's pkg/serve/deps_test.go forbids
// and the rig-migration plan ruled out: it adds no HTTP behaviour, holds no request
// state, and its NewSession is a pass-through.
//
// It returns the host's controller UNWRAPPED. That is load-bearing: harness's live
// registry evicts a dead session by watching an optional Done() channel on the value
// it was handed (serve.SessionDone), and a wrapper that did not forward Done would
// silently opt every session out of eviction, pinning a subscription and a goroutine
// per dead session.
type serveRig struct{ host serveHost }

// NewSession is a pass-through. The variadic rig.SessionOption is part of serve.Rig's
// generic contract; carbon supplies none (there is no seed snapshot).
func (r *serveRig) NewSession(ctx context.Context, _ ...rig.SessionOption) (session.SessionController, error) {
	return r.host.NewSession(ctx)
}

// RestoreSession checks existence BEFORE touching the rig. A clean "no such session"
// becomes serve.SessionNotFoundError (404); a FAILED check propagates unchanged
// (500), because a broken catalog must never be reported as a missing session; and a
// restore that fails for a session that DOES exist also propagates unchanged, because
// rewriting a busy-workspace or config-drift failure into a 404 would hide a real
// fault behind a benign status.
func (r *serveRig) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	exists, err := r.host.HasSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, serve.SessionNotFoundError{SessionID: id}
	}
	return r.host.RestoreSession(ctx, id)
}

// The instantiation serve.Handler is called with. Asserted here as well as in the
// test so a shape drift breaks the build, not just the suite.
var _ serve.Rig[session.SessionController, rig.SessionOption] = (*serveRig)(nil)
