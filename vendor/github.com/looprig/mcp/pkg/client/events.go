// This file defines the catalog members of the Event union and the client's
// event emitter.
//
// Every event is safe metadata by construction, which is a property of what the
// types can hold rather than of what the emitters remember to put in them: there
// is no field on any event here that can carry a credential, a tool argument, a
// resource body, or an unbounded string. The one free-text field an event has
// (Message, on the failure-shaped events) is filled from an *Error's already
// bounded Msg. See events_test.go, which sweeps the union by reflection and
// fails on a field that could hold anything else.
//
// The sealed Event interface and StateChanged live in handlers.go, next to the
// EventHandler that receives them.

package client

import "time"

// CatalogStale reports that a server announced a change to one of its catalog
// families. It means a refresh has been scheduled, not that anything has been
// fetched, validated, or adopted.
//
// It carries no delta: MCP's list-change notifications say only that a list
// changed, and a server's own account of how it changed would be untrusted
// input. What changed is knowable only from the candidate that follows.
type CatalogStale struct {
	// Binding names the binding whose catalog went stale.
	Binding Name
	// Family is the catalog family the server says changed: "tools",
	// "prompts", or "resources".
	Family string
	// At is when the notification was observed.
	At time.Time
}

// ResourceUpdated reports that a server announced a change to a resource this
// binding subscribed to (see Client.Subscribe). It arrives only for a
// subscribed resource, and only until Client.Unsubscribe.
//
// Like CatalogStale it carries no delta: MCP's resource-update notification says
// only that a resource changed, never how, and a server's account of its own
// change would be untrusted input. A caller that wants the new value re-reads
// the resource with ReadResource. The URI is server-supplied and bounded before
// it gets here — it names what changed, it is not an instruction.
type ResourceUpdated struct {
	// Binding names the binding whose subscribed resource changed.
	Binding Name
	// URI names the resource the server says changed. It may be a sub-resource
	// of the one that was subscribed to.
	URI string
	// At is when the notification was observed.
	At time.Time
}

// CatalogCandidate reports that a complete, validated generation was fetched and
// differs from the adopted one. It is the event a caller waits for before
// choosing a safe boundary at which to Adopt.
//
// The candidate is not adopted by this event. Nothing a Loop can see has changed
// yet — that is the point of the candidate/adopted split.
type CatalogCandidate struct {
	// Binding names the binding.
	Binding Name
	// Generation is the candidate's ordinal, and the value Adopt takes.
	Generation uint64
	// Digest is the candidate's hex catalog digest.
	Digest string
	// Adopted is the ordinal of the generation still in force.
	Adopted uint64
	// At is when the candidate was published.
	At time.Time
}

// CatalogRefreshed reports that a refresh completed and produced a catalog
// identical to the adopted one — the server announced a change that, once
// refetched and digested, turned out not to change what this binding sees.
//
// It is a distinct event from CatalogCandidate rather than a candidate nobody
// need adopt, because the two ask different things of a caller: a candidate
// needs a safe boundary and an Adopt, and this needs nothing at all. Publishing
// a no-op candidate would make every caller re-derive that distinction, and some
// would get it wrong by adopting — churning a toolset generation for a catalog
// that did not change.
type CatalogRefreshed struct {
	// Binding names the binding.
	Binding Name
	// Generation is the ordinal of the generation still in force.
	Generation uint64
	// Digest is its hex catalog digest.
	Digest string
	// At is when the refresh completed.
	At time.Time
}

// CatalogAdopted reports that a candidate became the binding's adopted
// generation, at the caller's request. It is the completion of the sequence
// CatalogStale -> CatalogCandidate -> (caller's safe boundary) -> Adopt.
type CatalogAdopted struct {
	// Binding names the binding.
	Binding Name
	// Generation is the newly adopted ordinal.
	Generation uint64
	// Digest is its hex catalog digest.
	Digest string
	// Previous is the ordinal it replaced, or 0 if there was none.
	Previous uint64
	// At is when adoption happened.
	At time.Time
}

// CatalogRejected reports that a refresh failed to produce a usable candidate:
// the server was unreachable, its catalog was defective, or it exceeded a bound.
//
// The prior adopted generation remains in force. A rejection never blanks a
// catalog that was valid — a server that starts answering badly makes its
// *changes* unavailable, not its existing tools.
type CatalogRejected struct {
	// Binding names the binding.
	Binding Name
	// Class classifies why the refresh failed.
	Class FailureClass
	// Message is the failure's bounded, normalized text.
	Message string
	// Adopted is the ordinal of the generation still in force.
	Adopted uint64
	// Retrying reports whether the binding will try again under its policy.
	Retrying bool
	// At is when the refresh failed.
	At time.Time
}

// ConnectionLost reports that a binding's connection failed for a reason that
// means it is gone: a transport that closed, a stream that desynchronized.
//
// It does not mean the binding is unusable. What it means is that every request
// in flight when it happened has failed — tool calls indeterminately, since the
// connection took the evidence with it — and that the binding is degraded until
// it reconnects.
type ConnectionLost struct {
	// Binding names the binding.
	Binding Name
	// Class classifies the failure that ended the connection.
	Class FailureClass
	// Message is the failure's bounded, normalized text.
	Message string
	// Adopted is the ordinal of the generation still in force. It survives a
	// lost connection.
	Adopted uint64
	// Retrying reports whether the binding will try to reconnect. It is false
	// when policy forbids it, and false on the last report when the retry
	// budget is spent.
	Retrying bool
	// At is when the loss was observed.
	At time.Time
}

// ConnectionRestored reports that a binding has a new logical connection, with
// its own handshake and its own freshly discovered catalog.
//
// It is not an adoption. The catalog that came with the new connection is a
// candidate like any other: the caller adopts it at a safe boundary, because a
// socket reconnecting is not a reason to change what a model was told mid-turn.
type ConnectionRestored struct {
	// Binding names the binding.
	Binding Name
	// Server is what the reconnected server claims to be. Cosmetic: it names a
	// peer, it never authorizes one.
	Server ServerIdentity
	// Drift is bounded text describing how the server's identity differs from
	// the one this binding first connected to, or empty when it is the same
	// server. It is reported, never enforced — a restarted server with a new
	// version is the ordinary reason to be reconnecting.
	Drift string
	// Adopted is the ordinal of the generation still in force, which the
	// reconnect did not touch.
	Adopted uint64
	// Generation is the ordinal of the generation discovered over the new
	// connection. It is a candidate unless it was identical to the adopted one,
	// in which case there is nothing to adopt.
	Generation uint64
	// At is when the connection was restored.
	At time.Time
}

// ServerLog reports one log record a server sent. It is the Event-stream mirror
// of LogMessage, for an application that observes a binding through events alone
// rather than installing a separate log handler.
//
// Every field is server-supplied and bounded (Text to Limits.MaxLogMessageBytes)
// before it gets here. It is diagnostics from an untrusted peer: never a fact
// about the host, and never an instruction.
type ServerLog struct {
	// Binding names the server that logged.
	Binding Name
	// Level is the severity the server claimed.
	Level LogLevel
	// Logger is the server-side logger name, if it sent one.
	Logger string
	// Text is the bounded message.
	Text string
	// At is when the record was observed.
	At time.Time
}

// RequestProgress reports one progress notification from an in-flight call.
//
// It arrives only for calls that asked for progress (CallOpts.Progress): a
// server may only send progress for a request carrying a progress token, and a
// token is only attached when the caller installed a callback. So this event
// mirrors that stream for observers, it does not create one.
//
// Progress is a server's claim about itself. It may go backwards, stall, or
// never finish, and it never extends a call's deadline (see CallOpts.Deadline).
type RequestProgress struct {
	// Binding names the server reporting progress.
	Binding Name
	// Progress is how far the server claims to have got.
	Progress float64
	// Total is the server's claimed total, or 0 when it did not say.
	Total float64
	// Message is the server's bounded description of what it is doing.
	Message string
	// At is when the report was observed.
	At time.Time
}

// ElicitationRequested reports that a server asked for human input, and that the
// request passed the boundary's checks and reached the host's handler.
//
// It deliberately carries none of what was asked. The message, the schema and —
// above all — the URL stay out of the event stream: design §Elicitation is
// explicit that the full action URL and its query parameters are not written to
// journals or ordinary events, and a URL's query string is where a server would
// put a token. Mode is what an observer needs and all it gets: enough to know a
// person is being asked something and what kind of thing it is.
//
// It is not a promise that a human saw anything. What happened next is the
// handler's business, and ElicitationResolved is where it is reported.
type ElicitationRequested struct {
	// Binding names the server that asked.
	Binding Name
	// Mode is the kind of elicitation.
	Mode ElicitMode
	// At is when the request reached the handler.
	At time.Time
}

// ElicitationResolved reports how an elicitation ended. Exactly one follows
// every ElicitationRequested.
//
// Action is the answer that went to the server, and it is the zero ElicitAction
// ("unknown") when none did: a handler that failed, timed out, or answered with
// something the client refused to put on the wire. That is a real outcome and
// not a gap — the server was told the request failed — so it is reported rather
// than silently dropped, and it is distinguishable from a decline, which is a
// person's answer rather than a host's failure.
type ElicitationResolved struct {
	// Binding names the server that asked.
	Binding Name
	// Mode is the kind of elicitation, repeated from the request so an observer
	// need not correlate to know what was answered.
	Mode ElicitMode
	// Action is what the server was told, or the zero action when the
	// elicitation failed instead of being answered.
	Action ElicitAction
	// Duration is how long the handler took: the time a person was being waited
	// on, which is the figure an operator wants and the only one this event can
	// honestly report.
	Duration time.Duration
	// At is when the outcome was observed.
	At time.Time
}

func (ConnectionLost) event()       {}
func (ConnectionRestored) event()   {}
func (ServerLog) event()            {}
func (RequestProgress) event()      {}
func (ElicitationRequested) event() {}
func (ElicitationResolved) event()  {}

// # Auth state: deferred, and why
//
// The design's event set also lists auth state. It is not here.
//
// (Elicitation was listed alongside it and is no longer deferred:
// internal/protocol's session now registers an SDK elicitation handler — guarded
// so the capability is never advertised on a client's behalf — and the client
// wires Handlers.Elicitation to it, so the two events above have a real
// producer. See Client.elicitAdapter.)
//
// Auth state has a producer and no seam. This is worth stating precisely,
// because the obvious reading — "nothing authenticates yet" — is false and would
// send whoever picks this up looking in the wrong place:
//
//   - pkg/auth models the posture fully and tracks it live: auth.Status,
//     auth.State (anonymous/required/authenticated/expired/denied/failed), and
//     OAuthProvider.Status(), which reports what the last completed flow
//     established without blocking or performing I/O.
//   - pkg/transport/streamablehttp authenticates for real: Config.Auth is an
//     auth.HeaderProvider consulted per request, and its diagnostics record the
//     first auth failure (see recordAuthError/authError), which already reaches
//     callers as a classified FailureAuth* error.
//
// What is missing is the wire between them, and it is more than a field:
//
//   - protocol.ConnectConfig has OnLog and OnListChanged but no OnAuth. Adding
//     one means either importing pkg/auth into internal/protocol — inverting the
//     module's layering, since protocol is the SDK boundary and today depends on
//     nothing above it — or mirroring auth.State into a neutral enum here, the
//     way client.State mirrors lifecycle.State.
//   - a transport holds an auth.HeaderProvider, whose only method is Headers.
//     It cannot observe a posture through that interface: reaching Status()
//     needs an optional-interface assertion, which is a new cross-package
//     contract rather than a wiring detail.
//   - auth.Status is a snapshot, not a stream, so *when* to read it is a policy
//     decision — realistically after each Headers call, in a transport's
//     per-request path, with change detection so that a steady state does not
//     emit an event per request.
//   - lifecycle.StateAuthenticating is declared and never entered in production
//     as a direct consequence: Connect goes starting -> discovering, because
//     nothing tells it that this binding's transport authenticates at all. That
//     needs its own seam (the client must know before it dials, not after).
//
// So the work is a protocol contract, a transport contract, an observation
// policy, and a lifecycle seam — across three packages, two of them outside this
// phase. It is deferred deliberately rather than approximated: an
// AuthStateChanged that only ever fired on failure would be a worse contract
// than none, and inferring "authenticated" from "the server did not refuse us"
// would put this module's guess where pkg/auth deliberately reports a fact (see
// StatusOf: the states that are not derivable are built by the flow that learns
// them).

func (CatalogStale) event()     {}
func (ResourceUpdated) event()  {}
func (CatalogCandidate) event() {}
func (CatalogRefreshed) event() {}
func (CatalogAdopted) event()   {}
func (CatalogRejected) event()  {}

// emit delivers e to the application's event handler, if it installed one.
//
// It is a method on Client rather than a bare call so that every emitter goes
// through one place: the handler is captured once at construction and read
// without a lock (it is immutable for the Client's life), and a nil handler is
// dropped here rather than at each of a dozen call sites.
//
// The handler runs on the emitting goroutine and blocks it, per EventHandler's
// contract. It is foreign code, so nothing this client guards may be held across
// this call — every caller here emits with c.mu released.
func (c *Client) emit(e Event) {
	if c.eventHandler == nil {
		return
	}
	c.eventHandler(e)
}
