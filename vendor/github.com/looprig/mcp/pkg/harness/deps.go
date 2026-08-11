// This file defines what the adapter needs from its host.
//
// Every interface here is declared on the consumer side, by the package that
// calls it, and each is the narrowest thing that will do: the adapter asks a
// host to open a gate, to publish an event, to say which approval scopes an MCP
// tool may offer, and to tell the time. It does not ask for a Session, a Hub, or
// a config object it would then have to be trusted with (CLAUDE.md: least
// privilege — never pass a god-object when a narrow interface suffices).
//
// # What is deliberately absent: TokenStore and BrowserOpener
//
// The implementation plan lists a token store and a browser opener among these
// dependencies. They are not here, and their absence is a finding rather than an
// omission: this module's own layering means the Manager can never consume them.
//
// A TokenStore and a BrowserOpener are the inputs to auth.NewOAuthProvider,
// which yields an auth.HeaderProvider that an application installs on
// streamablehttp.Config.Auth. That happens while the application is BUILDING the
// transport — and a Binding carries an already-built client.Definition, whose
// Transport is a sealed TransportFactory the Manager only ever dials. By the
// time a Binding exists, auth is wired and its credentials are the transport's
// private business, which is exactly where they should be. Taking a TokenStore
// here would mean holding every binding's credentials in a component that has no
// use for them.
//
// URL elicitation does not need a BrowserOpener either: it becomes a
// gate.OpenURLPayload and the host decides how to present it (design
// §Elicitation), which is the whole point of routing it through a gate.

package mcpharness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// Clock reports the current time. It exists so tests can drive timeouts and
// timestamps deterministically rather than sleeping.
type Clock interface {
	Now() time.Time
}

// systemClock is the real clock, used when Deps.Clock is nil.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// GateRequest asks the host to put a question to a human (or to a policy that
// answers for one). It is the adapter's whole human-input vocabulary: an MCP
// elicitation becomes one of these, and nothing MCP-shaped goes any further
// (design §Elicitation).
type GateRequest struct {
	// Kind is the gate scenario: gate.KindForm or gate.KindOpenURL.
	Kind gate.Kind
	// Payload is the authoritative request. It is a sealed gate.Payload, so a
	// host cannot be handed a shape the gate codec would reject.
	Payload gate.Payload
	// Prompt is the presentation projection an opener renders.
	Prompt gate.Prompt
	// Restorable states whether the gate may survive a restore boundary. An
	// open-url gate must never be restorable — its action target is
	// deliberately not journaled, so a restored gate could only ever be a
	// broken one, and gate.ValidateGate rejects it (see gate.OpenURLPayload).
	Restorable bool
	// Binding names the MCP binding that asked, for attribution.
	Binding string
	// LoopID is the Loop the request is on behalf of, or zero for a
	// Session-scoped binding's own startup — an elicitation raised during
	// initialization belongs to no Loop yet.
	LoopID uuid.UUID
}

// GateResponse is how the gate was answered. Action is one of the
// gate.FormAction* values; Values carries form answers and is nil for anything
// but an accepted form.
type GateResponse struct {
	Action string
	Values map[string]string
}

// GateOpener opens a Harness gate and blocks until it is resolved.
//
// It must honor ctx: the adapter cancels a pending elicitation when its MCP
// request is cancelled or its binding shuts down, and a host that ignored that
// would strand a server request nobody can answer.
type GateOpener interface {
	OpenGate(ctx context.Context, req GateRequest) (GateResponse, error)
}

// SampleRole is who authored a sampling message. The zero value is not a role.
type SampleRole uint8

// The sampling message roles.
const (
	// SampleRoleUser marks a message the server attributes to the user.
	SampleRoleUser SampleRole = iota + 1
	// SampleRoleAssistant marks a message the server attributes to the model.
	SampleRoleAssistant
)

// String returns a stable lowercase identifier, or "unknown".
func (r SampleRole) String() string {
	switch r {
	case SampleRoleUser:
		return "user"
	case SampleRoleAssistant:
		return "assistant"
	default:
		return "unknown"
	}
}

// SampleMessage is one turn of a sampling conversation. Both fields are the
// server's: it chose the text and it chose whom to attribute it to. Neither is
// an instruction to the host.
type SampleMessage struct {
	// Role is who the server says authored this message. It is always a
	// declared role.
	Role SampleRole
	// Text is the message body, bounded by the binding's Limits.
	Text string
}

// SampleRequest is a server asking the host to spend model budget on its
// behalf.
//
// Every field but Binding and LoopID is server-supplied and untrusted. The
// server's own steering — its model preferences, its temperature, its request to
// have the host harvest conversation context — is dropped by pkg/client before
// this exists, so what arrives here is the request reduced to what a host can
// safely act on: some text, and a ceiling.
//
// # Why this type has nowhere to put a reference
//
// Every field is a string, an int, or an array of bytes. That is the design's
// "sampling never receives a Harness Session controller or unrestricted tool
// registry", enforced structurally: a SamplingPolicy cannot be handed authority
// through this type because there is no field shaped like authority, and a field
// added later must argue with TestSamplingSeamCarriesNoAuthority, which sweeps
// this struct by field KIND. pkg/client holds up the same end for its own
// SampleRequest, and internal/protocol registers only the SDK's tool-free
// handler — so a tool cannot enter from the server's side either.
type SampleRequest struct {
	// Binding names the server that asked. It is the whole of "which authority
	// is this": a policy that trusts one server to spend and not another
	// switches on this.
	Binding string
	// LoopID is the Loop the request is on behalf of, or zero for a
	// Session-scoped binding.
	//
	// The zero is not a gap, for the reason GateRequest.LoopID gives: a
	// Session's server is shared, and a sampling request it raises belongs to the
	// Session rather than to whichever Loop happened to be calling. A policy that
	// budgets per Loop must therefore treat the zero as "the Session's own
	// budget" rather than as an unattributed request to be waved through.
	LoopID uuid.UUID
	// SystemPrompt is the system prompt the server asked for, bounded. It is a
	// request, not a fact: a policy is free to replace or ignore it.
	SystemPrompt string
	// Messages is the conversation the server wants completed. Never empty:
	// pkg/client refuses a sampling request with no messages before it reaches
	// here.
	Messages []SampleMessage
	// MaxTokens is the completion budget, already capped against the binding's
	// Limits.MaxSamplingTokens. A server may lower this ceiling and never raise
	// it, so it is an upper bound the policy may spend within — not a number to
	// honor.
	MaxTokens int
}

// SampleResult is the completion the host's policy produced.
type SampleResult struct {
	// Model names the model the policy actually ran, which need not be one the
	// server asked for. It is required: pkg/client refuses a result without one,
	// because a completion nobody can attribute to a model is not auditable.
	Model string
	// Text is the completion.
	Text string
	// StopReason is why generation stopped.
	StopReason string
}

// ErrSamplingDenied is how a SamplingPolicy refuses to spend. Return it, or
// anything wrapping it, and the server is told the host declined; the request is
// audited as denied rather than failed.
//
// The distinction is worth the sentinel. A denial is policy working — a host is
// always entitled to decline to spend, and an operator reading a stream of them
// is reading a budget being enforced. A failure is the host trying and breaking,
// which is a defect. A policy that returned an ordinary error for both would make
// its own budget enforcement indistinguishable from its own bugs.
var ErrSamplingDenied = errors.New("mcp: the sampling policy declined")

// SamplingPolicy services a server's request for an LLM completion, under the
// application's policy and nobody else's.
//
// # What the adapter does and does not decide
//
// Server-requested sampling gives a server authority to initiate model work and
// spend (design §Optional sampling). The application supplies model selection,
// budget, permission, recursion, tool-use, and content policy; this adapter
// invents none of it. It routes the request, attributes it to a binding and a
// Loop, translates the answer, and audits both — and that is the whole of its
// job. There is deliberately no default: a nil Deps.Sampling is not a permissive
// policy or a restrictive one, it is no policy, and a binding that asked to
// advertise the capability without one fails to start (see Deps.Sampling).
//
// Sample is called on the connection's goroutine while the server waits. It must
// honor ctx, which already carries the binding's request timeout and dies with
// the connection: a policy that ignored it would pin a dispatch goroutine and a
// sampling slot on a completion nobody is waiting for any more.
//
// The caps that bound how MUCH a server may ask for — tokens, concurrency, chain
// depth — are pkg/client's and are enforced before this is called, so a policy
// never has to implement them to be safe. What a policy adds is whether this
// spend is one the application wants to make at all.
type SamplingPolicy interface {
	Sample(ctx context.Context, req SampleRequest) (SampleResult, error)
}

// EventPublisher receives the adapter's protocol-neutral integration events.
//
// The method is named PublishEvent so that *hub.Hub satisfies this interface
// structurally, with no adapter in between — the same deliberately structural
// coupling harness uses for its CommandRunner seam. The adapter still declares
// the interface, so it depends on the one method it calls rather than on the
// Hub.
type EventPublisher interface {
	PublishEvent(ctx context.Context, ev event.Event) error
}

// Approval persistence breadth is no longer this module's concern. Under the
// access-profile model the three-action Deny/Gated/Allow decision — and how
// broadly an Allow may persist — is owned by the harness gate, which resolves
// the tool.invoke requirement each MCP tool emits from PrepareCall against the
// consumer's product access source. The adapter's whole permission contribution
// is that stable, redacted requirement (see tools.go: CapabilityToolInvoke,
// ToolInvokeIdentity); it holds no scope policy of its own, so there is nothing
// here to configure or default.

// NoticeKind classifies an adapter notice. The zero value is not a kind.
type NoticeKind uint8

const (
	// NoticeToolNameCollision reports a binding refused from a Loop's
	// namespace because one of its model-facing names is already held by
	// another binding. The offending binding contributes nothing to that Loop;
	// the incumbent keeps serving.
	NoticeToolNameCollision NoticeKind = iota + 1
	// NoticeAdopted reports that a Loop's toolset was replaced with a newer
	// catalog generation at its idle boundary.
	NoticeAdopted
	// NoticeAdoptionFailed reports that a replacement was refused. The Loop
	// keeps the generation it had; nothing partial was installed.
	NoticeAdoptionFailed
	// NoticeAdoptionUnsupported reports a Loop that cannot host external tools
	// at all — a foreign loop, whose toolset belongs to its foreign agent. It
	// is a permanent property of the Loop, not a failure to retry.
	NoticeAdoptionUnsupported
	// NoticeEventRejected reports an integration status that could not be
	// published: this adapter built one the event contract refuses, or the host
	// declined it. It is the sink of last resort — the fact that the sink for
	// facts is broken has nowhere else to go.
	NoticeEventRejected
	// NoticeElicitationDeclined reports a server's request for human input that
	// was refused without ever being shown to anyone: an unsupported schema, a
	// field soliciting a credential, an unusable URL. The server is told
	// "decline"; this says why, which the server is deliberately not told.
	NoticeElicitationDeclined
	// NoticeSamplingRequested reports that a server asked the host to spend
	// model budget. It is raised when the request ARRIVES, not when it is
	// admitted: a request the client's depth or concurrency cap refused is
	// exactly the one an operator wants to see.
	//
	// It carries the request's shape — how many messages, what budget, what
	// chain depth — and none of its content. See sampleAudit.
	NoticeSamplingRequested
	// NoticeSamplingResolved reports how a sampling request ended. Exactly one
	// follows every NoticeSamplingRequested, including for the requests refused
	// before any model ran.
	NoticeSamplingResolved
	// NoticeSamplingDenied reports WHY the host refused, which the server is
	// deliberately not told — the same rule NoticeElicitationDeclined follows,
	// for the same reason: a server that can enumerate a host's policy can
	// search for a request that gets past it.
	NoticeSamplingDenied
)

// String returns a stable lowercase identifier, or "unknown".
func (k NoticeKind) String() string {
	switch k {
	case NoticeToolNameCollision:
		return "tool_name_collision"
	case NoticeAdopted:
		return "adopted"
	case NoticeAdoptionFailed:
		return "adoption_failed"
	case NoticeAdoptionUnsupported:
		return "adoption_unsupported"
	case NoticeEventRejected:
		return "event_rejected"
	case NoticeElicitationDeclined:
		return "elicitation_declined"
	case NoticeSamplingRequested:
		return "sampling_requested"
	case NoticeSamplingResolved:
		return "sampling_resolved"
	case NoticeSamplingDenied:
		return "sampling_denied"
	default:
		return "unknown"
	}
}

// Notice is one thing the adapter needs an operator to be able to see.
//
// It exists because the design's "reports" cannot be an event.Event: the
// Harness event set is sealed (Event.isEvent is unexported), so no external
// module can add a member, and none of the existing members can express "this
// binding's tools were refused for this Loop". Deps.Events therefore carries
// the events Harness itself defines, and this carries the facts only MCP knows.
// The alternative — inventing a Harness event type for a protocol Harness must
// not know about — is exactly what this module's boundary exists to prevent.
//
// Every field is safe to log as-is: names are validated identifiers and
// Message is this module's own bounded text.
type Notice struct {
	// Kind classifies the notice.
	Kind NoticeKind
	// Binding names the binding concerned.
	Binding string
	// LoopID is the Loop concerned, or zero when the notice is not about one.
	LoopID uuid.UUID
	// Generation is the catalog generation concerned, or 0.
	Generation uint64
	// Message is bounded, redacted explanatory text.
	Message string
}

// Reporter receives the adapter's notices. It is optional; a nil Deps.Reporter
// drops them.
//
// Report must not block: it is called on the goroutine that discovered the
// fact, which may be a Loop's adoption path or a live toolset assembly. Hand
// off anything expensive.
type Reporter interface {
	Report(Notice)
}

// report delivers a notice when a Reporter is installed.
func (m *Manager) report(n Notice) {
	if m.deps.Reporter == nil {
		return
	}
	m.deps.Reporter.Report(n)
}

// Deps are the host capabilities the Manager needs. Gates and Events are
// required; the rest select documented defaults when nil.
type Deps struct {
	// SessionID identifies the Session these bindings belong to. It is the
	// coordinate every event.IntegrationStatus is stamped with, and an event
	// carrying the wrong Session's ID — or none — is one that either reaches the
	// wrong subscribers or is refused as invalid. The Manager cannot derive it:
	// it is deliberately given a GateOpener and an EventPublisher rather than a
	// Session (see this file's header), so nothing it holds knows the answer.
	//
	// It is OPTIONAL, and the zero value is a supported way to build a Manager
	// rather than a mistake: an application that contributes its MCP identity to
	// the Session's config fingerprint must discover its servers BEFORE the
	// Session exists, so there is no ID to give yet. Such an application leaves
	// this zero and calls BindSession once rig.NewSession has minted one. See
	// attach.go, which owns that ordering and is the only reason this field is
	// optional.
	//
	// Supplying it here is still the right thing for an application that does not
	// contribute a fingerprint: the Manager is attached from birth and never has
	// a window in which a status has nowhere to go.
	SessionID uuid.UUID
	// Gates routes elicitation to a human. Required: a binding that advertises
	// the elicitation capability with nowhere to ask would be a lie told to a
	// server.
	Gates GateOpener
	// Events receives integration events. Required: a binding whose failures
	// nobody can observe is one an operator cannot fix.
	Events EventPublisher
	// Sampling services servers' requests for LLM completions, under the
	// application's own policy. Optional, and its absence is the default: this
	// module never spends a host's model budget because it was composed, only
	// because it was asked to.
	//
	// # Nil means the capability is not advertised, and that is not a downgrade
	//
	// A nil here yields a nil client.Handlers.Sampling for every binding (see
	// Manager.samplingHandler). That is the whole of the fail-closed rule, and it
	// is deliberately NOT a rule this file enforces a second time:
	// client.Handlers.advertised already owns it, and owns it for elicitation and
	// roots identically — a capability the Definition requests with no handler to
	// serve it is a FailureInvalidConfig, never a silent downgrade. So a Binding
	// whose Server.Capabilities.Sampling is true with no policy installed fails to
	// connect, before its transport is ever dialed, with the same classified error
	// an application would get from pkg/client directly. A required binding takes
	// its owner's startup down with it; an optional one fails as a whole, serving
	// nothing — never a binding that quietly comes up with its tools and without
	// the capability it advertised.
	//
	// Re-deriving that check here would be a second opinion on a decision one
	// package already makes: two places to keep in step, and the wrong one wins
	// whenever they disagree.
	//
	// # Why this is not a field on Binding
	//
	// Sampling authority IS per-binding, and it already lands there: a binding
	// advertises the capability only if its own Server.Capabilities.Sampling says
	// so, and SampleRequest.Binding names the asker, so one policy decides per
	// server. What a Binding must not hold is the policy VALUE. A Binding is
	// immutable configuration that gets digested into the Session's configuration
	// identity (see identity.go), and an interface has no stable encoding to
	// digest — it would either be excluded, making the digest lie about what a
	// binding may spend, or included, making a restore report drift because the
	// host allocated a new policy. Deps is where host capabilities live, for
	// exactly this reason: Gates is not a Binding field either.
	Sampling SamplingPolicy
	// Reporter receives the adapter's own notices — the facts the sealed
	// Harness event set cannot express. Optional; nil drops them.
	Reporter Reporter
	// Clock is the time source. Nil selects the system clock.
	Clock Clock
}

// normalized returns deps with its optional members filled in. It never mutates
// the caller's value.
func (d Deps) normalized() Deps {
	if d.Clock == nil {
		d.Clock = systemClock{}
	}
	return d
}

// validate rejects a Deps missing a required member. It fails closed at
// construction: a nil Gates discovered at the moment a server asks for input is
// a server request that hangs, and a nil Events discovered during a failure is a
// failure nobody sees.
//
// SessionID is deliberately not checked. It is optional (see the field), and the
// thing that keeps a Manager from running unattached by accident is
// StartAdoption, which refuses one — a check at the moment the omission would
// start costing an operator events, rather than one at a moment an application
// may honestly not have the answer.
func (d Deps) validate() error {
	if d.Gates == nil {
		return fmt.Errorf("Deps.Gates is nil; supply a GateOpener")
	}
	if d.Events == nil {
		return fmt.Errorf("Deps.Events is nil; supply an EventPublisher")
	}
	return nil
}
