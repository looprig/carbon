package app

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/session"
	mcpauth "github.com/looprig/mcp/pkg/auth"
	mcpclient "github.com/looprig/mcp/pkg/client"
	mcpharness "github.com/looprig/mcp/pkg/harness"
	"github.com/looprig/mcp/pkg/transport/sse"
	"github.com/looprig/mcp/pkg/transport/stdio"
	"github.com/looprig/mcp/pkg/transport/streamablehttp"
)

// mcpEnvPassThrough is the fixed, minimal baseline of this process's
// environment a stdio MCP server child may inherit, if a given name happens
// to be set. Nothing else in this process's environment reaches a child by
// default -- design §1.5's fail-closed posture for a server's environment: a
// server that needs a credential must receive it explicitly, via
// mcpServerSpec.env (mcpEnvVarsFrom), never by inheriting this process's
// wider environment.
var mcpEnvPassThrough = []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}

// mcpAllRoles is the sole Generic loop identity -- the exact loop name a
// binding's Visibility selects by -- used when roles are omitted or empty.
var mcpAllRoles = []string{"generic"}

// mcpDefinitions turns a validated spec list (loadMCPConfig's result) into
// ready-to-hand-to-Manager bindings. It is pure construction: no network
// call and no long-lived process are started here. transport.New only
// validates configuration -- for stdio that includes resolving the command
// via exec.LookPath -- and a later task's Manager.Start is what actually
// connects.
//
// A construction failure on any one spec aborts the whole batch immediately
// and returns (nil, err): mcpDefinitions never returns a partial slice
// alongside an error, matching this feature's fail-closed posture
// throughout (design §1.5). Specs are consumed in the order given; Task 7's
// normalizeMCPConfig already returns them sorted by binding name, so the
// result is deterministic for free.
func mcpDefinitions(specs []mcpServerSpec) ([]mcpharness.Binding, error) {
	bindings := make([]mcpharness.Binding, 0, len(specs))
	for _, spec := range specs {
		binding, err := mcpBindingFor(spec)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// mcpBindingFor builds and validates one spec's Binding.
//
// Both failure points here -- transport construction and Binding.Validate --
// already return errors from the mcp module's own *client.Error family,
// which is secret-free and bounded by that module's own discipline. They
// are wrapped in coderig's own *MCPConfigError anyway, naming spec.name as
// Binding, for two reasons: transport.New's own errors are built with an
// empty Binding (every transport's New always calls client.NewError with
// binding ""), so without this wrap a caller could not tell which server
// failed at all; and wrapping both failure points the same way keeps this
// feature's whole error family -- Tasks 6, 7, and this one -- consistent,
// rather than having some mcp.json failures carry a typed, bounded
// *MCPConfigError and others carry a raw *client.Error the rest of the
// package does not otherwise handle.
func mcpBindingFor(spec mcpServerSpec) (mcpharness.Binding, error) {
	factory, err := mcpTransportFor(spec)
	if err != nil {
		return mcpharness.Binding{}, mcpConfigFailure(spec.name, "transport", err)
	}

	binding := mcpharness.Binding{
		Name: spec.name,
		Server: mcpclient.Definition{
			Name:      mcpclient.Name(spec.name),
			Transport: factory,
			Compat:    mcpCompatFor(spec.kind),
		},
		Scope:      mcpharness.ScopeSession,
		Visibility: mcpharness.Named(mcpVisibilityRoles(spec.roles)...),
		Required:   false,
	}
	if err := binding.Validate(); err != nil {
		return mcpharness.Binding{}, mcpConfigFailure(spec.name, "binding", err)
	}
	return binding, nil
}

// mcpCompatFor resolves the compatibility profile a spec's Definition needs.
//
// This is a deviation from a literal "zero Timeouts/Limits/Compat = defaults"
// reading of the task: client.Definition.Validate's checkTransportCompat
// (mcp/pkg/client/compat.go) unconditionally rejects Transport.Kind() ==
// "sse" unless Compat.Permits(TolerateLegacySSE) -- and a zero Compat
// normalizes to ProfileDefault, which deliberately excludes that tolerance
// ("Legacy SSE is not in [ProfileDefault]: a transport is a deliberate
// choice, and a binding should not acquire an older wire protocol by
// default", per that file's own doc comment). Left at zero, every "sse"
// spec would fail Binding.Validate every time, which would make Task 6/7's
// already-shipped "sse" server kind entirely non-constructible here.
//
// The deliberateness ProfileDefault is guarding against is already
// satisfied one layer up: normalizeMCPServer's own comment records that
// "sse" is never inferred and a caller wanting it "must say so explicitly"
// -- so an mcp.json entry with "type": "sse" already IS the explicit,
// on-purpose choice the mcp module's Compat gate exists to require. Every
// other tolerance ProfileLegacy carries is identical to ProfileDefault's;
// it adds exactly TolerateLegacySSE and nothing else. stdio and http keep
// the zero value (ProfileDefault), unchanged from the task's instruction.
func mcpCompatFor(kind string) mcpclient.Profile {
	if kind == "sse" {
		return mcpclient.ProfileLegacy
	}
	return mcpclient.Profile{}
}

// mcpTransportFor builds the transport factory for one spec, per its kind.
// spec.kind is already validated to be exactly one of "stdio", "http", or
// "sse" by normalizeMCPServer (Task 6); the default case is defense in
// depth, not a reachable path for a spec that actually came from
// loadMCPConfig.
func mcpTransportFor(spec mcpServerSpec) (mcpclient.TransportFactory, error) {
	switch spec.kind {
	case "stdio":
		return stdio.New(stdio.Config{
			Command: spec.command,
			Args:    spec.args,
			Env: stdio.EnvAllowlist{
				PassThrough: mcpEnvPassThrough,
				Vars:        mcpEnvVarsFrom(spec.env),
			},
		})
	case "http":
		return streamablehttp.New(streamablehttp.Config{
			Endpoint: spec.url,
			Headers:  mcpHeadersFrom(spec.headers),
			// HTTPClient stays nil: the transport builds its own from
			// Timeouts. A shared session HTTP client would be refused here
			// anyway (New rejects a non-zero Timeout, which severs the
			// streams this transport is built on).
			// Timeouts stays zero: defaults.
		})
	case "sse":
		return sse.New(sse.Config{
			Endpoint: spec.url,
			Headers:  mcpHeadersFrom(spec.headers),
		})
	default:
		return nil, fmt.Errorf("unknown transport kind %q", spec.kind)
	}
}

// mcpEnvVarsFrom converts a spec's env map into the explicit []stdio.Var the
// transport's EnvAllowlist wants, sorted by name. The sort is not tidiness:
// spec.env is a Go map, whose iteration order is randomized per run, and an
// unsorted result would make binding construction -- and later, a
// fingerprint digest built over it -- nondeterministic across runs even
// though the map's content never changed. Empty input returns nil, matching
// copyMCPServerStringMap's convention elsewhere in this package.
func mcpEnvVarsFrom(env map[string]string) []stdio.Var {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	vars := make([]stdio.Var, 0, len(names))
	for _, name := range names {
		vars = append(vars, stdio.Var{Name: name, Value: env[name]})
	}
	return vars
}

// mcpHeadersFrom converts a spec's headers map into the []auth.Header the
// http/sse transports want, sorted by name for the same determinism reason
// mcpEnvVarsFrom is. auth.Header's fields are private, so NewHeader is the
// only way to construct one; New validates each header itself (Validate),
// so this function does not duplicate that check.
func mcpHeadersFrom(headers map[string]string) []mcpauth.Header {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]mcpauth.Header, 0, len(names))
	for _, name := range names {
		result = append(result, mcpauth.NewHeader(name, headers[name]))
	}
	return result
}

// mcpVisibilityRoles resolves a spec's roles to the set a Binding's
// Visibility is built from: empty/nil (normalizeMCPServerRoles's "not yet
// resolved" sentinel) means Generic, and a non-empty list -- already sorted
// and deduplicated by normalizeMCPServerRoles -- is used as given.
func mcpVisibilityRoles(specRoles []string) []string {
	if len(specRoles) == 0 {
		return mcpAllRoles
	}
	return specRoles
}

// mcpGateOpener routes MCP elicitation to the session's host-owned gate
// (session.GateHost, obtained by asserting the controller rig.NewSession
// returns). It is late-binding: the mcpharness.Manager this feeds is built
// before the session exists -- ConfigDigest must enter the rig fingerprint
// before rig.NewSession is even called (design
// docs/plans/2026-08-05-coderig-mcp-and-permission-review-design.md
// section 1.2.3) -- so there is necessarily a window in which an
// elicitation could arrive with nowhere to go. Bind installs the host once
// the session is live (Task 10's job, not this one's); before that, and
// permanently for a headless composition, which never calls Bind at all
// (matching the headless permission posture: MCP elicitation is exactly
// the kind of human-input request headless mode has nothing to answer
// with), every OpenGate call refuses with a typed error rather than
// blocking forever or silently succeeding.
type mcpGateOpener struct {
	mu   sync.Mutex
	host session.GateHost // nil until Bind
}

// Bind installs the live session's host-owned gate surface. It is
// mutex-guarded so a concurrent OpenGate can never observe a half-written
// host, even though Task 10's wiring calls it exactly once, right after
// rig.NewSession.
func (o *mcpGateOpener) Bind(h session.GateHost) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.host = h
}

// OpenGate maps one MCP elicitation onto session.GateHost.OpenHostGate and
// blocks until it is answered. This is the FAITHFUL mapping, not a
// refuse-with-reason placeholder: every gate.Gate field GateRequest does
// not supply directly is set from something read in harness's own
// published contract for this exact call, never guessed --
//
//   - Resolver is ALWAYS gate.ResolverSession. session.GateHost's doc
//     comment (harness/pkg/session/session.go) states the contract is
//     "host-owned gates ONLY (gate.KindForm and gate.KindOpenURL with
//     gate.ResolverSession)", and OpenHostGate refuses
//     (*session.GateError{Kind: GateKindMismatch}) anything else before it
//     is ever journaled -- there is no other legal value here.
//   - Blocks/Effect are gate.BlocksToolCall/gate.EffectResume. This
//     matches harness's own external-consumer proof for this exact call --
//     harness/pkg/rig/gate_host_test.go's formGate/openURLGate helpers,
//     exercised against the real rig.NewSession + OpenHostGate path -- not
//     an invented value: an MCP elicitation blocks the tool call that
//     raised it, and resolving it resumes that call, the same semantics
//     those helpers encode for every host-owned gate harness itself tests.
//   - Criticality and ResponsePolicy are left at their zero values. Every
//     gate constructor in production harness code today -- both loop-owned
//     (internal/loopruntime/gate.go's permissionGate/askUserGate) and the
//     host-owned pair above -- also leaves Criticality unset, so zero is
//     the established convention, not an omission. ResponsePolicy's zero
//     value resolves to gate.PolicyWait (pkg/gate/policy.go's
//     EffectiveAction), which sessionruntime's resolveGatePolicy accepts
//     unconditionally: an MCP elicitation carries no timeout policy of its
//     own through this call, so "leave the gate open until answered or
//     closed" is correct, not a default standing in for a real value.
//   - Subject is left zero. pkg/event/validate.go's gateIdentityProfile
//     documents that a ResolverSession gate's LoopID/TurnID/StepID are
//     OPTIONAL ("a loop-attributed elicitation carries a LoopID, startup
//     carries none") -- exactly GateRequest.LoopID's own doc comment -- and
//     GateRequest carries no TurnID/StepID at all, so there is nothing to
//     put there.
//   - Prompt and Restorable are forwarded from req unchanged. Prompt is
//     already a gate.Prompt (the identical type), and Restorable is the
//     caller's own already-enforced invariant
//     (mcpharness.GateRequest.Restorable's doc: "an open-url gate must
//     never be restorable"); forwarding it means that if it were ever
//     violated, gate.ValidateGate rejects the open on the host side too,
//     instead of this adapter silently dropping the flag.
//
// Kind and Payload are req.Kind/req.Payload unchanged: both are already
// the exact harness gate types (gate.Kind, gate.Payload), so there is no
// translation to get wrong.
//
// A failure awaiting the answer (including ctx cancellation -- OpenGate
// must honor ctx per mcpharness.GateOpener's doc) withdraws the gate
// (CloseGate) before returning, so a caller that gives up never leaves an
// orphaned open gate a human could still answer into the void. The close
// uses context.WithoutCancel: the gate is durable state the original,
// possibly-cancelled ctx has no further say over, the same idiom this
// module's own harness dependency uses for identical cleanup-after-
// cancellation calls (e.g. internal/sessionruntime/session.go's shutdown
// path, internal/sessionruntime/review_adapter.go's fault reporting).
func (o *mcpGateOpener) OpenGate(ctx context.Context, req mcpharness.GateRequest) (mcpharness.GateResponse, error) {
	o.mu.Lock()
	host := o.host
	o.mu.Unlock()
	if host == nil {
		return mcpharness.GateResponse{}, &mcpGateOpenerUnboundError{Binding: req.Binding}
	}

	g := gate.Gate{
		Kind:       req.Kind,
		Resolver:   gate.ResolverSession,
		Blocks:     gate.BlocksToolCall,
		Effect:     gate.EffectResume,
		Prompt:     req.Prompt,
		Restorable: req.Restorable,
	}

	id, err := host.OpenHostGate(ctx, req.LoopID, g, req.Payload)
	if err != nil {
		return mcpharness.GateResponse{}, fmt.Errorf("mcp gate opener: open host gate for binding %q: %w", req.Binding, err)
	}

	answer, err := host.AwaitGateAnswer(ctx, id)
	if err != nil {
		_ = host.CloseGate(context.WithoutCancel(ctx), id, gate.CloseAbandoned)
		return mcpharness.GateResponse{}, fmt.Errorf("mcp gate opener: await answer for binding %q: %w", req.Binding, err)
	}

	return mcpharness.GateResponse{Action: answer.Action, Values: answer.Values}, nil
}

var _ mcpharness.GateOpener = (*mcpGateOpener)(nil)

// mcpGateOpenerUnboundError reports an mcpGateOpener whose host has not
// been bound: either the session has not finished construction yet (a
// transient state Task 10's binding order should make unreachable in
// practice), or -- the permanent case -- the session is headless, whose
// composition never calls Bind at all (design section 1.2.3: "Headless
// sessions install an always-refusing opener, matching the headless
// permission posture"). Both refuse identically and by construction rather
// than through a headless-only special case, so this is also exactly what
// a genuine startup race would produce: there is no silent success to
// accidentally rely on either way.
type mcpGateOpenerUnboundError struct {
	// Binding names the MCP binding whose elicitation could not be routed.
	Binding string
}

func (e *mcpGateOpenerUnboundError) Error() string {
	return fmt.Sprintf("mcp gate opener: no session gate host bound (binding %q); MCP elicitation cannot be answered", e.Binding)
}

// mcpEventPublisher routes the Manager's integration events to the session's
// own event stream. Like mcpGateOpener, it is late-binding: the Manager this
// feeds is constructed before the session exists (its ConfigDigest must enter
// the rig fingerprint before rig.NewSession is called), so there is
// necessarily a window in which an event has nowhere to go. Unlike Deps.Gates,
// Deps.Events is REQUIRED at Manager construction (mcpharness/deps.go's
// Deps.validate), so this cannot be left nil the way a Reporter can -- an
// always-non-nil, always-safe placeholder is the only way to satisfy that
// requirement before a publishable session exists.
//
// Before Bind, PublishEvent drops the event and returns nil rather than an
// error. This is not a workaround: attach.go's own doc for BindSession
// documents exactly this window and why nothing durable is lost in it --
// event.IntegrationStatus is Ephemeral precisely because the latest status
// supersedes every earlier one, and BindSession republishes every binding's
// CURRENT status the moment there is somewhere to publish it. So the
// session's event stream opens knowing the truth about every server; what a
// pre-Bind drop loses is only an intermediate status of a connection still
// settling before the session it serves existed.
//
// Bind is called at most once, right after the session/controller exists,
// mirroring mcpGateOpener.Bind. Both interactive and headless sessions bind
// it: publishing an integration status is not a human-input capability the
// way opening a gate is, so there is no headless-specific refusal here.
type mcpEventPublisher struct {
	mu     sync.Mutex
	target mcpharness.EventPublisher // nil until Bind
}

// Bind installs the live session's publishing capability. It is
// mutex-guarded so a concurrent PublishEvent can never observe a
// half-written target.
func (p *mcpEventPublisher) Bind(target mcpharness.EventPublisher) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.target = target
}

// PublishEvent implements mcpharness.EventPublisher. Before Bind it drops ev
// and returns nil; see this type's doc for why that is safe rather than a
// silent failure.
func (p *mcpEventPublisher) PublishEvent(ctx context.Context, ev event.Event) error {
	p.mu.Lock()
	target := p.target
	p.mu.Unlock()
	if target == nil {
		return nil
	}
	return target.PublishEvent(ctx, ev)
}

var _ mcpharness.EventPublisher = (*mcpEventPublisher)(nil)

// maxMCPNoticeBacklog bounds mcpNoticeRecorder's retained history so a
// long-lived session cannot grow it without limit. Once full, Report drops
// the oldest retained notice and counts the drop instead of blocking or
// growing further.
const maxMCPNoticeBacklog = 256

// mcpNoticeRecorder is coderig's mcpharness.Reporter: a bounded, in-memory,
// always-usable sink for the adapter's own notices (tool-name collisions,
// adoption failures, and so on -- see mcpharness.NoticeKind).
//
// # Why this is NOT late-binding, unlike mcpGateOpener
//
// A late-binding shape earns its complexity only when something real
// exists to eventually bind to. mcpGateOpener binds to session.GateHost, a
// published capability session.SessionController genuinely exposes once
// rig.NewSession returns. No equivalent exists for a Reporter as of this
// task: session.Session's whole public surface is SubscribeEvents
// (read-only, and scoped to harness's own sealed event.Event set --
// mcpharness.Notice is explicitly NOT a member; see
// mcp/pkg/harness/deps.go's Notice doc for why it can't be) and
// RespondGate. There is no method anywhere an external package can call to
// inject an out-of-band notice onto a live session, at bind time or any
// other time. internal/app's one existing PublishEvent-shaped seam
// (foreign.EventPublisher, internal/app/acpchildren.go) is reached deep
// inside per-child-loop construction, well after a Loop already exists --
// the same "the Manager exists before the thing it would bind to" timing
// problem the GateOpener has, except here there is nothing waiting at the
// other end of a Bind either.
//
// So: a plain, always-live, bounded in-memory recorder, safe to construct
// standalone and safe to leave permanently disconnected. Task 10 decides
// whether to wire an *mcpNoticeRecorder in as Deps.Reporter or leave
// Deps.Reporter nil -- both are sanctioned outcomes for that task -- and
// either choice leaves this type unchanged.
type mcpNoticeRecorder struct {
	mu      sync.Mutex
	notices []mcpharness.Notice
	dropped uint64
}

// newMCPNoticeRecorder returns an empty recorder, ready to use.
func newMCPNoticeRecorder() *mcpNoticeRecorder {
	return &mcpNoticeRecorder{}
}

// Report implements mcpharness.Reporter. It never blocks and never panics:
// Manager.report calls it "on the goroutine that discovered the fact"
// (mcp/pkg/harness/deps.go's Reporter doc), so this only ever takes an
// uncontended mutex and appends. Once maxMCPNoticeBacklog notices are
// held, the oldest is dropped and Dropped's count increments -- a Reporter
// that grew without bound would be exactly the kind of resource a
// long-lived headless server exhausts first.
func (r *mcpNoticeRecorder) Report(n mcpharness.Notice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.notices) >= maxMCPNoticeBacklog {
		r.notices = r.notices[1:]
		r.dropped++
	}
	r.notices = append(r.notices, n)
}

// Notices returns a copy of the retained backlog, oldest first.
func (r *mcpNoticeRecorder) Notices() []mcpharness.Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]mcpharness.Notice, len(r.notices))
	copy(out, r.notices)
	return out
}

// Dropped reports how many notices were evicted for exceeding the backlog
// bound.
func (r *mcpNoticeRecorder) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

var _ mcpharness.Reporter = (*mcpNoticeRecorder)(nil)
