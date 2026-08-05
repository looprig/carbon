package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/tui"
)

// mustHeadlessTestStores opens an ISOLATED in-memory store (NOT the process-shared headless
// singleton) so a session-opening unit test never contends on the real current-checkout root
// lease with sibling tests. Each caller gets its own leaser, so its exclusive-root lease is
// private to the test.
func mustHeadlessTestStores(t *testing.T) *swarmStores {
	t.Helper()
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatalf("openStores(memstore) error = %v", err)
	}
	return stores
}

func TestProductionModelsPersistedOpenLoadsExactlyOnce(t *testing.T) {
	configured := testModel()
	configured.Name = "persisted-configured-primer"
	loads := 0
	factory := &SessionStoreFactory{
		stores: mustHeadlessTestStores(t),
		loadModels: func() (productionModels, error) {
			loads++
			return productionModels{PrimerClient: &fakeLLM{}, PrimerModel: configured, PrimerAlias: "persisted-primer-alias", PrimerEfforts: []model.Effort{model.EffortHigh}, ConfigRev: "persisted-model-rev"}, nil
		},
	}
	agent, err := factory.Open(context.Background(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	if loads != 1 {
		t.Fatalf("production model loads = %d, want exactly 1", loads)
	}
	options, err := agent.LoopRuntimeOptions(context.Background(), agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Models) != 1 || options.Models[0].ID != "persisted-primer-alias" || options.Models[0].Label != "persisted-primer-alias" || options.Models[0].Description != "" {
		t.Fatalf("runtime models = %#v, want only configured primer alias", options.Models)
	}
	if len(options.Efforts) != 1 || options.Efforts[0].ID != tui.EffortID(model.EffortHigh) {
		t.Fatalf("runtime efforts = %#v, want only configured high effort", options.Efforts)
	}
}

func TestPersistedOpenRoutesNativeAgentThroughRuntimeClientAcrossRestore(t *testing.T) {
	primerModel := testModel()
	delegateModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "persisted-delegate", model.WithTools(), model.WithThinking())
	delegateModel.Limits = model.ContextLimits{WindowTokens: 128_000}
	primer := &managedScript{}
	delegate := &fakeLLM{chunks: finalText("native child complete")}
	var phase string
	var childID string
	parentStep := 0
	primer.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, builder.Name) {
			return nil, fmt.Errorf("primer client received non-parent request")
		}
		switch phase {
		case "":
			switch parentStep {
			case 0:
				parentStep++
				return startAgentCall("persisted-native-start", `{"agent_type":"planner","instructions":"initial","model":"persisted-delegate","effort":"low"}`), nil
			case 1:
				parentStep++
				return finalText("initial parent complete"), nil
			}
		case "restored":
			if parentStep == 0 {
				parentStep++
				return messageAgentCall("persisted-native-message", fmt.Sprintf(`{"agent_id":%q,"message":"continue"}`, childID)), nil
			}
			if parentStep == 1 {
				parentStep++
				return finalText("restored parent complete"), nil
			}
		}
		return nil, fmt.Errorf("unexpected persisted parent step phase=%q step=%d", phase, parentStep)
	}

	runtimeClient, err := newModelRoutingClient([]modelBinding{
		{Model: primerModel, Client: primer},
		{Model: delegateModel, Client: delegate},
	})
	if err != nil {
		t.Fatalf("newModelRoutingClient() error = %v", err)
	}
	configured := productionModels{
		PrimerClient:  primer,
		RuntimeClient: runtimeClient,
		PrimerModel:   primerModel,
		PrimerAlias:   "persisted-primer",
		PrimerEfforts: []model.Effort{model.EffortNone},
		ACP: []ACPGatewaySource{{
			Alias: "persisted-delegate", Description: "Persisted delegate", Client: delegate,
			Model: delegateModel, DefaultEffort: model.EffortLow,
			Efforts: []model.Effort{model.EffortLow, model.EffortMedium},
		}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			planner.Name:  {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "persisted-delegate", Effort: model.EffortLow},
			builder.Name:  {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "persisted-delegate", Effort: model.EffortLow},
			reviewer.Name: {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "persisted-delegate", Effort: model.EffortLow},
		},
		ConfigRev: "persisted-runtime-client-rev",
	}
	factory := &SessionStoreFactory{
		stores: mustHeadlessTestStores(t),
		loadModels: func() (productionModels, error) {
			return configured, nil
		},
	}

	first, err := factory.Open(context.Background(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	_, observed := runManagedTurnObserved(t, first, "start persisted native agent")
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("initial Close() error = %v", err)
	}
	for _, raw := range observed {
		if started, ok := raw.(event.LoopStarted); ok && started.AgentName == planner.Name {
			childID = started.LoopID.String()
		}
	}
	if childID == "" {
		t.Fatal("initial native start emitted no planner child")
	}

	phase = "restored"
	parentStep = 0
	restored, err := factory.Open(context.Background(), SessionSelector{Resume: first.SessionID()}, Config{})
	if err != nil {
		t.Fatalf("restored Open() error = %v", err)
	}
	if got := runManagedTurn(t, restored, "message persisted native agent"); got != "restored parent complete" {
		t.Fatalf("restored parent final = %q", got)
	}
	if err := restored.Close(context.Background()); err != nil {
		t.Fatalf("restored Close() error = %v", err)
	}

	streamRequests, _ := delegate.capturedRequests()
	if len(streamRequests) != 2 {
		t.Fatalf("delegate stream requests = %d, want initial and restored child turns", len(streamRequests))
	}
	for index, req := range streamRequests {
		if req.Model.Name != delegateModel.Name || req.Model.Sampling.Effort != model.EffortLow {
			t.Fatalf("delegate request %d model=%q effort=%q, want %q/low", index, req.Model.Name, req.Model.Sampling.Effort, delegateModel.Name)
		}
	}
}

func TestProductionOpenRejectsInvalidModelsBeforeOpeningPersistence(t *testing.T) {
	home := t.TempDir()
	setProcessHome(t, home)
	modelsPath := filepath.Join(home, ".looprig", "models.json")
	if err := os.MkdirAll(filepath.Dir(modelsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "store")
	factory, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatalf("NewSessionStoreFactory() error = %v", err)
	}
	defer func() { _ = factory.Close() }()
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store directory after factory construction = %v, want absent", err)
	}
	if _, err := factory.Open(context.Background(), SessionSelector{}, Config{}); err == nil {
		t.Fatal("Open() error = nil, want invalid model configuration error")
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store directory after invalid Open = %v, want absent", err)
	}
}

func TestRestoreRejectsModelConfigRevisionDrift(t *testing.T) {
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()
	ctx := context.Background()

	openAccess, openCfg := headlessTestAccess(t, Config{ModelConfigRev: "model-rev-a"}, root)
	definitions, err := swarmDefinitions(&fakeLLM{}, testModel(), openCfg, openAccess)
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRig(definitions, stores, root, openCfg, false)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := assembly.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	restore := func(revision string) error {
		restoreAccess, restoreCfg := headlessTestAccess(t, Config{ModelConfigRev: revision}, root)
		restoreDefinitions, err := swarmDefinitions(&fakeLLM{}, testModel(), restoreCfg, restoreAccess)
		if err != nil {
			return err
		}
		restoreAssembly, err := buildRig(restoreDefinitions, stores, root, restoreCfg, false)
		if err != nil {
			return err
		}
		restored, err := restoreAssembly.RestoreSession(ctx, sessionID)
		if err == nil {
			_ = restored.Shutdown(ctx)
		}
		return err
	}
	if err := restore("model-rev-b"); err == nil {
		t.Fatal("restore with changed model configuration revision succeeded")
	}
	if err := restore("model-rev-a"); err != nil {
		t.Fatalf("restore with identical model configuration revision failed: %v", err)
	}
}

func TestBuildRigRegistersConversationCompaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "default composition"},
		{name: "runtime skills composition", cfg: Config{RuntimeSkills: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			definitions := swarmDefs(t, tt.cfg)
			stores := mustHeadlessTestStores(t)
			if _, err := buildRig(definitions, stores, t.TempDir(), tt.cfg, false); err != nil {
				t.Fatalf("buildRig() error = %v", err)
			}
		})
	}
}

func TestInvalidCompactionCompositionDoesNotOpenSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempt  func(*testing.T, *swarmStores) error
		wantType func(error) bool
	}{
		{
			name: "unsupported inference policy",
			attempt: func(t *testing.T, stores *swarmStores) error {
				unsupported := testModel()
				unsupported.Provider = "unsupported"
				_, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(unsupported), Config{}, stores, t.TempDir())
				return err
			},
			wantType: func(err error) bool {
				var target *UnsupportedInferenceProviderError
				return errors.As(err, &target)
			},
		},
		{
			name: "invalid loop compaction policy",
			attempt: func(t *testing.T, _ *swarmStores) error {
				policy, err := newConversationContextPolicy(testModel(), nil)
				if err != nil {
					t.Fatalf("newConversationContextPolicy() error = %v", err)
				}
				policy.compaction.CounterPolicy = loop.CounterPolicyUnknown
				access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
				_, err = swarmDefinitionsWithContextPolicy(&fakeLLM{}, testModel(), cfg, policy, access)
				return err
			},
			wantType: func(err error) bool {
				var target *LoopDefinitionError
				return errors.As(err, &target)
			},
		},
		{
			name: "invalid hustle registration",
			attempt: func(t *testing.T, stores *swarmStores) error {
				definitions := swarmDefs(t, Config{})
				_, err := buildRigWithRegistration(
					definitions, stores, t.TempDir(), Config{}, false,
					rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota},
					conversationHustleRegistration{limits: conversationHustleLimits()},
					permissionReviewRegistration{},
				)
				return err
			},
			wantType: func(err error) bool {
				var target *rig.DefinitionError
				return errors.As(err, &target)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stores := mustHeadlessTestStores(t)
			err := tt.attempt(t, stores)
			if err == nil || !tt.wantType(err) {
				t.Fatalf("construction error = %T %v, want expected typed failure", err, err)
			}
			metas, listErr := stores.catalog.ListSessions(context.Background())
			if listErr != nil {
				t.Fatalf("ListSessions() error = %v", listErr)
			}
			if len(metas) != 0 {
				t.Errorf("session catalog contains %d entries after failed construction, want 0", len(metas))
			}
		})
	}
}

func TestHeadlessCompactionValidationPrecedesStoreOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model model.Model
	}{
		{name: "unsupported provider", model: func() model.Model {
			value := testModel()
			value.Provider = "unsupported"
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storeOpened := false
			_, err := newWithClientUsingStores(
				context.Background(), &fakeLLM{}, newModelFactoryFor(tt.model), Config{},
				func() (*swarmStores, error) {
					storeOpened = true
					return mustHeadlessTestStores(t), nil
				},
			)
			var unsupported *UnsupportedInferenceProviderError
			if !errors.As(err, &unsupported) {
				t.Fatalf("newWithClientUsingStores() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
			}
			if storeOpened {
				t.Error("store provider called before compaction policy validation")
			}
		})
	}
}

func TestCompactionWiringSurvivesHeadlessNewRestoreAndClear(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "default composition"},
		{name: "runtime skills composition", cfg: Config{RuntimeSkills: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stores := mustHeadlessTestStores(t)
			root := t.TempDir()

			factory := newModelFactoryFor(testModel())
			first, err := newSessionOverStores(ctx, &fakeLLM{}, factory, tt.cfg, stores, root)
			if err != nil {
				t.Fatalf("headless new error = %v", err)
			}
			firstID := first.SessionID()
			firstFingerprint := durableSessionFingerprint(t, stores, firstID)
			if err := first.Close(ctx); err != nil {
				t.Fatalf("headless Close() error = %v", err)
			}

			definitions := swarmDefs(t, tt.cfg)
			// Restore folds the SAME workspace-derived access digest as the original open
			// (new and restore over the same checkout produce the same digest), so the
			// rig-level fingerprint matches.
			_, restoreCfg := headlessTestAccess(t, tt.cfg, root)
			assembly, err := buildRig(definitions, stores, root, restoreCfg, false)
			if err != nil {
				t.Fatalf("restore buildRig() error = %v", err)
			}
			restoredController, err := assembly.RestoreSession(ctx, firstID)
			if err != nil {
				t.Fatalf("RestoreSession() error = %v", err)
			}
			restored, err := newSessionAdapter(ctx, restoredController, stores.session, true)
			if err != nil {
				t.Fatalf("newSessionAdapter(restore) error = %v", err)
			}
			if err := restored.Close(ctx); err != nil {
				t.Fatalf("restored Close() error = %v", err)
			}

			cleared, err := newSessionOverStores(ctx, &fakeLLM{}, factory, tt.cfg, stores, root)
			if err != nil {
				t.Fatalf("clear reopen error = %v", err)
			}
			defer func() { _ = cleared.Close(ctx) }()
			if cleared.SessionID() == firstID {
				t.Fatalf("clear SessionID = original %v, want fresh session", firstID)
			}
			clearedFingerprint := durableSessionFingerprint(t, stores, cleared.SessionID())
			if !clearedFingerprint.Equal(firstFingerprint) {
				t.Errorf("clear fingerprint = %+v, want original %+v", clearedFingerprint, firstFingerprint)
			}
		})
	}
}

// TestExclusiveCheckoutContentionAndHandoff proves the Phase-B exclusive-workspace invariant:
// two sessions over the SAME store + SAME checkout root contend on the exclusive root lease —
// the second cannot open while the first holds it — and once the first is Closed the lease is
// released so a third session opens cleanly (release/handoff). This is the mechanism that
// makes two headless sessions contend on the shared current checkout.
func TestExclusiveCheckoutContentionAndHandoff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()

	first, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("first session open error = %v", err)
	}

	// The second open on the SAME root must not proceed while the first holds the lease.
	// A bounded context ensures the test fails loud rather than hanging if the backend
	// blocks instead of failing fast — either way the second session cannot open.
	blockedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	second, err := newSessionOverStores(blockedCtx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	cancel()
	if err == nil {
		_ = second.Close(ctx)
		_ = first.Close(ctx)
		t.Fatal("second session opened while the first held the exclusive root lease, want a contention error")
	}

	// Release the first session's lease; a third session then opens (handoff).
	if err := first.Close(ctx); err != nil {
		t.Fatalf("first session Close error = %v", err)
	}
	third, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("third session open after handoff error = %v", err)
	}
	if err := third.Close(ctx); err != nil {
		t.Fatalf("third session Close error = %v", err)
	}
}

// TestHeadlessNewAndRestoreRoundTrip proves a session opened over an isolated store can be
// Shutdown and RESTORED by id over the SAME store (the rig owns new + restore), and that the
// restored session's active loop id matches the original — parity for the headless rig builder.
func TestHeadlessNewAndRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()

	first, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactory(), Config{}, stores, root)
	if err != nil {
		t.Fatalf("new session error = %v", err)
	}
	id := first.SessionID()
	activeLoop := first.ActiveLoopID()
	if id.IsZero() || activeLoop.IsZero() {
		t.Fatalf("new session id/active loop zero: id=%v active=%v", id, activeLoop)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	access, restoreCfg := headlessTestAccess(t, Config{}, root)
	definitions, err := swarmDefinitions(&fakeLLM{}, newModelFactory()(), restoreCfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions error = %v", err)
	}
	assembly, err := buildRig(definitions, stores, root, restoreCfg, false)
	if err != nil {
		t.Fatalf("buildRig error = %v", err)
	}
	controller, err := assembly.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession error = %v", err)
	}
	restored, err := newSessionAdapter(ctx, controller, stores.session, true)
	if err != nil {
		t.Fatalf("newSessionAdapter(restore) error = %v", err)
	}
	t.Cleanup(func() { _ = restored.Close(ctx) })

	if restored.SessionID() != id {
		t.Errorf("restored SessionID = %v, want %v", restored.SessionID(), id)
	}
	if restored.ActiveLoopID() != activeLoop {
		t.Errorf("restored ActiveLoopID = %v, want %v", restored.ActiveLoopID(), activeLoop)
	}
}

// TestDefaultDataDir proves the default store root is the ~/.looprig/store path the CLI falls
// back to when --data-dir is unset.
func TestDefaultDataDir(t *testing.T) {
	t.Parallel()

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	if want := filepath.Join(home, ".looprig", "store"); got != want {
		t.Errorf("DefaultDataDir() = %q, want %q", got, want)
	}
}

// TestNewSessionStoreFactoryLifecycle proves the persisted factory opens over an on-disk store
// and closes cleanly, and that List starts empty (no sessions until one is opened).
func TestNewSessionStoreFactoryLifecycle(t *testing.T) {
	t.Parallel()

	f, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStoreFactory error = %v", err)
	}
	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("List() = %d sessions, want 0 for a fresh store", len(metas))
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close error = %v", err)
	}
}

func TestSessionStoreFactoryClosePreventsLateStoreOpen(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "store")
	factory, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := factory.List(context.Background()); err == nil {
		t.Fatal("List() after Close() succeeded, want closed-factory error")
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store directory after close-before-use = %v, want absent", err)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestSessionStoreFactoryCloseAndListAreSerialized(t *testing.T) {
	factory, err := NewSessionStoreFactory(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var listErr, closeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, listErr = factory.List(context.Background())
	}()
	go func() {
		defer wg.Done()
		<-start
		closeErr = factory.Close()
	}()
	close(start)
	wg.Wait()
	if closeErr != nil {
		t.Fatalf("concurrent Close() error = %v", closeErr)
	}
	if listErr != nil {
		var closedErr *StoreClosedError
		if !errors.As(listErr, &closedErr) {
			t.Fatalf("concurrent List() error = %v, want nil or StoreClosedError", listErr)
		}
	}
	if _, err := factory.List(context.Background()); err == nil {
		t.Fatal("List() after concurrent Close() succeeded")
	}
}

// ModelFactory is a plain func type; this compile-time assertion documents its shape: it
// yields the swarm's shared, secret-free model.Model identity (no system, no secret).
var _ ModelFactory = func() model.Model {
	return model.Model{}
}
