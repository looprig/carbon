# Primer Model Picker Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Let Carbon's `/model` TUI command list and switch between every `models.json` entry tagged `uses: ["primer", ...]` (today: the local deepseek model, `chutes-kimi-k3`, `chutes-glm-5.2`), instead of only ever showing the one active model.

**Architecture:** `compileProductionModels` collects all primer-capable entries into a new `productionModels.PrimerCandidates` roster (mirroring how it already collects delegate-capable entries into `ACP`). That roster threads through `Config` into `RuntimeAgent`, which uses it to list real choices in `LoopRuntimeOptions.Models`, validate `SetModel` against any candidate (not just the current one), and key `Efforts`/`SetEffort` admission off whichever candidate is currently selected instead of a value frozen at session-open. When no roster is configured (today's lower-level test helpers that build a bare `Config{}`), every code path falls back to today's exact single-model behavior — this is purely additive.

**Tech Stack:** Go, `github.com/looprig/carbon` module (`internal/app` package), `github.com/looprig/harness/pkg/loop`, `github.com/looprig/tui`.

**Design doc:** `docs/plans/2026-08-04-primer-model-picker-design.md`

---

## Before you start

Read `internal/app/runtime_controls.go` and `internal/app/productionmodels.go` in full — every task below edits one or both. Run `go test ./internal/app/... -run TestRuntimeCatalog -v` and `go test ./internal/app/... -run TestProductionModels -v` once at the start to confirm today's baseline passes, so you know any later failure is yours.

All commands below run from `~/code/looprig/carbon` (the module root).

---

### Task 1: `PrimerCandidate` type and `productionModels.PrimerCandidates`

**Files:**
- Modify: `internal/app/productionmodels.go`
- Test: `internal/app/productionmodels_test.go`

**Step 1: Write the failing test**

Add this test to `internal/app/productionmodels_test.go` (after `TestProductionModelsConstructsCredentialBoundClients`):

```go
func TestProductionModelsCollectsAllPrimerCapableCandidates(t *testing.T) {
	primaryModel := model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234/v1", "primary", model.WithTools())
	altModel := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "alt-primer", model.WithTools(), model.WithThinking())
	delegateOnlyModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "delegate-only", model.WithTools())
	config := normalizedModelConfig{
		PrimerDefault: "fixture-primary",
		DelegateDefaults: []normalizedDelegateDefault{
			{Role: "planner", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
			{Role: "builder", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
			{Role: "reviewer", Harness: "codex", Model: "fixture-delegate-only", Effort: model.EffortNone},
		},
		Models: []normalizedModelTarget{
			{Alias: "fixture-primary", Description: "Primary", Model: primaryModel, Uses: []string{"primer"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
			{Alias: "fixture-alt-primer", Description: "Alternate primer", Model: altModel, Uses: []string{"primer", "delegate"}, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortHigh}, DefaultEffort: model.EffortLow},
			{Alias: "fixture-delegate-only", Description: "Delegate only", Model: delegateOnlyModel, Uses: []string{"delegate"}, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		},
	}

	got, err := compileProductionModels(config, func(model.Model, auth.APIKey) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}

	want := []PrimerCandidate{
		{Alias: "fixture-primary", Description: "Primary", Model: primaryModel, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "fixture-alt-primer", Description: "Alternate primer", Model: altModel, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
	if !reflect.DeepEqual(got.PrimerCandidates, want) {
		t.Fatalf("PrimerCandidates = %#v, want %#v", got.PrimerCandidates, want)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app/... -run TestProductionModelsCollectsAllPrimerCapableCandidates -v`
Expected: FAIL — `productionModels` has no field `PrimerCandidates` (compile error).

**Step 3: Write minimal implementation**

In `internal/app/productionmodels.go`:

1. Add the type, above `type productionModels struct`:

```go
// PrimerCandidate is one models.json entry tagged primer-capable
// (uses: ["primer", ...]). RuntimeAgent uses the roster of these to list
// and switch the primer loop's model at runtime.
type PrimerCandidate struct {
	Alias         string
	Description   string
	Model         model.Model
	Efforts       []model.Effort
	DefaultEffort model.Effort
}
```

2. Add `PrimerCandidates []PrimerCandidate` to the `productionModels` struct, after `PrimerEfforts`.

3. In `compileProductionModels`, inside the `for _, target := range config.Models` loop (the same loop that appends to `delegateSources`), add primer collection alongside the existing delegate collection:

```go
		var primerCandidates []PrimerCandidate
		if containsModelConfigUse(target.Uses, "primer") {
			primerCandidates = append(primerCandidates, PrimerCandidate{
				Alias:         target.Alias,
				Description:   target.Description,
				Model:         target.Model.Clone(),
				Efforts:       append([]model.Effort(nil), target.Efforts...),
				DefaultEffort: target.DefaultEffort,
			})
		}
```

Since `primerCandidates` must accumulate across loop iterations, declare it once before the loop (next to `delegateSources`) and append inside the loop instead of redeclaring with `var` each iteration — i.e. add `primerCandidates := make([]PrimerCandidate, 0, len(config.Models))` next to the `delegateSources := make(...)` line, and inside the loop just do the `if containsModelConfigUse(target.Uses, "primer") { primerCandidates = append(primerCandidates, PrimerCandidate{...}) }` block (no `var` redeclaration).

4. In the `return productionModels{...}` literal, add `PrimerCandidates: primerCandidates,` after `PrimerEfforts: primerEfforts,`.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/app/... -run 'TestProductionModels' -v`
Expected: PASS for all `TestProductionModels*` tests, including the new one and the existing `TestProductionModelsConstructsCredentialBoundClients` (which has one primer-tagged model, so it still passes unchanged — it doesn't assert on `PrimerCandidates` today, so no edit needed there).

**Step 5: Commit**

```bash
git add internal/app/productionmodels.go internal/app/productionmodels_test.go
git commit -m "feat: collect all primer-capable models into PrimerCandidates"
```

---

### Task 2: Thread `PrimerCandidates` through `Config`

**Files:**
- Modify: `internal/app/config.go`
- Modify: `internal/app/swarm.go:282-283` (inside `newWithProductionModelsLoader`)
- Modify: `internal/app/persistence.go:386-387` (inside `SessionStoreFactory.Open`)

There's no isolated unit test for this plumbing alone (it's exercised end-to-end by Task 3's tests) — this task is a direct, mechanical edit. Do it before Task 3 so the constructor changes in Task 3 have somewhere to read from.

**Step 1: Add the field to `Config`**

In `internal/app/config.go`, after the `PrimerEfforts` field:

```go
	// PrimerCandidates is every configured primer-capable model
	// (uses: ["primer", ...]), in models.json order. Production composition
	// copies it from productionModels before runtime assembly; RuntimeAgent
	// uses it to offer a real /model picker instead of one fixed choice.
	PrimerCandidates []PrimerCandidate
```

**Step 2: Populate it at both composition sites**

In `internal/app/swarm.go`, in `newWithProductionModelsLoader`, right after the existing line `cfg.PrimerEfforts = append([]model.Effort(nil), configured.PrimerEfforts...)`:

```go
	cfg.PrimerCandidates = append([]PrimerCandidate(nil), configured.PrimerCandidates...)
```

In `internal/app/persistence.go`, in `SessionStoreFactory.Open`, right after the existing line `cfg.PrimerEfforts = append([]model.Effort(nil), configured.PrimerEfforts...)`:

```go
			cfg.PrimerCandidates = append([]PrimerCandidate(nil), configured.PrimerCandidates...)
```

(Match the surrounding indentation — this one is inside the `else` block, one tab deeper than the `swarm.go` copy.)

**Step 3: Build to verify it compiles**

Run: `go build ./...`
Expected: succeeds (nothing reads `Config.PrimerCandidates` yet, so this is inert).

**Step 4: Commit**

```bash
git add internal/app/config.go internal/app/swarm.go internal/app/persistence.go
git commit -m "feat: thread PrimerCandidates through Config"
```

---

### Task 3: `RuntimeAgent` holds the candidate roster

**Files:**
- Modify: `internal/app/runtime_controls.go`
- Modify: `internal/app/swarm.go:344,387` (the two `newRuntimeAgentWithPrimerAlias` call sites)
- Modify: `internal/app/permission_review_integration_test.go:276`

**Step 1: Write the failing test**

Add to `internal/app/runtime_controls_test.go`:

```go
func multiPrimerCandidates() []PrimerCandidate {
	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	return []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
}

func openAcceptanceAgentWithPrimerCandidates(t *testing.T) (*RuntimeAgent, *swarmStores) {
	t.Helper()
	stores := mustHeadlessTestStores(t)
	cfg := Config{PrimerCandidates: multiPrimerCandidates()}
	agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent, stores
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
```

Add `"github.com/looprig/inference/model"` to the test file's imports (needed for `model.CustomModel`, `model.APIFormatOpenAI`, `model.WithTools`, `model.WithThinking`, `model.Effort*`).

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app/... -run TestRuntimeCatalogListsAllPrimerCandidates -v`
Expected: FAIL — `Config` has no usable candidate wiring into `RuntimeAgent` yet (the roster never reaches `LoopRuntimeOptions`, so `options.Models` still has its old single hardcoded row and the length check fails).

**Step 3: Write minimal implementation**

In `internal/app/runtime_controls.go`:

1. Add a field to `RuntimeAgent`:

```go
type RuntimeAgent struct {
	*sessionadapter.Adapter
	sess             session.SessionController
	root             string
	access           *sessionAccess
	primerAlias      string
	primerEfforts    []model.Effort
	primerCandidates []PrimerCandidate
}
```

2. Rename the constructor and add the parameter:

```go
func newRuntimeAgentWithPrimerCandidates(adapter *sessionadapter.Adapter, sess session.SessionController, root string, access *sessionAccess, primerAlias string, primerEfforts []model.Effort, primerCandidates []PrimerCandidate) *RuntimeAgent {
	return &RuntimeAgent{
		Adapter:          adapter,
		sess:             sess,
		root:             root,
		access:           access,
		primerAlias:      primerAlias,
		primerEfforts:    append([]model.Effort(nil), primerEfforts...),
		primerCandidates: append([]PrimerCandidate(nil), primerCandidates...),
	}
}
```

3. Add two small helpers near `containsPrimerEffort`:

```go
func findPrimerCandidate(candidates []PrimerCandidate, alias string) (PrimerCandidate, bool) {
	for _, c := range candidates {
		if c.Alias == alias {
			return c, true
		}
	}
	return PrimerCandidate{}, false
}

func currentPrimerCandidate(candidates []PrimerCandidate, current model.Model) (PrimerCandidate, bool) {
	for _, c := range candidates {
		if runtimeModelKeyFor(c.Model) == runtimeModelKeyFor(current) {
			return c, true
		}
	}
	return PrimerCandidate{}, false
}
```

4. Update `publicModelID` to prefer a roster match:

```go
func (a *RuntimeAgent) publicModelID(value model.Model) string {
	if c, ok := currentPrimerCandidate(a.primerCandidates, value); ok {
		return c.Alias
	}
	if a.primerAlias != "" {
		return a.primerAlias
	}
	return modelID(value)
}
```

5. Rewrite `LoopRuntimeOptions`'s model/effort section (the mode-catalog block above it is unchanged):

```go
	selectedModel := handle.Model()
	if len(a.primerCandidates) == 0 {
		publicID := a.publicModelID(selectedModel)
		options.Models = []tui.ModelOption{{ID: tui.ModelID(publicID), Label: publicID}}
	} else {
		options.Models = make([]tui.ModelOption, 0, len(a.primerCandidates))
		for _, c := range a.primerCandidates {
			options.Models = append(options.Models, tui.ModelOption{ID: tui.ModelID(c.Alias), Label: c.Alias, Description: c.Description})
		}
	}
	efforts := a.primerEfforts
	if current, ok := currentPrimerCandidate(a.primerCandidates, selectedModel); ok {
		efforts = current.Efforts
	} else if len(efforts) == 0 && selectedModel.Caps.Thinking {
		efforts = []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}
	}
	for _, effort := range efforts {
		label := string(effort)
		if label == "" {
			label = "Model default"
		}
		options.Efforts = append(options.Efforts, tui.EffortOption{ID: tui.EffortID(effort), Label: label})
	}
	return options, nil
}
```

(This replaces the old five lines starting at `selectedModel := handle.Model()` down to the `for _, effort := range efforts` loop — the loop body itself is unchanged, only what feeds `efforts` and `options.Models` changes.)

6. Update the two call sites in `internal/app/swarm.go` (lines 344 and 387) from:

```go
	return newRuntimeAgentWithPrimerAlias(adapter, adapter.Controller(), root, access, cfg.PrimerAlias, cfg.PrimerEfforts), nil
```

to:

```go
	return newRuntimeAgentWithPrimerCandidates(adapter, adapter.Controller(), root, access, cfg.PrimerAlias, cfg.PrimerEfforts, cfg.PrimerCandidates), nil
```

7. Update `internal/app/permission_review_integration_test.go:276` from:

```go
	agent := newRuntimeAgentWithPrimerAlias(adapter, controller, root, access, "", nil)
```

to:

```go
	agent := newRuntimeAgentWithPrimerCandidates(adapter, controller, root, access, "", nil, nil)
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/app/... -run 'TestRuntimeCatalog' -v`
Expected: PASS, including the pre-existing `TestRuntimeCatalogExposesModesAndModel` (it uses `Config{}`, so `primerCandidates` is empty and it hits the unchanged fallback branch — `len(options.Models) == 1` still holds).

Then run the full package to make sure the rename didn't strand anything: `go build ./... && go test ./internal/app/... 2>&1 | tail -40`
Expected: PASS (or pre-existing unrelated failures only — none are expected).

**Step 5: Commit**

```bash
git add internal/app/runtime_controls.go internal/app/runtime_controls_test.go internal/app/swarm.go internal/app/permission_review_integration_test.go
git commit -m "feat: RuntimeAgent lists all configured primer candidates in /model"
```

---

### Task 4: `SetModel` switches to any configured candidate

**Files:**
- Modify: `internal/app/runtime_controls.go`
- Test: `internal/app/runtime_controls_test.go`

**Step 1: Write the failing test**

Add to `internal/app/runtime_controls_test.go`:

```go
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app/... -run 'TestSetModel' -v`
Expected: FAIL — `SetModel` today rejects any ID other than the exact current model, so `SetModel(ctx, loopID, "candidate-b")` returns the "stale or unknown" error and every one of these tests fails at that first call.

**Step 3: Write minimal implementation**

Replace `SetModel` in `internal/app/runtime_controls.go`:

```go
func (a *RuntimeAgent) SetModel(ctx context.Context, loopID uuid.UUID, id tui.ModelID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	if len(a.primerCandidates) == 0 {
		selectedModel := controller.Model()
		if a.publicModelID(selectedModel) != string(id) {
			return fmt.Errorf("carbon: model choice %q is stale or unknown", id)
		}
		return controller.Change(ctx, loop.ChangeModel(selectedModel))
	}
	candidate, ok := findPrimerCandidate(a.primerCandidates, string(id))
	if !ok {
		return fmt.Errorf("carbon: model choice %q is stale or unknown", id)
	}
	changes := []loop.Change{loop.ChangeModel(candidate.Model)}
	if currentEffort := controller.Model().Sampling.Effort; !containsPrimerEffort(candidate.Efforts, currentEffort) {
		changes = append(changes, loop.ChangeEffort(candidate.DefaultEffort))
	}
	return controller.Change(ctx, changes...)
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/app/... -run 'TestSetModel|TestRuntimeCatalog|TestRuntimeController' -v`
Expected: PASS, including the pre-existing `TestRuntimeControllerRejectsUnknownTypedChoices` (it uses `Config{}` → empty roster → still hits the `len(a.primerCandidates) == 0` branch, unchanged behavior).

**Step 5: Commit**

```bash
git add internal/app/runtime_controls.go internal/app/runtime_controls_test.go
git commit -m "feat: SetModel switches the primer to any configured candidate"
```

---

### Task 5: `SetEffort` admits the currently selected candidate's efforts

**Files:**
- Modify: `internal/app/runtime_controls.go`
- Test: `internal/app/runtime_controls_test.go`

**Step 1: Write the failing test**

Add to `internal/app/runtime_controls_test.go`:

```go
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/app/... -run TestSetEffortAdmitsCurrentCandidateOnly -v`
Expected: FAIL — `SetEffort` currently gates on `a.primerEfforts`, which is empty for this fixture (`Config{PrimerCandidates: ...}` never sets `PrimerEfforts`), so `len(a.primerEfforts) != 0` is false and BOTH calls succeed unconditionally — the first assertion (expecting the "high" call on candidate-a to fail) fails.

**Step 3: Write minimal implementation**

Replace `SetEffort` in `internal/app/runtime_controls.go`:

```go
func (a *RuntimeAgent) SetEffort(ctx context.Context, loopID uuid.UUID, id tui.EffortID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	effort := model.Effort(id)
	if !effort.Valid() {
		return fmt.Errorf("carbon: effort choice %q is unknown", id)
	}
	admitted := a.primerEfforts
	if current, ok := currentPrimerCandidate(a.primerCandidates, controller.Model()); ok {
		admitted = current.Efforts
	}
	if len(admitted) != 0 && !containsPrimerEffort(admitted, effort) {
		return fmt.Errorf("carbon: effort choice %q is not admitted by the configured primer", id)
	}
	return controller.Change(ctx, loop.ChangeEffort(effort))
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./internal/app/... -run 'TestSetEffort|TestSetModel|TestRuntimeCatalog|TestRuntimeController' -v`
Expected: PASS, including the pre-existing `TestRuntimeControllerRejectsUnknownTypedChoices` (`Config{}` → `a.primerCandidates` empty → `currentPrimerCandidate` never matches → falls back to `a.primerEfforts`, which is nil/empty there too, so the `len(admitted) != 0` guard is false and the effort-admission check is skipped exactly as it was before this task — that test's failure comes from `!effort.Valid()`, unchanged).

**Step 5: Commit**

```bash
git add internal/app/runtime_controls.go internal/app/runtime_controls_test.go
git commit -m "feat: SetEffort admits the currently selected primer candidate only"
```

---

### Task 6: Full regression pass

**Files:** none (verification only)

**Step 1: Run the whole module's tests with race detection**

Run: `go build ./... && go vet ./... && go test -race ./... 2>&1 | tail -80`
Expected: PASS across the module. If anything outside `internal/app` fails, stop and investigate — it means something in this change leaked beyond the intended surface (it shouldn't; every edit in Tasks 1-5 stayed inside `internal/app`).

**Step 2: gofmt check**

Run: `gofmt -l internal/app/`
Expected: no output (empty = clean).

**Step 3: No commit needed** — this task only verifies Tasks 1-5. If something fails, fix it under the task that introduced it and re-commit there rather than adding a separate "fix" commit here.

---

### Task 7: Update `carbon/CLAUDE.md`

**Files:**
- Modify: `CLAUDE.md`

**Step 1: Make the edit**

Find this line (currently in the "Architecture" section):

```
Do not add a generic agent registry or model tier catalog. The roster is a small fixed set of Loop definitions. Runtime choices belong in Loop modes and model effort. Do not reintroduce a confinement bridge, a security-limit ordinal, or any in-session authority-mutation surface.
```

Replace it with:

```
Do not add an open-ended agent registry. The primer loop may expose a bounded picker over models.json entries tagged primer-capable (uses: ["primer", ...]); delegate roles remain fixed via delegate_defaults. Do not reintroduce a confinement bridge, a security-limit ordinal, or any in-session authority-mutation surface.
```

**Step 2: No test** — this is documentation. Confirm the file still reads sensibly around the edit with a quick read-through of the surrounding paragraph.

**Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: reflect the bounded primer model picker in the architecture rule"
```

---

## After this plan

`RuntimeAgent` now exposes every `uses: ["primer"]`-tagged model from `~/.looprig/models.json` through `/model`, and switching is real (not a no-op re-affirmation of the current model). Delegate roles (planner/builder/reviewer) are untouched — they still resolve solely through `delegate_defaults`, per the design doc's explicit scope boundary.
