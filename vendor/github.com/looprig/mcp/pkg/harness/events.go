// This file is the other crossing point: an MCP client's event on one side, a
// Harness event on the other, and — as with tools.go — nothing MCP-shaped past
// it.
//
// # Why there are two sinks, and which fact goes where
//
// The adapter has two ways to tell a host something, and they are not
// alternatives. Deps.Events carries event.IntegrationStatus, which Harness
// defines and every consumer already subscribes to; it answers the one question
// an operator asks — is this server up? Deps.Reporter carries this module's own
// Notice values, which answer questions Harness has no vocabulary for: a
// binding refused from a Loop's namespace over a name collision, a toolset
// adopted at an idle. The split is not "important vs unimportant". It is
// "protocol-neutral vs MCP-specific", and it is the module boundary itself:
// event.IntegrationStatus would read identically for a language server, while
// NoticeToolNameCollision would not.
//
// # Redaction is a property of the mapping, not of a reviewer's memory
//
// Every client event that reports a failure carries a Message — bounded and
// normalized by the client that classified it, but still text a third-party
// server influenced. None of it reaches an event here. Detail is built from the
// FailureClass and this file's own literals, which is the same rule tools.go
// applies to a tool result ("the class, never the message") and for the same
// reason: a class is what an operator can act on, and a message is what a server
// chose to say. ServerLog and RequestProgress carry server text and a server's
// claims about itself; neither becomes a status at all.
//
// See events_test.go's canary sweep, which puts a marker in every
// server-influenced field of every client event and fails if it ever surfaces.

package mcpharness

import (
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/mcp/pkg/client"
)

// IntegrationSource is the event.IntegrationStatus.Source every status this
// adapter publishes carries. It is the namespace a binding name lives in: two
// integrations may each have a binding called "github", and Source is what tells
// them apart.
const IntegrationSource = "mcp"

// handlersFor builds the connection callbacks for one binding: the
// server-initiated requests this host is prepared to serve, and the event stream
// it observes.
//
// It is a method on Manager, and the binding is passed rather than closed over
// by name, because a callback must be able to find its binding's state without
// consulting the Manager's table — the table's lock is held during
// reconfiguration, and a server that elicits at that moment must still be
// answerable.
//
// The event and elicitation handlers are installed unconditionally. That is not
// the same as advertising a capability: client.Handlers' contract is that
// installing a handler the Definition did not ask for is legal and the handler is
// simply never called, while asking for a capability with no handler is a
// configuration error Connect rejects. So the binding's own Definition decides
// whether this host offers elicitation; this method only makes sure that a
// binding which does ask has somewhere to route.
//
// Sampling is conditional, and the asymmetry is Deps': Gates is a required
// dependency, so an elicitor always has a host to ask, while Sampling is optional
// and a nil one means the application supplied no policy. Passing that nil
// through — rather than installing a sampler that could only ever refuse — is what
// lets Connect's rule fire on a binding that requests the capability without one.
// See Manager.samplingHandler.
func (m *Manager) handlersFor(bs *bindingState) client.Handlers {
	return client.Handlers{
		Event:       func(ev client.Event) { m.onClientEvent(bs, ev) },
		Elicitation: &elicitor{m: m, bs: bs},
		Sampling:    m.samplingHandler(bs),
	}
}

// onClientEvent translates one client event and publishes it, if it is a status
// at all.
//
// It runs on the client's emitting goroutine and blocks it (EventHandler's
// contract), so it does no I/O beyond the publish itself and holds no lock. It
// must also tolerate being called before bs.cl is set: handlersFor is built and
// installed before client.Connect returns, so the very first StateChanged of a
// binding's life arrives while the Manager still has no *client.Client for it.
// Nothing here reads one.
func (m *Manager) onClientEvent(bs *bindingState, ev client.Event) {
	// Audited first, and separately: a sampling event is never a status — a
	// server being told no about spending money says nothing about whether it is
	// up — so it has its own sink rather than a branch in statusFor. See
	// sampleAudit.
	m.sampleAudit(bs, ev)
	status, ok := m.statusFor(bs.binding.Name, ev)
	if !ok {
		return
	}
	m.publish(status)
}

// publish addresses a status to the Manager's Session, validates it, and sends it
// to the host.
//
// Addressing happens here rather than in status because it is the part that can
// fail: a Manager that has not been bound to a Session yet has no coordinate to
// stamp, and a session-scoped event without one is refused by ValidateEvent and
// could only misroute if it were not. Such a status is dropped, and the drop is
// bounded and repaired rather than silent — BindSession republishes every
// binding's current status the moment there is a Session to publish to. See
// attach.go, which owns that window and explains why nothing durable is in it.
//
// The validation is not ceremony. hub.PublishEvent does NOT validate a public
// event's body — it checks nil, visibility, and type, then (for an Ephemeral
// event) delivers without ever reaching the journal's append-time validation. So
// the producer is the validator, which is the same contract loopruntime honors
// for its own Ephemeral bodies (it calls event.ValidateEvent before publishing
// ContextPressure). An adapter that skipped it could put an unbounded Detail or
// an undeclared State on the stream and nothing downstream would stop it.
//
// A rejected status is dropped rather than escalated: it is a defect on this
// side of the boundary, and there is no caller to return it to — this runs on a
// client's event goroutine. It goes to the Reporter, which is exactly the sink
// for "something MCP knows that Harness cannot say".
func (m *Manager) publish(status event.IntegrationStatus) {
	sessionID, bound := m.boundSession()
	if !bound {
		return
	}
	status.Coordinates.SessionID = sessionID
	if err := event.ValidateEvent(status); err != nil {
		m.report(Notice{
			Kind:    NoticeEventRejected,
			Binding: status.Name,
			Message: fmt.Sprintf("an integration status was not published: %v", err),
		})
		return
	}
	// The Manager's own context, not a caller's: this is a background
	// notification on a client's goroutine, and the only thing that should stop
	// it is the Manager shutting down.
	if err := m.deps.Events.PublishEvent(m.ctx, status); err != nil {
		m.report(Notice{
			Kind:    NoticeEventRejected,
			Binding: status.Name,
			Message: fmt.Sprintf("the host refused an integration status: %v", err),
		})
	}
}

// statusFor maps one client event onto the status it implies. ok is false for an
// event that says nothing about whether the binding is up.
//
// It is a pure function of the event, deliberately. The alternative — remembering
// the last failure on the binding and attaching it to the next StateChanged —
// would need a lock on a path that runs on the client's goroutine, and would
// still be a guess about ordering. Instead each event contributes the status it
// can prove on its own, and the stream self-heals: event.IntegrationStatus is
// Ephemeral precisely because the latest one supersedes every earlier one, so a
// consumer renders the last and is never wrong for long.
func (m *Manager) statusFor(binding string, ev client.Event) (event.IntegrationStatus, bool) {
	switch e := ev.(type) {
	case client.StateChanged:
		state, ok := integrationState(e.To)
		if !ok {
			return event.IntegrationStatus{}, false
		}
		return m.status(binding, state, ""), true

	case client.ConnectionLost:
		// Retrying is the whole difference between "wait" and "act": a binding
		// that will reconnect is degraded, and one that will not is failed.
		state := event.IntegrationFailed
		if e.Retrying {
			state = event.IntegrationDegraded
		}
		return m.status(binding, state, fmt.Sprintf("the connection was lost (%s)", e.Class)), true

	case client.ConnectionRestored:
		// Drift is deliberately not reported here. It is bounded text describing
		// a server's own claimed identity, which makes it server-influenced
		// content, and Detail is not where server content goes. A host that wants
		// it reads Manager.Status, where the client records it as metadata.
		return m.status(binding, event.IntegrationReady, "the connection was re-established"), true

	case client.CatalogRejected:
		// Degraded, never Failed: a rejected refresh leaves the adopted
		// generation in force (see client.CatalogRejected). The binding's
		// existing tools go on working; it is its CHANGES that are unavailable.
		return m.status(binding, event.IntegrationDegraded,
			fmt.Sprintf("a catalog refresh was rejected (%s); generation %d is still in force", e.Class, e.Adopted)), true

	default:
		// Everything else is not a statement about whether the binding is up.
		// CatalogStale/Candidate/Refreshed/Adopted are a healthy server changing
		// its mind; ServerLog is a server's diagnostics; RequestProgress is a
		// server's claim about one call. A default case rather than an
		// enumeration is required: client.Event is sealed to pkg/client, which
		// may add a member, and an unknown member must be ignored rather than
		// panic a client's event goroutine.
		return event.IntegrationStatus{}, false
	}
}

// status builds one status's body. It is deliberately not addressed to a Session:
// publish does that, because the Session is the one part of a status that a
// Manager may not know yet (see attach.go).
//
// The EventID is minted here because the hub does not mint one for a public
// event — it stamps only the session events it derives itself — and
// ValidateEvent requires it. A minting failure yields a zero EventID, which
// ValidateEvent then refuses in publish; that is the fail-closed answer, and it
// is better than publishing an event with no identity.
func (m *Manager) status(binding string, state event.IntegrationState, detail string) event.IntegrationStatus {
	id, err := uuid.New()
	if err != nil {
		id = uuid.UUID{}
	}
	return event.IntegrationStatus{
		Header: event.Header{
			EventID:         id,
			EventVisibility: event.Public,
		},
		Source: IntegrationSource,
		Name:   binding,
		State:  state,
		Detail: detail,
	}
}

// integrationState projects a binding's lifecycle onto the five states a
// consumer outside this module can act on. ok is false for a state that is not
// one of this client's — which cannot happen, since client.State is a closed
// mirror of internal/lifecycle, but a default that invented a status would be
// worse than one that publishes nothing.
//
// The projection is lossy on purpose. event.IntegrationState is coarse because
// it must describe a language server as readily as an MCP binding, so the four
// ways a connection can be coming up all read as Starting, and the two ways it
// can be half-working all read as Degraded. A host that needs the full state
// machine reads Manager.Status, which still has it.
func integrationState(s client.State) (event.IntegrationState, bool) {
	switch s {
	case client.StateConfigured, client.StateStarting, client.StateAuthenticating, client.StateDiscovering:
		return event.IntegrationStarting, true
	case client.StateReady:
		return event.IntegrationReady, true
	case client.StateDegraded, client.StateReconnecting:
		// Reconnecting is Degraded rather than Failed because the binding has not
		// given up: its adopted catalog survives a lost connection, and a call
		// that arrives during a reconnect fails as itself rather than ending a
		// turn.
		return event.IntegrationDegraded, true
	case client.StateFailed:
		return event.IntegrationFailed, true
	case client.StateClosing, client.StateClosed:
		return event.IntegrationClosed, true
	default:
		return 0, false
	}
}
