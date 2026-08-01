package app

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/operator"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
)

// swarm_test.go proves the three-loop managed-delegation topology: swarmDefinitions yields
// exactly [operator-primary, operator, reviewer]; only the primer declares delegates + managed
// delegation and displays as "operator"; the primer's tool policy and prompt identity match the
// operator leaf's (minus the primer-only delegation guidance) so they cannot drift; and the
// headless New path brings the primer up as the durable root loop.

// swarmDefs builds the three definitions with the fake client + test model under cfg.
func swarmDefs(t *testing.T, cfg Config) []loop.Definition {
	t.Helper()
	access, cfg := headlessTestAccess(t, cfg, t.TempDir())
	defs, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("swarmDefinitions() len = %d, want 3", len(defs))
	}
	return defs
}

// TestSwarmDefinitionsTopology proves the three definitions, their order and names, and that
// ONLY the operator-primary primer declares delegates + managed delegation; both leaves are
// delegate-free with the zero (sync-only) delegation.
func TestSwarmDefinitionsTopology(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	primer, operatorLeaf, reviewerLeaf := defs[0], defs[1], defs[2]

	tests := []struct {
		name          string
		def           loop.Definition
		wantName      identity.AgentName
		wantDelegates int
		wantManaged   bool
	}{
		{name: "primer", def: primer, wantName: operatorPrimaryName, wantDelegates: 2, wantManaged: true},
		{name: "operator leaf", def: operatorLeaf, wantName: operator.Name, wantDelegates: 0, wantManaged: false},
		{name: "reviewer leaf", def: reviewerLeaf, wantName: reviewer.Name, wantDelegates: 0, wantManaged: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.def.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
			if got := len(tt.def.Delegates()); got != tt.wantDelegates {
				t.Errorf("len(Delegates()) = %d, want %d", got, tt.wantDelegates)
			}
			managed := tt.def.Delegation().Style == loop.DelegationManaged
			if managed != tt.wantManaged {
				t.Errorf("Delegation managed = %v, want %v", managed, tt.wantManaged)
			}
		})
	}

	// The primer's delegates are exactly the two leaves.
	delegates := map[identity.AgentName]bool{}
	for _, d := range primer.Delegates() {
		delegates[d] = true
	}
	if !delegates[operator.Name] || !delegates[reviewer.Name] {
		t.Errorf("primer delegates = %v, want operator + reviewer", primer.Delegates())
	}
}

func TestSwarmDefinitionsUseModesAndEffort(t *testing.T) {
	t.Parallel()
	for _, definition := range swarmDefs(t, Config{}) {
		if got := definition.InitialMode(); got != initialCodingMode {
			t.Errorf("%s initial mode = %q, want %q", definition.Name(), got, initialCodingMode)
		}
		modes := definition.Modes()
		if len(modes) != 2 {
			t.Fatalf("%s modes = %d, want 2", definition.Name(), len(modes))
		}
		wantEffort := map[loop.ModeName]model.Effort{
			"quick": model.EffortLow,
			"deep":  model.EffortMax,
		}
		for _, mode := range modes {
			if got, ok := wantEffort[mode.Name]; !ok || got != mode.Effort {
				t.Errorf("%s mode %q effort = %q, want %q", definition.Name(), mode.Name, mode.Effort, got)
			}
		}
	}
}

// TestSwarmDefinitionsCompactionComposition proves every native loop carries the
// same complete context policy and the trusted summary-consumption fragment once.
func TestSwarmDefinitionsCompactionComposition(t *testing.T) {
	t.Parallel()

	client := &fakeLLM{}
	model := testModel()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	defs, err := swarmDefinitions(client, model, cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("swarmDefinitions() len = %d, want 3", len(defs))
	}
	wantCounter := contextcount.CounterCapability{
		Transport:    contextcount.CounterTransportLocal,
		Retention:    contextcount.RetentionNone,
		TokenizerRev: contextcount.EstimatorRevision,
		Quality:      contextcount.CountQualityHeuristicEstimate,
	}
	wantInference, err := inferenceCapabilityForModel(model)
	if err != nil {
		t.Fatalf("inferenceCapabilityForModel() error = %v", err)
	}

	tests := []struct {
		name string
		def  loop.Definition
	}{
		{name: "operator primer", def: defs[0]},
		{name: "operator leaf", def: defs[1]},
		{name: "reviewer leaf", def: defs[2]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := strings.Count(tt.def.FingerprintInitial().EffectiveSystem, conversationSummaryConsumptionFragment); got != 1 {
				t.Errorf("summary fragment count = %d, want 1", got)
			}
			policy, ok := tt.def.CompactionPolicy()
			if !ok {
				t.Fatal("CompactionPolicy() configured = false, want true")
			}
			if want := conversationCompactionPolicy(); policy != want {
				t.Errorf("CompactionPolicy() = %+v, want %+v", policy, want)
			}

			bound, bindErr := tt.def.Bind(context.Background(), tool.Bindings{
				SessionID: mustUUID(t),
				LoopID:    mustUUID(t),
				Workspace: &tool.WorkspaceBinding{
					Root:         root,
					Coordinator:  &testWorkspaceCoordinator{},
					Observations: tool.NewWorkspaceObservations(),
				},
			})
			if bindErr != nil {
				t.Fatalf("Bind() error = %v", bindErr)
			}
			if got, configured := bound.CounterCapability(); !configured || got != wantCounter {
				t.Errorf("CounterCapability() = %+v, %v, want %+v, true", got, configured, wantCounter)
			}
			if got, configured := bound.InferenceCapability(); !configured || got != wantInference {
				t.Errorf("InferenceCapability() = %+v, %v, want %+v, true", got, configured, wantInference)
			}
			if bound.ContextCounter() == nil {
				t.Error("ContextCounter() = nil, want fixed local estimator")
			}
			if bound.Client() != client {
				t.Errorf("Client() type = %T, want originating client", bound.Client())
			}
			if got := bound.Model(); got.Key() != model.Key() {
				t.Errorf("Model().Key() = %+v, want %+v", got.Key(), model.Key())
			}
		})
	}

	primerSystem := defs[0].FingerprintInitial().EffectiveSystem
	operatorSystem := defs[1].FingerprintInitial().EffectiveSystem
	if got := strings.Replace(primerSystem, operatorDelegation, "", 1); got != operatorSystem {
		t.Errorf("primer-minus-delegation system != operator leaf system:\nprimer(-deleg)=%q\nleaf=%q", got, operatorSystem)
	}
}

// TestSwarmDefinitionsAntiDrift proves the primer and operator leaf share one base tool policy
// and one prompt identity: the operator leaf's effective
// system equals the primer's with the primer-only operatorDelegation guidance removed. This
// is the guard that the two operator faces cannot silently diverge.
func TestSwarmDefinitionsAntiDrift(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	primer, operatorLeaf := defs[0], defs[1]

	// The primer and operator leaf are built from the SAME operator.BuildTools result, so
	// their declared tool sets are byte-identical (the managed Subagent is added
	// structurally by the rig at bind, not part of either definition's WithTools). Whole-
	// definition PolicyRevision necessarily differs (name, system, delegation), so the
	// no-drift signal is the identical declared tool-name set.
	primerTools := append([]string(nil), primer.FingerprintInitial().ToolNames...)
	leafTools := append([]string(nil), operatorLeaf.FingerprintInitial().ToolNames...)
	sort.Strings(primerTools)
	sort.Strings(leafTools)
	if !slices.Equal(primerTools, leafTools) {
		t.Errorf("primer tool set %v != operator leaf %v — tool policy drifted", primerTools, leafTools)
	}
	if primer.PolicyRevision() == operatorLeaf.PolicyRevision() {
		t.Error("primer PolicyRevision equals operator leaf, want managed Subagent capability recorded")
	}

	primerSys := primer.FingerprintInitial().EffectiveSystem
	leafSys := operatorLeaf.FingerprintInitial().EffectiveSystem

	if !strings.Contains(primerSys, operatorDelegation) {
		t.Error("primer system does not carry the operatorDelegation guidance")
	}
	if strings.Contains(leafSys, operatorDelegation) {
		t.Error("operator leaf system carries the primer-only operatorDelegation guidance, want absent")
	}
	if got := strings.Replace(primerSys, operatorDelegation, "", 1); got != leafSys {
		t.Errorf("primer-minus-delegation system != operator leaf system:\nprimer(-deleg)=%q\nleaf=%q", got, leafSys)
	}
	// Both carry the shared identity, operator role, and trusted code-style catalog.
	for _, want := range []string{`<identity product="CodeRig">`, `<role name="operator">`, "<available_skills>", "code-style"} {
		if !strings.Contains(leafSys, want) {
			t.Errorf("operator leaf system missing %q", want)
		}
	}
}

func TestSwarmTaskToolRoster(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	for _, definition := range defs[1:] {
		t.Run(string(definition.Name()), func(t *testing.T) {
			assertTaskToolRoster(t, definition.FingerprintInitial().ToolNames)
		})
	}
}

func assertTaskToolRoster(t *testing.T, names []string) {
	t.Helper()
	want := []string{"TaskCreate", "TaskGet", "TaskList", "TaskUpdate"}
	got := make([]string, 0, len(want))
	for _, name := range names {
		if strings.HasPrefix(name, "Task") {
			got = append(got, name)
		}
		if name == "Todo" {
			t.Errorf("tool roster contains removed Todo tool: %v", names)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("task tool roster = %v, want exactly %v", got, want)
	}
}

// TestReviewerLeafIsReadOnly proves the reviewer leaf carries no write/edit/Subagent tools:
// its tool policy differs from the operator's, and it declares no delegates.
func TestReviewerLeafIsReadOnly(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	operatorLeaf, reviewerLeaf := defs[1], defs[2]
	if reviewerLeaf.PolicyRevision() == operatorLeaf.PolicyRevision() {
		t.Error("reviewer PolicyRevision == operator's, want a distinct read-only policy")
	}
	names := map[string]bool{}
	for _, n := range reviewerLeaf.FingerprintInitial().ToolNames {
		names[n] = true
	}
	for _, forbidden := range []string{"WriteFile", "EditFile", "Subagent"} {
		if names[forbidden] {
			t.Errorf("reviewer leaf carries %q, want read-only critique tools only", forbidden)
		}
	}
}

// TestOperatorDelegationIsWellFormedXML proves the primer-only operatorDelegation fragment is a
// single well-formed <delegation> element (it is baked into the primer system prompt).
func TestOperatorDelegationIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"delegation"`
	}
	if err := xml.Unmarshal([]byte(operatorDelegation), &probe); err != nil {
		t.Fatalf("operatorDelegation is not well-formed XML: %v", err)
	}
	if probe.XMLName.Local != "delegation" {
		t.Errorf("operatorDelegation root element = %q, want %q", probe.XMLName.Local, "delegation")
	}
}

// TestOperatorSpawnCaps pins the delegation safety caps the rig enforces (Depth 2 admits the
// one structural level primary→leaf; Quota 64 bounds total spawns).
func TestOperatorSpawnCaps(t *testing.T) {
	t.Parallel()
	if operatorSpawnDepth != 2 {
		t.Errorf("operatorSpawnDepth = %d, want 2 (structural depth-1: primary → non-spawning leaf)", operatorSpawnDepth)
	}
	if operatorSpawnQuota != 64 {
		t.Errorf("operatorSpawnQuota = %d, want 64", operatorSpawnQuota)
	}
}

// TestNewWithClientBuildsRootPrimer proves the headless New path (via the fake-client seam)
// builds a usable tui.Agent whose durable root loop is the operator-primary, DISPLAYED as
// "operator", started first (zero-parent). It is serial (not t.Parallel) because the exclusive
// current-checkout placement means only one headless session can hold the root lease at a time;
// it Closes the agent so the next serial session test can acquire.
func TestNewWithClientBuildsRootPrimer(t *testing.T) {
	ctx := context.Background()
	agent, err := newWithClient(ctx, &fakeLLM{}, newModelFactory(), Config{})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	var _ tui.Agent = agent

	active := agent.ActiveLoopID()
	if active.IsZero() {
		t.Fatal("ActiveLoopID() is zero")
	}

	started := swarmFirstRootLoop(t, agent.SessionID())
	if got := string(started.AgentName); got != string(operatorPrimaryName) {
		t.Errorf("root loop AgentName = %q, want %q", got, operatorPrimaryName)
	}
	if started.DisplayName != string(operator.Name) {
		t.Errorf("root loop DisplayName = %q, want %q", started.DisplayName, operator.Name)
	}
	if started.Description != operator.Description {
		t.Errorf("root loop Description = %q, want operator.Description", started.Description)
	}
	if started.LoopID != active {
		t.Errorf("primer LoopStarted LoopID = %v, want active %v", started.LoopID, active)
	}
}

// swarmFirstRootLoop drains the durable log of the headless session and returns the first
// zero-parent LoopStarted (the active primer, started first).
func swarmFirstRootLoop(t *testing.T, sessionID uuid.UUID) event.LoopStarted {
	t.Helper()
	stores, err := headlessStores()
	if err != nil {
		t.Fatalf("headlessStores() error = %v", err)
	}
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			t.Fatal("no zero-parent LoopStarted in the durable log")
		}
		if err != nil {
			t.Fatalf("cursor.Next() error = %v", err)
		}
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
			return started
		}
	}
}
