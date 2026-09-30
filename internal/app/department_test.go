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
	"github.com/looprig/host/harnessruntime"
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

// wrappedLauncher is launcher() with every launched controller passed through
// wrap before Carbon's target sees it; the REAL controller is what is recorded.
func (f *realRigFixture) wrappedLauncher(wrap func(session.SessionController) session.SessionController) SessionLauncher {
	inner := f.launcher()
	return launcherFunc(func(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
		controller, err := inner.Launch(ctx, scope)
		if err != nil {
			return nil, err
		}
		return wrap(controller), nil
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
// This holds the conservative capability set used for any launcher that makes
// no pooling claim. TestCarbonDepartmentDerivesPoolingFromLauncher covers the
// per-session-root launcher.
func TestSingleRootLauncherRemainsDedicated(t *testing.T) {
	t.Parallel()

	capabilities := carbonCapabilities()
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("carbonCapabilities().Validate() = %v, want nil", err)
	}

	if capabilities.SupportsPooled {
		t.Error("SupportsPooled = true for the single-root launcher")
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
	target := mustCarbonTargetOver(t, ignoring, CarbonCompatibilityID(fixture.cfg))

	_, err := target.Create(ctx, department.CreateRequest{
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
	return mustCarbonTargetOver(t, fixture.launcher(), CarbonCompatibilityID(fixture.cfg))
}

// mustCarbonTargetOver builds the Carbon launch target, through the production
// NewCarbonDepartment, over any launcher.
func mustCarbonTargetOver(t *testing.T, launcher SessionLauncher, compatibility department.CompatibilityID) department.LaunchTarget {
	t.Helper()
	dept, err := NewCarbonDepartment(launcher, compatibility)
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

	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, "input", strandedAttempt, predecessorEpoch); err != nil {
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
	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, "input", strandedAttempt, predecessorEpoch); err != nil {
		t.Fatalf("CloseAttempt (redelivered): %v, want the original closure replayed", err)
	}

	// AND A REFUSAL MUST REACH HOST. A closure whose attempt grant is not strictly
	// earlier than the runtime's own is refused by harness; the adapter must surface
	// that refusal rather than swallow it. This is the same hazard as the landing
	// assertion above, measured against the real dependency instead of a double: the
	// two together are what make "the adapter reports what harness decided" a fact.
	err = closer.CloseAttempt(ctx, sessionwire.CommandID("command-not-earlier"), mustUUIDForTest(t),
		"input", "attempt-not-earlier", successorEpoch)
	if err == nil {
		t.Fatal("a closure at the runtime's OWN grant was reported successful; host would settle the record with no tombstone written")
	}
	var unauthorized *runtimecommand.ClosureNotAuthorizedError
	if !errors.As(err, &unauthorized) {
		t.Errorf("the refusal is %v, want harness's *ClosureNotAuthorizedError carried through unchanged", err)
	}
}

// TestCarbonTargetRefusesASessionThatCannotCloseAnAttempt is the falsifier for
// the case above, moved to where harnessruntime now makes it: at LAUNCH.
//
// Carbon's target declares department.Recovery.AttemptCloser, and a session
// whose runtime-command applier cannot write a recovery closure would make that
// declaration false — a successor on it could never free a stranded attempt,
// and that session's whole command stream would block behind it. So the target
// refuses such a session before Host ever sees it, naming the capability, and
// gives the session back.
func TestCarbonTargetRefusesASessionThatCannotCloseAnAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	var probe *probeController
	target := mustCarbonTargetOver(t, fixture.wrappedLauncher(func(real session.SessionController) session.SessionController {
		probe = newProbe(real)
		return closerlessProbe{probe}
	}), CarbonCompatibilityID(fixture.cfg))
	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
	})
	if runtime != nil {
		t.Fatal("a session that cannot close a stranded attempt was launched under a target declaring AttemptCloser")
	}
	var incapable *harnessruntime.IncapableSessionError
	if !errors.As(err, &incapable) || !strings.Contains(strings.Join(incapable.Missing, ","), "runtimecommand.AttemptCloser") {
		t.Fatalf("Create = %v, want *harnessruntime.IncapableSessionError naming runtimecommand.AttemptCloser", err)
	}
	if _, held := probe.LeaseEpoch(); held {
		t.Fatal("the refused session still holds its journal lease; no successor could hydrate it")
	}
}

// closeProbeTarget launches one real session through Carbon's target with a
// probe whose closer records and answers from the test instead of harness.
func closeProbeTarget(t *testing.T, configure func(*probeController)) (department.Runtime, *probeController) {
	t.Helper()
	fixture := newRealRigFixture(t)
	var probe *probeController
	target := mustCarbonTargetOver(t, fixture.wrappedLauncher(func(real session.SessionController) session.SessionController {
		probe = newProbe(real)
		probe.stubClose = true
		if configure != nil {
			configure(probe)
		}
		return probe
	}), CarbonCompatibilityID(fixture.cfg))
	runtime, err := target.Create(context.Background(), department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })
	return runtime, probe
}

func mustCloser(t *testing.T, runtime department.Runtime) department.AttemptCloser {
	t.Helper()
	closer, ok := runtime.(department.AttemptCloser)
	if !ok {
		t.Fatal("the adapted runtime is not a department.AttemptCloser")
	}
	return closer
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

	runtime, probe := closeProbeTarget(t, nil)
	command := sessionwire.CommandID("command-forwarded-1")
	runtimeCommand := mustUUIDForTest(t)
	const attempt = "attempt-forwarded-1"
	const attemptEpoch = uint64(7)

	if err := mustCloser(t, runtime).CloseAttempt(context.Background(), command, runtimeCommand, "gate_response", attempt, attemptEpoch); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	calls := probe.recordedClosures()
	if len(calls) != 1 {
		t.Fatalf("the runtime's closer saw %d closures, want exactly 1", len(calls))
	}
	want := runtimecommand.Closure{
		CommandID:           runtimecommand.CommandID(command),
		RuntimeCommandID:    runtimeCommand,
		Kind:                runtimecommand.KindGateResponse,
		AttemptID:           runtimecommand.AttemptID(attempt),
		AttemptJournalEpoch: attemptEpoch,
	}
	if calls[0] != want {
		t.Errorf("the forwarded closure is %+v, want %+v", calls[0], want)
	}
}

// TestCloseAttemptPropagatesTheRuntimesRefusal is the assertion host's applier
// depends on.
//
// host reads a nil error from this method as "the tombstone is durable" and settles
// the record. So an adapter that discarded harness's refusal would make Host settle a
// command with NOTHING written — and the row that matters most is ErrEnduringEffect,
// where the predecessor's effect COMMITTED and only its evidence is missing. Settling
// `not_applied` there durably denies work the user actually received. harnessruntime
// also joins department.ErrEnduringEffect to that row, which is the arm Host branches
// on (the hand-written Carbon adapter did not).
func TestCloseAttemptPropagatesTheRuntimesRefusal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		err      error
		enduring bool
	}{
		{
			"an enduring effect: the predecessor's work committed and only its evidence is missing",
			&runtimecommand.EnduringEffectError{
				AttemptID:        runtimecommand.AttemptID("attempt-1"),
				CommandID:        runtimecommand.CommandID("command-1"),
				RuntimeCommandID: mustUUIDForTest(t),
				EffectSeq:        4,
			},
			true,
		},
		{
			"a grant that is not strictly later",
			&runtimecommand.ClosureNotAuthorizedError{
				AttemptID:           runtimecommand.AttemptID("attempt-1"),
				AttemptJournalEpoch: 2,
				Current:             2,
				Held:                true,
			},
			false,
		},
		{
			"an unreadable journal",
			errors.New("runtimecommand: the journal could not be fully read"),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime, _ := closeProbeTarget(t, func(p *probeController) { p.closeErr = tc.err })
			err := mustCloser(t, runtime).CloseAttempt(context.Background(),
				sessionwire.CommandID("command-1"), mustUUIDForTest(t), "input", "attempt-1", 1)
			if err == nil {
				t.Fatal("the refusal was reported as success; host would settle the record with no tombstone written")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("CloseAttempt = %v, want the runtime's own refusal carried through", err)
			}
			if got := errors.Is(err, department.ErrEnduringEffect); got != tc.enduring {
				t.Errorf("errors.Is(err, department.ErrEnduringEffect) = %t, want %t", got, tc.enduring)
			}
		})
	}
}

// TestCloseAttemptValidatesBeforeTouchingTheRuntime proves a malformed closure
// is refused before the runtime's closer is called. The runtime here would ACCEPT
// every row, so only the adapter's own Closure.Validate can produce the refusal —
// and the assertion that the closer was NOT CALLED is what makes that specific.
func TestCloseAttemptValidatesBeforeTouchingTheRuntime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		kind    string
		attempt string
	}{
		{"no attempt id", "input", ""},
		{"a kind neither vocabulary has ever held", "carbon_no_such_kind", "attempt-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime, probe := closeProbeTarget(t, nil)
			if err := mustCloser(t, runtime).CloseAttempt(context.Background(),
				sessionwire.CommandID("command-1"), mustUUIDForTest(t), tc.kind, tc.attempt, 1); err == nil {
				t.Fatal("the closure was accepted; it could tombstone the wrong command")
			}
			if calls := probe.recordedClosures(); len(calls) != 0 {
				t.Errorf("the runtime's closer was called %d times for a malformed closure, want 0", len(calls))
			}
		})
	}

	runtime, probe := closeProbeTarget(t, nil)
	if err := mustCloser(t, runtime).CloseAttempt(context.Background(),
		sessionwire.CommandID("command-1"), mustUUIDForTest(t), "input", "attempt-1", 1); err != nil {
		t.Fatalf("a well-formed closure was refused: %v", err)
	}
	if calls := probe.recordedClosures(); len(calls) != 1 {
		t.Errorf("the runtime's closer was called %d times for a well-formed closure, want 1", len(calls))
	}
}

// ---- the command vocabulary -------------------------------------------------

// applyProbeTarget launches one real session through Carbon's target with a
// probe that records every admitted command and does not reach harness, so
// what the adapter BUILT is observable even for a record harness would refuse.
func applyProbeTarget(t *testing.T) (department.Runtime, *probeController) {
	t.Helper()
	fixture := newRealRigFixture(t)
	var probe *probeController
	target := mustCarbonTargetOver(t, fixture.wrappedLauncher(func(real session.SessionController) session.SessionController {
		probe = newProbe(real)
		probe.stubApply = true
		return probe
	}), CarbonCompatibilityID(fixture.cfg))
	runtime, err := target.Create(context.Background(), department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = runtime.ReleaseResidency(context.Background()) })
	return runtime, probe
}

// TestCarbonDecodesEveryAdmittedKindsBody covers the body decode behind each
// kind, through Carbon's target.
//
// A create's first message is the row worth reading twice. harness makes Blocks
// OPTIONAL for a create so an idle create can still settle, which means an
// UNDECODED payload and an ABSENT one are the same value at harness's seam: a
// product that failed to decode a create's body would drive no turn, settle
// `applied`, and drop the user's first words in silence with the record looking
// perfectly correct. So the create arm asserts the BLOCKS, not the acceptance.
func TestCarbonDecodesEveryAdmittedKindsBody(t *testing.T) {
	t.Parallel()
	runtime, probe := applyProbeTarget(t)
	apply := func(command sessionwire.CommandID, kind string, payload []byte) error {
		return runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
			CommandID: command, RuntimeCommandID: mustUUIDForTest(t), Kind: kind, Payload: payload, AttemptID: "attempt-" + string(command),
		})
	}

	// The fixture is a CreateRequest Core's own strict decoder accepts, envelope
	// and all: exactly the body Factory canonically encodes.
	if err := apply("create-multi", "create", mustJSON(t, sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "create-multi"},
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"first"},{"Type":"text","Text":"second"}]`),
	})); err != nil {
		t.Fatalf("a create carrying a first message was refused: %v", err)
	}
	// A bare create is a CreateRequest with no blocks: Factory always stores the
	// canonical request. (An EMPTY body is refused since harnessruntime; the
	// hand-written adapter read one as a bare create, a body no Factory writes.)
	if err := apply("create-bare", "create", mustJSON(t, sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "create-bare"},
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
	})); err != nil {
		t.Fatalf("a bare create was refused (%v); refusing one wedges every session created with no opening message", err)
	}
	if err := apply("create-bad", "create", []byte(`{"blocks": not json}`)); err == nil {
		t.Error("a create body Core cannot read was accepted; it would apply as an empty turn and settle applied with the user's words gone")
	}
	if err := apply("input-empty", "input", nil); err == nil {
		t.Error("an empty input body was accepted; harness requires blocks for an input")
	}

	admitted := probe.recordedAdmitted()
	if len(admitted) != 2 {
		t.Fatalf("the applier saw %d admitted records, want 2 (the two well-formed creates); a refused body must never reach harness", len(admitted))
	}
	if n := len(admitted[0].Blocks); n != 2 {
		t.Errorf("a create's first message decoded to %d blocks, want 2: a product that concatenated or dropped one satisfies every substring check while losing exactly what a multi-block message is for", n)
	}
	if n := len(admitted[1].Blocks); n != 0 {
		t.Errorf("a bare create carried %d blocks, want none", n)
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

	runtime, probe := applyProbeTarget(t)
	err := runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
		CommandID:        sessionwire.CommandID("command-1"),
		RuntimeCommandID: mustUUIDForTest(t),
		Kind:             "carbon_no_such_kind",
		AttemptID:        "attempt-1",
	})
	var unsupported *harnessruntime.UnsupportedCommandError
	if !errors.As(err, &unsupported) {
		t.Fatalf("ApplyCommand with an unknown kind = %v, want *harnessruntime.UnsupportedCommandError", err)
	}
	if n := len(probe.recordedAdmitted()); n != 0 {
		t.Fatalf("the unknown kind reached the runtime's applier (%d records)", n)
	}
}

// TestCarbonCommandVocabularyIsTheReleasedOne pins the five kinds Factory admits
// against harness's own vocabulary, so this product cannot drift from the set
// Factory admits.
//
// It also pins the falsifier's kind: the string this package uses as an example of
// an unknown kind must be in NEITHER vocabulary. "restore" was an unknown kind until
// harness v0.36.0 named it and "gate_response" until v0.35.0 did, and a fixture on
// either went green the day the vocabulary widened while whatever it guarded was
// live.
func TestCarbonCommandVocabularyIsTheReleasedOne(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"create", "restore", "input", "interrupt", "gate_response"} {
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
		Kind:             "interrupt",
		AttemptID:        "attempt-1",
	})
	var unsupported *harnessruntime.UnsupportedCommandError
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "lease") {
		t.Fatalf("ApplyCommand under a released lease = %v, want *harnessruntime.UnsupportedCommandError naming the lease", err)
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
// Principal crosses on every kind and Metadata on create and input: Admitted is
// built field by field, and an omitted member is dropped in silence.
//
// The record is also validated on the RELEASED type's own rule, so a shape harness
// would refuse after the attempt is already durable is caught here instead.
func TestApplyCommandForwardsTheAttemptAndTheIdentities(t *testing.T) {
	t.Parallel()

	principal := sessionwire.Principal{
		Tenant:  sessionwire.TenantID("tenant-a"),
		Subject: sessionwire.SubjectID("user_alex"),
		Kind:    sessionwire.PrincipalKindActor,
	}
	envelope := func(kind string) sessionwire.CommandEnvelope {
		return sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: sessionwire.CommandID("command-" + kind)}
	}
	createBody := mustJSON(t, sessionwire.CreateRequest{
		CommandEnvelope: envelope("create"),
		SessionID:       sessionwire.SessionID("session-a"),
		AgentID:         CarbonAgentID,
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"first"}]`),
	})
	inputBody := mustJSON(t, sessionwire.InputRequest{
		CommandEnvelope: envelope("input"),
		SessionID:       sessionwire.SessionID("session-a"),
		Blocks:          mustBlocksJSON(t, `[{"Type":"text","Text":"more"}]`),
	})
	gateBody := mustJSON(t, sessionwire.GateResponseRequest{
		CommandEnvelope: envelope("gate_response"),
		SessionID:       sessionwire.SessionID("session-a"),
		GateID:          sessionwire.GateID(mustUUIDForTest(t).String()),
		Action:          "approve",
		Values:          map[string]json.RawMessage{},
		// Core requires EXACTLY ONE optimistic-open version, so an answer cannot be
		// applied to a different incarnation of the same gate id.
		ExpectedOpenJournalSeq: 3,
	})

	for _, tc := range []struct {
		kind       string
		payload    []byte
		wantKind   runtimecommand.Kind
		wantBlocks int
		wantAnswer bool
		metadata   sessionwire.MessageMetadata
	}{
		{"create", createBody, runtimecommand.KindCreate, 1, false, sessionwire.MessageMetadata{"space": "family"}},
		{"restore", nil, runtimecommand.KindRestore, 0, false, nil},
		{"input", inputBody, runtimecommand.KindInput, 1, false, sessionwire.MessageMetadata{"space": "family"}},
		{"interrupt", nil, runtimecommand.KindInterrupt, 0, false, nil},
		{"gate_response", gateBody, runtimecommand.KindGateResponse, 0, true, nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			runtime, probe := applyProbeTarget(t)
			epoch, held := runtime.LeaseEpoch()
			if !held || epoch == 0 {
				t.Fatalf("the runtime holds no journal grant (%d, %t)", epoch, held)
			}

			command := sessionwire.CommandID("command-" + tc.kind)
			runtimeCommand := mustUUIDForTest(t)
			attempt := "attempt-" + tc.kind

			if err := runtime.ApplyCommand(context.Background(), department.RuntimeCommand{
				CommandID:        command,
				RuntimeCommandID: runtimeCommand,
				Kind:             tc.kind,
				Payload:          tc.payload,
				AttemptID:        attempt,
				Principal:        &principal,
				Metadata:         tc.metadata,
			}); err != nil {
				t.Fatalf("ApplyCommand: %v", err)
			}
			admitted := probe.recordedAdmitted()
			if len(admitted) != 1 {
				t.Fatalf("the applier saw %d admitted records, want exactly 1", len(admitted))
			}
			got := admitted[0]

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
			if got.Principal == nil || *got.Principal != principal {
				t.Errorf("Principal = %+v, want %+v", got.Principal, principal)
			}
			if len(tc.metadata) > 0 {
				if got.Metadata["space"] != tc.metadata["space"] {
					t.Errorf("Metadata = %+v, want %+v", got.Metadata, tc.metadata)
				}
			} else if got.Metadata != nil {
				t.Errorf("Metadata = %+v on %q, want nil", got.Metadata, tc.kind)
			}
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
		Kind:             "input",
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

// TestCarbonTargetRefusesASessionThatCannotReportCommittedBytes holds the
// two-result capability's whole point, now at LAUNCH.
//
// A consumer must learn it is not one of those sessions BEFORE it starts persisting
// cursors, not after. harnessruntime refuses such a session at bind, so Host never
// publishes a residency route a client could attach a cursor to; Carbon's
// hand-written adapter refused only at the first subscription.
func TestCarbonTargetRefusesASessionThatCannotReportCommittedBytes(t *testing.T) {
	t.Parallel()

	fixture := newRealRigFixture(t)
	var probe *probeController
	target := mustCarbonTargetOver(t, fixture.wrappedLauncher(func(real session.SessionController) session.SessionController {
		probe = newProbe(real)
		return uncommittedProbe{probe}
	}), CarbonCompatibilityID(fixture.cfg))
	runtime, err := target.Create(context.Background(), department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
	})
	if runtime != nil {
		t.Fatal("a session with no committed stream was launched")
	}
	var incapable *harnessruntime.IncapableSessionError
	if !errors.As(err, &incapable) || !strings.Contains(strings.Join(incapable.Missing, ","), "committed public events") {
		t.Fatalf("Create = %v, want *harnessruntime.IncapableSessionError naming committed public events", err)
	}
	if _, held := probe.LeaseEpoch(); held {
		t.Fatal("the refused session still holds its journal lease")
	}
}

// TestCarbonRigSessionOptionsHonourAZeroIdentity pins the one rule a launcher must
// not get wrong, in the one place it is stated.
//
// rig.WithSessionID REFUSES a zero id — deliberately, unlike the internal option
// that ignores one — because silently substituting a minted id would put a name in a
// caller's immutable binding that resolves to nothing. A zero RigSessionID is
// nevertheless legitimate: it is a create for a session with no catalog record, and
// the rig mints the id. So the branch is made before harness sees it.
func TestCarbonRigSessionOptionsHonourAZeroIdentity(t *testing.T) {
	t.Parallel()

	if got := carbonRigSessionOptions(LaunchScope{}); len(got) != 0 {
		t.Errorf("a zero RigSessionID produced %d options, want none: rig.WithSessionID refuses a zero id", len(got))
	}
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	if got := carbonRigSessionOptions(LaunchScope{RigSessionID: id}); len(got) != 1 {
		t.Errorf("a named RigSessionID produced %d options, want exactly rig.WithSessionID", len(got))
	}
}

// singleRootLauncher is a launcher that makes no pooling claim, standing in for
// any composition that places every session over one workspace root.
type singleRootLauncher struct{}

func (singleRootLauncher) Launch(context.Context, LaunchScope) (session.SessionController, error) {
	return nil, errors.New("carbon test: single-root launcher does not launch")
}
