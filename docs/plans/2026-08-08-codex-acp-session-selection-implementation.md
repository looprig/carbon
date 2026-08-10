# Codex ACP Session Selection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make native Codex StartAgent selections apply the requested adapter model and reasoning effort after both fresh and restored ACP sessions are opened.

**Architecture:** `acp/launch.CodexConnector` will use the adapter-advertised ACP session config options, sharing the existing strict model/thought-level resolvers used by Claude. `foreignloops/driver/acp` will apply Codex model first and effort second after `session/new` or `session/load`, and will close owned resources on selection failure. Native launch arguments will no longer pretend to carry model or effort.

**Tech Stack:** Go 1.26, Agent Client Protocol session configuration, table-driven tests, race detector, committed Go vendor trees.

---

### Task 1: Add Codex ACP session selectors

**Files:**

- Modify: `acp/launch/codex_connector.go`
- Modify: `acp/launch/codex_connector_test.go`
- Modify: `acp/launch/codex.go`
- Modify: `acp/launch/codex_test.go`

**Step 1: Write failing session-selection tests**

Add a fake `sessionConfigurer` with advertised options and recorded calls. Add tests equivalent to:

```go
func TestCodexSelectModelThenEffortUsesAdvertisedOptions(t *testing.T) {
    sess := newFakeSessionConfigurer(
        modelOption("model", "gpt-5.6-sol", "gpt-5.6-luna"),
        effortOption("reasoning_effort", "medium", "max"),
    )
    connector := Codex("").WithModelEffort("gpt-5.6-luna", "max")

    requireNoError(t, connector.selectModel(context.Background(), sess))
    requireNoError(t, connector.selectEffort(context.Background(), sess))

    assertCalls(t, sess.calls,
        configCall{"model", "gpt-5.6-luna"},
        configCall{"reasoning_effort", "max"},
    )
}
```

Also prove:

- empty model/effort makes no wire call;
- model-only selection calls only `model`;
- an unadvertised model returns `*ModelAliasError` without a wire call;
- an unadvertised effort returns `*EffortAliasError` without the invalid call;
- changing model refreshes the cached options used by effort resolution;
- exported `SelectModel` and `SelectEffort` accept a real `*client.Session`;
- the connector remains immutable.

Update the method-set regression: Codex is now expected to expose the two session selectors, while arbitrary setters remain forbidden.

**Step 2: Run the tests and verify RED**

Run:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache \
  go test ./launch -run 'TestCodex.*(Select|Session|Effort|NativeArgs)' -count=1 -v
```

Expected: FAIL because Codex has no session selection methods and native arguments still contain `model`/`model_reasoning_effort`.

**Step 3: Implement minimal session selection**

Add methods parallel to Claude's narrow selectors:

```go
func (c *CodexConnector) SelectModel(ctx context.Context, sess *client.Session) error {
    return c.selectModel(ctx, sess)
}

func (c *CodexConnector) selectModel(ctx context.Context, sess sessionConfigurer) error {
    if c.Model == "" {
        return nil
    }
    configID, valueID, ok := resolveModelSelection(sess.ConfigOptions(), c.Model)
    if !ok {
        return &ModelAliasError{Alias: c.Model}
    }
    return sess.SetConfigOption(ctx, configID, valueID)
}

func (c *CodexConnector) SelectEffort(ctx context.Context, sess *client.Session) error {
    return c.selectEffort(ctx, sess)
}

func (c *CodexConnector) selectEffort(ctx context.Context, sess sessionConfigurer) error {
    if c.Effort == "" {
        return nil
    }
    configID, valueID, ok := resolveEffortSelection(sess.ConfigOptions(), c.Effort)
    if !ok {
        return &EffortAliasError{Effort: c.Effort, Alias: c.Effort}
    }
    return sess.SetConfigOption(ctx, configID, valueID)
}
```

Replace stale comments that describe post-session selection as permanently unsupported.

For `ConfigureNative`, omit the `model` and `model_reasoning_effort` pairs. Retain the complete-tuple validation and connector fields because the driver consumes them after opening the session. Do not change gateway behavior in this bug fix.

**Step 4: Run GREEN verification**

Run:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache \
  go test ./launch -run 'TestCodex.*(Select|Session|Effort|NativeArgs)' -count=1 -v
GOCACHE=/private/tmp/codex-acp-session-selection-gocache go test -race ./launch -count=1
go vet ./launch
git diff --check
```

Expected: PASS.

**Step 5: Commit**

```bash
git add launch/codex_connector.go launch/codex_connector_test.go launch/codex.go launch/codex_test.go
git commit -m "fix: select Codex model through ACP session"
```

### Task 2: Apply Codex selection after new and loaded sessions

**Files:**

- Modify: `foreignloops/driver/acp/driver.go`
- Modify: `foreignloops/driver/acp/driver_test.go`

**Step 1: Write failing driver lifecycle tests**

Extend the driver seams with a narrow Codex selector interface:

```go
type codexConnector interface {
    SelectModel(context.Context, session) error
    SelectEffort(context.Context, session) error
}
```

Write tests that inject a fake ACP client/session and prove:

- fresh native `gpt-5.6-luna`/`max` calls model then effort;
- restored native `gpt-5.6-luna`/`max` calls model then effort after `LoadSession`;
- the actual adapter model ID, not the friendly Carbon alias, reaches selection;
- managed empty selection makes no calls;
- model-only selection calls only model;
- gateway behavior is unchanged;
- model or effort selection failure closes the owned session/process exactly once;
- no selector becomes an environment variable or launch argument.

The central assertion should be:

```go
want := []selectionCall{
    {kind: "model", value: "gpt-5.6-luna"},
    {kind: "effort", value: "max"},
}
```

Run the same assertion for `AgentSessionID == ""` and a non-empty restored session ID.

**Step 2: Run tests and verify RED**

Run:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache \
  go test ./driver/acp -run 'TestNew.*Codex.*SessionSelection|TestRestore.*Codex.*SessionSelection|TestCodex.*SelectionFailure' -count=1 -v
```

Expected: FAIL because `connectorFor` returns no Codex session selector and the driver returns immediately after session creation.

**Step 3: Implement ordered post-session selection**

Refactor `connectorFor` to return both the launch adapter and the appropriate optional session selector without weakening the consumer-owned interfaces.

After `createSession` succeeds, for native Codex only:

```go
if codex != nil {
    if err := codex.SelectModel(driverCtx, sess); err != nil {
        return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select model: %w", err))
    }
    if nativeEffort(cfg.Effort) != "" {
        if err := codex.SelectEffort(driverCtx, sess); err != nil {
            return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select effort: %w", err))
        }
    }
}
```

Implement the production seam by safely converting the driver session to `*client.Session` and delegating to `launch.CodexConnector`, matching the existing Claude pattern. The same code path must run after both `NewSession` and `LoadSession`.

**Step 4: Run GREEN verification**

Run:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache \
  go test ./driver/acp -run 'TestNew.*Codex.*SessionSelection|TestRestore.*Codex.*SessionSelection|TestCodex.*SelectionFailure' -count=1 -v
GOCACHE=/private/tmp/codex-acp-session-selection-gocache go test -race ./driver/acp -count=1
go vet ./driver/acp
git diff --check
```

Expected: PASS.

**Step 5: Commit**

```bash
git add driver/acp/driver.go driver/acp/driver_test.go
git commit -m "fix: apply Codex selection after session open"
```

### Task 3: Refresh vendor, verify Carbon integration, and document the correction

**Files:**

- Modify through vendor command: `foreignloops/vendor/github.com/looprig/acp/launch/*`
- Modify: `carbon/docs/plans/2026-08-08-codex-acp-session-selection-design.md` only if implementation reveals a factual correction
- Test: `carbon/internal/app/acpchildren_test.go`
- Test: `carbon/internal/app/agent_tools_integration_test.go`

**Step 1: Add or tighten the Carbon regression**

Add a focused integration assertion proving the resolved runtime passed to the foreign driver contains adapter model `gpt-5.6-luna` and effort `max`, and that lazy child construction—not startup composition—owns session selection. Reuse existing builder seams; do not add production test hooks.

**Step 2: Verify the regression before vendor refresh**

Run:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache \
  go test ./internal/app -run 'TestACPChild.*AdapterModel|TestAgentTools.*Luna.*Max|TestNewACPComposition.*Preflight' -count=1 -v
```

Expected: existing propagation tests may already pass; if so, document that this layer was not the failing boundary and do not add a redundant test. The Task 1 and Task 2 RED tests remain the required bug reproduction.

**Step 3: Refresh foreignloops vendor**

Run the documented vendor flow with workspace mode disabled and local ACP replacement active:

```bash
GOWORK=off make vendor
```

Inspect the diff. It must contain only the reviewed ACP connector/session-selector source and module metadata required by that source. Reject unrelated dependency churn.

**Step 4: Run final verification sequentially**

Run from each owning worktree:

```bash
GOCACHE=/private/tmp/codex-acp-session-selection-gocache go test -race ./...
CGO_ENABLED=0 go build -trimpath ./...
go vet ./...
git diff --check
```

Run the Carbon suite with test-only loopback permission if the sandbox blocks `127.0.0.1:0`.

Then launch a fresh configured Codex child and inspect the live behavior. Success criteria:

- the session reports `gpt-5.6-luna` and `max`, or
- the adapter returns a bounded lazy selection error stating that exact model or effort is unavailable.

It must never silently remain on `gpt-5.6-sol`/`medium`.

**Step 5: Commit vendor and any non-redundant Carbon regression**

```bash
git add vendor
git commit -m "chore: refresh Codex ACP session selector vendor"
```

If Carbon gained a meaningful regression:

```bash
git add internal/app/*_test.go
git commit -m "test: cover Codex ACP session selection boundary"
```

### Task 4: Final review and integration

**Files:** None unless review finds a defect.

**Step 1: Review requirements**

Confirm line by line:

- fresh and restored sessions select model then effort;
- Luna/max is applied via ACP session configuration;
- model-only and managed selections retain their semantics;
- unavailable values fail lazily and visibly;
- resource cleanup remains exactly once;
- native model/effort launch arguments are gone;
- no environment variable carries effort;
- startup remains free of live ACP model validation.

**Step 2: Run independent spec and quality reviews**

Use fresh reviewers to inspect all feature commits and the installed adapter contract. Fix review findings with a new RED/GREEN cycle.

**Step 3: Merge locally only after verification**

Use the branch-finishing workflow. Do not push unless explicitly requested.
