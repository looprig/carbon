package app

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

func carbonDef(t *testing.T, cfg Config) loop.Definition {
	t.Helper()
	access, cfg := headlessTestAccess(t, cfg, t.TempDir())
	definition, err := carbonDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	return definition
}

func carbonTestDefinition(client inference.Client, model model.Model, cfg Config, access *sessionAccess) (loop.Definition, error) {
	return carbonDefinition(client, model, cfg, access, nil)
}

func carbonTestDefinitionWithContextPolicy(client inference.Client, model model.Model, cfg Config, contextPolicy conversationContextPolicy, access *sessionAccess) (loop.Definition, error) {
	return carbonDefinitionWithContextPolicy(client, model, cfg, contextPolicy, access, nil)
}

func carbonTestDefinitionWithAdditionalTools(client inference.Client, model model.Model, cfg Config, access *sessionAccess, extras []tool.Definition) (loop.Definition, error) {
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates, cfg.DelegateModels)
	if err != nil {
		return loop.Definition{}, err
	}
	return carbonDefinitionWithContextPolicy(client, model, cfg, contextPolicy, access, extras)
}

func TestCarbonDefinitionIsSoleManagedLoop(t *testing.T) {
	t.Parallel()
	definition := carbonDef(t, Config{})
	if got := definition.Name(); got != carbon.Name {
		t.Fatalf("Name() = %q, want %q", got, carbon.Name)
	}
	if got := definition.Description(); got != carbon.Description {
		t.Fatalf("Description() = %q, want Carbon description", got)
	}
	if got := definition.Delegates(); !slices.Equal(got, []identity.AgentName{carbon.Name}) {
		t.Fatalf("Delegates() = %v, want [%q]", got, carbon.Name)
	}
	if got := definition.Delegation().Style; got != loop.DelegationManaged {
		t.Fatalf("Delegation().Style = %q, want managed", got)
	}
	if definition.PolicyRevision() == "" {
		t.Fatal("PolicyRevision() is empty")
	}

	initial := definition.FingerprintInitial()
	if !strings.Contains(initial.EffectiveSystem, carbon.SystemPrompt) {
		t.Fatal("Carbon system prompt is not used directly")
	}
	if got := strings.Count(initial.EffectiveSystem, "<delegation>"); got != 1 {
		t.Fatalf("delegation section count = %d, want exactly 1", got)
	}
	if got := strings.Count(initial.EffectiveSystem, "<available_skills>"); got != 0 {
		t.Fatalf("embedded skill catalog count = %d, want 0", got)
	}
	for _, name := range []string{"ReadFile", "WriteFile", "EditFile", "Bash", "ProcessOutput", "ProcessInput", "ProcessStop", "WebSearch", "Fetch", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "AskUser", "Skill"} {
		if !slices.Contains(initial.ToolNames, name) {
			t.Errorf("Carbon tool roster missing %q: %v", name, initial.ToolNames)
		}
	}
	for _, name := range []string{"Glob", "Grep"} {
		if slices.Contains(initial.ToolNames, name) {
			t.Errorf("Carbon tool roster unexpectedly includes %q: %v", name, initial.ToolNames)
		}
	}
}

func TestCarbonDefinitionUsesQuickAndDeepModes(t *testing.T) {
	t.Parallel()
	definition := carbonDef(t, Config{})
	if got := definition.InitialMode(); got != initialCodingMode {
		t.Fatalf("initial mode = %q, want %q", got, initialCodingMode)
	}
	if got := definition.Modes(); len(got) != 2 {
		t.Fatalf("modes = %d, want 2", len(got))
	} else {
		want := map[loop.ModeName]model.Effort{"quick": model.EffortLow, "deep": model.EffortMax}
		for _, mode := range got {
			if want[mode.Name] != mode.Effort {
				t.Errorf("mode %q effort = %q, want %q", mode.Name, mode.Effort, want[mode.Name])
			}
		}
	}
}

func TestCarbonDefinitionCompactionComposition(t *testing.T) {
	t.Parallel()
	client := &fakeLLM{}
	model := testModel()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definition, err := carbonDefinition(client, model, cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	if got := strings.Count(definition.FingerprintInitial().EffectiveSystem, conversationSummaryConsumptionFragment); got != 1 {
		t.Fatalf("summary fragment count = %d, want 1", got)
	}
	policy, ok := definition.CompactionPolicy()
	if !ok || policy != conversationCompactionPolicy() {
		t.Fatalf("CompactionPolicy() = %+v, configured=%v, want Carbon policy", policy, ok)
	}
	bound, err := definition.Bind(context.Background(), sessionScopedBindingsFor(
		t, uuid.UUID{1}, uuid.UUID{2}, root,
		&fakeSessionResourceRegistry{dir: t.TempDir()},
	))
	if err != nil {
		t.Fatalf("definition.Bind() error = %v", err)
	}
	if got, want := bound.ToolLimits().ResultBytes, 50*1024; got != want {
		t.Errorf("assembled loop tool result limit = %d, want %d", got, want)
	}
	boundCompaction, ok := bound.CompactionPolicy()
	if !ok {
		t.Fatal("assembled loop compaction policy not configured")
	}
	if boundCompaction.KeepRecentSegments != 2 || boundCompaction.KeepRecentTokens != 8_000 {
		t.Errorf("assembled loop compaction keep values = segments %d, tokens %d, want 2, 8000", boundCompaction.KeepRecentSegments, boundCompaction.KeepRecentTokens)
	}
}

func TestNewSessionHasCarbonAsSoleActivePrimer(t *testing.T) {
	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()
	agent, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), Config{}, stores, root)
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	roots := sessionRootLoops(t, stores, agent.SessionID())
	if len(roots) != 1 {
		t.Fatalf("root loop count = %d, want 1 (%v)", len(roots), roots)
	}
	rootLoop, ok := roots[carbon.Name]
	if !ok {
		t.Fatalf("sole root loop = %v, want %q", roots, carbon.Name)
	}
	if rootLoop.DisplayName != carbon.DisplayName {
		t.Fatalf("root loop display name = %q, want %q", rootLoop.DisplayName, carbon.DisplayName)
	}
	if agent.ActiveLoopID() != rootLoop.LoopID {
		t.Fatalf("ActiveLoopID() = %v, want Carbon root %v", agent.ActiveLoopID(), rootLoop.LoopID)
	}
}

func sessionRootLoops(t *testing.T, stores *sessionStores, sessionID uuid.UUID) map[identity.AgentName]event.LoopStarted {
	t.Helper()
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
