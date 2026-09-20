package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
)

// ---- fixtures --------------------------------------------------------------

// launcherFunc adapts a function to SessionLauncher.
type launcherFunc func(context.Context, LaunchScope) (session.SessionController, error)

func (f launcherFunc) Launch(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
	return f(ctx, scope)
}

// realRigFixture is a real Carbon rig over an in-memory backend: the production
// definition, the production access wiring, the production rig assembly. Only the
// inference client is a double.
//
// It is a REAL rig and not a stub on purpose. Every claim in this file is about
// something only a real harness session produces — a journal grant that moves when a
// successor takes over, a recovery closure harness will or will not accept, a
// disposition frame written before the effect. A stub controller would make each of
// those a statement about the stub.
type realRigFixture struct {
	rig    *rig.Rig
	stores *sessionStores
	llm    *fakeLLM
	cfg    Config
	root   string

	mu       sync.Mutex
	launched []session.SessionController
}

func newRealRigFixture(t *testing.T) *realRigFixture {
	t.Helper()
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores: %v", err)
	}
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	llm := &fakeLLM{}
	definition, err := carbonTestDefinition(llm, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition: %v", err)
	}
	assembled, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig: %v", err)
	}
	return &realRigFixture{rig: assembled, stores: stores, llm: llm, cfg: cfg, root: root}
}

// launcher returns a SessionLauncher over the fixture's rig that HONOURS
// LaunchScope.RigSessionID, which is what a product launcher must do.
//
// It RECORDS every controller it launches, and that is not bookkeeping. Host hands a
// test a department.Runtime, whose AttemptCloser returns only an error — so the
// ClosureResult harness actually produced, and with it the proof that a tombstone
// LANDED, is invisible from that side. Holding the controller is the only way to ask
// harness directly.
func (f *realRigFixture) launcher() SessionLauncher {
	return launcherFunc(func(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
		var (
			controller session.SessionController
			err        error
		)
		if scope.Restore {
			controller, err = f.rig.RestoreSession(ctx, scope.RigSessionID)
		} else {
			controller, err = f.rig.NewSession(ctx, carbonRigSessionOptions(scope)...)
		}
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.launched = append(f.launched, controller)
		f.mu.Unlock()
		return controller, nil
	})
}

// lastLaunched returns the most recently launched controller.
func (f *realRigFixture) lastLaunched(t *testing.T) session.SessionController {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.launched) == 0 {
		t.Fatal("no session has been launched")
	}
	return f.launched[len(f.launched)-1]
}

func mustUUIDForTest(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

// ---- the registry ----------------------------------------------------------

// TestCarbonDepartmentRegistersExactlyOneLaunchTarget holds runbook 08's scope rule
// at the registry: "Its Department contains the one Carbon product identity and its
// existing Rig launch target. Do not add an open-ended product agent registry."
//
// The negative half matters as much as the positive one. An unknown agent must be
// refused with runtime_unavailable and NOT with an invalid-request code: the request
// was well formed and this Host simply has no runtime for that agent, and telling a
// client its request was malformed sends it to fix something that is not broken.
func TestCarbonDepartmentRegistersExactlyOneLaunchTarget(t *testing.T) {
	t.Parallel()

	fixture := newRealRigFixture(t)
	dept, err := NewCarbonDepartment(fixture.launcher(), CarbonCompatibilityID(fixture.cfg))
	if err != nil {
		t.Fatalf("NewCarbonDepartment: %v", err)
	}

	if got := dept.Len(); got != 1 {
		t.Errorf("Department.Len() = %d, want exactly 1", got)
	}
	agents := dept.AgentIDs()
	if len(agents) != 1 || agents[0] != CarbonAgentID {
		t.Errorf("Department.AgentIDs() = %v, want [%q]", agents, CarbonAgentID)
	}
	if _, err := dept.Target(CarbonAgentID); err != nil {
		t.Errorf("Target(%q) = %v, want the Carbon target", CarbonAgentID, err)
	}

	_, err = dept.Target(sessionwire.AgentID("some-other-product"))
	var unknown *department.UnknownAgentError
	if !errors.As(err, &unknown) {
		t.Fatalf("Target(unknown) error = %v, want *department.UnknownAgentError", err)
	}
	if code := unknown.ErrorCode(); code != sessionwire.ErrorCodeRuntimeUnavailable {
		t.Errorf("UnknownAgentError.ErrorCode() = %q, want %q", code, sessionwire.ErrorCodeRuntimeUnavailable)
	}
}

// TestCarbonDepartmentRefusesAMissingLauncher proves the constructor rejects a
// composition with nothing to launch WITH, rather than building a registry whose one
// target fails on first use. department.NewRigTarget already validates before
// returning a target for exactly this reason; the launcher is the one input it
// cannot see.
func TestCarbonDepartmentRefusesAMissingLauncher(t *testing.T) {
	t.Parallel()

	if _, err := NewCarbonDepartment(nil, department.CompatibilityID("carbon-test")); err == nil {
		t.Fatal("NewCarbonDepartment(nil, ...) = nil error, want a refusal")
	}
}

// ---- capabilities ----------------------------------------------------------

// TestCarbonIsDedicatedOnlyUntilAPerSessionRootLauncherExists holds the capability
// declaration against what Carbon actually ships.
//
// The pooled row is the one with consequences, and they are not a clean failure. Host
// publishes a pooled seat for any target whose PoolingPermitted() holds; Factory's
// placement policy selects the first admissible candidate on the agent/runtime/placement
// triple and cannot see that the launcher will refuse; the refusal is flattened into a
// skipped candidate; and with one Host the round ends OutcomeNoCapacity with NO ERROR,
// retried every sweep forever. An operator sees a session that never places on a Host
// advertising free seats.
//
// So this test is coupled to the launcher on purpose: it asserts that the ONLY
// launcher Carbon ships refuses pooled, and that the declaration agrees. Flip one
// without the other and it fails.
func TestCarbonIsDedicatedOnlyUntilAPerSessionRootLauncherExists(t *testing.T) {
	t.Parallel()

	capabilities := carbonCapabilities()
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("carbonCapabilities().Validate() = %v, want nil", err)
	}

	if capabilities.SupportsPooled {
		t.Error("SupportsPooled = true: Carbon ships one launcher and it serves a single workspace root, so a pooled seat is a promise the composition cannot keep — Host advertises it, Factory attaches, and the session loops on no-capacity with no error")
	}
	if capabilities.PoolingPermitted() {
		t.Error("PoolingPermitted() = true; Host would publish a pooled seat")
	}
	if !capabilities.SupportsDedicated {
		t.Fatal("SupportsDedicated = false: Carbon would be unplaceable altogether")
	}
	placements := capabilities.PermittedPlacements()
	if len(placements) != 1 || placements[0] != sessionwire.HostPlacementDedicated {
		t.Errorf("PermittedPlacements() = %v, want exactly [dedicated]", placements)
	}

	// The declaration and the shipped launcher must agree. This is the coupling: the
	// day a per-session-root launcher exists, THIS assertion is what forces the
	// capability to be revisited rather than left false out of caution.
	launcher := NewServeHostLauncher(&ServeHost{workspace: "/served/root"})
	_, err := launcher.Launch(context.Background(), LaunchScope{
		SessionID: sessionwire.SessionID("session-a"),
		Placement: sessionwire.HostPlacementPooled,
	})
	var pooled *PooledPlacementUnsupportedError
	if !errors.As(err, &pooled) {
		t.Fatalf("the shipped launcher answered a pooled placement with %v; if it can now serve pooled, carbonCapabilities must say so", err)
	}

	// The capture value still has to be a legal pooled declaration, because it is what
	// decides pooling the day the flag flips and a field nobody checks is a field that
	// rots. PoolingPermitted lists the SAFE values and defaults everything else to
	// unsafe, so a typo, a zero value or a future Core constant all land on the unsafe
	// side.
	if capabilities.CaptureSafety != department.CaptureSafetyBoundedMaterialized {
		t.Errorf("CaptureSafety = %q, want bounded_materialized: Carbon's highest-output tools materialize their result under harness's declared ceiling", capabilities.CaptureSafety)
	}
	if !(department.Capabilities{
		SupportsPooled:  true,
		AdmissionWeight: 1,
		CaptureSafety:   capabilities.CaptureSafety,
	}).PoolingPermitted() {
		t.Error("the declared capture safety would forbid pooling even with SupportsPooled set; flipping the flag back would silently produce a dedicated-only target")
	}

	if capabilities.AdmissionWeight == 0 {
		t.Error("AdmissionWeight = 0; a target that costs nothing admits without bound")
	}
	if !capabilities.RequiresWorkspace {
		t.Error("RequiresWorkspace = false; every Carbon launch materializes a workspace and leases its root")
	}
	if capabilities.RequiresCheckpoint {
		t.Error("RequiresCheckpoint = true; Carbon restores a conversation from the journal, so requiring a checkpoint would refuse every restore of a session that never made one")
	}
}

// ---- the compatibility identity --------------------------------------------

// TestCarbonCompatibilityIDIsDeterministicAndRestoreCritical is R1.2 step 5.
//
// Determinism is the cheap half. The load-bearing half is that it MOVES with every
// restore-critical input: Host refuses a restore onto a runtime build whose
// CompatibilityID differs from the one the durable state was written by, so an
// identity that ignored (say) the MCP revision would let Host admit a restore that
// harness then rejects as configuration drift — after the session is placed and the
// workspace lease is taken.
func TestCarbonCompatibilityIDIsDeterministicAndRestoreCritical(t *testing.T) {
	t.Parallel()

	base := Config{
		AccessProfile:   AccessTrusted,
		AccessConfigRev: "access-rev-1",
		MCPConfigRev:    "mcp-rev-1",
		ModelConfigRev:  "models-rev-1",
	}
	baseline := CarbonCompatibilityID(base)

	if again := CarbonCompatibilityID(base); again != baseline {
		t.Errorf("CarbonCompatibilityID is not deterministic: %q then %q", baseline, again)
	}
	if err := baseline.Validate(); err != nil {
		t.Fatalf("CompatibilityID.Validate() = %v; Host may not write this to the wire", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(Config) Config
	}{
		{"the access policy revision", func(c Config) Config { c.AccessConfigRev = "access-rev-2"; return c }},
		{"the MCP capability revision", func(c Config) Config { c.MCPConfigRev = "mcp-rev-2"; return c }},
		{"the model catalogue revision", func(c Config) Config { c.ModelConfigRev = "models-rev-2"; return c }},
		{"the access profile", func(c Config) Config { c.AccessProfile = AccessReadOnly; return c }},
	} {
		if got := CarbonCompatibilityID(tc.mutate(base)); got == baseline {
			t.Errorf("changing %s did not move the compatibility identity (still %q); a restore onto an incompatible runtime would be admitted", tc.name, got)
		}
	}
}

// TestCarbonCompatibilityIDCarriesNoInput proves the identity cannot leak a
// configuration value verbatim.
//
// It is written as a SHAPE assertion rather than a substring search, and that is
// deliberate: a substring search proves only that the one secret the test thought of
// is absent. A fixed-length hex digest behind a fixed prefix can contain no input at
// all, which is the property the wire actually needs — this value is written to a
// HostLink capacity report, the least private place in this system.
func TestCarbonCompatibilityIDCarriesNoInput(t *testing.T) {
	t.Parallel()

	id := CarbonCompatibilityID(Config{
		AccessProfile:   AccessTrusted,
		AccessConfigRev: "sk-do-not-leak-me",
		MCPConfigRev:    "Bearer do-not-leak-me-either",
		ModelConfigRev:  "models-rev",
	})
	shape := regexp.MustCompile(`^carbon-[0-9a-f]{64}$`)
	if !shape.MatchString(string(id)) {
		t.Fatalf("CarbonCompatibilityID = %q, want %s: only a fixed-width digest can be proven to carry no input", id, shape)
	}
	if strings.Contains(string(id), "leak") {
		t.Fatalf("CarbonCompatibilityID = %q carries an input verbatim", id)
	}
}

// ---- create, restore and mismatch ------------------------------------------

// TestCarbonTargetCreatesUnderTheRequestedHarnessIdentity is host v0.3.0's obligation
// at Carbon's seam, and it is not cosmetic.
//
// Factory derives the runtime session id at create time and writes it into the
// session's immutable durable binding. A launcher that minted its own id would write
// the conversation to a journal the binding does not name: nothing could find it
// again and the next placement would start the conversation over, silently. department
// refuses such a launch and releases the mis-launched session, so the assertion here
// is that the id Host ASKED for is the id the runtime reports.
func TestCarbonTargetCreatesUnderTheRequestedHarnessIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	wanted := mustUUIDForTest(t)

	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: wanted,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })

	got, ok := department.RigSessionID(runtime)
	if !ok {
		t.Fatal("department.RigSessionID reported nothing; the adapted runtime lost harness's identity")
	}
	if got != wanted {
		t.Errorf("the runtime launched under %v, want the binding's %v", got, wanted)
	}
	if runtime.SessionID() != sessionwire.SessionID("session-a") {
		t.Errorf("Runtime.SessionID() = %q, want the wire identity Host asked about", runtime.SessionID())
	}
	if runtime.AgentID() != CarbonAgentID {
		t.Errorf("Runtime.AgentID() = %q, want %q", runtime.AgentID(), CarbonAgentID)
	}
}

// TestCarbonTargetRefusesALaunchUnderTheWrongIdentity is the falsifier for the case
// above: it proves department's identity check is reachable through THIS adapter,
// not merely present in the dependency. A launcher that ignores RigSessionID is
// exactly the product defect the check exists to catch.
func TestCarbonTargetRefusesALaunchUnderTheWrongIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	ignoring := launcherFunc(func(ctx context.Context, _ LaunchScope) (session.SessionController, error) {
		// Mints its own id, which is the mistake.
		return fixture.rig.NewSession(ctx)
	})
	target, err := department.NewRigTarget(&carbonRig{launcher: ignoring}, CarbonCompatibilityID(fixture.cfg), carbonCapabilities())
	if err != nil {
		t.Fatalf("NewRigTarget: %v", err)
	}

	_, err = target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if !errors.Is(err, department.ErrRigSessionIdentity) {
		t.Fatalf("Create with a minted id = %v, want department.ErrRigSessionIdentity", err)
	}
}

// TestCarbonTargetRestoresAndRefusesAnIncompatibleRuntime covers both halves of
// R1.2 step 1's restore row.
//
// The mismatch arm is what stops a session being relaunched onto a runtime build
// that did not write its durable state. It is refused with runtime_unavailable
// rather than an invalid-request code for UnknownAgentError's reason: the request is
// well formed and this Host has no runtime that can serve it.
func TestCarbonTargetRestoresAndRefusesAnIncompatibleRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	compatibility := target.CompatibilityID()

	created, err := target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rigID, _ := department.RigSessionID(created)
	if err := created.ReleaseResidency(ctx); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}

	restored, err := target.Restore(ctx, department.RestoreRequest{
		TenantID:        sessionwire.TenantID("tenant-a"),
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Placement:       sessionwire.HostPlacementDedicated,
		CompatibilityID: compatibility,
		RigSessionID:    rigID,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = restored.ReleaseResidency(context.Background()) })
	if got, _ := department.RigSessionID(restored); got != rigID {
		t.Errorf("the restored runtime reports %v, want %v", got, rigID)
	}

	_, err = target.Restore(ctx, department.RestoreRequest{
		TenantID:        sessionwire.TenantID("tenant-a"),
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Placement:       sessionwire.HostPlacementDedicated,
		CompatibilityID: department.CompatibilityID("carbon-some-other-build"),
		RigSessionID:    rigID,
	})
	var mismatch *department.CompatibilityMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Restore onto another build = %v, want *department.CompatibilityMismatchError", err)
	}
	if code := mismatch.ErrorCode(); code != sessionwire.ErrorCodeRuntimeUnavailable {
		t.Errorf("CompatibilityMismatchError.ErrorCode() = %q, want %q", code, sessionwire.ErrorCodeRuntimeUnavailable)
	}
}

// mustCarbonTarget builds the Carbon launch target over a fixture's rig.
func mustCarbonTarget(t *testing.T, fixture *realRigFixture) department.LaunchTarget {
	t.Helper()
	dept, err := NewCarbonDepartment(fixture.launcher(), CarbonCompatibilityID(fixture.cfg))
	if err != nil {
		t.Fatalf("NewCarbonDepartment: %v", err)
	}
	target, err := dept.Target(CarbonAgentID)
	if err != nil {
		t.Fatalf("Target(%q): %v", CarbonAgentID, err)
	}
	return target
}

// ---- the segregated capabilities -------------------------------------------

// TestCarbonRuntimeOffersEverySegregatedCapability proves the adapted value Host
// actually holds satisfies every capability Host requires AND the one it does not.
//
// The AttemptCloser row is the reason this test exists in this shape. department's
// adapter FORWARDS the closer if the launched session offers one and leaves the
// field at its zero value otherwise, so the capability is discovered on the value
// this assertion runs against — and a wrapper that failed to forward it would leave
// every stranded attempt blocked with the closer sitting unused one layer down. That
// was a real defect in department itself, found by a surviving mutant rather than by
// reading, on the one capability whose absence is legitimate and therefore invisible.
func TestCarbonRuntimeOffersEverySegregatedCapability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })

	// The six required ones are structural: department returns an
	// *IncapableRuntimeError instead of a Runtime when any is missing, so reaching
	// here at all proves them. The epoch is the one whose VALUE matters.
	epoch, held := runtime.LeaseEpoch()
	if !held {
		t.Fatal("LeaseEpoch reported held=false on a freshly launched session; harness's journal grant was not forwarded")
	}
	if epoch == 0 {
		t.Error("LeaseEpoch reported 0 while holding a lease; no pinned provider zeroes a live grant")
	}

	if _, ok := runtime.(department.AttemptCloser); !ok {
		t.Fatal("the adapted runtime does not satisfy department.AttemptCloser: a stranded attempt on this session could never be closed, and every later command would block behind it forever")
	}

	select {
	case <-runtime.Done():
		t.Error("Liveness reported the runtime already stopped answering")
	default:
	}
}

// ---- the headline: a successor frees a stranded attempt ---------------------

// TestASuccessorClosesAPredecessorsStrandedAttempt is the obligation host v0.5.0's
// §9 does not list and the tests lane found by measurement.
//
// # What it is about
//
// A Host durably begins ONE dispatch attempt before handing a command to a runtime.
// If that runtime dies — a pod eviction, a crash, a lost lease — the record is left
// `applying` with an attempt and no disposition frame. The store has nothing to
// settle from; the deadline sweep SKIPS an attempt-bearing record, so it never
// expires; and the consumer will not advance its cursor past a non-terminal record,
// so EVERY LATER COMMAND ON THAT SESSION IS BLOCKED BEHIND IT. The only exit is a
// successor writing the recovery closure, and Host asks the RUNTIME for it because
// only the runtime's journal can say whether the attempt left an effect.
//
// It is NOT a migration capability. The closure fires on any failover with an
// attempt in flight, which an all-current fleet reaches the first time a pod is
// evicted.
//
// # What is asserted, and why each half is needed
//
// The predecessor takes a session and its journal grant, then releases residency
// without ever settling the attempt. A successor restores the same session — which
// gives it a STRICTLY LATER journal grant, asserted rather than assumed — and closes
// the attempt. harness refuses the closure outright if the grant is not strictly
// later, so a test that did not assert the epochs moved could pass against a
// successor that was really the predecessor.
func TestASuccessorClosesAPredecessorsStrandedAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	rigID := mustUUIDForTest(t)

	predecessor, err := target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: rigID,
	})
	if err != nil {
		t.Fatalf("Create (predecessor): %v", err)
	}
	predecessorEpoch, held := predecessor.LeaseEpoch()
	if !held {
		t.Fatal("the predecessor holds no journal grant")
	}

	// The attempt Host authorized and the predecessor never settled. The identities
	// are Host's: the command id is the public retry-stable one and the attempt id
	// is the STORE's, written immutably before any dispatch. Nothing here mints one,
	// because a value invented on this side would name an attempt no evidence could
	// ever be about.
	const strandedCommand = sessionwire.CommandID("command-stranded-1")
	const strandedAttempt = "attempt-stranded-1"
	strandedRuntimeCommand := mustUUIDForTest(t)

	// The predecessor dies. ReleaseResidency is NONTERMINAL: the session stays
	// resumable and no SessionStopped is appended, which is the shape a drain or an
	// eviction leaves behind.
	if err := predecessor.ReleaseResidency(ctx); err != nil {
		t.Fatalf("ReleaseResidency (predecessor): %v", err)
	}

	successor, err := target.Restore(ctx, department.RestoreRequest{
		TenantID:        sessionwire.TenantID("tenant-a"),
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Placement:       sessionwire.HostPlacementDedicated,
		CompatibilityID: target.CompatibilityID(),
		RigSessionID:    rigID,
	})
	if err != nil {
		t.Fatalf("Restore (successor): %v", err)
	}
	t.Cleanup(func() { _ = successor.ReleaseResidency(context.Background()) })

	successorEpoch, held := successor.LeaseEpoch()
	if !held {
		t.Fatal("the successor holds no journal grant")
	}
	if successorEpoch <= predecessorEpoch {
		t.Fatalf("the successor's journal grant is %d and the predecessor's was %d; a closure needs a STRICTLY later grant, so this case would prove nothing",
			successorEpoch, predecessorEpoch)
	}

	closer, ok := successor.(department.AttemptCloser)
	if !ok {
		t.Fatal("the successor offers no recovery closure: the stranded attempt is unclosable and this session's whole command stream is wedged permanently")
	}
	successorController := fixture.lastLaunched(t)

	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, carbonKindInput, strandedAttempt, predecessorEpoch); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}

	// THE TOMBSTONE MUST HAVE LANDED, and a nil error is not that assertion.
	//
	// host reads `err == nil` from this method as "the closure is durable" and
	// proceeds straight to settling the record. An adapter that returned nil while
	// harness had refused would make Host settle a command with NO tombstone written,
	// and would specifically skip the ErrEnduringEffect arm — the case where the
	// predecessor's effect committed and only its evidence is missing, which settling
	// `not_applied` durably denies. So the landing is asserted, not inferred.
	//
	// It is asserted from the CONTROLLER because department.AttemptCloser returns only
	// an error: harness's ClosureResult, which carries Appended and the sequence, does
	// not cross that seam. Re-offering the identical closure to harness directly must
	// report Appended=false at a non-zero sequence, which is exactly "an identical
	// closure was already durable, and here is where the original landed".
	harnessCloser, ok := successorController.(runtimecommand.AttemptCloser)
	if !ok {
		t.Fatal("the launched controller offers no runtimecommand.AttemptCloser")
	}
	replay, err := harnessCloser.CloseAttempt(ctx, runtimecommand.Closure{
		CommandID:           runtimecommand.CommandID(strandedCommand),
		RuntimeCommandID:    strandedRuntimeCommand,
		Kind:                runtimecommand.KindInput,
		AttemptID:           runtimecommand.AttemptID(strandedAttempt),
		AttemptJournalEpoch: predecessorEpoch,
	})
	if err != nil {
		t.Fatalf("re-offering the closure to harness: %v", err)
	}
	if replay.Appended {
		t.Error("harness appended a SECOND tombstone for the same attempt; the adapter's call did not land, so nothing was durable to deduplicate against")
	}
	if replay.Sequence == 0 {
		t.Error("harness reports the original closure at sequence 0; the adapter's call never landed")
	}

	// A REDELIVERED recovery is not a second tombstone. Host may re-offer the
	// closure after a crash between the write and the acknowledgement, and a second
	// refusal here would put the session right back where it started.
	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, carbonKindInput, strandedAttempt, predecessorEpoch); err != nil {
		t.Fatalf("CloseAttempt (redelivered): %v, want the original closure replayed", err)
	}

	// AND A REFUSAL MUST REACH HOST. A closure whose attempt grant is not strictly
	// earlier than the runtime's own is refused by harness; the adapter must surface
	// that refusal rather than swallow it. This is the same hazard as the landing
	// assertion above, measured against the real dependency instead of a double: the
	// two together are what make "the adapter reports what harness decided" a fact.
	err = closer.CloseAttempt(ctx, sessionwire.CommandID("command-not-earlier"), mustUUIDForTest(t),
		carbonKindInput, "attempt-not-earlier", successorEpoch)
	if err == nil {
		t.Fatal("a closure at the runtime's OWN grant was reported successful; host would settle the record with no tombstone written")
	}
	var unauthorized *runtimecommand.ClosureNotAuthorizedError
	if !errors.As(err, &unauthorized) {
		t.Errorf("the refusal is %v, want harness's *ClosureNotAuthorizedError carried through unchanged", err)
	}
}

// TestARuntimeWithNoCloserRefusesRatherThanConcluding is the falsifier for the case
// above. A composition whose session cannot write a closure must REFUSE, naming the
// missing capability — never decide the command's fate without the evidence a
// closure would have produced.
//
// The refusal is also what makes the capability's absence diagnosable at all. Host's
// own adapter answers department.ErrNoAttemptCloser for a runtime that offers none;
// this adapter always offers the METHOD (it is a product, and it knows what it
// composed), so the distinction moves into the error, exactly as department's own
// sentinel doc describes for the same reason.
func TestARuntimeWithNoCloserRefusesRatherThanConcluding(t *testing.T) {
	t.Parallel()

	runtime := &carbonRuntime{controller: &closerlessController{}}
	err := runtime.CloseAttempt(context.Background(),
		sessionwire.CommandID("command-1"), mustUUIDForTest(t), carbonKindInput, "attempt-1", 1)
	if !errors.Is(err, ErrCarbonRuntimeCannotClose) {
		t.Fatalf("CloseAttempt on a closerless runtime = %v, want ErrCarbonRuntimeCannotClose", err)
	}
	// AND IT MUST REACH HOST'S "no closer" ARM. host's disposition applier branches
	// on department.ErrNoAttemptCloser and has a case written for exactly this
	// composition shape — a composed runtime that declares the method for every
	// runtime, so an absent closer arrives as a refusal rather than a failed type
	// assertion. An unwrapped sentinel falls into the default arm and surfaces as
	// RefusalStore, "ambiguous durable-store failure", which sends a composer to read
	// the store for a composition problem.
	if !errors.Is(err, department.ErrNoAttemptCloser) {
		t.Fatalf("CloseAttempt on a closerless runtime = %v, which does not wrap department.ErrNoAttemptCloser; host will report it as an ambiguous store failure", err)
	}
}

// closerlessController is a session.SessionController stand-in that offers neither
// the applier nor the closer. It is a DISTINCT TYPE rather than a flag on a fake,
// because Go method sets are not conditional: a single struct with a nil func field
// would satisfy every assertion and the discovery would never be exercised.
type closerlessController struct{ session.SessionController }

// recordingCloserController offers a closure that RECORDS what it was handed and
// answers from a script.
//
// It is PERMISSIVE by default, and that is what makes it useful. harness's own closer
// validates and fences, so against a real session a malformed or misdirected closure
// errs either way and a test asserting only "an error happened" cannot tell which
// layer refused — nor can it see what the adapter actually forwarded, since
// department.AttemptCloser returns only an error. Here the runtime accepts
// everything, so what is recorded IS what the adapter built.
type recordingCloserController struct {
	session.SessionController
	calls    []runtimecommand.Closure
	failWith error
}

func (c *recordingCloserController) CloseAttempt(_ context.Context, closure runtimecommand.Closure) (runtimecommand.ClosureResult, error) {
	c.calls = append(c.calls, closure)
	if c.failWith != nil {
		return runtimecommand.ClosureResult{}, c.failWith
	}
	return runtimecommand.ClosureResult{Appended: true, Sequence: uint64(len(c.calls))}, nil
}

// TestCloseAttemptForwardsTheCallersIdentities proves the tombstone is about the
// command Host named, under the kind and attempt Host named.
//
// EVERY FIELD IS A DIFFERENT WAY TO TOMBSTONE THE WRONG THING, and none of them is
// caught downstream. harness keys closure idempotency on `command-disposition:<attemptID>`,
// so a closure carrying the wrong CommandID still deduplicates cleanly on redelivery
// and a redelivery assertion cannot see it; a wrong Kind writes a disposition the
// settlement verifier correlates against a different record; and a substituted
// AttemptID names an attempt no evidence was ever about. The author grant is
// deliberately absent from this list — harness stamps that from the live lease it
// holds, because a caller-supplied author epoch would be a caller-authored proof.
func TestCloseAttemptForwardsTheCallersIdentities(t *testing.T) {
	t.Parallel()

	controller := &recordingCloserController{}
	runtime := &carbonRuntime{controller: controller}

	command := sessionwire.CommandID("command-forwarded-1")
	runtimeCommand := mustUUIDForTest(t)
	const attempt = "attempt-forwarded-1"
	const attemptEpoch = uint64(7)

	if err := runtime.CloseAttempt(context.Background(), command, runtimeCommand, carbonKindGateResponse, attempt, attemptEpoch); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if len(controller.calls) != 1 {
		t.Fatalf("the runtime's closer saw %d closures, want exactly 1", len(controller.calls))
	}
	got := controller.calls[0]
	want := runtimecommand.Closure{
		CommandID:           runtimecommand.CommandID(command),
		RuntimeCommandID:    runtimeCommand,
		Kind:                runtimecommand.KindGateResponse,
		AttemptID:           runtimecommand.AttemptID(attempt),
		AttemptJournalEpoch: attemptEpoch,
	}
	if got != want {
		t.Errorf("the forwarded closure is %+v, want %+v", got, want)
	}
}

// TestCloseAttemptPropagatesTheRuntimesRefusal is the assertion host's applier
// depends on and the suite previously did not make.
//
// host reads a nil error from this method as "the tombstone is durable" and settles
// the record. So an adapter that discarded harness's refusal would make Host settle a
// command with NOTHING written — and the row that matters most is ErrEnduringEffect,
// where the predecessor's effect COMMITTED and only its evidence is missing. Settling
// `not_applied` there durably denies work the user actually received, and it is the
// one error in this protocol that can never be recovered from.
//
// Each row asserts the error arrives UNWRAPPED ENOUGH to branch on, because host
// branches on the type, not on a message.
func TestCloseAttemptPropagatesTheRuntimesRefusal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			"an enduring effect: the predecessor's work committed and only its evidence is missing",
			&runtimecommand.EnduringEffectError{
				AttemptID:        runtimecommand.AttemptID("attempt-1"),
				CommandID:        runtimecommand.CommandID("command-1"),
				RuntimeCommandID: mustUUIDForTest(t),
				EffectSeq:        4,
			},
		},
		{
			"a grant that is not strictly later",
			&runtimecommand.ClosureNotAuthorizedError{
				AttemptID:           runtimecommand.AttemptID("attempt-1"),
				AttemptJournalEpoch: 2,
				Current:             2,
				Held:                true,
			},
		},
		{
			"an unreadable journal",
			errors.New("runtimecommand: the journal could not be fully read"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller := &recordingCloserController{failWith: tc.err}
			runtime := &carbonRuntime{controller: controller}

			err := runtime.CloseAttempt(context.Background(),
				sessionwire.CommandID("command-1"), mustUUIDForTest(t), carbonKindInput, "attempt-1", 1)
			if err == nil {
				t.Fatal("the refusal was reported as success; host would settle the record with no tombstone written")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("CloseAttempt = %v, want the runtime's own refusal carried through", err)
			}
		})
	}
}

// TestCloseAttemptValidatesBeforeTouchingTheRuntime is the falsifier for the
// adapter's own Closure.Validate call, and it needs a permissive runtime to make.
//
// A tombstone is the one record that must never be written on a caller's say-so, and
// an adapter that forwarded an unvalidated closure would be relying on a guard in
// another module to hold a rule this one states. Here the runtime would ACCEPT every
// row, so only the adapter's own check can produce the refusal — and the assertion
// that the closer was NOT CALLED is what makes that specific.
//
// The unknown-kind row is here rather than against a real session for exactly this
// reason. Against a real session it passed on harness's EPOCH fence (author grant 1
// against attempt epoch 1), not on the kind check it claimed to test: a vacuous row
// that would have stayed green with the kind forwarding removed.
func TestCloseAttemptValidatesBeforeTouchingTheRuntime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		kind    string
		attempt string
	}{
		{"no attempt id", carbonKindInput, ""},
		{"a kind neither vocabulary has ever held", "carbon_no_such_kind", "attempt-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller := &recordingCloserController{}
			runtime := &carbonRuntime{controller: controller}

			if err := runtime.CloseAttempt(context.Background(),
				sessionwire.CommandID("command-1"), mustUUIDForTest(t), tc.kind, tc.attempt, 1); err == nil {
				t.Fatal("the closure was accepted; it could tombstone the wrong command")
			}
			if len(controller.calls) != 0 {
				t.Errorf("the runtime's closer was called %d times for a malformed closure, want 0: the adapter must refuse before the runtime is touched", len(controller.calls))
			}
		})
	}

	controller := &recordingCloserController{}
	runtime := &carbonRuntime{controller: controller}
	if err := runtime.CloseAttempt(context.Background(),
		sessionwire.CommandID("command-1"), mustUUIDForTest(t), carbonKindInput, "attempt-1", 1); err != nil {
		t.Fatalf("a well-formed closure was refused: %v", err)
	}
	if len(controller.calls) != 1 {
		t.Errorf("the runtime's closer was called %d times for a well-formed closure, want 1", len(controller.calls))
	}
}

// ---- the command vocabulary -------------------------------------------------

// TestCarbonAppliesEveryAdmittedKindAndRefusesAnUnknownOne covers the five-kind
// vocabulary and the one arm that must fail closed.
//
// A create's first message is the row worth reading twice. harness makes Blocks
// OPTIONAL for a create so an idle create can still settle, which means an
// UNDECODED payload and an ABSENT one are the same value at harness's seam: a
// product that failed to decode a create's body would drive no turn, settle
// `applied`, and drop the user's first words in silence with the record looking
// perfectly correct. So the create arm asserts the BLOCKS, not the acceptance.
func TestCarbonDecodesEveryAdmittedKindsBody(t *testing.T) {
	t.Parallel()

	// The fixture is a CreateRequest Core's own strict decoder accepts, envelope
	// and all. A hand-rolled object shaped like one would prove nothing: this is
	// exactly the body Factory canonically encodes, and the adapter's whole job is
	// to read THAT.
	created, err := carbonCreateBlocks(mustJSON(t, sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "command-1"},
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"first"},{"Type":"text","Text":"second"}]`),
	}))
	if err != nil {
		t.Fatalf("carbonCreateBlocks: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("a create's first message decoded to %d blocks, want 2: a product that concatenated or dropped one satisfies every substring check while losing exactly what a multi-block message is for", len(created))
	}

	bare, err := carbonCreateBlocks(nil)
	if err != nil {
		t.Fatalf("a bare create was refused (%v); refusing one wedges every session created with no opening message", err)
	}
	if len(bare) != 0 {
		t.Errorf("a bare create carried %d blocks, want none", len(bare))
	}

	if _, err := carbonCreateBlocks([]byte(`{"blocks": not json}`)); err == nil {
		t.Error("a create body Core cannot read was accepted; it would apply as an empty turn and settle applied with the user's words gone")
	}

	if _, err := carbonInputBlocks(nil); err == nil {
		t.Error("an empty input body was accepted; harness requires blocks for an input")
	}
}

// TestCarbonRefusesAnUnknownKindBeforeAnyDurableWrite holds the fail-closed arm.
//
// A kind a newer Factory admits and this build does not know is REFUSED, not
// guessed at. Guessing is what dropped a create's first message for a whole release,
// and a refusal before any durable write leaves the record where a Host that
// understands the kind can take it.
func TestCarbonRefusesAnUnknownKindBeforeAnyDurableWrite(t *testing.T) {
	t.Parallel()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	runtime, err := target.Create(context.Background(), department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })

	err = runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
		CommandID:        sessionwire.CommandID("command-1"),
		RuntimeCommandID: mustUUIDForTest(t),
		Kind:             "carbon_no_such_kind",
		AttemptID:        "attempt-1",
	})
	if !errors.Is(err, ErrCarbonUnknownCommandKind) {
		t.Fatalf("ApplyCommand with an unknown kind = %v, want ErrCarbonUnknownCommandKind", err)
	}
}

// TestCarbonCommandVocabularyIsTheReleasedOne pins the five kinds against harness's
// own vocabulary, so this product cannot drift from the set Factory admits.
//
// It also pins the falsifier's kind: the string this package uses as an example of
// an unknown kind must be in NEITHER vocabulary. "restore" was an unknown kind until
// harness v0.36.0 named it and "gate_response" until v0.35.0 did, and a fixture on
// either went green the day the vocabulary widened while whatever it guarded was
// live.
func TestCarbonCommandVocabularyIsTheReleasedOne(t *testing.T) {
	t.Parallel()

	kinds := carbonCommandKinds()
	if len(kinds) != 5 {
		t.Fatalf("carbonCommandKinds() names %d kinds, want 5", len(kinds))
	}
	for _, kind := range kinds {
		if !runtimecommand.Kind(kind).Valid() {
			t.Errorf("%q is not a kind harness names; this product would refuse a command Factory admits", kind)
		}
	}
	if runtimecommand.Kind("carbon_no_such_kind").Valid() {
		t.Error(`"carbon_no_such_kind" is now a named kind; the unknown-kind falsifier must move to a string neither vocabulary has ever held`)
	}
}

// TestCarbonRefusesACommandWhenItHoldsNoLease proves the adapter refuses BEFORE the
// apply rather than sending a guessed epoch.
//
// harness checks an admitted command's LeaseEpoch for EQUALITY against the lease the
// runtime holds, so a zero would be refused there anyway — but as a malformed record
// rather than as a lost lease, which sends an operator looking for the wrong failure.
func TestCarbonRefusesACommandWhenItHoldsNoLease(t *testing.T) {
	t.Parallel()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	runtime, err := target.Create(context.Background(), department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A RELEASED lease is the real shape of this: the session is still a valid Go
	// value and every method still answers, but its journal grant is gone.
	if err := runtime.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	if _, held := runtime.LeaseEpoch(); held {
		t.Fatal("the released session still reports a held lease; this case would not exercise the refusal")
	}

	err = runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
		CommandID:        sessionwire.CommandID("command-1"),
		RuntimeCommandID: mustUUIDForTest(t),
		Kind:             carbonKindInterrupt,
		AttemptID:        "attempt-1",
	})
	if !errors.Is(err, ErrCarbonRuntimeNoLease) {
		t.Fatalf("ApplyCommand under a released lease = %v, want ErrCarbonRuntimeNoLease", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}

func mustBlocksJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("the fixture blocks are not valid JSON: %s", raw)
	}
	return json.RawMessage(raw)
}

// ---- what ApplyCommand actually hands harness -------------------------------

// recordingApplierController records every Admitted record the adapter builds and
// accepts all of them.
//
// It reports a held lease at a distinctive epoch so the forwarded LeaseEpoch can be
// told apart from a zero or a constant. It is permissive for the same reason
// recordingCloserController is: harness validates, so against a real session a
// mis-built Admitted errs either way — and what the adapter FORWARDED never crosses
// the department seam at all, because ApplyCommand returns only an error.
type recordingApplierController struct {
	session.SessionController
	epoch    uint64
	held     bool
	admitted []runtimecommand.Admitted
}

func (c *recordingApplierController) LeaseEpoch() (uint64, bool) { return c.epoch, c.held }

func (c *recordingApplierController) ApplyRuntimeCommand(_ context.Context, admitted runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	c.admitted = append(c.admitted, admitted)
	return runtimecommand.Disposition{CommandID: admitted.CommandID, RuntimeCommandID: admitted.RuntimeCommandID}, nil
}

// TestApplyCommandForwardsTheAttemptAndTheIdentities is the ApplyCommand counterpart
// of the closure identity test, and the AttemptID row is the one with teeth.
//
// harness REQUIRES an attempt id only for gate_response; for the other four kinds it
// is optional, because a legacy admitted record carries none and an applier handed
// one writes no disposition at all. So a Carbon that dropped the attempt id would
// still be accepted by harness for input, interrupt, create and restore — and the
// disposition frame would lose the attempt identity SILENTLY. The store settles from
// that frame, and evidence about one attempt is not evidence about another.
//
// The record is also validated on the RELEASED type's own rule, so a shape harness
// would refuse after the attempt is already durable is caught here instead.
func TestApplyCommandForwardsTheAttemptAndTheIdentities(t *testing.T) {
	t.Parallel()

	const epoch = uint64(9)
	createBody := mustJSON(t, sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "command-1"},
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"first"}]`),
	})
	inputBody := mustJSON(t, sessionwire.InputRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "command-1"},
		SessionID:       sessionwire.SessionID("session-a"),
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"more"}]`),
	})
	gateBody := mustJSON(t, sessionwire.GateResponseRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "command-1"},
		SessionID:       sessionwire.SessionID("session-a"),
		GateID:          sessionwire.GateID(mustUUIDForTest(t).String()),
		Action:          "approve",
		Values:          map[string]json.RawMessage{},
		// Core requires EXACTLY ONE optimistic-open version, so an answer cannot be
		// applied to a different incarnation of the same gate id. A fixture omitting
		// both is refused, which is the decode arm working.
		ExpectedOpenJournalSeq: 3,
	})

	for _, tc := range []struct {
		kind       string
		payload    []byte
		wantKind   runtimecommand.Kind
		wantBlocks int
		wantAnswer bool
	}{
		{carbonKindCreate, createBody, runtimecommand.KindCreate, 1, false},
		{carbonKindRestore, nil, runtimecommand.KindRestore, 0, false},
		{carbonKindInput, inputBody, runtimecommand.KindInput, 1, false},
		{carbonKindInterrupt, nil, runtimecommand.KindInterrupt, 0, false},
		{carbonKindGateResponse, gateBody, runtimecommand.KindGateResponse, 0, true},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			controller := &recordingApplierController{epoch: epoch, held: true}
			runtime := &carbonRuntime{controller: controller}

			command := sessionwire.CommandID("command-" + tc.kind)
			runtimeCommand := mustUUIDForTest(t)
			attempt := "attempt-" + tc.kind

			if err := runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
				CommandID:        command,
				RuntimeCommandID: runtimeCommand,
				Kind:             tc.kind,
				Payload:          tc.payload,
				AttemptID:        attempt,
			}); err != nil {
				t.Fatalf("ApplyCommand: %v", err)
			}
			if len(controller.admitted) != 1 {
				t.Fatalf("the applier saw %d admitted records, want exactly 1", len(controller.admitted))
			}
			got := controller.admitted[0]

			if got.AttemptID != runtimecommand.AttemptID(attempt) {
				t.Errorf("AttemptID = %q, want %q: the disposition frame the store settles from would name the wrong attempt, or none",
					got.AttemptID, attempt)
			}
			if got.CommandID != runtimecommand.CommandID(command) {
				t.Errorf("CommandID = %q, want %q", got.CommandID, command)
			}
			if got.RuntimeCommandID != runtimeCommand {
				t.Errorf("RuntimeCommandID = %v, want %v", got.RuntimeCommandID, runtimeCommand)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.LeaseEpoch != epoch {
				t.Errorf("LeaseEpoch = %d, want the epoch the runtime reports holding (%d); harness checks it for EQUALITY against its own lease",
					got.LeaseEpoch, epoch)
			}
			if len(got.Blocks) != tc.wantBlocks {
				t.Errorf("Blocks = %d, want %d", len(got.Blocks), tc.wantBlocks)
			}
			if (got.GateResponse != nil) != tc.wantAnswer {
				t.Errorf("GateResponse present = %t, want %t", got.GateResponse != nil, tc.wantAnswer)
			}
			// The released type's own rule, so a record harness would refuse AFTER the
			// attempt is durable is caught here instead of there.
			if err := got.Validate(); err != nil {
				t.Errorf("the admitted record harness was handed does not validate: %v", err)
			}
		})
	}
}

// ---- R1.2 step 4: the committed publication projection ----------------------

// TestSubscribeCommittedCarriesTheCommittedBytes drives the event projection end to
// end over a real session, which nothing did before.
//
// # Why a driven case and not a shape check
//
// The projection is about fifty lines of filter, goroutine and body carry, and every
// one of its failure modes is SILENT. The worst is the filter: an EventFilter is
// DECLARED INTEREST evaluated before the send, so the zero value selects no loop at
// all — it is "nothing", not "everything" — and a Carbon composed with it would open
// a link, publish no events for the life of the session, and report no error anywhere.
// Nothing but a driven case can tell those apart.
//
// # What is asserted
//
// The publication must carry the COMMITTED bytes verbatim, not a re-projection: a
// consumer joining a durable tail to this live stream would otherwise render two
// different bodies for one event. It must carry the public EventID the durable append
// committed under, because that is what a consumer dedupes on, and the CoveredThrough
// watermark, because that is what a consumer resumes from. And it must be stamped with
// the scope's tenant and session, since Host relays the record unchanged.
func TestSubscribeCommittedCarriesTheCommittedBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID:     sessionwire.TenantID("tenant-a"),
		SessionID:    sessionwire.SessionID("session-a"),
		AgentID:      CarbonAgentID,
		Placement:    sessionwire.HostPlacementDedicated,
		RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })

	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	publications, err := runtime.SubscribeCommitted(streamCtx, sessionwire.EventID(""))
	if err != nil {
		t.Fatalf("SubscribeCommitted: %v", err)
	}

	// Drive one real turn through the PRODUCT path — an admitted input command — so
	// the events are the ones a placed session actually produces.
	if err := runtime.ApplyCommand(ctx, department.RuntimeCommand{
		CommandID:        sessionwire.CommandID("command-input-1"),
		RuntimeCommandID: mustUUIDForTest(t),
		Kind:             carbonKindInput,
		AttemptID:        "attempt-input-1",
		Payload: mustJSON(t, sessionwire.InputRequest{
			CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "command-input-1"},
			SessionID:       sessionwire.SessionID("session-a"),
			Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"hello"}]`),
		}),
	}); err != nil {
		t.Fatalf("ApplyCommand(input): %v", err)
	}

	// WAIT FOR A LOOP-SCOPED EVENT, not merely for "a publication", and this is the
	// assertion that gives the case teeth.
	//
	// Measured: with the zero EventFilter the stream still delivers the SESSION-scoped
	// events (SessionActive, SessionIdle) and delivers NO loop events at all — no
	// TurnStarted, no ContextMeasured, no LoopIdle. So a case that accepted the first
	// publication it saw passed under the broken filter about two runs in three, which
	// is worse than not testing it: a flaky green reads as an infrastructure problem.
	// TurnStarted is loop-scoped, is produced by every input, and carries the user's
	// own message, so asserting on it proves the filter selects loops AND that the
	// committed bytes are the real ones.
	var got sessionwire.EnduringPublication
	deadline := time.After(20 * time.Second)
	for got.EventID == "" {
		select {
		case publication, ok := <-publications:
			if !ok {
				t.Fatal("the publication channel closed before any loop-scoped event; the subscription ended without publishing the turn")
			}
			if bytes.Contains(publication.Body, []byte(`"type":"TurnStarted"`)) {
				got = publication
			}
		case <-deadline:
			t.Fatal("no loop-scoped publication within the deadline: an EventFilter is DECLARED INTEREST evaluated before the send, so the zero value selects no loop at all — a Carbon composed with it opens a link, publishes no turn for the life of the session, and reports no error anywhere")
		}
	}

	if got.TenantID != sessionwire.TenantID("tenant-a") || got.SessionID != sessionwire.SessionID("session-a") {
		t.Errorf("the publication is stamped tenant=%q session=%q, want the launch scope's; Host relays this record unchanged", got.TenantID, got.SessionID)
	}
	if got.EventID == "" {
		t.Error("the publication carries no public EventID; a consumer has nothing to dedupe on")
	}
	if got.JournalSeq == 0 {
		t.Error("the publication carries journal sequence 0")
	}
	if got.CoveredThrough != got.JournalSeq {
		t.Errorf("CoveredThrough = %d and JournalSeq = %d; they are equal by contract — the append that produced a delivery is the newest record it may claim",
			got.CoveredThrough, got.JournalSeq)
	}
	if !json.Valid(got.Body) {
		t.Fatalf("the publication body is not valid JSON: %q", got.Body)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the publication Core would carry does not validate: %v", err)
	}
	// The COMMITTED BYTES, carried rather than re-projected: the body is the durable
	// append's own, so it still holds the loop this turn ran in and the words the user
	// sent. A re-projection would be free to drop either.
	if !bytes.Contains(got.Body, []byte(`"loop_id"`)) {
		t.Errorf("the committed body names no loop: %s", got.Body)
	}
	if !bytes.Contains(got.Body, []byte("hello")) {
		t.Errorf("the committed body does not carry the input's own words: %s", got.Body)
	}

	// The subscription is bound to the context the caller passed, so a Host that stops
	// relaying stops the goroutine and closes the channel rather than leaking both for
	// the life of the session.
	stopStream()
	closeDeadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-publications:
			if !ok {
				return
			}
		case <-closeDeadline:
			t.Fatal("the publication channel stayed open after the subscription context was cancelled; the projection goroutine outlives its caller")
		}
	}
}

// TestSubscribeCommittedRefusesASessionThatCannotReportCommittedBytes holds the
// two-result capability's whole point.
//
// A consumer must learn it is not one of those sessions BEFORE it starts persisting
// cursors, not after. A projection that served a re-rendered approximation instead
// would let a consumer join a durable tail to a live stream that disagrees with it.
func TestSubscribeCommittedRefusesASessionThatCannotReportCommittedBytes(t *testing.T) {
	t.Parallel()

	runtime := &carbonRuntime{controller: &closerlessController{}}
	if _, err := runtime.SubscribeCommitted(context.Background(), sessionwire.EventID("")); !errors.Is(err, ErrCarbonNoPublications) {
		t.Fatalf("SubscribeCommitted on a session with no committed stream = %v, want ErrCarbonNoPublications", err)
	}
}
