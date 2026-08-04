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

func multiPrimerCandidates() []PrimerCandidate {
	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
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
func openAcceptanceAgentSelectingPrimerCandidate(t *testing.T, candidates []PrimerCandidate, selected model.Model) (*RuntimeAgent, *swarmStores) {
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

func openAcceptanceAgentWithPrimerCandidates(t *testing.T) (*RuntimeAgent, *swarmStores) {
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
