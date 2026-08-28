package app

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/credentials"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

// serveModelsFixtureLoader is a productionModelsContextLoader shaped exactly as
// compileProductionModelsWithContext would shape one, carrying a distinctive value in
// every field resolveServeModels is required to fold into Config. Each value is
// deliberately unique so a folding step that is dropped, or crossed with another
// field, cannot be masked by a coincidental match.
func serveModelsFixtureLoader(runtime *credentialRuntime) productionModelsContextLoader {
	return func(context.Context, string) (productionModels, error) {
		return productionModels{
			PrimerClient:  &fakeLLM{},
			PrimerModel:   testModel(),
			PrimerAlias:   "fixture-primer-alias",
			PrimerEfforts: []model.Effort{model.EffortNone, model.EffortLow},
			PrimerCandidates: []PrimerCandidate{
				{Alias: "fixture-primer-alias", Label: "Fixture", Model: testModel()},
			},
			ConfigRev:         "fixture-config-rev",
			credentialRuntime: runtime,
		}, nil
	}
}

// activeSessions reads the credential runtime's admitted-session counter. It is the
// only observation that distinguishes "beginSession ran" from "beginSession was
// skipped", which is the difference between a composition that holds a credential
// lease and one that has silently dropped it.
func (r *credentialRuntime) activeSessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeN
}

// TestResolveServeModelsFoldsProductionConfigAndBeginsTheCredentialSession pins the
// whole contract of the extraction: every field SessionStoreFactory.Open's else
// branch folded into Config is folded here, the returned client prefers the runtime
// client, and the credential session is ALREADY BEGUN when it returns, so the caller
// owns exactly one endSession.
func TestResolveServeModelsFoldsProductionConfigAndBeginsTheCredentialSession(t *testing.T) {
	runtime := &credentialRuntime{}
	resolved, err := resolveServeModels(context.Background(), Config{HomeDir: t.TempDir()}, serveModelsFixtureLoader(runtime), nil)
	if err != nil {
		t.Fatalf("resolveServeModels: %v", err)
	}
	t.Cleanup(func() { runtime.endSession() })

	if resolved.cfg.ModelConfigRev != "fixture-config-rev" {
		t.Errorf("cfg.ModelConfigRev = %q, want %q", resolved.cfg.ModelConfigRev, "fixture-config-rev")
	}
	if resolved.cfg.PrimerAlias != "fixture-primer-alias" {
		t.Errorf("cfg.PrimerAlias = %q, want %q", resolved.cfg.PrimerAlias, "fixture-primer-alias")
	}
	wantEfforts := []model.Effort{model.EffortNone, model.EffortLow}
	if len(resolved.cfg.PrimerEfforts) != len(wantEfforts) {
		t.Fatalf("cfg.PrimerEfforts = %v, want %v", resolved.cfg.PrimerEfforts, wantEfforts)
	}
	for i, effort := range wantEfforts {
		if resolved.cfg.PrimerEfforts[i] != effort {
			t.Errorf("cfg.PrimerEfforts[%d] = %q, want %q", i, resolved.cfg.PrimerEfforts[i], effort)
		}
	}
	if len(resolved.cfg.PrimerCandidates) != 1 || resolved.cfg.PrimerCandidates[0].Label != "Fixture" {
		t.Errorf("cfg.PrimerCandidates = %+v, want the single fixture candidate", resolved.cfg.PrimerCandidates)
	}
	if resolved.cfg.ACPChildren == nil {
		t.Error("cfg.ACPChildren is nil; withProductionACPChildren did not run")
	}
	if resolved.client == nil || resolved.factory == nil {
		t.Fatalf("client = %v, factory = %v, want both non-nil", resolved.client, resolved.factory)
	}
	if resolved.credentialRuntime != runtime {
		t.Errorf("credentialRuntime = %v, want the loader's runtime", resolved.credentialRuntime)
	}
	if got := runtime.activeSessions(); got != 1 {
		t.Errorf("credential activeSessions = %d, want 1 (the caller now owns one endSession)", got)
	}
}

// TestResolveServeModelsPrefersTheRuntimeClient proves the client selection is the
// production one: a configuration that resolved a distinct runtime client must not
// have the primer client substituted for it.
func TestResolveServeModelsPrefersTheRuntimeClient(t *testing.T) {
	primer := &fakeLLM{}
	runtimeClient := &fakeLLM{}
	loader := func(context.Context, string) (productionModels, error) {
		return productionModels{
			PrimerClient: primer, RuntimeClient: runtimeClient,
			PrimerModel: testModel(), PrimerAlias: "primer",
			PrimerEfforts: []model.Effort{model.EffortNone}, ConfigRev: "rev",
		}, nil
	}
	resolved, err := resolveServeModels(context.Background(), Config{HomeDir: t.TempDir()}, loader, nil)
	if err != nil {
		t.Fatalf("resolveServeModels: %v", err)
	}
	if resolved.client != inference.Client(runtimeClient) {
		t.Errorf("client = %v, want the runtime client %v", resolved.client, runtimeClient)
	}
}

// TestResolveServeModelsRejectsAnIncapableConfigAndReleasesCredentials proves the
// fail-closed half: a configuration with no usable primer is a
// *ModelConfigCapabilityError, and the credential runtime it arrived with is CLOSED
// rather than leaked. A leak here would pin the process credential registry entry for
// the life of `carbon serve`.
func TestResolveServeModelsRejectsAnIncapableConfigAndReleasesCredentials(t *testing.T) {
	runtime := &credentialRuntime{}
	loader := func(context.Context, string) (productionModels, error) {
		return productionModels{credentialRuntime: runtime}, nil
	}
	_, err := resolveServeModels(context.Background(), Config{HomeDir: t.TempDir()}, loader, nil)
	var capability *ModelConfigCapabilityError
	if !errors.As(err, &capability) {
		t.Fatalf("resolveServeModels err = %T %v, want *ModelConfigCapabilityError", err, err)
	}
	if closed, _ := runtime.lifecycleState(); !closed {
		t.Error("credential runtime was left open after an incapable model configuration")
	}
	if got := runtime.activeSessions(); got != 0 {
		t.Errorf("credential activeSessions = %d, want 0", got)
	}
}

// TestResolveServeModelsPropagatesALoaderFailure proves a load error is returned
// unwrapped rather than being reshaped into a capability error.
func TestResolveServeModelsPropagatesALoaderFailure(t *testing.T) {
	boom := errors.New("models.json unreadable")
	loader := func(context.Context, string) (productionModels, error) { return productionModels{}, boom }
	if _, err := resolveServeModels(context.Background(), Config{HomeDir: t.TempDir()}, loader, nil); !errors.Is(err, boom) {
		t.Fatalf("resolveServeModels err = %v, want %v", err, boom)
	}
}

// TestResolveServeModelsFallsBackToTheContextFreeLoader pins the loader precedence
// SessionStoreFactory.Open already had: the context loader when present, otherwise
// the plain one. Dropping the fallback would break every caller that only configures
// loadModels (permission_review_test.go's fixtures among them).
func TestResolveServeModelsFallsBackToTheContextFreeLoader(t *testing.T) {
	resolved, err := resolveServeModels(context.Background(), Config{HomeDir: t.TempDir()}, nil,
		permissionReviewFixtureLoader(false, model.Model{}, false))
	if err != nil {
		t.Fatalf("resolveServeModels: %v", err)
	}
	if resolved.cfg.PrimerAlias != "fixture-primer" {
		t.Errorf("cfg.PrimerAlias = %q, want the context-free loader's %q", resolved.cfg.PrimerAlias, "fixture-primer")
	}
}

// TestSessionStoreFactoryOpenResolvesModelsThroughResolveServeModels is the
// anti-duplication guard. Open's production model resolution must BE
// resolveServeModels, not a second copy of it: this asserts the folded values arrive
// on the assembled RuntimeAgent, so any mutation inside resolveServeModels' folding
// breaks this test too. If Open ever grows its own inline copy, a mutation of
// resolveServeModels would leave this green — which is exactly the drift the
// extraction exists to prevent.
func TestSessionStoreFactoryOpenResolvesModelsThroughResolveServeModels(t *testing.T) {
	ctx := context.Background()
	stores := mustHeadlessTestStores(t)
	factory := &SessionStoreFactory{stores: stores, loadModelsWithContext: serveModelsFixtureLoader(nil)}

	agent, err := factory.Open(ctx, SessionSelector{}, Config{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	if agent.primerAlias != "fixture-primer-alias" {
		t.Errorf("agent.primerAlias = %q, want %q", agent.primerAlias, "fixture-primer-alias")
	}
	if len(agent.primerEfforts) != 2 {
		t.Errorf("agent.primerEfforts = %v, want the loader's two efforts", agent.primerEfforts)
	}
	if len(agent.primerCandidates) != 1 || agent.primerCandidates[0].Label != "Fixture" {
		t.Errorf("agent.primerCandidates = %+v, want the single fixture candidate", agent.primerCandidates)
	}
}

// TestCredentialSessionAdmissionIsCountingNotOwnership is the precondition
// OpenServeHost depends on and that no test previously pinned. The TUI ties one
// beginSession/endSession pair to one agent, so the pair looks like an ownership
// token; ServeHost holds ONE admission for the whole process across many sessions,
// which is only sound if beginSession is a re-entrant COUNTER with no per-session
// refresh or per-session state.
//
// It is: activeN and the per-reference active counters both increment, a second
// admission is granted rather than refused, and the drain channel logout waits on
// closes only when the LAST admission ends. That last property is the real cost of a
// process-long hold and the reason it is recorded here — an in-process credential
// logout cannot complete while `carbon serve` holds its admission.
func TestCredentialSessionAdmissionIsCountingNotOwnership(t *testing.T) {
	ref, err := credentials.ParseReference("credential://openai/personal")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &credentialRuntime{
		refs:    map[credentials.Reference]struct{}{ref: {}},
		active:  map[credentials.Reference]int{},
		blocked: map[credentials.Reference]bool{},
	}

	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession #1: %v", err)
	}
	drain := runtime.activeDone
	if drain == nil {
		t.Fatal("activeDone is nil after the first admission; logout would have nothing to wait on")
	}
	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession #2 = %v, want a second admission (a process-long hold requires re-entrancy)", err)
	}
	if got := runtime.activeSessions(); got != 2 {
		t.Fatalf("activeSessions = %d, want 2", got)
	}
	runtime.mu.Lock()
	perRef := runtime.active[ref]
	runtime.mu.Unlock()
	if perRef != 2 {
		t.Errorf("active[%s] = %d, want 2 (logout drains against this counter)", ref, perRef)
	}

	runtime.endSession()
	select {
	case <-drain:
		t.Fatal("the drain channel closed while one admission was still outstanding; a live session would be logged out from under")
	default:
	}
	runtime.endSession()
	select {
	case <-drain:
	default:
		t.Fatal("the drain channel is still open after the last admission ended; logout would block forever")
	}
	if got := runtime.activeSessions(); got != 0 {
		t.Errorf("activeSessions = %d, want 0", got)
	}
}
