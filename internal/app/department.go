package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
)

// department.go adapts the ONE Carbon product identity into a Host launch target.
//
// It is the piece runbook 08 R1.2 asks for and nothing more. Carbon is the
// reference COMPOSITION: it does not reimplement Factory, Host, SessionStore or
// Department, and this file registers exactly one agent. Do not grow it into a
// product agent registry, and do not turn Carbon's internal subagent or ACP
// catalogue into Host launch targets — those are delegation targets inside one
// Carbon session, not separately placeable agents.
//
// The seam is deliberately narrow in one specific way. Host's department package
// declares WHAT it requires of a runtime in Host's own sessionwire identities; this
// file is where a harness session.SessionController is made to satisfy that. The
// two identity spaces are independent — harness identifies a session with a
// core/uuid.UUID and Host with an opaque sessionwire string — so nothing here casts
// one into the other, and RigSessionID is carried beside the Host identities rather
// than derived from them.

// CarbonAgentID is THE Carbon product identity on the wire, and it is a constant
// rather than configuration.
//
// A Department maps an agent identity to the target that launches it, fixed at
// construction. If this were configurable, two deployments of the same Carbon build
// could register different identities and a session's durable binding — which names
// the agent — would resolve on one and not the other. The identity is part of what
// a session is, so it belongs in the build.
const CarbonAgentID = sessionwire.AgentID("carbon")

// carbonCompatibilityPrefix labels the compatibility identity so an operator
// reading a capacity report can tell whose runtime build it is without a lookup.
// It is NOT parsed by anything: Core treats the field as opaque and so does Host.
const carbonCompatibilityPrefix = "carbon-"

// CarbonCompatibilityID is the runtime-build identity a restore is refused against,
// derived from EXACTLY the configuration harness already treats as restore-critical.
//
// It is the same input set as agentFingerprintFields — the agent kind, the access
// policy revision, the MCP capability revision, the model catalogue revision and
// Carbon's own access app-fields. That is not a convenience: Host refuses a restore
// onto a target whose CompatibilityID differs from the one the durable state was
// written by, and harness independently refuses a restore whose config fingerprint
// drifted. Deriving this from a DIFFERENT set would produce a Host that happily
// admits a restore harness then rejects, which is the worst of the two orders — the
// session is placed, the workspace lease is taken, and the refusal arrives from
// inside the rig.
//
// IT CONTAINS NO SECRET AND CANNOT. Every input is already a digest or an opaque
// label: AccessConfigRev, MCPConfigRev and ModelConfigRev are content digests whose
// producers are contractually secret-free, and the app-fields are the access profile
// name. models.json's inline API keys never reach any of them. The value is written
// to a HostLink capacity report, which is the least private place in this system, so
// a leak here would be a leak to every Factory in the pool.
func CarbonCompatibilityID(cfg Config) department.CompatibilityID {
	fields := agentFingerprintFields(cfg)

	// A canonical, self-delimiting encoding rather than fmt.Sprintf("%v"). A
	// struct-printing verb reorders nothing today but is not a contract, and two
	// adjacent fields whose values could run together would let a change in one be
	// cancelled by a change in the other. Every line is "key=value" with the value
	// length-free but the key unique, and the map is sorted.
	var canonical strings.Builder
	write := func(key, value string) {
		canonical.WriteString(key)
		canonical.WriteByte('=')
		canonical.WriteString(strconv.Quote(value))
		canonical.WriteByte('\n')
	}
	write("agent_kind", fields.AgentKind)
	write("runtime_skills", strconv.FormatBool(fields.RuntimeSkills))
	write("native_permission_policy_rev", fields.NativePermissionPolicyRev)
	write("external_capability_rev", fields.ExternalCapabilityRev)
	write("runtime_catalog_rev", fields.RuntimeCatalogRev)
	appKeys := make([]string, 0, len(fields.AppFields))
	for key := range fields.AppFields {
		appKeys = append(appKeys, key)
	}
	sort.Strings(appKeys)
	for _, key := range appKeys {
		write("app."+key, fields.AppFields[key])
	}

	sum := sha256.Sum256([]byte(canonical.String()))
	return department.CompatibilityID(carbonCompatibilityPrefix + hex.EncodeToString(sum[:]))
}

// carbonCapabilities is the conservative capability set for a launcher that
// makes no pooling claim. NewCarbonDepartment derives the pooled bit from the
// supplied launcher, so a launcher that makes no pooling claim cannot advertise
// pooled seats and a PooledLauncher can.
//
// # SupportsPooled is FALSE here
//
// A launcher that places every session over one root using
// rig.WithExclusiveWorkspace takes a root lease that conflicts even within one
// process. Advertising a pooled seat for it would leave Factory selecting a Host
// that cannot launch the session. PooledLauncher instead creates one durable workspace
// root and rig per session, with a separate journal backend per tenant.
// NewCarbonDepartment reads that launcher's capability and
// advertises pooling only for it.
//
// # The rest
//
// CaptureSafety is BOUNDED_MATERIALIZED and not streaming. Carbon's highest-output
// tools (Bash and ReadFile) return a materialized result: the bytes are resident
// before the loop can bound them, and the bound is harness's declared ceiling
// (loop.DefaultMaterializedToolResultBytes) rather than a stream. It is declared
// honestly because it decides whether a PooledLauncher may be advertised.
//
// RequiresWorkspace is true because every Carbon launch materializes a workspace and
// takes an exclusive lease on its root. RequiresCheckpoint is false: Carbon restores a
// conversation from the session journal, and a workspace checkpoint is an optional
// convenience rather than a precondition — a target that required one would refuse
// every restore of a session that never made one.
func carbonCapabilities() department.Capabilities {
	return department.Capabilities{
		SupportsPooled:     false,
		SupportsDedicated:  true,
		RequiresWorkspace:  true,
		RequiresCheckpoint: false,
		AdmissionWeight:    1,
		CaptureSafety:      department.CaptureSafetyBoundedMaterialized,
	}
}

// LaunchScope is the context ONE Carbon launch is built under.
//
// # It is the SEAM for R1.2 step 3, not evidence that step 3 is met
//
// Step 3 requires every session-dependent binding to stay per-session in pooled mode.
// PooledLauncher constructs the access evaluator, gate, workspace, process
// supervisor, credential admission and MCP manager within each Launch. Carbon
// does not yet capture session objects, so an object prefix will be added only
// together with its writer and reader.
//
// So this type exists to make the per-session context EXPRESSIBLE. A launcher that
// ignored it and reused one process-wide binding would compile perfectly and would
// cross-wire two tenants' sessions; the scope is a value, and the obligation is stated
// on it, precisely because nothing in the type system will catch that.
type LaunchScope struct {
	TenantID  sessionwire.TenantID
	SessionID sessionwire.SessionID
	AgentID   sessionwire.AgentID
	Placement sessionwire.HostPlacement

	// WorkspaceRoot is the materialized workspace this launch runs against.
	WorkspaceRoot string

	// THERE IS DELIBERATELY NO ObjectNamespace FIELD, and its absence is the honest
	// state rather than an oversight.
	//
	// Step 3 names an "object prefix", and Host does supply one in
	// StorageContext.Namespace. An earlier revision of this type carried it — and
	// nothing read it. A field that is populated on every launch and consulted by
	// nothing is worse than a missing one: a later reader assumes it is honoured, and
	// the day Carbon gains durable object capture a pooled Carbon would write every
	// tenant's objects under one un-namespaced prefix with this struct apparently
	// saying otherwise.
	//
	// Carbon captures no objects at all today (it wires no rig.WithToolResultCapture),
	// so there is nothing to scope. The field belongs with the code that scopes
	// something, which is R1.3's per-session-root launcher, and it should be added
	// there together with its reader.

	// RigSessionID is HARNESS'S identity to launch under: the runtime session id the
	// session's immutable durable binding names, derived by Factory at create time
	// and NOT the sessionwire SessionID.
	//
	// A launcher MUST honour it. department refuses a launch whose session ID() is
	// not the one it asked for and releases the mis-launched session again, because
	// a session launched under a minted id writes its conversation to a journal the
	// binding does not name: nothing can find it again and the next placement starts
	// the conversation over, silently.
	//
	// ZERO IS LEGITIMATE AND MEANS "mint one". It arises only for a create with no
	// durable catalog record; department checks nothing in that case.
	RigSessionID uuid.UUID

	// Restore reports which half of the launch this is. It is a field rather than
	// two methods on the launcher because everything else about the two is
	// identical, and a launcher that must branch can, while one that need not is
	// spared a duplicated signature.
	Restore bool
}

// SessionLauncher builds ONE session-scoped Carbon runtime for a launch.
//
// It is an interface at the CONSUMER, which is this file, and its single production
// implementation is the process composition root's. That inversion is the whole
// reason this package can be tested at all: building a real Carbon session needs a
// model catalogue, a credential admission, a sandbox executor set and an MCP
// composition, none of which a unit test should have to stand up in order to prove
// that a create is refused when the rig launches under the wrong identity.
type SessionLauncher interface {
	// Launch builds and starts one session under scope, returning harness's own
	// controller UNWRAPPED.
	//
	// The controller is returned unwrapped on purpose. Host discovers six segregated
	// capabilities by type assertion and harness's live registry evicts a dead
	// session by watching an optional Done() channel on the value it was handed; a
	// wrapper that forgot to forward either would silently opt the session out of a
	// capability or out of eviction, and both failures are invisible until the day
	// they matter.
	Launch(context.Context, LaunchScope) (session.SessionController, error)
}

// The refusals this adapter makes on its own behalf. Each one names a capability
// the launched controller did not offer, rather than a generic failure, because the
// diagnosis for each is different: a missing Applier is a composition that wired no
// persistence, a missing lease is a session whose grant was lost or released, and a
// missing closer is a runtime that cannot free a stranded predecessor.
var (
	ErrCarbonRuntimeCannotApply = errors.New("carbon: the launched session cannot apply an admitted runtime command")
	ErrCarbonRuntimeNoLease     = errors.New("carbon: the launched session reports no held journal lease")
	// ErrCarbonRuntimeCannotClose WRAPS department.ErrNoAttemptCloser, and the wrap
	// is the whole value of the error.
	//
	// host's disposition applier branches on that sentinel and has an arm written for
	// exactly this composition shape — a composed runtime reaches it through a wrapper
	// that declares the method for every runtime, so "no closer" arrives as a REFUSAL
	// rather than as a failed type assertion. Without the wrap this lands in the
	// default arm and surfaces as RefusalStore, "ambiguous durable-store failure",
	// which sends an operator to look at the store for a composition problem. Both
	// outcomes block the command, so this is a diagnosis fix and not a correctness one
	// — but the whole reason a composer reads that refusal is to find out what to fix.
	ErrCarbonRuntimeCannotClose = fmt.Errorf("%w: carbon: the launched session offers no recovery closure",
		department.ErrNoAttemptCloser)
	// ErrCarbonRuntimeCannotAbandon WRAPS department.ErrNoPersistenceFaults, for
	// ErrCarbonRuntimeCannotClose's reason: Host reaches this runtime through a
	// wrapper that declares the method for every runtime, so "cannot abandon"
	// arrives as a refusal and the sentinel is what names the missing capability.
	ErrCarbonRuntimeCannotAbandon = fmt.Errorf("%w: carbon: the launched session offers no crash-equivalent release",
		department.ErrNoPersistenceFaults)
	ErrCarbonUnknownCommandKind = errors.New("carbon: this product runtime does not apply this command kind")
	ErrCarbonNoPublications     = errors.New("carbon: the launched session cannot report committed public events")
)

// The five admitted command kinds, restated because neither Factory nor Host
// exports them.
//
// THEY ARE FIVE, NOT THREE, as of harness v0.36.0. Nothing in this repository may
// use "restore" or "gate_response" as an example of an unknown kind: both were
// unknown once and both are now named, and a fixture built on either went green the
// day the vocabulary widened while whatever it guarded was live.
const (
	carbonKindCreate       = "create"
	carbonKindRestore      = "restore"
	carbonKindInput        = "input"
	carbonKindInterrupt    = "interrupt"
	carbonKindGateResponse = "gate_response"
)

// carbonCommandKinds is the vocabulary as one value so a test can iterate it
// rather than restate it.
func carbonCommandKinds() []string {
	return []string{carbonKindCreate, carbonKindRestore, carbonKindInput, carbonKindInterrupt, carbonKindGateResponse}
}

// carbonRig is the product's department.Rig: it turns Host's two launch requests
// into one session-scoped Carbon runtime each.
type carbonRig struct {
	launcher SessionLauncher
}

var _ department.Rig = (*carbonRig)(nil)

// NewSession satisfies department.Rig for a create, LAUNCHING UNDER THE REQUESTED
// IDENTITY. See LaunchScope.RigSessionID for why that is not cosmetic.
func (r *carbonRig) NewSession(ctx context.Context, req department.RigCreateRequest) (department.RigSession, error) {
	return r.launch(ctx, LaunchScope{
		TenantID:      req.TenantID,
		SessionID:     req.SessionID,
		AgentID:       req.AgentID,
		Placement:     req.Placement,
		WorkspaceRoot: req.WorkspaceRoot,
		RigSessionID:  req.RigSessionID,
	})
}

// RestoreSession satisfies department.Rig for a relaunch over durable state. Host
// supplies harness's identity separately from the request because the two identity
// spaces are independent and a restore that does not carry it has nothing to
// restore FROM.
func (r *carbonRig) RestoreSession(ctx context.Context, id uuid.UUID, req department.RigRestoreRequest) (department.RigSession, error) {
	return r.launch(ctx, LaunchScope{
		TenantID:      req.TenantID,
		SessionID:     req.SessionID,
		AgentID:       req.AgentID,
		Placement:     req.Placement,
		WorkspaceRoot: req.WorkspaceRoot,
		RigSessionID:  id,
		Restore:       true,
	})
}

func (r *carbonRig) launch(ctx context.Context, scope LaunchScope) (department.RigSession, error) {
	if r.launcher == nil {
		return nil, errors.New("carbon: no session launcher is configured for the Carbon launch target")
	}
	controller, err := r.launcher.Launch(ctx, scope)
	if err != nil {
		return nil, err
	}
	if controller == nil {
		// department reports a nil session as ErrNoRigSession, but only when the
		// interface value itself is nil; a typed nil controller would pass its
		// check and panic at the first capability call. Refusing here keeps the
		// failure at the launcher that produced it.
		return nil, department.ErrNoRigSession
	}
	return &carbonRuntime{controller: controller, scope: scope}, nil
}

// carbonRuntime adapts one launched harness session to Host's department.RigSession
// and the segregated capabilities Host discovers on it.
//
// EVERY CAPABILITY IS A METHOD ON THIS TYPE, which means department's own discovery
// always succeeds and the refusal moves from the assertion into the method. That is
// the right shape for a PRODUCT adapter and the wrong one for a test double: a
// product knows what it composed, and a refusal that names the missing capability
// diagnoses better than an *IncapableRuntimeError listing six.
type carbonRuntime struct {
	controller session.SessionController
	scope      LaunchScope
}

var _ department.RigSession = (*carbonRuntime)(nil)

// The two OPTIONAL capabilities Host discovers by type assertion. Each compiles
// and runs without being forwarded, and each wedges a session when it is not:
// without AttemptCloser a successor cannot free a stranded attempt (host v0.5.0),
// and without PersistenceFaults a storage outage leaves a faulted runtime
// resident and every command behind it pending (host v0.8.0/v0.8.1). These
// assertions make dropping either a build failure rather than an outage.
var (
	_ department.AttemptCloser     = (*carbonRuntime)(nil)
	_ department.PersistenceFaults = (*carbonRuntime)(nil)
)

// ID is harness's identity for the launched session.
func (s *carbonRuntime) ID() uuid.UUID { return s.controller.SessionID() }

// WaitIdle satisfies department.IdleWaiter.
func (s *carbonRuntime) WaitIdle(ctx context.Context) error {
	waiter, ok := s.controller.(session.IdleWaiter)
	if !ok {
		return fmt.Errorf("carbon: the launched session cannot report idleness")
	}
	return waiter.WaitIdle(ctx)
}

// Done satisfies department.Liveness. A session that reports no liveness channel is
// given a CLOSED one rather than nil: a nil channel blocks forever, so every drain
// supervisor selecting on it would wait for the life of the process, whereas a
// closed channel reports "this runtime has stopped answering", which is the honest
// answer about a session that cannot say.
func (s *carbonRuntime) Done() <-chan struct{} {
	live, ok := s.controller.(session.Liveness)
	if !ok {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return live.Done()
}

// ReleaseResidency satisfies department.Releaser. It is NONTERMINAL: the session
// remains resumable and no SessionStopped is appended.
func (s *carbonRuntime) ReleaseResidency(ctx context.Context) error {
	releaser, ok := s.controller.(session.Releaser)
	if !ok {
		return fmt.Errorf("carbon: the launched session cannot release residency without ending the conversation")
	}
	return releaser.ReleaseResidency(ctx)
}

// LeaseEpoch satisfies department.LeaseEpochReporter.
//
// THE TWO RESULTS ARE THE CONTRACT and this forwards both. A released or lost lease
// reports (0,false), and a caller must branch on held: no pinned provider zeroes a
// released lease's epoch, so reading the number alone hands back a live-looking dead
// value. This is the RUNTIME's journal grant and must never be answered from Host's
// residency grant — they are different counters over different namespaces that
// merely both start at 1.
func (s *carbonRuntime) LeaseEpoch() (uint64, bool) {
	reporter, ok := s.controller.(session.LeaseEpochReporter)
	if !ok {
		return 0, false
	}
	return reporter.LeaseEpoch()
}

// persistenceFaults answers the launched session's durable-health capability as
// ONE unit: harness v0.38.0's fault reporter and its crash-equivalent release.
// Half of it is refused as none of it, because a fault Host can observe but not
// abandon on would only move the wedge, and an abandon with no fault signal is
// never reached by the supervisor.
func (s *carbonRuntime) persistenceFaults() (session.PersistenceFaultReporter, session.ResidencyAbandoner, bool) {
	reporter, reports := s.controller.(session.PersistenceFaultReporter)
	abandoner, abandons := s.controller.(session.ResidencyAbandoner)
	if !reports || !abandons {
		return nil, nil, false
	}
	return reporter, abandoner, true
}

// PersistenceFaulted satisfies department.PersistenceFaults: the channel that
// closes when harness latches a persistence fault (one failed journal append,
// e.g. a storage outage). Host treats it closing as lost residency: it halts the
// command consumer, calls AbandonResidency and releases the session so a
// successor restores it from the journal.
//
// FORWARDED, NOT OPTIONAL FOR A PRODUCT (host v0.8.1). department discovers the
// capability by assertion, so a wrapper that omitted this method would compile
// and run, and a storage outage would then leave the session resident with every
// command behind it pending until its apply deadline. A session that offers no
// fault signal answers nil, which never fires: it is simply not supervised.
func (s *carbonRuntime) PersistenceFaulted() <-chan struct{} {
	reporter, _, ok := s.persistenceFaults()
	if !ok {
		return nil
	}
	return reporter.PersistenceFaulted()
}

// PersistenceFault satisfies department.PersistenceFaults: the latched fault,
// or nil.
func (s *carbonRuntime) PersistenceFault() error {
	reporter, _, ok := s.persistenceFaults()
	if !ok {
		return nil
	}
	return reporter.PersistenceFault()
}

// AbandonResidency satisfies department.PersistenceFaults: harness's
// crash-equivalent release. It seals the session's logs, writes nothing (no
// SessionStopped, so the session stays restorable) and hands the journal lease
// back. Host calls it for a faulted runtime, a stranded attempt, and (host
// v0.8.2) a residency grant it has lost.
func (s *carbonRuntime) AbandonResidency(ctx context.Context) error {
	_, abandoner, ok := s.persistenceFaults()
	if !ok {
		return ErrCarbonRuntimeCannotAbandon
	}
	return abandoner.AbandonResidency(ctx)
}

// SubscribeCommitted satisfies department.PublicationSubscriber: it projects the
// session's own committed public events onto the wire record Host relays.
//
// THE PROJECTION IS A CARRY, NOT A RE-RENDER. Every delivery on harness's committed
// stream carries the exact canonical public body the durable append stored, the
// public EventID it committed under, and a CoveredThrough watermark equal to that
// append's sequence. Re-projecting the runtime event here would let a consumer
// joining a durable tail to this live stream render two different bodies for one
// event, which is the failure the committed stream exists to prevent.
//
// # What `after` does and does not do
//
// A live subscription begins at the moment it is made, so every publication it
// yields is necessarily at or after any event the caller has already seen. It does
// NOT replay the gap between `after` and the subscription: that gap is filled by
// the consumer's own bounded journal read, which is exactly the join
// sessionwire.EnduringPublication documents ("the durable identity and sequence let
// a consumer join this fast path with a bounded journal read after reconnect or
// overflow"). Positioning a LIVE stream on an opaque historical EventID would need
// an ordering the id does not carry; inventing one here would be a guess dressed as
// a resume.
//
// A session whose persistence cannot report committed bytes is REFUSED rather than
// served a re-projected approximation. That is the two-result capability's whole
// point: a consumer must learn it is not one of those sessions before it starts
// persisting cursors, not after.
func (s *carbonRuntime) SubscribeCommitted(ctx context.Context, _ sessionwire.EventID) (<-chan sessionwire.EnduringPublication, error) {
	provider, ok := s.controller.(session.CommittedPublicEventProvider)
	if !ok {
		return nil, ErrCarbonNoPublications
	}
	source, ok := provider.CommittedPublicEvents()
	if !ok {
		return nil, ErrCarbonNoPublications
	}
	// Enduring events from EVERY loop. A ZERO filter selects no loop at all: an
	// EventFilter is DECLARED INTEREST evaluated before the send, so an empty one is
	// not "everything" — it is "nothing", and a gate opened inside a delegate would
	// never be published.
	subscription, err := source.SubscribeCommittedPublicEvents(event.EventFilter{
		Enduring: event.LoopScope{All: true},
	})
	if err != nil {
		return nil, err
	}

	out := make(chan sessionwire.EnduringPublication)
	go func() {
		defer close(out)
		defer func() { _ = subscription.Close() }()
		for delivery := range subscription.Events() {
			// A delivery with no committed public body is not a publication. It is
			// dropped rather than forwarded with an empty Body, because an empty
			// body is a record a consumer would render as a real event.
			if delivery.EventID == "" || len(delivery.PublicBody) == 0 {
				continue
			}
			publication := sessionwire.EnduringPublication{
				TenantID:       s.scope.TenantID,
				SessionID:      s.scope.SessionID,
				EventID:        sessionwire.EventID(delivery.EventID),
				JournalSeq:     delivery.JournalSeq,
				CoveredThrough: delivery.CoveredThrough,
				Body:           json.RawMessage(delivery.PublicBody),
			}
			select {
			case out <- publication:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// ApplyCommand satisfies department.CommandApplier for ALL FIVE admitted kinds,
// through harness's own runtime-command seam.
//
// # Why every kind goes through the applier and none is vouched for
//
// runtimecommand.Applier writes the durable application prefix and the kind's
// disposition frame, and that frame is the ONLY evidence the store settles from. A
// product that short-circuited any kind — applied it some other way and let
// something else report success — would settle a command the agent never received.
// That has happened twice in this system's history: once when every kind was vouched
// for and a gate response settled `applied` while the gate stayed open, and once
// when only create and restore were, and a user's first message dropped in silence
// with the record looking perfectly settled.
//
// # The decode is BY KIND, with Core's own records
//
// A create's stored body is a Core CreateRequest and an input's is an InputRequest:
// Factory canonically encodes the whole request, so the two are different shapes
// carrying the same blocks. Decoding with Core's decoder and handing the blocks to
// content.UnmarshalBlocks is what makes a MULTI-BLOCK and a NON-TEXT first message
// cross faithfully. A decoder that scraped text members out of arbitrary JSON would
// accept a body of the wrong kind without noticing AND drop an image, and neither
// loss is visible at the store.
//
// # A bare create is not an error
//
// harness makes Blocks OPTIONAL for a create precisely so a session created with no
// opening message can still settle. Refusing one here would wedge every such
// session; carrying nothing for a create that HAS a first message is the defect this
// whole path exists to prevent, which is why an undecodable body is an error rather
// than an empty turn.
func (s *carbonRuntime) ApplyCommand(ctx context.Context, cmd department.RuntimeCommand) error {
	applier, ok := s.controller.(runtimecommand.Applier)
	if !ok {
		return ErrCarbonRuntimeCannotApply
	}
	epoch, held := s.LeaseEpoch()
	if !held {
		// REFUSED BEFORE THE APPLY, not applied under a guessed epoch. harness
		// checks the admitted record's LeaseEpoch for EQUALITY against the lease
		// the runtime holds, so a zero would be refused there anyway — but it would
		// be refused as a malformed record rather than as a lost lease, which sends
		// an operator looking for the wrong failure.
		return ErrCarbonRuntimeNoLease
	}

	admitted := runtimecommand.Admitted{
		CommandID:        runtimecommand.CommandID(cmd.CommandID),
		RuntimeCommandID: cmd.RuntimeCommandID,
		LeaseEpoch:       epoch,
		AttemptID:        runtimecommand.AttemptID(cmd.AttemptID),
	}
	switch cmd.Kind {
	case carbonKindCreate:
		blocks, err := carbonCreateBlocks(cmd.Payload)
		if err != nil {
			return err
		}
		admitted.Kind, admitted.Blocks = runtimecommand.KindCreate, blocks
	case carbonKindRestore:
		// A restore carries NOTHING. Core's RestoreRequest has no blocks member and
		// Admitted.Validate refuses a restore carrying any, so forwarding a stray
		// payload would be refused after the attempt is already durable.
		admitted.Kind = runtimecommand.KindRestore
	case carbonKindInput:
		blocks, err := carbonInputBlocks(cmd.Payload)
		if err != nil {
			return err
		}
		admitted.Kind, admitted.Blocks = runtimecommand.KindInput, blocks
	case carbonKindInterrupt:
		admitted.Kind = runtimecommand.KindInterrupt
	case carbonKindGateResponse:
		answer, err := carbonGateAnswer(cmd)
		if err != nil {
			return err
		}
		admitted.Kind, admitted.GateResponse = runtimecommand.KindGateResponse, answer
	default:
		// A kind a newer Factory admits and this build does not know. It is REFUSED
		// rather than guessed at, and the refusal happens BEFORE any durable write,
		// so the record is left for a Host that understands it. Guessing is exactly
		// what dropped a create's first message for a whole release.
		return fmt.Errorf("%w: %q", ErrCarbonUnknownCommandKind, cmd.Kind)
	}

	_, err := applier.ApplyRuntimeCommand(ctx, admitted)
	return err
}

// CloseAttempt satisfies department.AttemptCloser: the capability a SUCCESSOR needs
// to free a session a predecessor stranded.
//
// # THIS IS NOT OPTIONAL FOR A PRODUCT, AND IT IS NOT A MIGRATION CAPABILITY
//
// Host asks the RUNTIME for the recovery closure, because only the runtime's journal
// can say whether a predecessor's attempt left an effect. A runtime that offers none
// is refused department.ErrNoAttemptCloser; the applier turns that into a blocked
// pass; the consumer BREAKS the pass and advances no cursor; the deadline sweep
// SKIPS an attempt-bearing record, so nothing expires it. The cost is not "liveness
// on one command" — it is that session's WHOLE COMMAND STREAM, permanently, until a
// composition that can close the attempt takes the session.
//
// The trigger is any failover with an attempt in flight: `close` fires whenever the
// record's attempt grant is older than the runtime's own. A pod eviction or a crash
// on an all-current fleet reaches it. Host's own reference implementation of this is
// in an INTERNAL package, so every external composer has to write it, and the
// discovery rate for a silently-optional capability is zero — which is why it is
// written out here at length rather than assumed.
//
// # Nothing is decided here
//
// The author grant is deliberately NOT a parameter: harness stamps it from the live
// lease this successor holds, because a caller-supplied author epoch would be a
// caller-authored proof and a tombstone is the one thing that must never be one.
// harness refuses the closure outright if its grant is not strictly later than the
// attempt's, or if the journal holds ANY enduring event caused by that runtime
// command — a tombstone over a committed effect is the one error this protocol
// cannot recover from.
func (s *carbonRuntime) CloseAttempt(
	ctx context.Context,
	command sessionwire.CommandID,
	runtimeCommand uuid.UUID,
	kind string,
	attempt string,
	attemptJournalEpoch uint64,
) error {
	closer, ok := s.controller.(runtimecommand.AttemptCloser)
	if !ok {
		return ErrCarbonRuntimeCannotClose
	}
	closure := runtimecommand.Closure{
		CommandID:           runtimecommand.CommandID(command),
		RuntimeCommandID:    runtimeCommand,
		Kind:                runtimecommand.Kind(kind),
		AttemptID:           runtimecommand.AttemptID(attempt),
		AttemptJournalEpoch: attemptJournalEpoch,
	}
	// VALIDATED ON THE RELEASED TYPE'S OWN RULE rather than a restatement of it. A
	// closure that cannot name an attempt is one that could tombstone the wrong
	// command, and Closure.Validate tracks Kind.Valid rather than holding a narrower
	// set — a kind a predecessor can apply but a closure refuses is a command no
	// successor can ever close.
	if err := closure.Validate(); err != nil {
		return err
	}
	_, err := closer.CloseAttempt(ctx, closure)
	return err
}

// carbonCreateBlocks reads a create's stored body — Core's CreateRequest — and
// returns its first message as content blocks.
//
// An EMPTY payload is a bare create: no blocks, no error. Anything else Core refuses
// is an error, because a body this product cannot read must not be silently applied
// as an empty turn — that is the exact shape of the defect host v0.5.0 exists to
// close, and it settles `applied` with the user's words gone.
func carbonCreateBlocks(payload []byte) ([]content.Block, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var request sessionwire.CreateRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("carbon: the create body is not a Core CreateRequest: %w", err)
	}
	return carbonBlocks(request.Blocks)
}

// carbonInputBlocks reads an input's stored body — Core's InputRequest.
//
// Unlike a create, harness REQUIRES blocks for an input and refuses one carrying
// none, so this arm cannot fail open the way the create arm structurally can.
func carbonInputBlocks(payload []byte) ([]content.Block, error) {
	if len(payload) == 0 {
		return nil, errors.New("carbon: the input body is empty")
	}
	var request sessionwire.InputRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("carbon: the input body is not a Core InputRequest: %w", err)
	}
	return carbonBlocks(request.Blocks)
}

// carbonBlocks decodes a Core request's blocks member with CORE'S OWN decoder, which
// is what makes a multi-block and a non-text message cross faithfully.
func carbonBlocks(raw json.RawMessage) ([]content.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	blocks, err := content.UnmarshalBlocks(raw)
	if err != nil {
		return nil, fmt.Errorf("carbon: the body's blocks are not Core content blocks: %w", err)
	}
	return blocks, nil
}

// carbonGateAnswer reads Core's gate-response record out of an admitted command's
// body and builds harness's answer from it.
//
// THE SOURCE IS ASSERTED HERE AND IS NOT TAKEN FROM THE BODY. Core's record carries
// no source, a Host sets it, and harness refuses a source a caller may not assert.
// Everything else about the answer — whether the gate is open, whether the action is
// one of its controls, whether the values satisfy its schema — is the session's to
// decide, and this pre-empts none of it.
func carbonGateAnswer(cmd department.RuntimeCommand) (*gate.GateResponse, error) {
	if len(cmd.Payload) == 0 {
		// A gate response whose body was offloaded to an object reference cannot be
		// answered here: Host does not dereference a PayloadRef and this adapter has
		// no object read of its own. It is refused rather than applied empty, which
		// would resolve the gate with no answer.
		return nil, fmt.Errorf("carbon: the gate response %s carries no inline body", cmd.CommandID)
	}
	var request sessionwire.GateResponseRequest
	if err := json.Unmarshal(cmd.Payload, &request); err != nil {
		return nil, fmt.Errorf("carbon: the gate response %s is not a Core record: %w", cmd.CommandID, err)
	}
	gateID, err := uuid.Parse(string(request.GateID))
	if err != nil {
		return nil, fmt.Errorf("carbon: the gate response %s names %q, which is not a gate identity: %w",
			cmd.CommandID, request.GateID, err)
	}
	return &gate.GateResponse{
		GateID: gate.ID(gateID),
		Action: request.Action,
		Values: request.Values,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}, nil
}

// NewCarbonDepartment builds the immutable registry a Host serves: ONE agent, the
// Carbon product, launched by launcher.
//
// The compatibility identity is the caller's because it is derived from the resolved
// session Config — the access revision, the MCP revision and the model catalogue
// revision are all known only after the composition root has loaded them, and a
// Department built before they are resolved would advertise a runtime identity no
// session it launches actually has.
func NewCarbonDepartment(launcher SessionLauncher, compatibility department.CompatibilityID) (*department.Department, error) {
	if launcher == nil {
		return nil, errors.New("carbon: a Carbon launch target needs a session launcher")
	}
	capabilities := carbonCapabilities()
	if capable, ok := launcher.(interface{ SupportsPooled() bool }); ok {
		capabilities.SupportsPooled = capable.SupportsPooled()
	}
	target, err := department.NewRigTarget(&carbonRig{launcher: launcher}, compatibility, capabilities)
	if err != nil {
		return nil, err
	}
	// A SLICE of one, which is what department.New takes and what it must take: a
	// map-shaped constructor could never see a duplicate registration, because
	// duplicate computed keys silently last-wins. One registration is Carbon's
	// whole Department by design.
	return department.New([]department.Registration{{AgentID: CarbonAgentID, Target: target}})
}

// carbonRigSessionOptions is the harness option list a launcher must apply for a
// scope, exported through this package so the production launcher and a test
// launcher cannot disagree about it.
//
// It exists for ONE rule that is easy to get wrong in each launcher separately: a
// non-zero RigSessionID becomes rig.WithSessionID, and a zero one becomes no option
// at all. rig.WithSessionID refuses a zero id (unlike the internal option, which
// ignores one), because silently substituting a minted id would put a name in a
// caller's immutable binding that resolves to nothing.
//
// Freshness of the id is NOT verified and the lease is not a substitute: reusing an
// ENDED session's id re-opens that stream under a fresh grant and appends a second
// SessionStarted, and only a LIVE holder is refused. The caller owns not reusing an
// id; here that caller is Factory, which derives it once at create.
func carbonRigSessionOptions(scope LaunchScope) []rig.SessionOption {
	if scope.RigSessionID.IsZero() {
		return nil
	}
	return []rig.SessionOption{rig.WithSessionID(scope.RigSessionID)}
}
