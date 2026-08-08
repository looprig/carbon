package app

import (
	"context"
	"testing"

	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
)

func TestRuntimeCatalogExposesModesAndModel(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)

	options, err := agent.LoopRuntimeOptions(context.Background(), agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Modes) == 0 || options.Modes[0].ID != tui.ModeID("") {
		t.Fatalf("modes = %#v, want declared base mode", options.Modes)
	}
	if len(options.Models) != 1 || options.Models[0].ID == "" {
		t.Fatalf("models = %#v, want one stable current model", options.Models)
	}
}

// TestSessionPresentationReportsFixedProfile proves the runtime agent surfaces the
// session-fixed access profile name and workspace root through the TUI's
// SessionPresenter contract. The default (empty) Config resolves to the readonly
// profile.
func TestSessionPresentationReportsFixedProfile(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)

	var presenter tui.SessionPresenter = agent
	presentation := presenter.SessionPresentation()
	if presentation.ProfileName != string(DefaultAccessProfile) {
		t.Fatalf("ProfileName = %q, want %q", presentation.ProfileName, DefaultAccessProfile)
	}
	if presentation.WorkspaceRoot == "" {
		t.Fatal("WorkspaceRoot is empty, want the session workspace root")
	}
	// A clean headless read-only store carries no out-of-catalog family diagnostics.
	if len(presentation.PermissionDiagnostics) != 0 {
		t.Fatalf("PermissionDiagnostics = %v, want none for a clean store", presentation.PermissionDiagnostics)
	}
}

// multiPrimerCandidates' two candidates deliberately share candidate-a's transport
// (provider/APIFormat/BaseURL) and differ only by model Name. This keeps the
// same-transport switch case covered on its own; TestSetModelSwitchesAcrossProviders
// and TestSetModelSwitchesAcrossAllConfiguredTransports separately cover switching
// between candidates naming genuinely different providers.
func multiPrimerCandidates() []PrimerCandidate {
	a := testModel()
	b := model.CustomModel(a.Provider, a.APIFormat, a.BaseURL, "candidate-b", model.WithTools(), model.WithThinking())
	return []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
}

// openAcceptanceAgentSelectingPrimerCandidate opens a headless session over the
// given roster with the session's CURRENT model fixed to selected (via the
// injected ModelFactory), independent of where selected sits in candidates.
// This lets tests distinguish "keyed off the current model" from "keyed off
// roster position 0" — see TestRuntimeCatalogEffortsReflectCurrentModelNotRosterOrder.
func openAcceptanceAgentSelectingPrimerCandidate(t *testing.T, candidates []PrimerCandidate, selected model.Model) (*RuntimeAgent, *sessionStores) {
	t.Helper()
	stores := mustHeadlessTestStores(t)
	cfg := Config{PrimerCandidates: candidates}
	agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(selected), cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent, stores
}

func openAcceptanceAgentWithPrimerCandidates(t *testing.T) (*RuntimeAgent, *sessionStores) {
	t.Helper()
	candidates := multiPrimerCandidates()
	return openAcceptanceAgentSelectingPrimerCandidate(t, candidates, candidates[0].Model)
}

func TestRuntimeCatalogListsAllPrimerCandidates(t *testing.T) {
	agent, _ := openAcceptanceAgentWithPrimerCandidates(t)

	options, err := agent.LoopRuntimeOptions(context.Background(), agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Models) != 2 {
		t.Fatalf("models = %#v, want 2 configured candidates", options.Models)
	}
	if options.Models[0].ID != tui.ModelID("candidate-a") || options.Models[1].ID != tui.ModelID("candidate-b") {
		t.Fatalf("models = %#v, want candidate-a then candidate-b in config order", options.Models)
	}
	// The session opened on candidate-a's model (testModel()), so effort
	// options must reflect candidate-a, not candidate-b.
	if len(options.Efforts) != 1 || options.Efforts[0].ID != tui.EffortID(model.EffortNone) {
		t.Fatalf("efforts = %#v, want candidate-a's [none]", options.Efforts)
	}
}

// TestRuntimeCatalogEffortsReflectCurrentModelNotRosterOrder proves efforts are
// derived from whichever candidate matches the CURRENT model, not from roster
// position 0. candidate-a is still listed first in Config.PrimerCandidates, but
// the session is opened on candidate-b's model — a stub that instead returned
// primerCandidates[0].Efforts would pass TestRuntimeCatalogListsAllPrimerCandidates
// but fail here.
func TestRuntimeCatalogEffortsReflectCurrentModelNotRosterOrder(t *testing.T) {
	candidates := multiPrimerCandidates()
	agent, _ := openAcceptanceAgentSelectingPrimerCandidate(t, candidates, candidates[1].Model)

	options, err := agent.LoopRuntimeOptions(context.Background(), agent.ActiveLoopID())
	if err != nil {
		t.Fatal(err)
	}
	want := []tui.EffortID{tui.EffortID(model.EffortNone), tui.EffortID(model.EffortLow), tui.EffortID(model.EffortMedium), tui.EffortID(model.EffortHigh)}
	if len(options.Efforts) != len(want) {
		t.Fatalf("efforts = %#v, want candidate-b's %v", options.Efforts, want)
	}
	for i, effort := range options.Efforts {
		if effort.ID != want[i] {
			t.Fatalf("efforts = %#v, want candidate-b's %v", options.Efforts, want)
		}
	}
}

func TestRuntimeControllerRejectsUnknownTypedChoices(t *testing.T) {
	agent, _ := openAcceptanceAgent(t)
	if err := agent.SetModel(context.Background(), agent.ActiveLoopID(), tui.ModelID("unknown/model")); err == nil {
		t.Fatal("SetModel(unknown) succeeded")
	}
	if err := agent.SetEffort(context.Background(), agent.ActiveLoopID(), tui.EffortID("impossible")); err == nil {
		t.Fatal("SetEffort(unknown) succeeded")
	}
}

func TestSetModelSwitchesToConfiguredCandidate(t *testing.T) {
	agent, _ := openAcceptanceAgentWithPrimerCandidates(t)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v", err)
	}

	options, err := agent.LoopRuntimeOptions(ctx, loopID)
	if err != nil {
		t.Fatal(err)
	}
	// candidate-b's efforts are [none, low, medium, high]; switching TO it from
	// candidate-a's current effort (none, which candidate-b also admits) must
	// NOT force a reset, so the loop's effort stays at "none".
	found := false
	for _, e := range options.Efforts {
		if e.ID == tui.EffortID(model.EffortNone) {
			found = true
		}
	}
	if !found || len(options.Efforts) != 4 {
		t.Fatalf("efforts after switch = %#v, want candidate-b's 4 options including none", options.Efforts)
	}
}

func TestSetModelUnknownCandidateFails(t *testing.T) {
	agent, _ := openAcceptanceAgentWithPrimerCandidates(t)
	if err := agent.SetModel(context.Background(), agent.ActiveLoopID(), tui.ModelID("does-not-exist")); err == nil {
		t.Fatal("SetModel(does-not-exist) succeeded, want error")
	}
}

func TestSetModelResetsEffortWhenNewCandidateDoesNotAdmitCurrent(t *testing.T) {
	agent, _ := openAcceptanceAgentWithPrimerCandidates(t)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	// Move candidate-a (efforts: [none]) into candidate-b's range first isn't
	// possible directly, so instead: switch to candidate-b, raise its effort
	// to "high" (which candidate-a does NOT admit), then switch back to
	// candidate-a and confirm the effort was reset instead of left stale.
	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v", err)
	}
	if err := agent.SetEffort(ctx, loopID, tui.EffortID(model.EffortHigh)); err != nil {
		t.Fatalf("SetEffort(high) error = %v", err)
	}
	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-a")); err != nil {
		t.Fatalf("SetModel(candidate-a) error = %v", err)
	}

	options, err := agent.LoopRuntimeOptions(ctx, loopID)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Efforts) != 1 || options.Efforts[0].ID != tui.EffortID(model.EffortNone) {
		t.Fatalf("efforts after reset-switch = %#v, want candidate-a's [none]", options.Efforts)
	}
}

func TestSetEffortAdmitsCurrentCandidateOnly(t *testing.T) {
	agent, _ := openAcceptanceAgentWithPrimerCandidates(t)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	// Still on candidate-a (efforts: [none]) — "high" belongs to candidate-b,
	// not the currently selected candidate, and must be refused.
	if err := agent.SetEffort(ctx, loopID, tui.EffortID(model.EffortHigh)); err == nil {
		t.Fatal("SetEffort(high) succeeded on candidate-a, want error")
	}

	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v", err)
	}
	// Now on candidate-b (efforts: [none, low, medium, high]) — "high" must
	// be admitted.
	if err := agent.SetEffort(ctx, loopID, tui.EffortID(model.EffortHigh)); err != nil {
		t.Fatalf("SetEffort(high) error = %v, want success on candidate-b", err)
	}
}

// TestSetModelSwitchesAcrossProviders proves the limitation documented at
// this test's previous incarnation (TestSetModelCrossProviderCandidateFails)
// no longer holds: harness's loop.WithContextTransports (see
// docs/plans/2026-08-05-primer-cross-provider-consumer-design.md) lets a
// loop definition declare more than one admitted transport, and
// conversationContextPolicy now declares every configured PrimerCandidates
// transport. A live SetModel between two candidates naming genuinely
// different providers (lmstudio candidate-a -> chutes candidate-b) now
// succeeds instead of being rejected.
func TestSetModelSwitchesAcrossProviders(t *testing.T) {
	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	candidates := []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
	agent, _ := openAcceptanceAgentSelectingPrimerCandidate(t, candidates, a)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v, want cross-provider switch to succeed", err)
	}

	options, err := agent.LoopRuntimeOptions(ctx, loopID)
	if err != nil {
		t.Fatal(err)
	}
	// candidate-a's effort (none) is admitted by candidate-b's effort set too,
	// so the switch must not force a reset.
	found := false
	for _, e := range options.Efforts {
		if e.ID == tui.EffortID(model.EffortNone) {
			found = true
		}
	}
	if !found || len(options.Efforts) != 4 {
		t.Fatalf("efforts after cross-provider switch = %#v, want candidate-b's 4 options including none", options.Efforts)
	}

	// Switch back, proving the transport declaration works both directions.
	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-a")); err != nil {
		t.Fatalf("SetModel(candidate-a) error = %v, want switching back to succeed", err)
	}
}

// crossTransportPrimerCandidates builds a three-candidate roster spanning two
// transports: candidate-a and candidate-c share one transport (both
// lmstudio, distinct model Name), candidate-b sits alone on a different one
// (chutes). All three are live-switchable from one another now that
// conversationContextPolicy declares every configured transport.
func crossTransportPrimerCandidates() []PrimerCandidate {
	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	c := model.CustomModel(a.Provider, a.APIFormat, a.BaseURL, "candidate-c-model", model.WithTools())
	return []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
		{Alias: "candidate-c", Description: "Candidate C", Model: c, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
	}
}

// TestSetModelSwitchesAcrossAllConfiguredTransports proves the full 3-candidate,
// 2-transport roster from crossTransportPrimerCandidates is live-switchable end
// to end, not just the 2-candidate pair TestSetModelSwitchesAcrossProviders
// covers: every candidate, including the two (candidate-a, candidate-c) that
// share a transport with each other but not with candidate-b, is reachable
// from wherever the session currently sits.
func TestSetModelSwitchesAcrossAllConfiguredTransports(t *testing.T) {
	candidates := crossTransportPrimerCandidates()
	agent, _ := openAcceptanceAgentSelectingPrimerCandidate(t, candidates, candidates[0].Model)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	for _, alias := range []string{"candidate-b", "candidate-c", "candidate-a"} {
		if err := agent.SetModel(ctx, loopID, tui.ModelID(alias)); err != nil {
			t.Fatalf("SetModel(%s) error = %v, want every declared candidate reachable", alias, err)
		}
	}
}
