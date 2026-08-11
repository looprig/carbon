// This file owns one ordering problem: an application that wants its MCP
// configuration in its Session's config fingerprint must discover its servers
// before the Session exists, and a Manager used to demand the Session's ID to be
// born at all.
//
// # The circularity
//
// 2026-07-16-session-versioning-migration-design.md §Configuration freeze and
// adoption is explicit about the order:
//
//	"A candidate manifest freezes only after the application has fully assembled
//	 the Rig configuration needed by a Session: ... externally discovered tools
//	 required for the initial tool catalog are known ..."
//
// So: discover, then create. Harness already permits exactly that —
// ExternalCapabilityRev is supplied to rig.Define through
// rig.ConfigFingerprintFields, which is frozen before rig.NewSession mints an ID
// and stamps the fingerprint onto SessionStarted. Nothing on the Harness side
// needed to move.
//
// What blocked it was here. Deps.SessionID was required, so a Manager could not
// exist before a Session; ConfigDigest is a Manager method; and the digest is an
// input to the Session's creation. An application therefore had no order it
// could run in, and the only way to compose at all was to stamp an empty rev at
// creation and a real one at restore — a mismatch on the second run of every
// application that uses MCP, forever, reported as a configuration change nobody
// made.
//
// # Why the ID is what moved, and nothing else
//
// The whole of the Manager's need for a SessionID was one line: the coordinate on
// event.IntegrationStatus (see events.go). Nothing else consulted it — not
// Start, not connect, not discovery, not the catalog, not a tool binding, not a
// gate. And identity does not need it either: a BindingIdentity is per-binding
// configuration, negotiated server identity, and the adopted catalog, none of
// which is a fact about a Session. So the fix is not a new seam, it is deleting a
// requirement that was never real, and making the one place that IS real say so:
//
//	mgr, _ := NewManager(bindings, Deps{Gates: g, Events: e}) // no ID yet
//	mgr.Start(ctx)                                            // connect + discover
//	rev := mgr.ConfigDigest()                                 // now it is knowable
//	r, _ := rig.Define(..., rig.WithFingerprintFields(rig.ConfigFingerprintFields{
//		ExternalCapabilityRev: rev,
//	}))
//	sess, _ := r.NewSession(ctx)  // the ID exists for the first time here
//	mgr.BindSession(sess.SessionID())
//
// Deps.Events did NOT move with it, and the asymmetry is not an oversight. An
// EventPublisher is the application's own: neither rig nor session exposes a Hub,
// so a host has always had to bring its own sink, and it has one before it has a
// Session. Making it late-bound too would have been a fabricated dependency —
// coupling a thing that is available to a thing that is not.
//
// # The window, and why it costs nothing durable
//
// Between Start and BindSession a binding's status has no Session to belong to.
// Those statuses are dropped rather than published (publish), because a
// session-scoped event with no SessionID is one event.ValidateEvent refuses and a
// hub could only misroute — and rather than paper over that, BindSession
// republishes every binding's CURRENT status the moment there is somewhere to put
// it. So the Session's stream opens with the truth about every server, and what
// was lost is only the intermediate states of a connection that was still
// settling before the Session it serves existed.
//
// Nothing durable is at stake in that window: event.IntegrationStatus is
// Ephemeral precisely because the latest supersedes every earlier one (see
// events.go), so a status is never journal data. And the window is observable
// without any of this — Start returns a *StartupError naming every required
// binding that failed, and Status reports every binding at any moment.

package mcpharness

import (
	"fmt"

	"github.com/looprig/core/uuid"
)

// ErrAlreadyBound is returned by a second BindSession.
//
// Rebinding is refused rather than allowed because a Manager's bindings belong to
// one Session: the connections are live, their elicitations route to one host's
// gates, and the tool.invoke approvals their calls raise are decided against one
// host's access source. A Manager that changed Sessions under a running server
// would move all of that somewhere the user never agreed to.
var ErrAlreadyBound = fmt.Errorf("mcp: manager is already bound to a session")

// ErrNotBound is returned by an operation that needs a Session from a Manager
// that has none.
var ErrNotBound = fmt.Errorf("mcp: manager is not bound to a session; supply Deps.SessionID or call BindSession")

// BindSession attaches a Manager to the Session its bindings serve, and is how an
// application that discovered its servers BEFORE creating that Session closes the
// loop. See this file's header for the ordering it exists for.
//
// It is idempotent in neither direction: a Manager built with a Deps.SessionID is
// already bound and returns ErrAlreadyBound, as does a second call.
//
// On success it republishes every binding's current status, so that the Session's
// event stream opens knowing what each server actually is — including the ones
// that reached their final state while there was still no Session to tell.
func (m *Manager) BindSession(sessionID uuid.UUID) error {
	if sessionID.IsZero() {
		return fmt.Errorf("mcp: BindSession: sessionID is zero; supply the Session's ID")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	m.mu.Unlock()

	if !m.sessionID.CompareAndSwap(nil, &sessionID) {
		return ErrAlreadyBound
	}
	m.republishStatuses()
	return nil
}

// boundSession returns the Session's ID and whether the Manager has one.
func (m *Manager) boundSession() (uuid.UUID, bool) {
	id := m.sessionID.Load()
	if id == nil {
		return uuid.UUID{}, false
	}
	return *id, true
}

// republishStatuses publishes the current status of every binding.
//
// It reads each binding's state through the same projection Status uses, rather
// than remembering what it would have published while unbound. That is the honest
// answer and the cheap one: a replayed history of a connection's settling is not
// something a consumer of an Ephemeral status wants — only the latest one has ever
// meant anything — and what a Session needs to know at its first breath is what
// its servers are NOW.
func (m *Manager) republishStatuses() {
	for _, st := range m.Status() {
		state, ok := integrationState(st.Client.State)
		if !ok {
			continue
		}
		m.publish(m.status(st.Name, state, ""))
	}
}
