# Native ACP Model and Effort Allowlist Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `native_acp.models` an authoritative model-and-effort allowlist applied lazily by `StartAgent`, and return bounded ACP wire error code/message details to the parent agent.

**Architecture:** CodeRig extends its strict version-2 configuration with structured native model entries and compiles their efforts directly into Harness's existing runtime catalogue. ACP launch owns adapter-specific model/effort application; foreignloops projects only ACP JSON-RPC code/message into a model-facing terminal error; Harness preserves that explicitly safe detail in foreground and background delegation results. CodeRig stops opening ACP sessions at startup to validate configured model availability.

**Tech Stack:** Go 1.26.4, strict `encoding/json`, ACP JSON-RPC/client/launch packages, foreignloops ACP driver/backend, Harness runtime catalogue and delegation tools, CodeRig composition, existing vendored sibling modules.

---

## Baseline

Worktrees live under `/Users/ipotter/code/looprig/.worktrees/native-acp-model-effort/` with one worktree each for `acp`, `foreignloops`, `harness`, and `coderig`. The local `go.work` binds those four worktrees and the unchanged Core/Eval/Inference/Storage siblings.

Baseline evidence on 2026-08-08:

- `acp`: `go test ./...` passed.
- `harness`: `go test ./...` passed.
- `foreignloops`: the full parallel baseline timed out in the existing empty-environment Claude/Codex process tests; each failing test passed immediately in isolation.
- `coderig`: the full baseline hit a `TempDir` cleanup race in `TestProcessToolsSiblingLoopsCannotAccessEachOthersHandles`; the test passed immediately in isolation.

Use `GOCACHE=/private/tmp/native-acp-model-effort-gocache` for all commands. Run required final suites with `-race` per each module's contributor instructions.

### Task 1: Extend CodeRig's strict native ACP configuration

**Files:**

- Modify: `coderig/internal/app/modelconfig.go`
- Modify: `coderig/internal/app/modelconfig_normalize.go`
- Modify: `coderig/internal/app/modelconfig_digest.go`
- Modify: `coderig/internal/app/modelconfig_native_test.go`
- Modify: `coderig/internal/app/modelconfig_digest_test.go`

**Step 1: Write failing decode and normalization tests**

Add tests proving that `native_acp.<harness>.models` accepts both legacy strings and strict structured values:

```go
{"model":"gpt-5.6-sol","efforts":["medium"],"default_effort":"medium"}
```

Assert:

- omitted `models` remains distinguishable from a present list;
- a legacy string normalizes to the existing model-only/`none` behavior;
- a structured entry preserves its model, exact effort allowlist, and default;
- unknown fields, empty efforts, duplicate efforts/models, unsupported efforts, padded/invalid model IDs, and defaults outside the list fail with `*ModelConfigError`;
- structured-entry order does not affect the secret-free digest, but changing a model, effort, or default does.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./internal/app -run 'TestModelConfigNativeACP|TestModelConfigRejectsInvalidNativeACP|TestModelConfigNativeACPIdentity' -count=1 -v
```

Expected: decode fails because `models` is still `[]string`, or normalized profiles lack effort fields.

**Step 3: Implement the minimal strict union decoder**

Introduce private wire and normalized values shaped like:

```go
type nativeACPModelConfig struct {
    Model         string
    Efforts       []string
    DefaultEffort string
    Legacy        bool
}

type normalizedNativeACPModel struct {
    Model         string
    Efforts       []model.Effort
    DefaultEffort model.Effort
}
```

Give the wire type an `UnmarshalJSON` method that accepts either one JSON string or one strict object. Decode the object with `DisallowUnknownFields`, require exactly one top-level value, and reuse existing alias/effort validation helpers. Preserve a nil `Models` pointer for harness-managed mode. Sort normalized models by ID and efforts by neutral effort rank before digesting.

Do not expose credentials or raw file content through errors.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2. Expected: PASS.

**Step 5: Commit CodeRig schema changes**

```bash
git add internal/app/modelconfig.go internal/app/modelconfig_normalize.go internal/app/modelconfig_digest.go internal/app/modelconfig_native_test.go internal/app/modelconfig_digest_test.go
git commit -m "feat: configure native ACP model efforts"
```

### Task 2: Compile configured native efforts into StartAgent without live preflight

**Files:**

- Modify: `coderig/internal/app/productionmodels.go`
- Modify: `coderig/internal/app/acpcatalog.go`
- Modify: `coderig/internal/app/acpproduction.go`
- Modify: `coderig/internal/app/acpchildren.go`
- Modify: `coderig/internal/app/acpnative_phase3_test.go`
- Modify: `coderig/internal/app/acpchildren_test.go`
- Modify: `coderig/internal/app/agent_tools_integration_test.go`

**Step 1: Write failing catalogue and no-preflight tests**

Add tests proving:

- configured native models compile to `loop.RuntimeModelOption` with exact `Efforts` and `DefaultEffort`;
- generated `StartAgent` schema exposes only those model/effort combinations;
- omitted `models` remains a model-less harness-managed branch;
- production composition does not call the ACP session preflight callback for model availability;
- configured native rows remain in the catalogue regardless of transient adapter availability;
- static invalid config, missing executable configuration, and access-profile validation remain fail-closed.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./internal/app -run 'TestACP.*Native|TestNewACPComposition.*Preflight|TestAgentTools.*Native' -count=1 -v
```

Expected: native options still expose only `EffortNone`, and composition invokes live model preflight/filtering.

**Step 3: Implement catalogue propagation and lazy availability**

Change `ACPNativeProfile.Models` from aliases alone to typed native model choices carrying alias, efforts, and default. Build runtime options from those values instead of hard-coding `EffortNone`.

Split static ACP composition from live child launch. Do not invoke `preflightProductionACPExecutable` or filter configured runtime rows based on session/model availability during CodeRig startup. Preserve executable resolution and clean-path/runnable checks without starting an ACP process. Keep the preflight helpers only if gateway construction tests or another explicit diagnostic API still owns them; otherwise remove dead production plumbing and update comments.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2. Expected: PASS.

Also run:

```bash
go test ./internal/app -run 'TestModelConfig|TestProductionModels|TestACP|TestAgentTools' -count=1
```

Expected: PASS.

**Step 5: Commit CodeRig catalogue changes**

```bash
git add internal/app/productionmodels.go internal/app/acpcatalog.go internal/app/acpproduction.go internal/app/acpchildren.go internal/app/*_test.go
git commit -m "feat: lazily select native ACP runtimes"
```

### Task 3: Apply model and effort at the ACP launch boundary

**Files:**

- Modify: `acp/launch/codex_connector.go`
- Modify: `acp/launch/codex.go`
- Modify: `acp/launch/codex_connector_test.go`
- Modify: `acp/launch/codex_test.go`
- Modify: `acp/launch/claude_connector.go`
- Modify: `acp/launch/claude_connector_test.go`

**Step 1: Write failing Codex and Claude selector tests**

Codex tests should construct a connector with base model `gpt-5.6-sol` and effort `medium`, then assert native argv contains separate immutable overrides:

```text
-c model=gpt-5.6-sol
-c model_reasoning_effort=medium
```

Assert omitted model/effort emits neither override, a partial tuple is rejected, and `WithModelEffort` returns an independent connector.

Claude tests should expose model and thought-level config options, select model `sonnet`, then effort `high`, and verify ordered `session/set_config_option` calls. Missing/unadvertised effort must return a typed, bounded selection error without issuing the invalid wire call. Omitted selectors remain no-ops.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./launch -run 'TestCodex.*Effort|TestClaude.*Effort' -count=1 -v
```

Expected: connector types and effort selection methods do not exist.

**Step 3: Implement adapter-owned selection**

Add a neutral effort string to `CodexConnector` and build `model_reasoning_effort` only for a complete explicit native selection. Keep gateway behavior compatible and immutable.

Extend Claude's small consumer-owned session interface with the existing config-option operation only; add `SelectEffort` that resolves category `thought_level` and applies the advertised value. Generalize the current model-option resolver without accepting arbitrary categories or values.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2, then:

```bash
go test ./launch -count=1
go test -race ./launch -count=1
```

Expected: PASS.

**Step 5: Commit ACP changes**

```bash
git add launch/codex_connector.go launch/codex.go launch/codex_connector_test.go launch/codex_test.go launch/claude_connector.go launch/claude_connector_test.go
git commit -m "feat: apply native ACP model effort"
```

### Task 4: Thread the selected effort through the foreign ACP driver

**Files:**

- Modify: `foreignloops/driver/acp/config.go`
- Modify: `foreignloops/driver/acp/driver.go`
- Modify: `foreignloops/driver/acp/driver_test.go`
- Modify: `foreignloops/driver/acp/config_test.go`

**Step 1: Write failing configuration/application tests**

Add tests proving:

- explicit native selection requires model and effort together for the new structured path;
- harness-managed native selection permits both empty;
- Codex construction receives both model and effort;
- Claude construction applies model first and effort second;
- a typed selection error closes the owned ACP process/session exactly once;
- effort never becomes an environment variable.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./driver/acp -run 'Test.*Effort|TestNew.*Selection' -count=1 -v
```

Expected: `Config` and connector seams have no effort field/method.

**Step 3: Implement minimal effort threading**

Add `Effort` to `acp.Config`, validation, launch connector construction, and the Claude connector seam. Keep Harness-managed empty values unchanged. Do not interpret adapter-specific combined model IDs in this module.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2, then:

```bash
go test -race ./driver/acp -count=1
```

Expected: PASS.

**Step 5: Commit foreignloops selection changes**

```bash
git add driver/acp/config.go driver/acp/driver.go driver/acp/config_test.go driver/acp/driver_test.go
git commit -m "feat: thread native ACP effort"
```

### Task 5: Project bounded ACP wire errors into foreign-loop failures

**Files:**

- Modify: `foreignloops/driver/driver.go`
- Modify: `foreignloops/driver/acp/turn.go`
- Modify: `foreignloops/driver/acp/turn_test.go`
- Modify: `foreignloops/backend/errors.go`
- Modify: `foreignloops/backend/mapper.go`
- Modify: `foreignloops/backend/turn.go`
- Modify: `foreignloops/backend/errors_test.go`
- Modify: `foreignloops/backend/mapper_test.go`
- Modify: `foreignloops/backend/turn_test.go`

**Step 1: Write failing safe-projection tests**

Create ACP prompt failures carrying a `protocol.Error`/`protocol.Fault` with:

- a numeric code;
- message `Usage limit reached; resets at 3:00 PM`;
- `Data`, wrapped cause, path, URL, token, and multiline/control-character sentinels.

Assert the terminal event and resulting `event.TurnFailed.Err` retain only a bounded single-line string like:

```text
ACP error -32000: Usage limit reached; resets at 3:00 PM
```

Assert all data/cause/stderr/path/token sentinels are absent. Non-protocol failures remain fixed-category. Add max-length and invalid-UTF-8 boundary cases.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./driver/acp ./backend -run 'Test.*ACP.*Error|Test.*ModelFacing.*Failure' -count=1 -v
```

Expected: ACP prompt errors collapse to `acp prompt failed`, and backend has no explicit model-facing error type.

**Step 3: Implement explicit safe failure types**

Add a boolean/specific variant to `driver.Event` that marks only already-projected model-facing details. In `driver/acp`, use `errors.As` for ACP wire errors, copy only exported `Code` and `Message`, normalize to one line, replace invalid UTF-8, and cap bytes. Never include `Data` or `Unwrap()` causes.

Map a safe event to a dedicated backend error type implementing:

```go
type modelFacingError interface {
    ModelFacingError() string
}
```

Ordinary `ForeignResultError` remains non-model-facing so other foreign drivers cannot accidentally expose arbitrary provider output.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2, then:

```bash
go test -race ./driver/acp ./backend -count=1
```

Expected: PASS.

**Step 5: Commit foreignloops error changes**

```bash
git add driver/driver.go driver/acp/turn.go driver/acp/turn_test.go backend/errors.go backend/mapper.go backend/turn.go backend/*_test.go
git commit -m "feat: preserve safe ACP failure details"
```

### Task 6: Preserve safe child failure details through StartAgent

**Files:**

- Modify: `harness/internal/sessionruntime/delegation.go`
- Modify: `harness/internal/sessionruntime/delegation_test.go`
- Modify: `harness/internal/delegationtool/start_agent.go`
- Modify: `harness/internal/delegationtool/agent_tools_test.go`

**Step 1: Write failing foreground, background, and restore tests**

Use a child `TurnFailed.Err` implementing `ModelFacingError() string`. Assert:

- foreground `DelegateResult.Response` contains the bounded detail and `StartAgent` returns `error: agent failed: <detail>`;
- a background completion envelope carries the same detail;
- restored background reconciliation derives the same result from the durable `TurnFailed` event;
- an ordinary error that lacks the marker remains `error: agent failed`;
- oversized marked details are bounded by the existing delegate output limit.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./internal/sessionruntime ./internal/delegationtool -run 'Test.*FailureDetail|Test.*Failed.*StartAgent' -count=1 -v
```

Expected: failed drains and restored folds discard the error detail; formatter ignores failed response text.

**Step 3: Implement structural safe-detail extraction**

At the Harness boundary, use `errors.As` only for the narrow structural interface:

```go
interface { ModelFacingError() string }
```

Extract from `drainFailedError` and durable `TurnFailed.Err`, bound with existing UTF-8 helpers, and place the result in `DelegateResult.Response`. Update failed foreground formatting to include non-empty safe detail. Do not stringify arbitrary errors.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2, then:

```bash
go test -race ./internal/sessionruntime ./internal/delegationtool -count=1
```

Expected: PASS.

**Step 5: Commit Harness changes**

```bash
git add internal/sessionruntime/delegation.go internal/sessionruntime/delegation_test.go internal/delegationtool/start_agent.go internal/delegationtool/agent_tools_test.go
git commit -m "feat: return safe delegate failure details"
```

### Task 7: Integrate lazy runtime errors in CodeRig

**Files:**

- Modify: `coderig/internal/app/acpchildren.go`
- Modify: `coderig/internal/app/acpchildren_test.go`
- Modify: `coderig/internal/app/agent_tools_integration_test.go`

**Step 1: Write failing child-construction tests**

Inject ACP JSON-RPC errors during native child construction and assert CodeRig's model-facing error contains only bounded code/message. Assert arbitrary internal errors still collapse to `coderig: ACP child unavailable`, and cancellation/deadline identity remains intact.

Also assert the selected `loop.Resolved.Effort` reaches `acpdriver.Config.Effort` for both Codex and Claude.

**Step 2: Run tests and verify RED**

Run:

```bash
go test ./internal/app -run 'TestBoundedACPChildError|TestACPChild.*Effort|TestAssembledStartAgent.*ACP.*Failure' -count=1 -v
```

Expected: `boundedACPChildError` discards protocol code/message and child config omits effort.

**Step 3: Implement CodeRig's product boundary**

Thread resolved effort into `acpdriver.Config`. Replace the single sentinel-only boundary with a typed model-facing error only when the cause is an ACP wire error; copy only bounded code/message. Keep fixed errors for all other causes.

**Step 4: Run tests and verify GREEN**

Run the focused command from Step 2, then:

```bash
go test ./internal/app -run 'TestACP|TestAgentTools' -count=1
```

Expected: PASS.

**Step 5: Commit CodeRig integration changes**

```bash
git add internal/app/acpchildren.go internal/app/acpchildren_test.go internal/app/agent_tools_integration_test.go
git commit -m "feat: surface safe ACP child errors"
```

### Task 8: Update guidance, vendor trees, and local operator configuration

**Files:**

- Modify: `coderig/CLAUDE.md`
- Modify: `coderig/CONTRIBUTING.md`
- Modify generated vendor trees only through each module's `make vendor`
- Modify outside repository after approval: `~/.looprig/coderig/models.json`

**Step 1: Update current documentation**

Document structured native entries, omitted/harness-managed semantics, lazy runtime validation, and bounded ACP code/message propagation. Remove statements that explicit native efforts are always `none` or model availability is preflighted at startup.

**Step 2: Refresh vendored local modules in dependency order**

Run the owning module's documented `make vendor` flow after source tests are green:

1. ACP.
2. Harness.
3. foreignloops with updated ACP/Harness.
4. CodeRig with updated ACP/foreignloops/Harness.

Inspect every vendor diff and reject unrelated version churn or embedded VCS metadata.

**Step 3: Run module security checks before commits**

For each affected module, run its required `make secure`/lint/build commands. If a network-only vulnerability database check is unavailable, report that separately; do not treat it as a source/test pass.

**Step 4: Commit documentation/vendor changes per repository**

Use module-scoped commits such as:

```bash
git commit -m "docs: document native ACP allowlists"
git commit -m "chore: refresh vendored looprig modules"
```

**Step 5: Update the operator-managed config safely**

Use a secret-preserving JSON transformation against `~/.looprig/coderig/models.json`; never print API keys. Set `acp_launchers.claude-code.executable` to the installed `claude-agent-acp` path and install exactly the approved structured entries:

```json
"codex": [
  {"model":"gpt-5.6-sol","efforts":["medium"],"default_effort":"medium"},
  {"model":"gpt-5.6-luna","efforts":["max"],"default_effort":"max"}
]
```

```json
"claude-code": [
  {"model":"sonnet","efforts":["high"],"default_effort":"high"},
  {"model":"opus","efforts":["high"],"default_effort":"high"},
  {"model":"fable","efforts":["medium"],"default_effort":"medium"}
]
```

Preserve owner-only mode `0600`, every unrelated field, and all credential values.

### Task 9: Final verification

**Files:** None unless verification exposes a defect; any fix restarts a focused RED/GREEN cycle.

**Step 1: Run focused cross-module tests**

```bash
go test -race ./launch -count=1
go test -race ./driver/acp ./backend -count=1
go test -race ./internal/sessionruntime ./internal/delegationtool -count=1
go test -race ./internal/app -run 'TestModelConfig|TestProductionModels|TestACP|TestAgentTools' -count=1
```

Run each command from its owning worktree.

**Step 2: Run full affected-module suites sequentially**

```bash
go test -race ./...
```

Run separately in ACP, foreignloops, Harness, and CodeRig to avoid the baseline process-test timing flakes caused by four concurrent suites.

**Step 3: Build and inspect**

```bash
CGO_ENABLED=0 go build -trimpath ./...
git diff --check
git status --short
```

Run in every affected worktree and inspect all diffs/commits.

**Step 4: Verify the real config without exposing secrets**

Run a projection that prints only `version`, `native_acp`, and `acp_launchers`, validate mode `0600`, and start CodeRig far enough to prove startup performs no ACP session/model validation.

**Step 5: Exercise lazy selections**

Launch one configured Codex pair and one configured Claude pair through the assembled `StartAgent` path. Confirm either successful execution or a parent-visible bounded ACP code/message. Confirm an unavailable selection fails at `StartAgent`, not CodeRig startup, and does not disclose ACP data/stderr/internal causes.

**Step 6: Review requirements line by line**

Confirm:

- omitted model list gives harness-managed freedom;
- configured list is the only advertised model/effort set;
- no startup model/effort validation occurs;
- selected pair is applied adapter-correctly;
- useful ACP code/message reaches the parent;
- sensitive error channels remain excluded;
- persistence remains exact and fail-closed;
- config credentials and unrelated user changes are preserved.
