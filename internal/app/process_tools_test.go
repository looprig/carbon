package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools"
)

// process_tools_test.go covers the Generic session-supervised Bash definition
// plus its three argument-free process companions.

// fakeSessionResourceRegistry is a tool.SessionResourceRegistry test double
// mirroring the tools module's own fakeProcessRegistry (tools/
// definitions_test.go): the factory runs at most once, and every caller
// afterward -- regardless of build order -- receives the SAME resource,
// matching Harness's real per-session registry's get-or-create semantics.
// factoryCalls counts how many times the factory actually ran.
type fakeSessionResourceRegistry struct {
	dir string

	mu           sync.Mutex
	resource     tool.SessionResource
	factoryCalls int
	err          error
}

func (r *fakeSessionResourceRegistry) GetOrCreate(_ context.Context, _ string, factory func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if r.resource == nil {
		r.factoryCalls++
		resource, err := factory(r.dir)
		if err != nil {
			return nil, err
		}
		r.resource = resource
	}
	return r.resource, nil
}

var _ tool.SessionResourceRegistry = (*fakeSessionResourceRegistry)(nil)

// lifetimeCoordinator is a minimal tool.WorkspaceCoordinator AND
// tool.WorkspaceLifetimeCoordinator test double: Acquire/AcquireLifetime
// both grant an always-releasable permit. A supervised Bash call's
// runSupervised (bash/supervised.go) requires a coordinator that also
// implements WorkspaceLifetimeCoordinator -- toolsets_hostreads_test.go's
// noopCoordinator does not, so process-services tests need this instead.
type lifetimeCoordinator struct{}

func (lifetimeCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return lifetimePermit{}, nil
}
func (lifetimeCoordinator) Healthy() error { return nil }
func (lifetimeCoordinator) AcquireLifetime(context.Context, tool.WorkspaceAccess) (tool.WorkspacePermit, error) {
	return lifetimePermit{}, nil
}

type lifetimePermit struct{}

func (lifetimePermit) Release() {}

var (
	_ tool.WorkspaceCoordinator         = lifetimeCoordinator{}
	_ tool.WorkspaceLifetimeCoordinator = lifetimeCoordinator{}
)

// processBindingsFor builds real tool.Bindings for one loop within its own
// fresh session, carrying a Process binding over registry and a workspace
// bound at root with lifetimeCoordinator.
func processBindingsFor(t *testing.T, loopID uuid.UUID, root string, registry tool.SessionResourceRegistry) tool.Bindings {
	t.Helper()
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (session): %v", err)
	}
	return sessionScopedBindingsFor(t, sessionID, loopID, root, registry)
}

// sessionScopedBindingsFor is processBindingsFor's sibling for tests that
// need several loops sharing ONE session (matching how Harness hands every
// loop in a session the identical *tool.ProcessBinding).
func sessionScopedBindingsFor(t *testing.T, sessionID, loopID uuid.UUID, root string, registry tool.SessionResourceRegistry) tool.Bindings {
	t.Helper()
	return tool.Bindings{
		SessionID: sessionID,
		LoopID:    loopID,
		Workspace: &tool.WorkspaceBinding{
			Root:         root,
			Observations: tool.NewWorkspaceObservations(),
			Coordinator:  lifetimeCoordinator{},
		},
		Process: &tool.ProcessBinding{Registry: registry},
	}
}

// mustExecutorSetWithCapacity mirrors toolsets_hostreads_test.go's
// mustExecutorSet but under AccessTrusted with a caller-chosen executor
// capacity, for tests that resolve more than one Loop ID against the same
// set (mustExecutorSet's own cap of 1 executor is too small for those).
func mustExecutorSetWithCapacity(t *testing.T, root string, max int) *sandbox.ExecutorSet {
	t.Helper()
	profile, err := coderigProfile(AccessTrusted, root)
	if err != nil {
		t.Fatalf("coderigProfile: %v", err)
	}
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(max))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// mustUnconfinedExecutorSet builds a real *sandbox.ExecutorSet under
// AccessUnconfined (sandbox.Isolation = Unconfined), the one profile that
// can actually SPAWN a session-supervised background process on this
// platform today: sandbox's own Seatbelt-confined Darwin backend rejects
// Start with ErrLifetimeContainmentUnavailable for every OTHER (sandboxed)
// profile, since no kernel-enforced process-tree teardown proof is
// available there yet (process_adapter.go's mapStartError doc comment).
// Tests that only resolve/compare *sandbox.Executor identities, never
// actually spawn a command, use the ordinary mustExecutorSet instead.
func mustUnconfinedExecutorSet(t *testing.T, root string) *sandbox.ExecutorSet {
	t.Helper()
	profile, err := coderigProfile(AccessUnconfined, root)
	if err != nil {
		t.Fatalf("coderigProfile: %v", err)
	}
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(4))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	return set
}

func countDefinitionsByName(defs []tool.Definition, name string) int {
	n := 0
	for _, d := range defs {
		if d.Name() == name {
			n++
		}
	}
	return n
}

func findDefinitionByName(t *testing.T, defs []tool.Definition, name string) tool.Definition {
	t.Helper()
	for _, d := range defs {
		if d.Name() == name {
			return d
		}
	}
	t.Fatalf("no definition named %q in roster", name)
	return nil
}

// TestProcessToolsGenericRosterContainsBashAndProcessTrioExactlyOnce proves
// Generic carries Bash, ProcessOutput, ProcessInput, and ProcessStop exactly
// once, and Bash declares both workspace and process requirements.
func TestProcessToolsRostersContainBashAndProcessTrioExactlyOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	cases := []struct {
		name string
		defs []tool.Definition
	}{
		{name: "generic", defs: genericToolDefinitions(set, nil, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"Bash", "ProcessOutput", "ProcessInput", "ProcessStop"} {
				if got := countDefinitionsByName(tc.defs, name); got != 1 {
					t.Errorf("%s roster contains %d %q definitions, want exactly 1", tc.name, got, name)
				}
			}
			bashDef := findDefinitionByName(t, tc.defs, "Bash")
			want := tool.RequiresWorkspace | tool.RequiresProcessServices
			if got := bashDef.Requirements(); got != want {
				t.Errorf("%s Bash.Requirements() = %v, want %v", tc.name, got, want)
			}
			for _, name := range []string{"ProcessOutput", "ProcessInput", "ProcessStop"} {
				def := findDefinitionByName(t, tc.defs, name)
				if got := def.Requirements(); got != tool.RequiresProcessServices {
					t.Errorf("%s %s.Requirements() = %v, want %v", tc.name, name, got, tool.RequiresProcessServices)
				}
			}
		})
	}
}

// TestProcessToolsBashDefinitionResolverReceivesExactLoopIDAndSharesExecutor proves: at
// Build, bashDefinition's captured resolver is invoked with EXACTLY
// bindings.LoopID, exactly once, and the *sandbox.Executor it resolves
// (via newProcessRunnerResolver's own set.For(loopID.String()) call) is
// pointer-identical to a DIRECT set.For(loopID.String()) call -- the same
// memoized lookup bashDefinition's own synchronous foreground path and
// accessGate (access_acceptance_test.go's
// TestAcceptanceProcessEnabledBashSharesExecutorAcrossGateAndBuild) both
// make against the SAME set for the SAME LoopID. A second, distinct LoopID
// resolves a distinct executor, proving genuine per-loop resolution.
func TestProcessToolsBashDefinitionResolverReceivesExactLoopIDAndSharesExecutor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSetWithCapacity(t, root, 2)

	var (
		mu     sync.Mutex
		calls  int
		gotIDs []uuid.UUID
	)
	spy := func(ctx context.Context, loopID uuid.UUID) (tool.AsyncProcessRunner, error) {
		mu.Lock()
		calls++
		gotIDs = append(gotIDs, loopID)
		mu.Unlock()
		return newProcessRunnerResolver(set)(ctx, loopID)
	}

	definition := bashDefinition(set, spy)

	loopA, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	if _, err := definition.Build(context.Background(), processBindingsFor(t, loopA, root, &fakeSessionResourceRegistry{dir: t.TempDir()})); err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("resolver called %d times, want 1", calls)
	}
	if gotIDs[0] != loopA {
		mu.Unlock()
		t.Fatalf("resolver received LoopID %v, want %v", gotIDs[0], loopA)
	}
	mu.Unlock()

	wantA, err := set.For(loopA.String())
	if err != nil {
		t.Fatalf("set.For(loopA): %v", err)
	}
	runnerA, err := newProcessRunnerResolver(set)(context.Background(), loopA)
	if err != nil {
		t.Fatalf("newProcessRunnerResolver()(loopA): %v", err)
	}
	adapterA, ok := runnerA.(processRunnerAdapter)
	if !ok {
		t.Fatalf("runner type = %T, want processRunnerAdapter", runnerA)
	}
	if adapterA.exec != wantA {
		t.Fatal("resolver's executor is not the same *sandbox.Executor as a direct set.For() lookup for loopA")
	}

	// A second, distinct Loop ID resolves a DIFFERENT executor -- proving
	// this is genuine per-loop resolution, not a single memoized value that
	// happens to match by coincidence.
	loopB, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	if _, err := definition.Build(context.Background(), processBindingsFor(t, loopB, root, &fakeSessionResourceRegistry{dir: t.TempDir()})); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	mu.Lock()
	if calls != 2 || gotIDs[1] != loopB {
		mu.Unlock()
		t.Fatalf("resolver calls/IDs after second Build = %d/%v, want 2/[.. %v]", calls, gotIDs, loopB)
	}
	mu.Unlock()
	runnerB, err := newProcessRunnerResolver(set)(context.Background(), loopB)
	if err != nil {
		t.Fatalf("newProcessRunnerResolver()(loopB): %v", err)
	}
	adapterB := runnerB.(processRunnerAdapter)
	if adapterB.exec == adapterA.exec {
		t.Fatal("distinct Loop IDs resolved the SAME executor, want distinct per-Loop executors")
	}
}

// TestProcessToolsBashDefinitionRejectsResolverFailuresWithoutProducingBash covers the
// three ways bashDefinition's Build must abort before producing a tool: a
// nil resolver, a resolver returning an error, and a resolver returning a
// nil runner with no error.
func TestProcessToolsBashDefinitionRejectsResolverFailuresWithoutProducingBash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}

	t.Run("nil resolver", func(t *testing.T) {
		t.Parallel()
		definition := bashDefinition(set, nil)
		built, err := definition.Build(context.Background(), processBindingsFor(t, loopID, root, &fakeSessionResourceRegistry{dir: t.TempDir()}))
		if built != nil {
			t.Fatalf("Build() tools = %v, want nil", built)
		}
		var buildErr *tools.DefinitionBuildError
		if !errors.As(err, &buildErr) || buildErr.Dependency != "resolver" {
			t.Fatalf("Build() error = %v, want *tools.DefinitionBuildError{Dependency: \"resolver\"}", err)
		}
	})

	t.Run("resolver returns an error", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("resolver boom")
		calls := 0
		resolver := func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
			calls++
			return nil, wantErr
		}
		definition := bashDefinition(set, resolver)
		built, err := definition.Build(context.Background(), processBindingsFor(t, loopID, root, &fakeSessionResourceRegistry{dir: t.TempDir()}))
		if built != nil {
			t.Fatalf("Build() tools = %v, want nil", built)
		}
		var buildErr *tools.DefinitionBuildError
		if !errors.As(err, &buildErr) || buildErr.Dependency != "runner" {
			t.Fatalf("Build() error = %v, want *tools.DefinitionBuildError{Dependency: \"runner\"}", err)
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("Build() error does not wrap the resolver error: %v", err)
		}
		if calls != 1 {
			t.Fatalf("resolver called %d times, want 1 (no retry after failure)", calls)
		}
	})

	t.Run("resolver returns a nil runner with no error", func(t *testing.T) {
		t.Parallel()
		resolver := func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
			return nil, nil
		}
		definition := bashDefinition(set, resolver)
		built, err := definition.Build(context.Background(), processBindingsFor(t, loopID, root, &fakeSessionResourceRegistry{dir: t.TempDir()}))
		if built != nil {
			t.Fatalf("Build() tools = %v, want nil", built)
		}
		if err == nil {
			t.Fatal("Build() error = nil, want a nil-runner rejection")
		}
	})
}

// TestProcessToolsBashDefinitionRejectsZeroLoopIDBeforeResolverInvoked proves zero/missing
// LoopID is rejected by Harness's OWN Definition.Build binding validation
// BEFORE bashDefinition's factory (and so its resolver) ever runs.
func TestProcessToolsBashDefinitionRejectsZeroLoopIDBeforeResolverInvoked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	calls := 0
	resolver := func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
		calls++
		return nil, errors.New("must never be called")
	}
	definition := bashDefinition(set, resolver)

	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	bindings := tool.Bindings{
		SessionID: sessionID,
		// LoopID intentionally left zero.
		Workspace: &tool.WorkspaceBinding{Root: root, Observations: tool.NewWorkspaceObservations(), Coordinator: lifetimeCoordinator{}},
		Process:   &tool.ProcessBinding{Registry: &fakeSessionResourceRegistry{dir: t.TempDir()}},
	}
	built, err := definition.Build(context.Background(), bindings)
	if built != nil {
		t.Fatalf("Build() tools = %v, want nil", built)
	}
	if err == nil {
		t.Fatal("Build() error = nil, want Harness's zero-LoopID rejection")
	}
	var invalid *tool.InvalidBindingsError
	if !errors.As(err, &invalid) {
		t.Fatalf("Build() error = %T %v, want *tool.InvalidBindingsError (Harness's own binding validation, not coderig's)", err, err)
	}
	if calls != 0 {
		t.Fatalf("resolver called %d times, want 0 -- Harness must reject before the factory/resolver ever runs", calls)
	}
}

// TestProcessToolsCompanionDefinitionsShareSessionRegistryRegardlessOfBuildOrder
// proves ProcessOutput/Input/Stop carry no resolver or runner (their Go
// signature is argument-free) and may create the shared, runner-free
// process.Supervisor session resource FIRST, before Bash's own Build ever
// runs -- all four still resolve the SAME single registry entry.
func TestProcessToolsCompanionDefinitionsShareSessionRegistryRegardlessOfBuildOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := &fakeSessionResourceRegistry{dir: t.TempDir()}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New(): %v", err)
	}
	bindings := processBindingsFor(t, loopID, root, registry)

	for _, def := range []tool.Definition{tools.ProcessOutputDefinition(), tools.ProcessInputDefinition(), tools.ProcessStopDefinition()} {
		if _, err := def.Build(context.Background(), bindings); err != nil {
			t.Fatalf("%s.Build() error = %v", def.Name(), err)
		}
	}

	set := mustExecutorSet(t, root)
	bashDef := bashDefinition(set, newProcessRunnerResolver(set))
	if _, err := bashDef.Build(context.Background(), bindings); err != nil {
		t.Fatalf("Bash.Build() error = %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factoryCalls != 1 {
		t.Fatalf("supervisor factory invoked %d times across 4 separately built definitions, want 1", registry.factoryCalls)
	}
}

// TestProcessToolsHarnessBindingSharesSessionRegistryAcrossSiblingLoops proves
// two sibling loops in the SAME session -- carrying the identical
// *tool.ProcessBinding, exactly as Harness's own session.go hands every
// loop's Bindings the SAME *sessionResources -- resolve the SAME shared
// supervisor registry entry.
func TestProcessToolsHarnessBindingSharesSessionRegistryAcrossSiblingLoops(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := &fakeSessionResourceRegistry{dir: t.TempDir()}
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (session): %v", err)
	}
	loopA, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (loopA): %v", err)
	}
	loopB, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (loopB): %v", err)
	}

	if _, err := tools.ProcessOutputDefinition().Build(context.Background(), sessionScopedBindingsFor(t, sessionID, loopA, root, registry)); err != nil {
		t.Fatalf("loopA ProcessOutput.Build() error = %v", err)
	}
	if _, err := tools.ProcessOutputDefinition().Build(context.Background(), sessionScopedBindingsFor(t, sessionID, loopB, root, registry)); err != nil {
		t.Fatalf("loopB ProcessOutput.Build() error = %v", err)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factoryCalls != 1 {
		t.Fatalf("supervisor factory invoked %d times across 2 sibling loops in one session, want 1 (one shared session registry)", registry.factoryCalls)
	}
}

// toolResultText extracts the single text block a Bash/ProcessOutput result
// always renders (bash/result.go's renderSupervisedResult and process/
// output_tool.go's renderProcessOutputResults both build *tool.ToolResult
// via tool.TextResult, one *content.TextBlock).
func toolResultText(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("tool result = %+v, want exactly one content block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("tool result block = %T, want *content.TextBlock", result.Content[0])
	}
	return block.Text
}

// runPreparedCall drives one PrepareCall+InvokableRun round trip through
// built, matching bash/supervised_test.go's own established pattern.
func runPreparedCall(t *testing.T, built tool.InvokableTool, argsJSON string) *tool.ToolResult {
	t.Helper()
	preparer, ok := built.(tool.CallPreparer)
	if !ok {
		t.Fatalf("built tool %T does not implement tool.CallPreparer", built)
	}
	execID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (execution): %v", err)
	}
	req, artifact, err := preparer.PrepareCall(context.Background(), execID, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: execID, Request: req, Artifact: artifact})
	result, err := built.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() returned a Go error %v; this tool never returns one", err)
	}
	return result
}

// TestProcessToolsSiblingLoopsCannotAccessEachOthersHandles is the real,
// end-to-end proof behind "sibling loops cannot access one another's
// handles": loopA starts a real background command through the SAME
// bashDefinition Task 27 wires into the roster, backed by a real
// *sandbox.Executor (mustUnconfinedExecutorSet -- command execution is
// sandbox.Allow under every selected profile including Unconfined, so this
// needs no gate/grant round trip; Unconfined specifically is required to
// actually spawn a supervised background process on this platform today,
// see mustUnconfinedExecutorSet's own doc comment). LoopB, a sibling loop in
// the identical session, queries that process through its OWN ProcessOutput
// and gets "not_found" -- process/output_tool.go's own documented behavior,
// since ownership (SessionID + LoopID) is immutable and a cross-owner
// handle renders identically to a missing one. LoopA's own ProcessOutput
// finds it.
func TestProcessToolsSiblingLoopsCannotAccessEachOthersHandles(t *testing.T) {
	root := t.TempDir()
	set := mustUnconfinedExecutorSet(t, root)
	resolver := newProcessRunnerResolver(set)
	registry := &fakeSessionResourceRegistry{dir: t.TempDir()}

	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (session): %v", err)
	}
	loopA, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (loopA): %v", err)
	}
	loopB, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (loopB): %v", err)
	}

	bashDef := bashDefinition(set, resolver)
	builtBash, err := bashDef.Build(context.Background(), sessionScopedBindingsFor(t, sessionID, loopA, root, registry))
	if err != nil {
		t.Fatalf("Bash.Build() error = %v", err)
	}
	if len(builtBash) != 1 {
		t.Fatalf("Bash.Build() returned %d tools, want 1", len(builtBash))
	}

	result := runPreparedCall(t, builtBash[0], `{"command": "echo ready", "background": true}`)
	var started struct {
		Status    string `json:"status"`
		ProcessID string `json:"process_id"`
		Error     string `json:"error"`
	}
	text := toolResultText(t, result)
	if err := json.Unmarshal([]byte(text), &started); err != nil {
		t.Fatalf("unmarshal Bash background result %q: %v", text, err)
	}
	if started.Error != "" || started.ProcessID == "" {
		t.Fatalf("Bash background result = %+v, want a process_id and no error", started)
	}

	outputB, err := tools.ProcessOutputDefinition().Build(context.Background(), sessionScopedBindingsFor(t, sessionID, loopB, root, registry))
	if err != nil {
		t.Fatalf("loopB ProcessOutput.Build() error = %v", err)
	}
	queryArgs := `{"process_id": "` + started.ProcessID + `"}`
	resultB := runPreparedCall(t, outputB[0], queryArgs)
	var queriedB struct {
		ProcessID string `json:"process_id"`
		Status    string `json:"status"`
		Error     string `json:"error"`
	}
	textB := toolResultText(t, resultB)
	if err := json.Unmarshal([]byte(textB), &queriedB); err != nil {
		t.Fatalf("unmarshal loopB ProcessOutput result %q: %v", textB, err)
	}
	if queriedB.Error != "not_found" {
		t.Fatalf("loopB (sibling loop) ProcessOutput result = %+v, want error \"not_found\"", queriedB)
	}

	outputA, err := tools.ProcessOutputDefinition().Build(context.Background(), sessionScopedBindingsFor(t, sessionID, loopA, root, registry))
	if err != nil {
		t.Fatalf("loopA ProcessOutput.Build() error = %v", err)
	}
	resultA := runPreparedCall(t, outputA[0], queryArgs)
	var queriedA struct {
		ProcessID string `json:"process_id"`
		Status    string `json:"status"`
		Error     string `json:"error"`
	}
	textA := toolResultText(t, resultA)
	if err := json.Unmarshal([]byte(textA), &queriedA); err != nil {
		t.Fatalf("unmarshal loopA ProcessOutput result %q: %v", textA, err)
	}
	if queriedA.Error != "" || queriedA.Status == "" {
		t.Fatalf("loopA (owning loop) ProcessOutput result = %+v, want a populated status and no error", queriedA)
	}
}

// TestProcessToolsForeignEngineRosterRejectsProcessEnabledTools
// proves Coderig's process-enabled roster never actually reaches a foreign
// (ACP) engine loop: Harness's OWN, already-reviewed protection
// (internal/sessionruntime's validateProcessServiceEngines /
// processBindingFor, mapped to *rig.LifecycleError{Kind:
// LifecycleProcessNotificationsUnsupported} by pkg/rig/lifecycle.go) rejects
// session construction outright for a loop.Definition explicitly declared
// on a foreign engine whose tools carry tool.RequiresProcessServices --
// exactly what genericToolDefinitions' Bash/Process* definitions now do.
// This is the SAME rejection mechanism harness's own reviewed test,
// TestForeignLoopRejectsProcessServices (internal/sessionruntime), exercises
// via the identical loop.WithEngine construction; this test additionally
// proves CODERIG's real roster composition (not a stand-in) triggers it.
func TestProcessToolsForeignEngineRosterRejectsProcessEnabledTools(t *testing.T) {
	root := t.TempDir()
	set := mustExecutorSet(t, root)

	definition, err := loop.Define(
		loop.WithName(generic.Name),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithTools(genericToolDefinitions(set, nil, nil)...),
		loop.WithEngine(loop.EngineForeignClaude),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	assembly, err := buildRig([]loop.Definition{definition}, stores, root, Config{}, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}

	_, err = assembly.NewSession(context.Background())
	if err == nil {
		t.Fatal("NewSession() error = nil, want a rejected foreign-engine process-services build")
	}
	var lifecycleErr *rig.LifecycleError
	if !errors.As(err, &lifecycleErr) {
		t.Fatalf("NewSession() error = %T %v, want *rig.LifecycleError", err, err)
	}
	if lifecycleErr.Kind != rig.LifecycleProcessNotificationsUnsupported {
		t.Fatalf("LifecycleError.Kind = %q, want %q", lifecycleErr.Kind, rig.LifecycleProcessNotificationsUnsupported)
	}
	if got := err.Error(); got == "" {
		t.Fatal("NewSession() error string is empty")
	}
}
