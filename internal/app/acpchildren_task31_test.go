package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
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
	"github.com/looprig/storage/memstore"
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
	primer, err := loop.Define(
		loop.WithName(operatorPrimaryName),
		loop.WithInference(client, testModel()),
		loop.WithTools(probe.definition()),
		loop.WithAccessGate(approveAllAccessGate{}),
		loop.WithPolicyRevision("acp-task31"),
		loop.WithDelegates(identity.AgentName("operator")),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := loop.Define(
		loop.WithName(identity.AgentName("operator")),
		loop.WithInference(client, testModel()),
	)
	if err != nil {
		t.Fatal(err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(
		[]loop.Definition{primer, child}, stores, t.TempDir(), cfg, false,
		rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota},
		registration, permissionReviewRegistration{}, cfg.ACPChildren,
	)
	if err != nil {
		t.Fatal(err)
	}
	return assembly, stores, probe
}

func gatewayRuntimeCatalogForTask31(t *testing.T, clients map[model.ProviderName]inference.Client) loop.RuntimeCatalog {
	t.Helper()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{"operator"},
		GatewayClients: clients,
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
	rootID := live.ActiveLoop().ID()
	started, err := probe.captured().Execute(ctx, tool.DelegateRequest{
		Operation: tool.DelegateStart, Agent: "operator", Message: "build", Wait: false, Runtime: codexMaxRuntime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := started.DelegateID
	if childID.IsZero() {
		t.Fatal("delegated child id is zero")
	}
	liveCalls, _, runtime, _, _ := recorder.snapshot()
	if liveCalls != 1 || runtime.Profile != "acp/codex" || runtime.ModelAlias != "gpt-5.6-luna" || runtime.Effort != model.EffortMax {
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
				sawStarted = ev.AgentRuntime != nil && ev.AgentRuntime.Harness == "codex" && ev.AgentRuntime.Profile == "acp/codex" && ev.AgentRuntime.CredentialMode == string(loop.CredentialGatewayBacked) && ev.AgentRuntime.ModelAlias == "gpt-5.6-luna"
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
	if restoredCalls != 1 || restoredRuntime.Profile != "acp/codex" || restoredRuntime.ModelAlias != "gpt-5.6-luna" || restoredRuntime.Effort != model.EffortMax {
		t.Fatalf("restored builder calls=%d runtime=%+v", restoredCalls, restoredRuntime)
	}
	if seed.AgentSessionID != "acp-live-session" || seed.ForeignSID != "acp-live-session" {
		t.Fatalf("restore seed = %+v, want durable ACP session id", seed)
	}
	if _, ok := restored.Loop(rootID); !ok {
		t.Fatal("restored primer is missing")
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
	rootID := live.ActiveLoop().ID()
	started, err := probe.captured().Execute(ctx, tool.DelegateRequest{
		Operation: tool.DelegateStart, Agent: "operator", Message: "build", Wait: false, Runtime: codexMaxRuntime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := started.DelegateID
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
	primer, err := loop.Define(loop.WithName(operatorPrimaryName), loop.WithInference(client, testModel()), loop.WithTools(probe2.definition()), loop.WithAccessGate(approveAllAccessGate{}), loop.WithPolicyRevision("acp-task31"), loop.WithDelegates(identity.AgentName("operator")), loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}))
	if err != nil {
		t.Fatal(err)
	}
	child, err := loop.Define(loop.WithName(identity.AgentName("operator")), loop.WithInference(client, testModel()))
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	currentRig, err := buildRigWithRegistrationAndACP([]loop.Definition{primer, child}, stores, t.TempDir(), missingCfg, true, rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, registration, permissionReviewRegistration{}, missingComposition)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := currentRig.RestoreSession(ctx, live.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Shutdown(ctx) }()
	if _, ok := restored.Loop(rootID); !ok {
		t.Fatal("primer did not restore")
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

func TestACPCompositionWithoutProfilesHidesSelectorsAndFailsStartBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &managedScript{}
	var schema string
	var result string
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if step == 0 {
			for _, info := range req.Tools {
				if info.Name == "Subagent" {
					schema = string(info.Schema)
				}
			}
			step++
			return toolCall("no-acp", `{"action":"start","description":"work","prompt":"do it","subagent_type":"operator","run_in_background":true}`), nil
		}
		result = lastToolText(req)
		return finalText("parent done"), nil
	}
	probe := &delegateProbe{}
	primer, err := loop.Define(
		loop.WithName(operatorPrimaryName), loop.WithInference(client, testModel()), loop.WithTools(probe.definition()),
		loop.WithAccessGate(approveAllAccessGate{}), loop.WithPolicyRevision("acp-task31"),
		loop.WithDelegates(identity.AgentName("operator")), loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := loop.Define(loop.WithName(identity.AgentName("operator")), loop.WithInference(client, testModel()))
	if err != nil {
		t.Fatal(err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	emptyComposition := testACPEmptyComposition(t)
	assembly, err := buildRigWithRegistrationAndACP(
		[]loop.Definition{primer, child}, stores, t.TempDir(), Config{ACPChildren: emptyComposition}, false,
		rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, registration, permissionReviewRegistration{}, emptyComposition,
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
		t.Fatalf("Subagent schema is invalid JSON: %v", err)
	}
	for _, field := range []string{"agent_harness", "model", "effort"} {
		if _, ok := schemaDocument.Properties[field]; ok {
			t.Errorf("empty catalog schema advertises %q", field)
		}
	}
	if !strings.Contains(result, "runtime selection is unavailable") {
		t.Fatalf("empty catalog start result = %q, want bounded no-runtime error", result)
	}
}
