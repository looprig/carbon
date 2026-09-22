package app

import (
	"context"
	"fmt"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/inference"
)

// serveoptions.go holds the composition options and launch helpers the browser
// serve path (OpenServeStorage and PooledLauncher) shares. The former single-root
// ServeHost -- one process-lifetime rig admitting one live session at a time -- was
// retired with harness pkg/serve; the pooled launcher builds one rig per session.

// ServeHostOption configures OpenServeStorage's pooled launcher.
type ServeHostOption func(*serveHostConfig)

type serveHostConfig struct {
	buildClient func() (inference.Client, ModelFactory, error)
}

// WithServeInferenceClient bypasses models.json and production ACP composition,
// supplying the inference client and model factory directly. It mirrors
// SessionStoreFactory's buildClient seam: production never sets it, tests do, and it
// lets a test drive the real composition without a model configuration or a network.
func WithServeInferenceClient(build func() (inference.Client, ModelFactory, error)) ServeHostOption {
	return func(c *serveHostConfig) { c.buildClient = build }
}

// detachSessionLifetime strips cancellation from a caller's context while keeping its
// values: a served session's lifetime belongs to the launcher's owner and ends at an
// explicit shutdown, never at whatever context happened to ask for it.
//
// harness derives a session's WHOLE lifetime from the context passed to
// NewSession/RestoreSession: internal/sessionruntime's newSessionTopology and
// restore_constructor both do `sessionCtx, sessionCancel := context.WithCancel(ctx)`,
// and every loop context descends from that. Hand the rig a cancellable context and
// it mints a session that dies with its caller; the next Submit answers "session:
// loop exited".
//
// The cost is that a caller abandoning an open no longer aborts construction. That is
// the right trade: construction is short and takes leases whose release is owned by
// the session it is building, and a half-built session abandoned by a cancelled
// context is exactly the state that strands a workspace root. Values (trace/log
// context) are preserved, so nothing observable is lost.
func detachSessionLifetime(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// ForeignWorkspaceRootError reports a launch for a workspace root this composition
// does not serve. Silently launching it against another root would put one tenant's
// agent in another tenant's checkout, which is the one failure a composition root
// must never make quietly.
type ForeignWorkspaceRootError struct {
	SessionID sessionwire.SessionID
	Want      string
	Served    string
}

func (e *ForeignWorkspaceRootError) Error() string {
	return fmt.Sprintf(
		"carbon: session %s asked for workspace root %q, but this composition serves %q",
		e.SessionID, e.Want, e.Served)
}
