package app

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

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
func (f *realRigFixture) launcher() SessionLauncher {
	return launcherFunc(func(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
		if scope.Restore {
			return f.rig.RestoreSession(ctx, scope.RigSessionID)
		}
		return f.rig.NewSession(ctx, carbonRigSessionOptions(scope)...)
	})
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

// TestCarbonCapabilitiesAreAValidPooledDeclaration proves the declaration Carbon
// makes about its own placement is one department.New will accept, and that its
// capture safety actually permits pooling.
//
// The capture-safety row is the one that can go quietly wrong. PoolingPermitted
// lists the SAFE values and defaults everything else to unsafe, so a typo, a zero
// value, or a future Core constant all land on the unsafe side — and a target that
// declared SupportsPooled while carrying an unsafe capture value is DEDICATED-ONLY
// regardless of what it says. That would silently halve a pool's utility with no
// error anywhere.
func TestCarbonCapabilitiesAreAValidPooledDeclaration(t *testing.T) {
	t.Parallel()

	capabilities := carbonCapabilities()
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("carbonCapabilities().Validate() = %v, want nil", err)
	}
	if !capabilities.PoolingPermitted() {
		t.Errorf("PoolingPermitted() = false; Carbon declares SupportsPooled but its capture safety %q forbids it",
			capabilities.CaptureSafety)
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
	placements := capabilities.PermittedPlacements()
	if len(placements) != 2 {
		t.Errorf("PermittedPlacements() = %v, want both pooled and dedicated", placements)
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
	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, carbonKindInput, strandedAttempt, predecessorEpoch); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}

	// A REDELIVERED recovery is not a second tombstone. Host may re-offer the
	// closure after a crash between the write and the acknowledgement, and a second
	// refusal here would put the session right back where it started.
	if err := closer.CloseAttempt(ctx, strandedCommand, strandedRuntimeCommand, carbonKindInput, strandedAttempt, predecessorEpoch); err != nil {
		t.Fatalf("CloseAttempt (redelivered): %v, want the original closure replayed", err)
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
}

// TestCloseAttemptRefusesAClosureThatCannotNameAnAttempt proves the adapter
// validates on the RELEASED type's own rule before reaching the runtime.
//
// A closure that cannot name an attempt is one that could tombstone the wrong
// command, and a tombstone over a committed effect is the one error this protocol
// cannot recover from. The unknown kind used here is in NEITHER vocabulary and never
// has been: "restore" and "gate_response" were each an unknown kind once, and a
// fixture built on either went green the day the vocabulary widened.
func TestCloseAttemptRefusesAClosureThatCannotNameAnAttempt(t *testing.T) {
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
	closer := runtime.(department.AttemptCloser)

	for _, tc := range []struct {
		name    string
		kind    string
		attempt string
	}{
		{"no attempt id", carbonKindInput, ""},
		{"a kind neither vocabulary has ever held", "carbon_no_such_kind", "attempt-1"},
	} {
		if err := closer.CloseAttempt(context.Background(),
			sessionwire.CommandID("command-1"), mustUUIDForTest(t), tc.kind, tc.attempt, 1); err == nil {
			t.Errorf("CloseAttempt with %s = nil error, want a refusal", tc.name)
		}
	}
}

// closerlessController is a session.SessionController stand-in that offers neither
// the applier nor the closer. It is a DISTINCT TYPE rather than a flag on a fake,
// because Go method sets are not conditional: a single struct with a nil func field
// would satisfy every assertion and the discovery would never be exercised.
type closerlessController struct{ session.SessionController }

// recordingCloserController offers a closure that RECORDS and always succeeds. It
// exists for one assertion that nothing else can make: that a malformed closure is
// refused BEFORE the runtime is touched.
type recordingCloserController struct {
	session.SessionController
	calls int
}

func (c *recordingCloserController) CloseAttempt(context.Context, runtimecommand.Closure) (runtimecommand.ClosureResult, error) {
	c.calls++
	return runtimecommand.ClosureResult{Appended: true, Sequence: 1}, nil
}

// TestCloseAttemptValidatesBeforeTouchingTheRuntime is the falsifier for the
// adapter's own Closure.Validate call, and it needs a permissive runtime to make.
//
// harness's closer validates too, so against a real session a malformed closure errs
// either way and a test asserting only "an error happened" cannot tell which layer
// refused. The distinction is not academic: a tombstone is the one record that must
// never be written on a caller's say-so, and an adapter that forwarded an
// unvalidated closure would be relying on a guard in another module to hold a rule
// this one states. Here the runtime would ACCEPT it, so only the adapter's own check
// can produce the refusal.
func TestCloseAttemptValidatesBeforeTouchingTheRuntime(t *testing.T) {
	t.Parallel()

	controller := &recordingCloserController{}
	runtime := &carbonRuntime{controller: controller}

	if err := runtime.CloseAttempt(context.Background(),
		sessionwire.CommandID("command-1"), mustUUIDForTest(t), carbonKindInput, "", 1); err == nil {
		t.Fatal("a closure naming no attempt was accepted; it could tombstone the wrong command")
	}
	if controller.calls != 0 {
		t.Errorf("the runtime's closer was called %d times for a malformed closure, want 0: the adapter must refuse before the runtime is touched", controller.calls)
	}

	if err := runtime.CloseAttempt(context.Background(),
		sessionwire.CommandID("command-1"), mustUUIDForTest(t), carbonKindInput, "attempt-1", 1); err != nil {
		t.Fatalf("a well-formed closure was refused: %v", err)
	}
	if controller.calls != 1 {
		t.Errorf("the runtime's closer was called %d times for a well-formed closure, want 1", controller.calls)
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
