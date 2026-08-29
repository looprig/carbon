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
	"github.com/looprig/tools/skill"
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

// TestCarbonDefinitionUsesModelDefaultEffort pins the modeless primer
// composition: Carbon declares no modes, so every request resolves the
// definition's implicit base mode, whose effort is the WithInference model's
// Sampling.Effort — the value models.json's default_effort normalizes into.
// Removing the explicit quick/deep modes is what makes the configured
// default_effort the session's actual effort.
func TestCarbonDefinitionUsesModelDefaultEffort(t *testing.T) {
	t.Parallel()
	definition := carbonDef(t, Config{})
	if got := definition.Modes(); len(got) != 0 {
		t.Fatalf("declared modes = %d (%v), want 0: primer effort must come from the model descriptor's default_effort", len(got), got)
	}
	if got := definition.InitialMode(); got != "" {
		t.Fatalf("initial mode = %q, want empty (no modes)", got)
	}
}

// TestCarbonDefinitionInitialFingerprintIsModelDefaultEffort verifies the
// definition resolves its initial request model from the WithInference
// descriptor, not a mode override: a model stamped with EffortMax (the value
// models.json's default_effort normalizes into) must drive the initial
// fingerprint. With no declared modes, FingerprintInitial returns the base
// model untouched — the same base mode Bind resolves every request against.
func TestCarbonDefinitionInitialFingerprintIsModelDefaultEffort(t *testing.T) {
	t.Parallel()
	client := &fakeLLM{}
	wantEffort := model.EffortMax
	stamped := testModel()
	stamped.Sampling.Effort = wantEffort
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	definition, err := carbonDefinition(client, stamped, cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	if got := definition.FingerprintInitial().Model.Sampling.Effort; got != wantEffort {
		t.Fatalf("FingerprintInitial().Model.Sampling.Effort = %q, want %q (the model's default_effort)", got, wantEffort)
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

// legacyModefulCarbonDefinition rebuilds the definition Carbon shipped BEFORE the
// primer modes were removed: carbonDefinitionWithContextPolicy's options plus the
// quick/deep modes and the "quick" initial mode it used to declare.
//
// It exists to persist a session the way the older build did, because a durable
// start mode is the only way into the migration path every already-existing session
// takes. It is deliberately a test fixture rather than a mode seam on production:
// the removed modes stay removed, and this copy is free to rot the moment the real
// definition stops being able to restore what the old one wrote.
func legacyModefulCarbonDefinition(client inference.Client, m model.Model, cfg Config, access *sessionAccess) (loop.Definition, error) {
	contextPolicy, err := newConversationContextPolicy(m, cfg.PrimerCandidates, cfg.DelegateModels)
	if err != nil {
		return loop.Definition{}, err
	}
	loader := skill.NewEmbeddedSkillLoader(nil, nil)
	options := []loop.Option{
		loop.WithName(carbon.Name),
		loop.WithDisplayName(carbon.DisplayName),
		loop.WithDescription(carbon.Description),
		loop.WithInference(client, m),
		loop.WithSystem(contextPolicy.system(carbon.SystemPrompt)),
		loop.WithTools(carbonToolDefinitions(access.set, newHTTPClient(), skillDefinitionFor(loader))...),
		loop.WithAccessGate(access.gate),
		loop.WithPolicyRevision(contextPolicy.policyRevision(access.policyRev + ":" + managedAgentToolsRevision)),
		loop.WithRuntimeContext(newRuntimeContextProvider(runtimeSkillCatalogForAccess(access))),
		loop.WithDelegates(carbon.Name),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
		loop.WithModes(
			loop.Mode{Name: "quick", Effort: model.EffortLow, Instructions: "Prefer the shortest safe path. Keep investigation narrow and verification focused."},
			loop.Mode{Name: "deep", Effort: model.EffortMax, Instructions: "Investigate broadly, challenge assumptions, and verify the result thoroughly."},
		),
		loop.WithInitialMode(loop.ModeName("quick")),
	}
	options = append(options, contextPolicy.options()...)
	return loop.Define(options...)
}

// TestRestoreOfASessionStartedInARemovedModeNamesTheMissingMode closes the migration
// gap left by dropping the quick/deep modes. Sessions the older build persisted carry
// a durable start mode of "quick" (harness stamps bound.InitialMode() onto LoopStarted
// and folds it back into RestoredState.Mode). An ordinary resume of one is refused by
// the config-fingerprint gate, which is fine and already clear. A resume with
// SessionSelector.AllowConfigMismatch bypasses that gate and reaches harness's
// exact-name mode resolution, which misses and fails with a bare
// "loop: bind: invalid_definition: quick" -- opaque about the session, the reason, and
// the way forward.
//
// The assertion is on the actionable replacement: a typed *RestoredModeError naming
// the session and the mode that is gone, saying this build defines no modes and that
// effort now comes from models.json, and telling the user to start a new session.
// Against the old opaque error every one of those checks fails.
func TestRestoreOfASessionStartedInARemovedModeNamesTheMissingMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	root := t.TempDir()

	access, cfg := headlessTestAccess(t, Config{}, root)
	legacy, err := legacyModefulCarbonDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("legacyModefulCarbonDefinition() error = %v", err)
	}
	if got := legacy.InitialMode(); got != loop.ModeName("quick") {
		t.Fatalf("legacy fixture initial mode = %q, want %q: the fixture must persist the mode the old build did", got, "quick")
	}
	seedRig, err := buildRig(legacy, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig(legacy) error = %v", err)
	}
	controller, err := seedRig.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession(legacy) error = %v", err)
	}
	sid := controller.SessionID()
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown(legacy) error = %v", err)
	}

	current, err := carbonTestDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	adapter, err := openSessionWithDefinition(ctx, current, cfg, stores, root, SessionSelector{Resume: sid, AllowConfigMismatch: true}, permissionReviewRegistration{})
	if err == nil {
		_ = adapter.Controller().Shutdown(ctx)
		t.Fatalf("resume of a session started in a removed mode succeeded, want a mode-migration failure")
	}
	var modeErr *RestoredModeError
	if !errors.As(err, &modeErr) {
		t.Fatalf("resume error = %v (%T), want *RestoredModeError", err, err)
	}
	if modeErr.SessionID != sid {
		t.Errorf("RestoredModeError.SessionID = %v, want %v", modeErr.SessionID, sid)
	}
	if modeErr.Mode != loop.ModeName("quick") {
		t.Errorf("RestoredModeError.Mode = %q, want %q", modeErr.Mode, "quick")
	}
	if len(modeErr.Declared) != 0 {
		t.Errorf("RestoredModeError.Declared = %v, want none: this build declares no modes", modeErr.Declared)
	}
	// The underlying harness failure stays reachable: this wraps the diagnosis, it
	// does not swallow it.
	var bindErr *loop.BindError
	if !errors.As(err, &bindErr) || bindErr.Name != "quick" {
		t.Errorf("wrapped cause = %v, want the harness *loop.BindError naming %q", errors.Unwrap(err), "quick")
	}
	message := modeErr.Error()
	for _, want := range []string{sid.String(), `"quick"`, "no longer defines", "declares no modes", "default_effort", "Start a new session"} {
		if !strings.Contains(message, want) {
			t.Errorf("RestoredModeError.Error() = %q, missing %q", message, want)
		}
	}
}

// TestClassifyRestoreErrorPassesThroughUnrelatedFailures pins the other half of the
// classification: only a bind failure naming a mode the definition does not declare
// becomes a RestoredModeError. A bind failure naming a mode the definition DOES
// declare is some other fault and must not be relabelled as a migration, and neither
// must an unrelated error or a nil.
func TestClassifyRestoreErrorPassesThroughUnrelatedFailures(t *testing.T) {
	t.Parallel()
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	modeful, err := legacyModefulCarbonDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("legacyModefulCarbonDefinition() error = %v", err)
	}
	sid := uuid.MustParse("11111111-1111-4111-8111-111111111111")

	if got := classifyRestoreError(nil, sid, modeful); got != nil {
		t.Errorf("classifyRestoreError(nil) = %v, want nil", got)
	}
	plain := errors.New("store unavailable")
	if got := classifyRestoreError(plain, sid, modeful); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("classifyRestoreError(unrelated) = %v, want it passed through", got)
	}
	declared := &loop.BindError{Kind: loop.BindInvalidDefinition, Name: "quick", Index: -1}
	var modeErr *RestoredModeError
	if got := classifyRestoreError(declared, sid, modeful); errors.As(got, &modeErr) {
		t.Errorf("classifyRestoreError(bind error naming a DECLARED mode) = %v, want it passed through", got)
	}
	nameless := &loop.BindError{Kind: loop.BindInvalidDefinition, Index: -1}
	if got := classifyRestoreError(nameless, sid, modeful); errors.As(got, &modeErr) {
		t.Errorf("classifyRestoreError(bind error naming no mode) = %v, want it passed through", got)
	}
}
