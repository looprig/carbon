package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/tui"
)

// mustHeadlessTestStores opens an ISOLATED in-memory store (NOT the process-shared headless
// singleton) so a session-opening unit test never contends on the real current-checkout root
// lease with sibling tests. Each caller gets its own leaser, so its exclusive-root lease is
// private to the test.
func mustHeadlessTestStores(t *testing.T) *sessionStores {
	t.Helper()
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	return stores
}

func TestProductionModelsPersistedOpenLoadsExactlyOnce(t *testing.T) {
	configured := testModel()
	configured.Name = "persisted-configured-primer"
	loads := 0
	factory := &SessionStoreFactory{
		stores: mustHeadlessTestStores(t),
		loadModels: func(string) (productionModels, error) {
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
	// Isolate PATH so a real claude-code-acp/codex-acp installed on the host
	// (as happens on developer machines) cannot be discovered by the
	// env->config->PATH executable resolution and divert this delegate's
	// gateway row to a real ACP subprocess instead of the in-process
	// RuntimeClient this test asserts against.
	t.Setenv("PATH", t.TempDir())
	primerModel := testModel()
	delegateModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "persisted-delegate", model.WithTools(), model.WithThinking())
	delegateModel.Limits = model.ContextLimits{WindowTokens: 128_000}
	primer := &managedScript{}
	delegate := &fakeLLM{chunks: finalText("native child complete")}
	var phase string
	var childID string
	parentStep := 0
	primer.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("primer client received non-parent request")
		}
		switch phase {
		case "":
			switch parentStep {
			case 0:
				parentStep++
				return startAgentCall("persisted-native-start", `{"agent_type":"carbon","instructions":"initial","model":"persisted-delegate","effort":"low"}`), nil
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
		ConfigRev: "persisted-runtime-client-rev",
	}
	factory := &SessionStoreFactory{
		stores: mustHeadlessTestStores(t),
		loadModels: func(string) (productionModels, error) {
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
		if started, ok := raw.(event.LoopStarted); ok && started.AgentName == carbon.Name {
			childID = started.LoopID.String()
		}
	}
	if childID == "" {
		t.Fatal("initial native start emitted no Carbon child")
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

// TestSetModelCrossProviderSwitchSurvivesRestore proves the actual end-to-end
// story this plan exists for: a live cross-provider SetModel isn't just
// accepted in the moment (TestSetModelSwitchesAcrossProviders,
// runtime_controls_test.go) — the session it changed can close and restore
// afterward, because declaredContextTransports (internal/app/inference_policy.go)
// declares the full configured roster's transports on every native loop, not
// just the one active at Open time. Restore reuses the SAME store and
// workspace root as the original open (see
// TestCompactionWiringSurvivesHeadlessNewRestoreAndClear elsewhere in this
// file): new and restore over the same checkout fold the same access digest,
// so this must NOT use a fresh t.TempDir() for the restored session.
func TestSetModelCrossProviderSwitchSurvivesRestore(t *testing.T) {
	a := testModel() // lmstudio
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	candidates := []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}

	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()
	cfg := Config{PrimerCandidates: candidates}
	factory := newModelFactoryFor(a)

	first, err := newSessionOverStores(ctx, &fakeLLM{}, factory, cfg, stores, root)
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	sessionID := first.SessionID()
	loopID := first.ActiveLoopID()

	if err := first.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v, want cross-provider switch to succeed", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// openRuntimeAgent (assembly.go) is the shared construction seam newSessionOverStores
	// itself wraps with a hardcoded SessionSelector{}; there is no separate "restoring"
	// helper, so the restore leg calls it directly with a Resume selector, matching the
	// production restore path (SessionStoreFactory.openWithClient).
	restored, err := openRuntimeAgent(ctx, &fakeLLM{}, factory, cfg, stores, root, SessionSelector{Resume: sessionID}, false)
	if err != nil {
		t.Fatalf("restore after cross-provider switch error = %v, want restore to succeed", err)
	}
	t.Cleanup(func() { _ = restored.Close(ctx) })

	options, err := restored.LoopRuntimeOptions(ctx, restored.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	// options.Models unconditionally lists every configured candidate regardless
	// of which one is actually selected (LoopRuntimeOptions, runtime_controls.go),
	// so it would list candidate-b whether or not the switch survived restore —
	// asserting on it here would be a tautology. options.Efforts IS selection-aware
	// (keyed off currentPrimerCandidate(a.primerCandidates, handle.Model())), so
	// candidate-b's 4-option effort set only appears if the restored loop's model
	// genuinely resolved back to candidate-b, not candidate-a's single-effort set.
	// Mirrors the live-switch assertion in TestSetModelSwitchesAcrossProviders.
	found := false
	for _, e := range options.Efforts {
		if e.ID == tui.EffortID(model.EffortHigh) {
			found = true
		}
	}
	if !found || len(options.Efforts) != 4 {
		t.Fatalf("efforts after restore = %#v, want candidate-b's 4 options (including high), proving the cross-provider switch survived restore", options.Efforts)
	}
}

func TestProductionOpenRejectsInvalidModelsBeforeOpeningPersistence(t *testing.T) {
	home := t.TempDir()
	setProcessHome(t, home)
	modelsPath := filepath.Join(home, ".looprig", "carbon", "models.json")
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

func TestRestoreAdoptsModelConfigRevisionDrift(t *testing.T) {
	stores := mustHeadlessTestStores(t)
	root := t.TempDir()
	ctx := context.Background()

	openAccess, openCfg := headlessTestAccess(t, Config{ModelConfigRev: "model-rev-a"}, root)
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), openCfg, openAccess)
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRig(definition, stores, root, openCfg, false)
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
		restoreDefinition, err := carbonTestDefinition(&fakeLLM{}, testModel(), restoreCfg, restoreAccess)
		if err != nil {
			return err
		}
		restoreAssembly, err := buildRig(restoreDefinition, stores, root, restoreCfg, false)
		if err != nil {
			return err
		}
		restored, err := restoreAssembly.RestoreSession(ctx, sessionID)
		if err == nil {
			_ = restored.Shutdown(ctx)
		}
		return err
	}
	if err := restore("model-rev-b"); err != nil {
		t.Fatalf("restore with changed ephemeral model configuration revision failed: %v", err)
	}
	if err := restore("model-rev-a"); err != nil {
		t.Fatalf("restore after returning to the original model configuration revision failed: %v", err)
	}
}

func TestBuildRigRegistersConversationCompaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "default composition"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			definition := carbonDef(t, tt.cfg)
			stores := mustHeadlessTestStores(t)
			if _, err := buildRig(definition, stores, t.TempDir(), tt.cfg, false); err != nil {
				t.Fatalf("buildRig() error = %v", err)
			}
		})
	}
}

func TestInvalidCompactionCompositionDoesNotOpenSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempt  func(*testing.T, *sessionStores) error
		wantType func(error) bool
	}{
		{
			name: "unsupported inference policy",
			attempt: func(t *testing.T, stores *sessionStores) error {
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
			attempt: func(t *testing.T, _ *sessionStores) error {
				policy, err := newConversationContextPolicy(testModel(), nil, nil)
				if err != nil {
					t.Fatalf("newConversationContextPolicy() error = %v", err)
				}
				policy.compaction.CounterPolicy = loop.CounterPolicyUnknown
				access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
				_, err = carbonTestDefinitionWithContextPolicy(&fakeLLM{}, testModel(), cfg, policy, access)
				return err
			},
			wantType: func(err error) bool {
				var target *LoopDefinitionError
				return errors.As(err, &target)
			},
		},
		{
			name: "invalid hustle registration",
			attempt: func(t *testing.T, stores *sessionStores) error {
				definition := carbonDef(t, Config{})
				_, err := buildRigWithRegistration(
					definition, stores, t.TempDir(), Config{}, false,
					rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota},
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
				func() (*sessionStores, error) {
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

			definition := carbonDef(t, tt.cfg)
			// Restore folds the SAME workspace-derived access digest as the original open
			// (new and restore over the same checkout produce the same digest), so the
			// rig-level fingerprint matches.
			_, restoreCfg := headlessTestAccess(t, tt.cfg, root)
			assembly, err := buildRig(definition, stores, root, restoreCfg, false)
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
// restored session's active loop id matches the original — parity for the headless rig generic.
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
	definition, err := carbonTestDefinition(&fakeLLM{}, newModelFactory()(), restoreCfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition error = %v", err)
	}
	assembly, err := buildRig(definition, stores, root, restoreCfg, false)
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
	if want := filepath.Join(home, ".looprig", "carbon", "store"); got != want {
		t.Errorf("DefaultDataDir() = %q, want %q", got, want)
	}
}

// TestDefaultDataDirIn proves DefaultDataDirIn resolves the store root under an
// already-resolved looprig home directory (an overridden Config.HomeDir in
// particular), independent of the process's real HOME, and that DefaultDataDir()
// still matches DefaultDataDirIn applied to the process HOME default.
func TestDefaultDataDirIn(t *testing.T) {
	t.Parallel()

	override := t.TempDir()
	got, err := DefaultDataDirIn(override)
	if err != nil {
		t.Fatalf("DefaultDataDirIn(%q) error = %v", override, err)
	}
	if want := filepath.Join(override, "store"); got != want {
		t.Errorf("DefaultDataDirIn(%q) = %q, want %q", override, got, want)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	fromDefault, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir: %v", err)
	}
	fromIn, err := DefaultDataDirIn(filepath.Join(home, ".looprig", "carbon"))
	if err != nil {
		t.Fatalf("DefaultDataDirIn: %v", err)
	}
	if fromDefault != fromIn {
		t.Errorf("DefaultDataDir() = %q, DefaultDataDirIn(process home) = %q, want equal", fromDefault, fromIn)
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

// TestSessionStoreFactoryListOrdersMostRecentlyActiveFirst proves List honors its own
// documented contract ("most-recently-active-first"). The underlying catalog sorts
// ascending by session ID (harness's Catalog.ListSessions), which is random with respect
// to time, so three sessions opened in sequence would come back in an order unrelated to
// creation order unless List itself re-sorts by activity.
func TestSessionStoreFactoryListOrdersMostRecentlyActiveFirst(t *testing.T) {
	factory, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	factory.buildClient = func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}
	cfg := Config{}

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		agent, err := factory.Open(context.Background(), SessionSelector{}, cfg)
		if err != nil {
			t.Fatalf("Open() [%d] error = %v", i, err)
		}
		ids = append(ids, agent.SessionID())
		if err := agent.Close(context.Background()); err != nil {
			t.Fatalf("Close() [%d] error = %v", i, err)
		}
	}

	metas, err := factory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(metas) != len(ids) {
		t.Fatalf("List() = %d sessions, want %d", len(metas), len(ids))
	}
	for i, meta := range metas {
		want := ids[len(ids)-1-i]
		if meta.SessionID != want {
			t.Errorf("List()[%d].SessionID = %s, want %s (most-recently-created first)", i, meta.SessionID, want)
		}
	}
}

// TestSortSessionsByRecencyFallsBackToCreatedAt proves a session that never had a turn
// (LastActiveAt stays the zero value — only TurnStarted/StepDone/RestoreDone stamp it) still
// sorts by when it was opened, rather than dropping to the bottom of the list behind every
// session that has ever run a turn.
func TestSortSessionsByRecencyFallsBackToCreatedAt(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	metas := []sessionstore.SessionMeta{
		{SessionID: uuid.UUID{1}, CreatedAt: older},                      // never active
		{SessionID: uuid.UUID{2}, CreatedAt: older, LastActiveAt: newer}, // active after [0]
		{SessionID: uuid.UUID{3}, CreatedAt: newer},                      // never active, created after [0]
	}
	sortSessionsByRecency(metas)
	want := []uuid.UUID{{2}, {3}, {1}}
	for i, meta := range metas {
		if meta.SessionID != want[i] {
			t.Errorf("sortSessionsByRecency()[%d].SessionID = %s, want %s", i, meta.SessionID, want[i])
		}
	}
}

// TestSessionStoreFactoryListOnlyShowsCurrentWorkspaceSessions proves List scopes the
// catalog to the CURRENT working directory's session, matching how Claude Code itself
// scopes its own session history to the current project rather than showing every session
// ever opened anywhere. Two sessions opened from two different workspace roots must not
// cross-contaminate each other's listing.
func TestSessionStoreFactoryListOnlyShowsCurrentWorkspaceSessions(t *testing.T) {
	factory, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	factory.buildClient = func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}
	cfg := Config{}

	t.Chdir(t.TempDir())
	inA, err := factory.Open(context.Background(), SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("Open() in workspace A error = %v", err)
	}
	idA := inA.SessionID()
	if err := inA.Close(context.Background()); err != nil {
		t.Fatalf("Close() in workspace A error = %v", err)
	}

	t.Chdir(t.TempDir())
	inB, err := factory.Open(context.Background(), SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("Open() in workspace B error = %v", err)
	}
	idB := inB.SessionID()
	if err := inB.Close(context.Background()); err != nil {
		t.Fatalf("Close() in workspace B error = %v", err)
	}

	// Still chdir'd into workspace B: List must show ONLY idB.
	metas, err := factory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(metas) != 1 || metas[0].SessionID != idB {
		t.Fatalf("List() = %#v, want only workspace B's session %s (never %s)", metas, idB, idA)
	}
}

// TestFilterSameWorkspaceKeepsMatchingAndEmptyRoots is the fast, deterministic unit test
// for the pure filtering logic List uses: a session fingerprinted to root is kept, a
// session fingerprinted to a DIFFERENT root is dropped, and a session with no recorded
// workspace at all (empty WorkspaceRoot) is kept rather than treated as belonging to
// someone else's project.
func TestFilterSameWorkspaceKeepsMatchingAndEmptyRoots(t *testing.T) {
	metas := []sessionstore.SessionMeta{
		{SessionID: uuid.UUID{1}, ConfigFingerprint: event.ConfigFingerprint{WorkspaceRoot: "exclusive:/repo/a"}},
		{SessionID: uuid.UUID{2}, ConfigFingerprint: event.ConfigFingerprint{WorkspaceRoot: "exclusive:/repo/b"}},
		{SessionID: uuid.UUID{3}, ConfigFingerprint: event.ConfigFingerprint{}}, // no workspace recorded
	}
	got := filterSameWorkspace(metas, "/repo/a")
	want := []uuid.UUID{{1}, {3}}
	if len(got) != len(want) {
		t.Fatalf("filterSameWorkspace() = %#v, want %d entries", got, len(want))
	}
	for i, meta := range got {
		if meta.SessionID != want[i] {
			t.Errorf("filterSameWorkspace()[%d].SessionID = %s, want %s", i, meta.SessionID, want[i])
		}
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
// yields the Carbon session's shared, secret-free model.Model identity (no system, no secret).
var _ ModelFactory = func() model.Model {
	return model.Model{}
}

// --- Session resource-storage composition (persisted + headless providers) ---
//
// No Carbon Loop definition declares tool.RequiresProcessServices today (that lands with the
// process-supervision tools themselves, a later task), so these tests exercise the two
// providers directly and, where the actual harness restore/identity-anchor behavior is what
// is under test, over a minimal standalone rig assembled with a probe loop.Definition that
// DOES declare the requirement — mirroring harness's own pkg/rig/session_resource_storage_test.go
// coverage of the same seam, one level up.

// resourceProbeTool is the minimal InvokableTool a process-services loop.Definition needs to
// bind; it is never actually invoked by these tests (no turn runs), only bound at session
// construction.
type resourceProbeTool struct{}

func (resourceProbeTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "resource-probe", Desc: "process-resource-storage test probe", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (resourceProbeTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

// processResourceStorageAgentName is the sole primer of the minimal probe topology below.
const processResourceStorageAgentName = "resource-probe-agent"

// processResourceStorageDefinition builds the smallest loop.Definition that declares
// tool.RequiresProcessServices, so rig.Define's requiresProcessServices gate is satisfied and
// NewSession/RestoreSession actually resolve resource storage through the injected provider.
func processResourceStorageDefinition(t *testing.T) loop.Definition {
	t.Helper()
	definition, err := loop.Define(
		loop.WithName(processResourceStorageAgentName),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithTools(tool.NewDefinition("resource-probe", tool.RequiresProcessServices, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{resourceProbeTool{}}, nil
		})),
	)
	if err != nil {
		t.Fatalf("loop.Define(resource-probe) error = %v", err)
	}
	return definition
}

// processResourceStorageRig assembles a standalone rig (independent of the production Carbon
// topology) over store with provider installed as its session resource-storage provider.
func processResourceStorageRig(t *testing.T, store *sessionstore.Store, provider rig.SessionResourceStorageProvider) *rig.Rig {
	t.Helper()
	defined, err := rig.Define(
		rig.WithLoops(processResourceStorageDefinition(t)),
		rig.WithPrimers(processResourceStorageAgentName),
		rig.WithSessionStore(store),
		rig.WithSessionResourceStorage(provider),
	)
	if err != nil {
		t.Fatalf("rig.Define() error = %v", err)
	}
	return defined
}

// pathHasRootPrefix reports whether path is root itself or lexically nested under it, purely
// structurally (no filesystem access, so it works for paths that do not yet exist).
func pathHasRootPrefix(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// identityOverrideProvider wraps another provider, substituting its own Identity while
// preserving the wrapped provider's Path. It simulates Carbon's own resource-storage scheme
// changing shape (a version bump to sessionResourceStorageIdentity) without needing to
// actually touch the package constant.
type identityOverrideProvider struct {
	inner    rig.SessionResourceStorageProvider
	identity string
}

func (p identityOverrideProvider) StorageForSession(ctx context.Context, id uuid.UUID) (rig.SessionResourceStorage, error) {
	storage, err := p.inner.StorageForSession(ctx, id)
	if err != nil {
		return rig.SessionResourceStorage{}, err
	}
	storage.Identity = p.identity
	return storage, nil
}

// TestProcessResourceRootOutsideWorkspace proves both providers' resolved resource roots never
// live inside a session's workspace root, by construction: the persisted root is always a
// child of <data-dir>, and the headless root is always a child of a dedicated os.MkdirTemp
// base, neither of which has anything to do with wherever the caller's checkout happens to be.
func TestProcessResourceRootOutsideWorkspace(t *testing.T) {
	t.Parallel()

	dataDir := filepath.Join(t.TempDir(), "store")
	persisted := newPersistedResourceStorageProvider(dataDir)
	headless, err := newHeadlessResourceStorageProvider()
	if err != nil {
		t.Fatalf("newHeadlessResourceStorageProvider() error = %v", err)
	}

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}

	// A couple of representative workspace roots: a wholly unrelated checkout, and one that
	// happens to be a filesystem SIBLING of the data dir (the closest a workspace could
	// plausibly get without actually being an ancestor/descendant).
	workspaces := []string{
		t.TempDir(),
		filepath.Join(filepath.Dir(dataDir), "checkout"),
	}

	for _, provider := range []rig.SessionResourceStorageProvider{persisted, headless} {
		storage, err := provider.StorageForSession(context.Background(), id)
		if err != nil {
			t.Fatalf("StorageForSession() error = %v", err)
		}
		for _, workspace := range workspaces {
			if pathHasRootPrefix(storage.Path, workspace) {
				t.Fatalf("resource root %q lies inside workspace root %q", storage.Path, workspace)
			}
		}
	}
}

// TestProcessResourceRootStableAcrossRestore proves the persisted provider resolves the SAME
// path and identity for the same session id across two INDEPENDENTLY constructed provider
// instances over the same data dir — simulating a real Carbon process restart, where a fresh
// SessionStoreFactory rebuilds a fresh provider carrying no in-memory state from the prior
// run — and that a real RestoreSession succeeds using the second instance for a session opened
// with the first.
func TestProcessResourceRootStableAcrossRestore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open() error = %v", err)
	}

	firstProvider := newPersistedResourceStorageProvider(dataDir)
	firstRig := processResourceStorageRig(t, store, firstProvider)
	live, err := firstRig.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	secondProvider := newPersistedResourceStorageProvider(dataDir)
	firstStorage, err := firstProvider.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("first provider StorageForSession() error = %v", err)
	}
	secondStorage, err := secondProvider.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("second provider StorageForSession() error = %v", err)
	}
	if firstStorage != secondStorage {
		t.Fatalf("resource storage across independently-constructed providers over the same data dir = %+v vs %+v, want equal", firstStorage, secondStorage)
	}

	secondRig := processResourceStorageRig(t, store, secondProvider)
	restored, err := secondRig.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession() with a freshly-constructed provider over the same data dir error = %v, want success", err)
	}
	if err := restored.Shutdown(ctx); err != nil {
		t.Fatalf("restored Shutdown() error = %v", err)
	}
}

// TestProcessResourceRootIdentityMismatchFailsRestore proves that if the resource-storage
// identity a session was opened with ever differs from what a later restore's provider
// reports for the same session id and path — exactly what would happen if Carbon's own
// on-disk resource-storage scheme changed shape without a migration — harness's own identity
// anchor rejects the restore. It also proves the CONVERSE: restoring again with the original,
// undrifted identity succeeds, so the failure above is really about the identity mismatch and
// not some unrelated construction problem.
func TestProcessResourceRootIdentityMismatchFailsRestore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open() error = %v", err)
	}

	provider := newPersistedResourceStorageProvider(dataDir)
	liveRig := processResourceStorageRig(t, store, provider)
	live, err := liveRig.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	drifted := identityOverrideProvider{inner: provider, identity: sessionResourceStorageIdentity + "-simulated-scheme-change"}
	driftedRig := processResourceStorageRig(t, store, drifted)
	restored, restoreErr := driftedRig.RestoreSession(ctx, id)
	if restored != nil {
		_ = restored.Shutdown(ctx)
	}
	if restoreErr == nil {
		t.Fatal("RestoreSession() with a drifted resource-storage identity succeeded, want fail-closed rejection")
	}
	// *session.RestoreError is harness's one exported restore-failure type; the more
	// specific *sessionruntime.SessionResourceStorageError and its identity_mismatch Kind
	// live in harness's internal/sessionruntime and so cannot be errors.As'd from outside
	// the harness module. RestoreLoopFailed plus the wrapped cause's "identity_mismatch"
	// text is the most precise assertion available to a carbon-level test.
	var restoreError *session.RestoreError
	if !errors.As(restoreErr, &restoreError) || restoreError.Kind != session.RestoreLoopFailed {
		t.Fatalf("RestoreSession() error = %T %v, want *session.RestoreError{Kind: RestoreLoopFailed}", restoreErr, restoreErr)
	}
	if !strings.Contains(restoreErr.Error(), "identity_mismatch") {
		t.Fatalf("RestoreSession() error = %v, want harness's identity_mismatch resource-storage error", restoreErr)
	}

	undriftedRig := processResourceStorageRig(t, store, provider)
	recovered, err := undriftedRig.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession() with the ORIGINAL undrifted identity error = %v, want success (proves the drifted case above genuinely turned on the identity, not on some other difference)", err)
	}
	if err := recovered.Shutdown(ctx); err != nil {
		t.Fatalf("recovered Shutdown() error = %v", err)
	}
}

// TestHeadlessProcessResourceRootsAreIsolated proves two independently constructed headless
// providers — simulating two concurrently running headless Carbon processes — never resolve
// overlapping resource roots, even for the identical session id, and that two different
// session ids within the SAME provider get distinct subdirectories of its one shared base.
func TestHeadlessProcessResourceRootsAreIsolated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	first, err := newHeadlessResourceStorageProvider()
	if err != nil {
		t.Fatalf("newHeadlessResourceStorageProvider() error = %v", err)
	}
	second, err := newHeadlessResourceStorageProvider()
	if err != nil {
		t.Fatalf("newHeadlessResourceStorageProvider() error = %v", err)
	}
	if first.base == second.base {
		t.Fatalf("two independently constructed headless providers share base %q, want distinct process-owned bases", first.base)
	}

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	firstStorage, err := first.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("first.StorageForSession() error = %v", err)
	}
	secondStorage, err := second.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("second.StorageForSession() error = %v", err)
	}
	if firstStorage.Path == secondStorage.Path {
		t.Fatalf("two independent headless providers (simulating two concurrently running headless Carbon processes) resolved the SAME resource root %q for the same session id, want distinct process-owned bases", firstStorage.Path)
	}
	if pathHasRootPrefix(secondStorage.Path, first.base) || pathHasRootPrefix(firstStorage.Path, second.base) {
		t.Fatalf("headless provider roots overlap: first=%q (base %q) second=%q (base %q)", firstStorage.Path, first.base, secondStorage.Path, second.base)
	}

	otherID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	otherStorage, err := first.StorageForSession(ctx, otherID)
	if err != nil {
		t.Fatalf("first.StorageForSession(otherID) error = %v", err)
	}
	if otherStorage.Path == firstStorage.Path {
		t.Fatalf("two different session ids resolved the same headless resource root %q", firstStorage.Path)
	}
}

// TestHeadlessProcessResourceRootStableForSameProcessRestore proves that, within ONE running
// process (one provider instance), reconstructing a headless session — a real
// NewSession/Shutdown/RestoreSession round trip over the SAME provider — resolves the
// identical resource root both times and lets the restore succeed, matching the task's
// "SessionID subdirectory is stable for same-process reconstruction" contract.
func TestHeadlessProcessResourceRootStableForSameProcessRestore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider, err := newHeadlessResourceStorageProvider()
	if err != nil {
		t.Fatalf("newHeadlessResourceStorageProvider() error = %v", err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open() error = %v", err)
	}

	defined := processResourceStorageRig(t, store, provider)
	live, err := defined.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	before, err := provider.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("StorageForSession() before restore error = %v", err)
	}

	restored, err := defined.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession() within the same process error = %v, want success", err)
	}
	if err := restored.Shutdown(ctx); err != nil {
		t.Fatalf("restored Shutdown() error = %v", err)
	}

	after, err := provider.StorageForSession(ctx, id)
	if err != nil {
		t.Fatalf("StorageForSession() after restore error = %v", err)
	}
	if before != after {
		t.Fatalf("headless provider resource storage before restore = %+v, after = %+v, want stable within one process", before, after)
	}
}
