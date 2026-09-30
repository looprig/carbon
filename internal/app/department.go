package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
	"github.com/looprig/host/harnessruntime"
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
// declares WHAT it requires of a runtime in Host's own sessionwire identities, and
// Host's published github.com/looprig/host/harnessruntime makes a harness
// session.SessionController satisfy it; this file only supplies the Carbon launch
// behind that adapter. The two identity spaces are independent — harness
// identifies a session with a core/uuid.UUID and Host with an opaque sessionwire
// string — so nothing here casts one into the other, and RigSessionID is carried
// beside the Host identities rather than derived from them.

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
		// host v0.16.0 refuses, under its default durable profile, a target that
		// does not declare both. harnessruntime.Target forces them on and holds
		// them true at every bind (a session whose applier cannot close an
		// attempt, or whose fault channel is nil, is refused and released); they
		// are stated here so the capability set reads whole.
		Recovery: department.Recovery{AttemptCloser: true, PersistenceFaults: true},
	}
}

// LaunchScope is the context ONE Carbon launch is built under.
//
// # It is the SEAM for R1.2 step 3, not evidence that step 3 is met
//
// Step 3 requires every session-dependent binding to stay per-session in pooled mode.
// PooledLauncher constructs the access evaluator, gate, workspace, process
// supervisor, credential admission and MCP manager within each Launch. Tool-result
// objects are scoped without a prefix field: they land in the tenant's own journal
// backend, under the runtime session id (see the note on ObjectNamespace below).
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
	// Carbon now captures tool-result objects (toolresults.go), and still needs no
	// such field: each capture is written through the tenant's OWN harness journal
	// store -- a separate backend per tenant (PooledLauncher.tenantStores) -- under the
	// runtime session id harness supplies, so tenant and session scoping come from
	// the store and the session, not from a prefix a launcher could forget to apply.

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
	// The controller is returned unwrapped on purpose. harnessruntime discovers the
	// segregated capabilities by type assertion and harness's live registry evicts a dead
	// session by watching an optional Done() channel on the value it was handed; a
	// wrapper that forgot to forward either would silently opt the session out of a
	// capability or out of eviction, and both failures are invisible until the day
	// they matter.
	Launch(context.Context, LaunchScope) (session.SessionController, error)
}

// NewCarbonDepartment builds the immutable registry a Host serves: ONE agent, the
// Carbon product, launched by launcher through Host's published harness adapter.
//
// # The adapter is Host's, not Carbon's
//
// Everything between a launched harness session and Host's department seam —
// the six required capabilities, the recovery pair (AttemptCloser and
// PersistenceFaults, required at bind and declared on the target), the live
// subscription with text, reasoning and tool-step previews and its drop count
// (EphemeralDrops), the per-kind body decode with create re-presentation, the
// gate_response decode, the principal and metadata copy, the refusal of an
// unresolved PayloadRef, and the launch under the binding's RigSessionID — is
// github.com/looprig/host/harnessruntime. Carbon hand-wrote that adapter until
// host v0.16.0 published it; a hand copy re-derives every optional capability
// unaided, and forgetting one compiles and fails only on recovery.
//
// What stays Carbon's is the per-launch assembly behind SessionLauncher: the
// session-scoped rig, access policy, gate, MCP, credentials, workspace root, the
// tenant's own harness journal store and its tool-result capture. harnessruntime
// reaches it through RigsFunc, one Launcher per launch (launchScope).
//
// The compatibility identity is the caller's because it is derived from the resolved
// session Config — the access revision, the MCP revision and the model catalogue
// revision are all known only after the composition root has loaded them, and a
// Department built before they are resolved would advertise a runtime identity no
// session it launches actually has.
func NewCarbonDepartment(launcher SessionLauncher, compatibility department.CompatibilityID) (*department.Department, error) {
	target, err := newCarbonTarget(launcher, compatibility)
	if err != nil {
		return nil, err
	}
	// A SLICE of one, which is what department.New takes and what it must take: a
	// map-shaped constructor could never see a duplicate registration, because
	// duplicate computed keys silently last-wins. One registration is Carbon's
	// whole Department by design.
	return department.New([]department.Registration{harnessruntime.Registration(CarbonAgentID, target)})
}

// newCarbonTarget is the one Carbon launch target: harnessruntime.Target over
// launcher, with Carbon's capabilities. Target forces Recovery on and holds it
// true at every bind, so host.Compose's durable profile admits it.
func newCarbonTarget(launcher SessionLauncher, compatibility department.CompatibilityID) (department.LaunchTarget, error) {
	if launcher == nil {
		return nil, errors.New("carbon: a Carbon launch target needs a session launcher")
	}
	capabilities := carbonCapabilities()
	if capable, ok := launcher.(interface{ SupportsPooled() bool }); ok {
		capabilities.SupportsPooled = capable.SupportsPooled()
	}
	return harnessruntime.Target(carbonRigs(launcher), compatibility, capabilities)
}

// carbonRigs resolves every launch, create and restore alike, to a launchScope
// over launcher: the per-launch assembly (workspace, tenant journal store, MCP,
// credentials) happens inside the launcher, which is why this is RigsFunc and
// not SharedRig or RigPerTenant.
func carbonRigs(launcher SessionLauncher) harnessruntime.Rigs {
	return harnessruntime.RigsFunc(
		func(_ context.Context, req department.RigCreateRequest) (harnessruntime.Launcher, error) {
			return launchScope{launcher: launcher, scope: LaunchScope{
				TenantID:      req.TenantID,
				SessionID:     req.SessionID,
				AgentID:       req.AgentID,
				Placement:     req.Placement,
				WorkspaceRoot: req.WorkspaceRoot,
				RigSessionID:  req.RigSessionID,
			}}, nil
		},
		// Host supplies harness's identity separately from the request because the
		// two identity spaces are independent and a restore that does not carry it
		// has nothing to restore FROM.
		func(_ context.Context, id uuid.UUID, req department.RigRestoreRequest) (harnessruntime.Launcher, error) {
			return launchScope{launcher: launcher, scope: LaunchScope{
				TenantID:      req.TenantID,
				SessionID:     req.SessionID,
				AgentID:       req.AgentID,
				Placement:     req.Placement,
				WorkspaceRoot: req.WorkspaceRoot,
				RigSessionID:  id,
				Restore:       true,
			}}, nil
		},
	)
}

// launchScope is the harnessruntime.Launcher for ONE launch: it runs the
// product's SessionLauncher under the scope Host's request named.
//
// The launcher assembles and launches in one step, so the rig options
// harnessruntime hands NewSession are not applied here; they are derived from
// the same RigSessionID the scope carries, by the same rule
// carbonRigSessionOptions applies inside the launcher (rig.WithSessionID for a
// non-zero id, nothing for zero). The count is checked so the two derivations
// cannot silently disagree if harnessruntime ever passes another option.
type launchScope struct {
	launcher SessionLauncher
	scope    LaunchScope
}

var _ harnessruntime.Launcher = launchScope{}

// LaunchOptionsMismatchError refuses a launch whose rig options, or restore
// identity, differ from what the launch's scope would apply.
type LaunchOptionsMismatchError struct {
	Reason string
}

func (e *LaunchOptionsMismatchError) Error() string {
	return "carbon: the launch request disagrees with its scope: " + e.Reason
}

func (l launchScope) NewSession(ctx context.Context, options ...rig.SessionOption) (session.SessionController, error) {
	if l.scope.Restore {
		return nil, &LaunchOptionsMismatchError{Reason: "a restore scope was asked for a new session"}
	}
	if want := len(carbonRigSessionOptions(l.scope)); len(options) != want {
		return nil, &LaunchOptionsMismatchError{Reason: fmt.Sprintf("%d rig session options, the scope applies %d", len(options), want)}
	}
	return l.launcher.Launch(ctx, l.scope)
}

func (l launchScope) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	if !l.scope.Restore {
		return nil, &LaunchOptionsMismatchError{Reason: "a create scope was asked to restore"}
	}
	if id != l.scope.RigSessionID {
		return nil, &LaunchOptionsMismatchError{Reason: fmt.Sprintf("restore of %s, the scope names %s", id, l.scope.RigSessionID)}
	}
	return l.launcher.Launch(ctx, l.scope)
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
