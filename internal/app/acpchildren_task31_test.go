package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

type acpCompositionRecorder struct {
	mu sync.Mutex

	liveCalls       int
	restoredCalls   int
	liveRuntime     loop.RuntimeIdentity
	restoredRuntime loop.RuntimeIdentity
	restoreSeed     foreign.RestoredForeign
	backend         *acpCompositionBackend
}

func (r *acpCompositionRecorder) live(
	_ context.Context,
	_, _ uuid.UUID,
	_ loop.Provenance,
	_ foreign.EventPublisher,
	cfg loop.BoundDefinition,
	_ func() (uuid.UUID, error),
	_ *event.Factory,
) (loop.Backend, string, error) {
	r.mu.Lock()
	r.liveCalls++
	r.liveRuntime = cfg.RuntimeIdentity()
	backend := r.backend
	r.mu.Unlock()
	return backend, "acp-live-session", nil
}

func (r *acpCompositionRecorder) restored(
	_ context.Context,
	_, _ uuid.UUID,
	_ loop.Provenance,
	_ foreign.EventPublisher,
	cfg loop.BoundDefinition,
	_ func() (uuid.UUID, error),
	_ *event.Factory,
	seed foreign.RestoredForeign,
) (loop.Backend, error) {
	r.mu.Lock()
	r.restoredCalls++
	r.restoredRuntime = cfg.RuntimeIdentity()
	r.restoreSeed = seed
	r.mu.Unlock()
	return newACPCompositionBackend(), nil
}

func (r *acpCompositionRecorder) snapshot() (int, int, loop.RuntimeIdentity, loop.RuntimeIdentity, foreign.RestoredForeign) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveCalls, r.restoredCalls, r.liveRuntime, r.restoredRuntime, r.restoreSeed
}

type acpCompositionBackend struct {
	commands chan command.Command
	done     chan struct{}
	once     sync.Once
}

func newACPCompositionBackend() *acpCompositionBackend {
	backend := &acpCompositionBackend{commands: make(chan command.Command), done: make(chan struct{})}
	go backend.serve()
	return backend
}

func (b *acpCompositionBackend) serve() {
	for raw := range b.commands {
		switch cmd := raw.(type) {
		case command.Shutdown:
			cmd.Ack <- nil
			b.once.Do(func() { close(b.done) })
			return
		case command.Interrupt:
			cmd.Ack <- false
		}
	}
}

func (b *acpCompositionBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *acpCompositionBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *acpCompositionBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func testACPComposition(t *testing.T, catalog loop.RuntimeCatalog, recorder *acpCompositionRecorder) *ACPComposition {
	t.Helper()
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", recorder.live, recorder.restored); err != nil {
		t.Fatal(err)
	}
	return &ACPComposition{
		Catalog:  ACPCompiledCatalog{RuntimeCatalog: catalog, profiles: map[loop.RuntimeProfileName]struct{}{"acp/codex": {}}},
		Registry: &registry,
		Live:     dispatchACPBuilder(&registry),
		Restored: dispatchACPRestoredBuilder(&registry),
	}
}

func testACPEmptyComposition(t *testing.T) *ACPComposition {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	var registry foreign.BuilderRegistry
	return &ACPComposition{
		Catalog:  ACPCompiledCatalog{RuntimeCatalog: catalog},
		Registry: &registry,
		Live:     dispatchACPBuilder(&registry),
		Restored: dispatchACPRestoredBuilder(&registry),
	}
}

func testACPDelegationRig(t *testing.T, cfg Config) (*rig.Rig, *swarmStores, *delegateProbe) {
	t.Helper()
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
		return finalText("child done"), nil
	}}
	probe := &delegateProbe{}
	definitions, root, cfg := task31ProductionDefinitions(t, client, probe, cfg)
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(
		definitions, stores, root, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota},
		registration, permissionReviewRegistration{}, cfg.ACPChildren,
	)
	if err != nil {
		t.Fatal(err)
	}
	return assembly, stores, probe
}

func task31ProductionDefinitions(t *testing.T, client inference.Client, probe *delegateProbe, cfg Config) ([]loop.Definition, string, Config) {
	t.Helper()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, cfg, root)
	definitions, err := swarmDefinitionsWithAdditionalTools(client, testModel(), cfg, access, map[identity.AgentName][]tool.Definition{
		builder.Name: {probe.definition()},
	})
	if err != nil {
		t.Fatalf("swarmDefinitionsWithAdditionalTools() error = %v", err)
	}
	return definitions, root, cfg
}

func gatewayRuntimeCatalogForTask31(t *testing.T, clients map[model.ProviderName]inference.Client) loop.RuntimeCatalog {
	t.Helper()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:     []identity.AgentName{planner.Name, builder.Name, reviewer.Name},
		GatewayTargets: legacyTestGatewayTargets(clients),
		Defaults:       legacyTestDefaults([]identity.AgentName{planner.Name, builder.Name, reviewer.Name}),
		ClaudeSmall:    "sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.RuntimeCatalog
}

func codexMaxRuntime() *tool.DelegateRuntime {
	return &tool.DelegateRuntime{
		Harness: "codex", Profile: "acp/codex", Model: "gpt-5.6-luna", Effort: "max",
		Explicit: tool.DelegateRuntimeExplicit{Harness: true, Model: true, Effort: true},
	}
}

func replayACPEvents(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []event.Event {
	t.Helper()
	replayer, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	var events []event.Event
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
}

func task31PrimerRootIDs(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) map[identity.AgentName]uuid.UUID {
	t.Helper()
	rootIDs := make(map[identity.AgentName]uuid.UUID)
	for _, raw := range replayACPEvents(t, store, sessionID) {
		if ev, ok := raw.(event.LoopStarted); ok && ev.Cause.Coordinates.LoopID.IsZero() {
			rootIDs[ev.AgentName] = ev.LoopID
		}
	}
	wantPrimers := []identity.AgentName{planner.Name, builder.Name, reviewer.Name}
	if len(rootIDs) != len(wantPrimers) {
		t.Fatalf("durable root loops = %v, want planner, builder, and reviewer", rootIDs)
	}
	for _, name := range wantPrimers {
		if _, ok := rootIDs[name]; !ok {
			t.Fatalf("durable root loop %q is missing", name)
		}
	}
	return rootIDs
}

func assertTask31PrimersPresent(t *testing.T, rootIDs map[identity.AgentName]uuid.UUID, lookup func(uuid.UUID) (loop.Handle, bool)) {
	t.Helper()
	for _, name := range []identity.AgentName{planner.Name, builder.Name, reviewer.Name} {
		if _, ok := lookup(rootIDs[name]); !ok {
			t.Fatalf("primer %q is missing", name)
		}
	}
}

func TestACPPostureUsesProductionRolesOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role    string
		posture driver.Posture
	}{
		{role: string(planner.Name), posture: driver.PostureReadOnly},
		{role: string(builder.Name), posture: driver.PostureWorkspaceWrite},
		{role: string(reviewer.Name), posture: driver.PostureReadOnly},
	}
	for _, tt := range tests {
		got, err := acpPostureFor(tt.role)
		if err != nil {
			t.Fatalf("acpPostureFor(%q): %v", tt.role, err)
		}
		if got != tt.posture {
			t.Errorf("acpPostureFor(%q) = %q, want %q", tt.role, got, tt.posture)
		}
	}
	if _, err := acpPostureFor("operator"); err == nil {
		t.Fatal("acpPostureFor(\"operator\") succeeded; stale role must be rejected")
	}
}

func TestACPCompositionRestoresCodexRuntimeThroughCurrentCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	catalog := gatewayRuntimeCatalogForTask31(t, map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{}, "openai": &fakeLLM{},
	})
	recorder := &acpCompositionRecorder{backend: newACPCompositionBackend()}
	composition := testACPComposition(t, catalog, recorder)
	assembly, stores, probe := testACPDelegationRig(t, Config{ACPChildren: composition})

	live, err := assembly.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootIDs := task31PrimerRootIDs(t, stores.session, live.SessionID())
	assertTask31PrimersPresent(t, rootIDs, live.Loop)
	rootID := rootIDs[builder.Name]
	if live.ActiveLoop().ID() != rootID {
		t.Fatalf("active primer = %v, want builder root %v", live.ActiveLoop().ID(), rootID)
	}
	started, err := probe.captured().Execute(ctx, tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: string(reviewer.Name), Message: "review", WaitForResponse: false, Runtime: codexMaxRuntime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := started.AgentID
	if childID.IsZero() {
		t.Fatal("delegated child id is zero")
	}
	liveCalls, _, runtime, _, _ := recorder.snapshot()
	if liveCalls != 1 || runtime.Profile != "acp/codex" || runtime.ModelAlias != "gpt-5.6-luna@max" || runtime.Effort != model.EffortMax {
		t.Fatalf("live builder calls=%d runtime=%+v", liveCalls, runtime)
	}

	if err := live.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	events := replayACPEvents(t, stores.session, live.SessionID())
	var sawStarted, sawBound bool
	for _, raw := range events {
		switch ev := raw.(type) {
		case event.LoopStarted:
			if ev.LoopID == childID {
				sawStarted = ev.AgentRuntime != nil && ev.AgentRuntime.Harness == "codex" && ev.AgentRuntime.Profile == "acp/codex" && ev.AgentRuntime.CredentialMode == string(loop.CredentialGatewayBacked) && ev.AgentRuntime.ModelAlias == "gpt-5.6-luna@max"
			}
		case event.LoopAgentSessionBound:
			if ev.LoopID == childID && ev.ACPSessionID == "acp-live-session" {
				sawBound = true
			}
		}
	}
	if !sawStarted || !sawBound {
		t.Fatalf("durable child runtime/session binding missing: started=%v bound=%v", sawStarted, sawBound)
	}

	restored, err := assembly.RestoreSession(ctx, live.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Shutdown(ctx) }()
	_, restoredCalls, restoredRuntime, _, seed := recorder.snapshot()
	if restoredCalls != 1 || restoredRuntime.Profile != "acp/codex" || restoredRuntime.ModelAlias != "gpt-5.6-luna@max" || restoredRuntime.Effort != model.EffortMax {
		t.Fatalf("restored builder calls=%d runtime=%+v", restoredCalls, restoredRuntime)
	}
	if seed.AgentSessionID != "acp-live-session" || seed.ForeignSID != "acp-live-session" {
		t.Fatalf("restore seed = %+v, want durable ACP session id", seed)
	}
	assertTask31PrimersPresent(t, rootIDs, restored.Loop)
	if restored.ActiveLoop().ID() != rootID {
		t.Fatalf("restored active primer = %v, want builder root %v", restored.ActiveLoop().ID(), rootID)
	}
}

func TestACPCompositionMissingLunaTombstonesChildAndKeepsPrimer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fullCatalog := gatewayRuntimeCatalogForTask31(t, map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{}, "openai": &fakeLLM{},
	})
	recorder := &acpCompositionRecorder{backend: newACPCompositionBackend()}
	fullComposition := testACPComposition(t, fullCatalog, recorder)
	assembly, stores, probe := testACPDelegationRig(t, Config{ACPChildren: fullComposition})
	live, err := assembly.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootIDs := task31PrimerRootIDs(t, stores.session, live.SessionID())
	assertTask31PrimersPresent(t, rootIDs, live.Loop)
	rootID := rootIDs[builder.Name]
	if live.ActiveLoop().ID() != rootID {
		t.Fatalf("active primer = %v, want builder root %v", live.ActiveLoop().ID(), rootID)
	}
	started, err := probe.captured().Execute(ctx, tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: string(reviewer.Name), Message: "review", WaitForResponse: false, Runtime: codexMaxRuntime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := started.AgentID
	if err := live.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	missingCatalog := gatewayRuntimeCatalogForTask31(t, map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{},
	})
	missingComposition := testACPComposition(t, missingCatalog, &acpCompositionRecorder{backend: newACPCompositionBackend()})
	missingCfg := Config{ACPChildren: missingComposition}
	// Rebuild the same CodeRig topology with the current, deliberately incomplete catalog.
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) { return finalText("unused"), nil }}
	probe2 := &delegateProbe{}
	definitions, root, missingCfg := task31ProductionDefinitions(t, client, probe2, missingCfg)
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	currentRig, err := buildRigWithRegistrationAndACP(definitions, stores, root, missingCfg, true, rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration, permissionReviewRegistration{}, missingComposition)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := currentRig.RestoreSession(ctx, live.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Shutdown(ctx) }()
	assertTask31PrimersPresent(t, rootIDs, restored.Loop)
	if restored.ActiveLoop().ID() != rootID {
		t.Fatalf("restored active primer = %v, want builder root %v", restored.ActiveLoop().ID(), rootID)
	}
	if _, ok := restored.Loop(childID); !ok {
		t.Fatal("missing-runtime child was not retained as a tombstone")
	}
	var tombstoned bool
	for _, raw := range replayACPEvents(t, stores.session, live.SessionID()) {
		if ev, ok := raw.(event.LoopRestoreTombstoned); ok && ev.LoopID == childID {
			tombstoned = true
		}
	}
	if !tombstoned {
		t.Fatal("restore emitted no child tombstone")
	}
}

func TestACPCompositionWithoutProfilesUsesManagedNativeFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &managedScript{}
	var schema string
	var result string
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if requestHasRole(req, reviewer.Name) {
			return finalText("reviewer done"), nil
		}
		if !requestHasRole(req, builder.Name) {
			return nil, errors.New("unexpected role in managed native fallback request")
		}
		if step == 0 {
			for _, info := range req.Tools {
				if info.Name == "StartAgent" {
					schema = string(info.Schema)
				}
			}
			step++
			return startAgentCall("no-acp", `{"agent_type":"reviewer","instructions":"do it","wait_for_response":false}`), nil
		}
		result = lastToolText(req)
		return finalText("parent done"), nil
	}
	probe := &delegateProbe{}
	emptyComposition := testACPEmptyComposition(t)
	definitions, root, noACPCfg := task31ProductionDefinitions(t, client, probe, Config{ACPChildren: emptyComposition})
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(
		definitions, stores, root, noACPCfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration, permissionReviewRegistration{}, emptyComposition,
	)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := assembly.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAdapter(ctx, controller, stores.session, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agent.Close(ctx) }()
	if got := runManagedTurn(t, agent, "delegate"); got != "parent done" {
		t.Fatalf("parent result = %q", got)
	}
	var schemaDocument struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &schemaDocument); err != nil {
		t.Fatalf("StartAgent schema is invalid JSON: %v", err)
	}
	for _, field := range []string{"model", "effort"} {
		if _, ok := schemaDocument.Properties[field]; !ok {
			t.Errorf("ordinary native catalog schema omitted %q", field)
		}
	}
	var queued struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal([]byte(result), &queued); err != nil {
		t.Fatalf("managed native fallback result = %q: %v", result, err)
	}
	if queued.AgentID == "" || queued.Name == "" || queued.State != "working" {
		t.Fatalf("managed native fallback result = %q, want working agent handle", result)
	}
}
