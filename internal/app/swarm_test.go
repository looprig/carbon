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

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
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
	"github.com/looprig/storage/memstore"
	"github.com/looprig/tui"
)

// swarm_test.go proves the three-role managed-delegation topology: planner,
// builder, and reviewer are all primers and legal managed agent targets;
// builder is the initially active primer; and each role keeps its own prompt,
// tool, and access posture.

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

func TestSwarmDefinitionsTopology(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	want := []identity.AgentName{planner.Name, builder.Name, reviewer.Name}

	for i, def := range defs {
		t.Run(string(want[i]), func(t *testing.T) {
			if got := def.Name(); got != want[i] {
				t.Errorf("Name() = %q, want %q", got, want[i])
			}
			if got := len(def.Delegates()); got != len(want) {
				t.Errorf("len(Delegates()) = %d, want %d", got, len(want))
			}
			delegates := map[identity.AgentName]bool{}
			for _, delegate := range def.Delegates() {
				delegates[delegate] = true
			}
			for _, name := range want {
				if !delegates[name] {
					t.Errorf("delegates = %v, missing %q", def.Delegates(), name)
				}
			}
			if got := def.Delegation().Style; got != loop.DelegationManaged {
				t.Errorf("Delegation style = %q, want managed", got)
			}
		})
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

func TestSwarmDefinitionsCompactionComposition(t *testing.T) {
	t.Parallel()

	client := &fakeLLM{}
	nativeModel := testModel()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	defs, err := swarmDefinitions(client, nativeModel, cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	wantCounter := contextcount.CounterCapability{
		Transport:    contextcount.CounterTransportLocal,
		Retention:    contextcount.RetentionNone,
		TokenizerRev: contextcount.EstimatorRevision,
		Quality:      contextcount.CountQualityHeuristicEstimate,
	}
	wantInference, err := inferenceCapabilityForModel(nativeModel)
	if err != nil {
		t.Fatalf("inferenceCapabilityForModel() error = %v", err)
	}

	for _, def := range defs {
		t.Run(string(def.Name()), func(t *testing.T) {
			if got := strings.Count(def.FingerprintInitial().EffectiveSystem, conversationSummaryConsumptionFragment); got != 1 {
				t.Errorf("summary fragment count = %d, want 1", got)
			}
			policy, ok := def.CompactionPolicy()
			if !ok {
				t.Fatal("CompactionPolicy() configured = false, want true")
			}
			if want := conversationCompactionPolicy(); policy != want {
				t.Errorf("CompactionPolicy() = %+v, want %+v", policy, want)
			}
			bound, bindErr := def.Bind(context.Background(), tool.Bindings{
				SessionID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
				LoopID:    uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
				Workspace: &tool.WorkspaceBinding{Root: root, Coordinator: &testWorkspaceCoordinator{}, Observations: tool.NewWorkspaceObservations()},
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
			if got := bound.Model(); got.Key() != nativeModel.Key() {
				t.Errorf("Model().Key() = %+v, want %+v", got.Key(), nativeModel.Key())
			}
		})
	}
}

func TestSwarmDefinitionsRolePromptsAndTools(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	for _, def := range defs {
		system := def.FingerprintInitial().EffectiveSystem
		if !strings.Contains(system, `<identity product="CodeRig">`) {
			t.Errorf("%s missing shared identity", def.Name())
		}
		if !strings.Contains(system, `<role name="`+string(def.Name())+`">`) {
			t.Errorf("%s missing role prompt", def.Name())
		}
		if !strings.Contains(system, delegationGuidance) {
			t.Errorf("%s missing managed delegation guidance", def.Name())
		}
		names := def.FingerprintInitial().ToolNames
		hasWrite := slices.Contains(names, "WriteFile") || slices.Contains(names, "EditFile")
		switch def.Name() {
		case planner.Name, reviewer.Name:
			if hasWrite {
				t.Errorf("%s has mutating tools: %v", def.Name(), names)
			}
		case builder.Name:
			if !slices.Contains(names, "WriteFile") || !slices.Contains(names, "EditFile") {
				t.Errorf("builder tools = %v, want WriteFile and EditFile", names)
			}
		}
	}
}

func TestSwarmTaskToolRoster(t *testing.T) {
	t.Parallel()
	for _, definition := range swarmDefs(t, Config{}) {
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

func TestDelegationGuidanceIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"delegation"`
	}
	if err := xml.Unmarshal([]byte(delegationGuidance), &probe); err != nil {
		t.Fatalf("delegationGuidance is not well-formed XML: %v", err)
	}
	if probe.XMLName.Local != "delegation" {
		t.Errorf("delegationGuidance root element = %q, want delegation", probe.XMLName.Local)
	}
}

func TestDelegationSpawnCaps(t *testing.T) {
	t.Parallel()
	if delegationSpawnDepth != 2 {
		t.Errorf("delegationSpawnDepth = %d, want 2", delegationSpawnDepth)
	}
	if delegationSpawnQuota != 64 {
		t.Errorf("delegationSpawnQuota = %d, want 64", delegationSpawnQuota)
	}
}

func TestNewWithClientBuildsBuilderAsActivePrimer(t *testing.T) {
	ctx := context.Background()
	agent, err := newWithClient(ctx, &fakeLLM{}, newModelFactory(), Config{})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	active := agent.ActiveLoopID()
	if active.IsZero() {
		t.Fatal("ActiveLoopID() is zero")
	}
	roots := swarmRootLoops(t, agent.SessionID())
	if len(roots) != 3 {
		t.Fatalf("root loop count = %d, want 3", len(roots))
	}
	for _, name := range []identity.AgentName{planner.Name, builder.Name, reviewer.Name} {
		if _, ok := roots[name]; !ok {
			t.Errorf("missing root primer %q", name)
		}
	}
	builderRoot := roots[builder.Name]
	if builderRoot.LoopID != active {
		t.Errorf("ActiveLoopID() = %v, want builder root %v", active, builderRoot.LoopID)
	}
	if builderRoot.DisplayName != string(builder.Name) {
		t.Errorf("builder root DisplayName = %q, want %q", builderRoot.DisplayName, builder.Name)
	}
	if builderRoot.Description != builder.Description {
		t.Errorf("builder root Description = %q, want %q", builderRoot.Description, builder.Description)
	}
}

func TestSessionPresentationSurfacesACPDiagnostics(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		ACPDiagnostics: []string{"acp: codex unavailable: no executable (set acp_launchers in models.json or set CODEX_ACP_EXECUTABLE)"},
	}
	agent, err := newWithClient(ctx, &fakeLLM{}, newModelFactory(), cfg)
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	presentation := agent.SessionPresentation()
	found := false
	for _, line := range presentation.PermissionDiagnostics {
		if strings.Contains(line, "acp: codex unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ACP diagnostic in session presentation, got %v", presentation.PermissionDiagnostics)
	}
}

func TestProductionModelsLoadExactlyOnceAndConfigureCurrentPrimer(t *testing.T) {
	ctx := context.Background()
	configured := testModel()
	configured.Name = "configured-primer-only"
	loadCalls := 0
	storeCalls := 0
	agent, err := newWithProductionModelsLoader(ctx, Config{}, func(string) (productionModels, error) {
		loadCalls++
		return productionModels{
			PrimerClient: &fakeLLM{}, PrimerModel: configured, PrimerAlias: "configured-primer-alias", PrimerEfforts: []model.Effort{model.EffortHigh}, ConfigRev: "model-config-rev",
		}, nil
	}, func() (*swarmStores, error) {
		storeCalls++
		return openStores(memstore.New())
	})
	if err != nil {
		t.Fatalf("newWithProductionModelsLoader() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })
	if loadCalls != 1 {
		t.Fatalf("production model loads = %d, want exactly 1", loadCalls)
	}
	if storeCalls != 1 {
		t.Fatalf("store opens = %d, want 1", storeCalls)
	}
	options, err := agent.LoopRuntimeOptions(ctx, agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Models) != 1 || options.Models[0].ID != "configured-primer-alias" || options.Models[0].Label != "configured-primer-alias" || options.Models[0].Description != "" {
		t.Fatalf("runtime models = %#v, want only configured primer alias", options.Models)
	}
	if len(options.Efforts) != 1 || options.Efforts[0].ID != tui.EffortID(model.EffortHigh) {
		t.Fatalf("runtime efforts = %#v, want only configured high effort", options.Efforts)
	}
	for _, mode := range []tui.ModeID{"quick", "deep"} {
		if err := agent.SetMode(ctx, agent.ActiveLoopID(), mode); err == nil {
			t.Fatalf("SetMode(%q) succeeded without an admitted effort", mode)
		}
	}
	if err := agent.SetEffort(ctx, agent.ActiveLoopID(), tui.EffortID(model.EffortLow)); err == nil {
		t.Fatal("SetEffort(low) succeeded for high-only configured primer")
	}
}

func TestProductionModelsMissingPrimerFailsBeforeStoreOpen(t *testing.T) {
	loadCalls := 0
	storeCalls := 0
	agent, err := newWithProductionModelsLoader(context.Background(), Config{}, func(string) (productionModels, error) {
		loadCalls++
		return productionModels{}, nil
	}, func() (*swarmStores, error) {
		storeCalls++
		return openStores(memstore.New())
	})
	if agent != nil {
		t.Fatalf("agent = %T, want nil", agent)
	}
	var capabilityErr *ModelConfigCapabilityError
	if !errors.As(err, &capabilityErr) {
		t.Fatalf("error = %v, want *ModelConfigCapabilityError", err)
	}
	if loadCalls != 1 {
		t.Fatalf("production model loads = %d, want exactly 1", loadCalls)
	}
	if storeCalls != 0 {
		t.Fatalf("store opens = %d, want 0", storeCalls)
	}
}

func TestProductionModelsLoaderFailureHappensBeforeStoreOpen(t *testing.T) {
	wantErr := modelConfigFailure("decode", errors.New("fixture invalid configuration"))
	storeCalls := 0
	agent, err := newWithProductionModelsLoader(context.Background(), Config{}, func(string) (productionModels, error) {
		return productionModels{}, wantErr
	}, func() (*swarmStores, error) {
		storeCalls++
		return openStores(memstore.New())
	})
	if agent != nil {
		t.Fatalf("agent = %T, want nil", agent)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want loader error %v", err, wantErr)
	}
	if storeCalls != 0 {
		t.Fatalf("store opens = %d, want 0", storeCalls)
	}
}

func swarmRootLoops(t *testing.T, sessionID uuid.UUID) map[identity.AgentName]event.LoopStarted {
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
	roots := map[identity.AgentName]event.LoopStarted{}
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return roots
		}
		if err != nil {
			t.Fatalf("cursor.Next() error = %v", err)
		}
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
			roots[started.AgentName] = started
		}
	}
}
