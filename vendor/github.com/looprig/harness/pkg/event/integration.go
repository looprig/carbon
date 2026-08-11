package event

// # Why Harness defines an event for something Harness does not implement
//
// An integration is any live external capability a Session runs alongside its
// loops: an MCP server set, a language server, a plugin host. Harness does not
// implement one and must not know what any of them speak.
//
// It must still be able to SAY how one is doing. The event union is sealed
// (Event.isEvent is unexported), so no external module can ever add a member —
// which means an integration living outside this module has no way to put its
// own status on the stream that every consumer already reads, and no composition
// root can bridge one on its behalf. The alternative that keeps appearing is a
// side-channel: a reporter interface the integration hands its host, which the
// host then renders separately. That works, and it is what the MCP adapter does
// today for the facts only MCP knows — but it means the one question an operator
// asks about an integration ("is it up?") is the one question the event stream
// cannot answer, and every consumer has to grow a second subscription with its
// own lifetime, ordering, and failure modes to learn it.
//
// So the seam is here, and it is deliberately coarse. This event carries no
// protocol vocabulary: Source and Name are opaque identifiers the integration
// chooses, State is the five-value answer an operator acts on, and Detail is
// bounded prose. Nothing in it is MCP-shaped, and nothing in it would have to
// change to carry a language server instead.

// IntegrationState is how an integration is doing, in the only granularity a
// consumer outside it can act on. The zero value is not a state.
//
// It is five values on purpose. An integration's own lifecycle is invariably
// richer — the MCP client alone distinguishes configured, starting,
// authenticating, discovering, ready, degraded, reconnecting, failed, closing,
// and closed — and mirroring that here would be this package learning one
// integration's model and imposing it on the next. The projection is lossy by
// design; an integration that needs its full state machine seen renders it from
// its own status surface, which it still owns.
type IntegrationState uint8

const (
	// IntegrationStarting is an integration coming up. It is not serving yet,
	// and it is not a fault: everything is starting once.
	IntegrationStarting IntegrationState = iota + 1
	// IntegrationReady is an integration serving normally.
	IntegrationReady
	// IntegrationDegraded is an integration still serving, but with less than
	// it advertised — a lost connection it is retrying, a capability that
	// failed. It is distinct from Failed because the difference is actionable:
	// a degraded integration may recover on its own.
	IntegrationDegraded
	// IntegrationFailed is an integration not serving. It may still recover.
	IntegrationFailed
	// IntegrationClosed is an integration that has shut down. It is terminal
	// for this Session.
	IntegrationClosed
)

// String returns a stable lowercase identifier, or "unknown".
func (s IntegrationState) String() string {
	switch s {
	case IntegrationStarting:
		return "starting"
	case IntegrationReady:
		return "ready"
	case IntegrationDegraded:
		return "degraded"
	case IntegrationFailed:
		return "failed"
	case IntegrationClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a declared state. It fails closed on the zero value
// and on anything outside the set, which is what makes an unset State a
// rejected event rather than a silently rendered "starting".
func (s IntegrationState) Valid() bool {
	switch s {
	case IntegrationStarting, IntegrationReady, IntegrationDegraded, IntegrationFailed, IntegrationClosed:
		return true
	default:
		return false
	}
}

// Maximum sizes for the free-text and identifier fields. They exist because an
// integration is not part of this module and its inputs are not this module's to
// trust: a server-influenced name or message must not be able to grow an event
// without bound. See ValidateEvent, which enforces them at the publish boundary.
const (
	// MaxIntegrationSourceBytes caps Source.
	MaxIntegrationSourceBytes = 64
	// MaxIntegrationNameBytes caps Name.
	MaxIntegrationNameBytes = 128
	// MaxIntegrationDetailBytes caps Detail.
	MaxIntegrationDetailBytes = 512
)

// IntegrationStatus reports the live state of one external integration.
//
// It is Ephemeral, which is a claim about what it means rather than a
// convenience: a status self-heals — the next one supersedes it completely, and
// a consumer that missed three learns the truth from the fourth. It is also the
// only honest classification available. An integration is a LIVE resource that a
// restore reconstructs by reconnecting, never by replaying bytes, so a journaled
// status could only ever describe a connection that no longer exists. Persisting
// one would invite a restored Session to render a server as "ready" before
// anything had dialled it.
//
// Every field is safe to journal even though none of it is: Source and Name are
// the integration's own validated identifiers, State is a closed enum, and
// Detail is bounded prose the integration has already redacted. Credentials,
// tokens, URLs with secrets, raw arguments, and server content must never appear
// in Detail — an integration that cannot say what is wrong without one says less.
type IntegrationStatus struct {
	ephemeral
	sessionScoped
	Header
	// Source names the kind of integration, e.g. "mcp". It is the namespace
	// Name lives in: two integrations may both call a binding "github".
	Source string `json:"source"`
	// Name identifies the one integration within Source, e.g. a binding name.
	Name string `json:"name"`
	// State is how it is doing.
	State IntegrationState `json:"state"`
	// Detail is bounded, redacted explanatory text, or empty. It is
	// diagnostics, never an instruction and never a fact the host should act
	// on programmatically — that is what State is for.
	Detail string `json:"detail,omitempty"`
}

func (IntegrationStatus) isEvent() {}
