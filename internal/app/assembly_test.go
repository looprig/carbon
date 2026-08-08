package app

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
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

func genericDefs(t *testing.T, cfg Config) []loop.Definition {
	t.Helper()
	access, cfg := headlessTestAccess(t, cfg, t.TempDir())
	defs, err := genericTestDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("genericTestDefinitions() error = %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("genericTestDefinitions() len = %d, want 1", len(defs))
	}
	return defs
}

// genericTestDefinitions keeps older assembly-oriented tests concise while the
// production composition root remains a direct genericDefinition call.
func genericTestDefinitions(client inference.Client, model model.Model, cfg Config, access *sessionAccess) ([]loop.Definition, error) {
	definition, err := genericDefinition(client, model, cfg, access, nil)
	if err != nil {
		return nil, err
	}
	return []loop.Definition{definition}, nil
}

func genericTestDefinitionsWithContextPolicy(client inference.Client, model model.Model, cfg Config, contextPolicy conversationContextPolicy, access *sessionAccess) ([]loop.Definition, error) {
	definition, err := genericDefinitionWithContextPolicy(client, model, cfg, contextPolicy, access, nil)
	if err != nil {
		return nil, err
	}
	return []loop.Definition{definition}, nil
}

func genericTestDefinitionsWithAdditionalTools(client inference.Client, model model.Model, cfg Config, access *sessionAccess, extras []tool.Definition) ([]loop.Definition, error) {
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates, cfg.DelegateModels)
	if err != nil {
		return nil, err
	}
	definition, err := genericDefinitionWithContextPolicy(client, model, cfg, contextPolicy, access, extras)
	if err != nil {
		return nil, err
	}
	return []loop.Definition{definition}, nil
}

func TestGenericDefinitionIsSoleManagedLoop(t *testing.T) {
	t.Parallel()
	definition := genericDefs(t, Config{})[0]
	if got := definition.Name(); got != generic.Name {
		t.Fatalf("Name() = %q, want %q", got, generic.Name)
	}
	if got := definition.Description(); got != generic.Description {
		t.Fatalf("Description() = %q, want Generic description", got)
	}
	if got := definition.Delegates(); !slices.Equal(got, []identity.AgentName{generic.Name}) {
		t.Fatalf("Delegates() = %v, want [%q]", got, generic.Name)
	}
	if got := definition.Delegation().Style; got != loop.DelegationManaged {
		t.Fatalf("Delegation().Style = %q, want managed", got)
	}
	if definition.PolicyRevision() == "" {
		t.Fatal("PolicyRevision() is empty")
	}

	initial := definition.FingerprintInitial()
	if !strings.Contains(initial.EffectiveSystem, generic.SystemPrompt) {
		t.Fatal("Generic system prompt is not used directly")
	}
	if got := strings.Count(initial.EffectiveSystem, "<delegation>"); got != 1 {
		t.Fatalf("delegation section count = %d, want exactly 1", got)
	}
	if got := strings.Count(initial.EffectiveSystem, "<available_skills>"); got != 1 {
		t.Fatalf("skill catalog count = %d, want exactly 1", got)
	}
	for _, name := range []string{"ReadFile", "WriteFile", "EditFile", "Glob", "Grep", "Bash", "ProcessOutput", "ProcessInput", "ProcessStop", "WebSearch", "Fetch", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "AskUser", "Skill"} {
		if !slices.Contains(initial.ToolNames, name) {
			t.Errorf("Generic tool roster missing %q: %v", name, initial.ToolNames)
		}
	}
}

func TestGenericDefinitionUsesQuickAndDeepModes(t *testing.T) {
	t.Parallel()
	definition := genericDefs(t, Config{})[0]
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

func TestGenericDefinitionCompactionComposition(t *testing.T) {
	t.Parallel()
	client := &fakeLLM{}
	model := testModel()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definition, err := genericDefinition(client, model, cfg, access, nil)
	if err != nil {
		t.Fatalf("genericDefinition() error = %v", err)
	}
	if got := strings.Count(definition.FingerprintInitial().EffectiveSystem, conversationSummaryConsumptionFragment); got != 1 {
		t.Fatalf("summary fragment count = %d, want 1", got)
	}
	policy, ok := definition.CompactionPolicy()
	if !ok || policy != conversationCompactionPolicy() {
		t.Fatalf("CompactionPolicy() = %+v, configured=%v, want CodeRig policy", policy, ok)
	}
}

func TestNewSessionHasGenericAsSoleActivePrimer(t *testing.T) {
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
	rootLoop, ok := roots[generic.Name]
	if !ok {
		t.Fatalf("sole root loop = %v, want %q", roots, generic.Name)
	}
	if agent.ActiveLoopID() != rootLoop.LoopID {
		t.Fatalf("ActiveLoopID() = %v, want Generic root %v", agent.ActiveLoopID(), rootLoop.LoopID)
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
