# SWE Harness Rig Migration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Migrate SWE from manual harness session, workspace, and subagent wiring to the final immutable rig API without retaining compatibility paths.

**Architecture:** Use one builder to define operator-primary, operator, and reviewer loops
and create an immutable rig for each resolved `Open` (config and mismatch policy arrive at
that boundary). Create/restore sessions exclusively through that rig. Build tools,
permissions, sandbox runners, and skills from per-loop bindings; use harness-managed
delegation and native workspace checkpoints. Adapt the resulting
`session.SessionController` to the already-migrated CLI contract.

**Tech Stack:** Go 1.26, harness rig/loop/session/tool APIs, fsstore, sessionstore, workspacestore, inference, sandbox, Bubble Tea CLI adapter.

---

## Preconditions

- The reviewed harness release containing `rig.Define`, variadic `Rig.NewSession`, `Rig.RestoreSession`, managed delegation, workspace placement, and native snapshots is tagged as the version selected below (expected `v0.10.0`; use the actual released tag).
- The CLI migration described by `../cli/docs/plans/2026-07-11-harness-rig-migration-implementation.md` is released first.
- The selected harness release owns lease-guarded session offload-blob GC for both new and
  restored rig sessions. If it does not, stop: SWE cannot preserve `scheduleGC` because rig
  intentionally hides the journal lease, and deleting the ticker would regress collection.
- Run all commands with `GOWORK=off`. Keep the repository's relative `replace` directives for local private modules.
- Do not begin with production rewrites. Each task adds failing tests, proves the expected failure, implements one seam, verifies it, and commits.

### Task 0: Land blocking harness metadata and offload-GC support

**Repository:** `github.com/looprig/harness` (not SWE)

**Files:**
- Create: `docs/plans/2026-07-12-rig-loop-display-and-offload-gc-design.md`
- Create: `pkg/rig/offload_gc.go`
- Create: `internal/sessionruntime/offload_gc.go`
- Create: `internal/sessionruntime/offload_gc_test.go`
- Modify: `pkg/loop/definition.go`
- Modify: `pkg/loop/definition_test.go`
- Modify: `pkg/event/event.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/tools/subagent.go`
- Modify: `pkg/tools/subagent_test.go`
- Modify: `internal/sessionruntime/delegation.go`
- Modify: `internal/sessionruntime/delegation_test.go`
- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/restore_constructor.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Modify: `internal/sessionruntime/lifecycle_test.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/rig_test.go`
- Modify: `pkg/sessionstore/gc.go`
- Modify: `pkg/sessionstore/gc_test.go`

Do not start SWE Task 1 until this harness task is implemented, reviewed, tagged, and its
commit/tag is recorded here.

**Step 1: Write failing definition metadata tests**

Pin `loop.WithDisplayName` and `loop.WithDescription` in the listed harness design amendment.
Display/description are immutable, defensively copied, fingerprinted, and
restored. `LoopStarted` carries display name while its existing agent name remains the topology
key. Managed Subagent catalogs render target display names/descriptions and resolve a selected
display name to the allowed definition key. Display aliases must be unique among one parent's
allowed delegate targets, but the non-delegate primer and operator leaf may both display as
`operator`.

Prove an `operator-primary` key displays as `operator`, operator/reviewer descriptions reach
`Subagent.Info`, old events fall back to the topology key, and leaves stay delegate-free.

**Step 2: Write failing rig-owned offload-GC tests**

Pin this explicit policy API in the design amendment:

```go
rig.WithOffloadGC(rig.OffloadGCPolicy{
    Interval: 5 * time.Minute,
    Timeout:  time.Minute,
})
```

Use a manual tick seam in tests. New and restored rig sessions must run
`sessionstore.OpenObjectGC` only while their session lease is held. Introduce one internal
reader/writer journal-admission gate at the common `SessionJournal.Append` wrapper, covering
the complete event, command, `GatePreparedRecord`, `GateOpened`, and `GateResolved`
append/offload calls plus the GC scan/delete transaction. Every append takes a reader; GC takes the writer
only after native `SessionIdle`. A forced test blocks an offload before its pointer append and
proves GC cannot scan/delete until that append completes; a second forced gate-record offload
proves the same barrier. Shutdown stops and joins GC before
appending `SessionStopped` and before lease release; lease loss cancels it. This is session
offload GC, never workspace snapshot GC.

**Step 3: Verify RED**

```bash
GOWORK=off go test -race ./pkg/loop ./pkg/event ./pkg/tools ./pkg/rig ./pkg/sessionstore ./internal/sessionruntime -run 'Test.*Display|Test.*DelegateCatalog|Test.*OffloadGC'
```

Expected: missing metadata and GC policy/lifecycle APIs.

**Step 4: Implement and verify**

Run:

```bash
GOWORK=off go test -race ./pkg/loop ./pkg/event ./pkg/rig ./pkg/sessionstore ./internal/sessionruntime
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off go vet ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
git diff --check
```

Then commit in the harness repository:

```bash
git add docs/plans/2026-07-12-rig-loop-display-and-offload-gc-design.md pkg/loop pkg/event pkg/rig pkg/sessionstore internal/sessionruntime
git commit -m "feat(rig): own loop metadata and session object gc"
```

Release a harness tag containing this commit. Supporting restored-session collection is a new
improvement: current SWE schedules its ticker only for new sessions.

### Task 0.5: Amend and release CLI display-name rendering (CLI repository)

**Files:**
- Modify: `docs/plans/2026-07-11-harness-rig-migration-design.md`
- Modify: `docs/plans/2026-07-11-harness-rig-migration-implementation.md`
- Modify: `tui/transcript.go`
- Modify: `tui/transcript_test.go`
- Modify: `tui/sessioncore.go`
- Modify: `tui/sessioncore_test.go`

After Task 0's harness tag is selected, add failing CLI projection tests: `LoopStarted` with a
display name renders/stores that name, missing display name falls back to `Header.AgentName`,
and active/focus identity remains keyed by loop ID. Update the approved CLI plan, bump the
harness dependency/vendor, implement the projection, run CLI unit/race/vendor gates, and commit:

```bash
git add docs/plans/2026-07-11-harness-rig-migration-*.md tui/transcript.go tui/transcript_test.go tui/sessioncore.go tui/sessioncore_test.go go.mod go.sum vendor
git commit -m "feat(tui): render loop display identities"
```

Release CLI and record its tag before SWE Task 1.

### Task 1: Upgrade harness and CLI dependencies before using final symbols

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `vendor/`

**Step 1: Record the expected dependency surface**

Add a temporary compile test in `swarms/swe/dependency_test.go` that references:

```go
var _ = rig.Define
var _ = loop.Define
var _ session.SessionController
var _ = event.ActiveLoopChanged{}
var _ = event.LoopStarted{DisplayName: "operator"}
var _ tui.Agent
var _ = loop.WithDisplayName
var _ = rig.WithOffloadGC
```

The last assertion must use the migrated CLI interface, including `ActiveLoopID` and loop-targeted image capability.

Record the Task 0 harness commit/tag in this plan before updating dependencies.

**Step 2: Verify RED**

Run:

```bash
GOWORK=off go test -race ./swarms/swe -run TestDependencySurface
```

Expected: compile failure against harness `v0.5.2` / CLI `v0.3.1` because final symbols or method sets are absent.

**Step 3: Update and vendor**

Update `github.com/looprig/harness` and `github.com/looprig/cli` to the reviewed releases, then run:

```bash
GOWORK=off go mod tidy
GOWORK=off go mod vendor
```

Do not use an unreleased pseudo-version accidentally. Verify `vendor/modules.txt` records both selected versions. Vendor is package-pruned; verify imported packages/symbols rather than expecting every harness directory.

**Step 4: Verify GREEN**

```bash
GOWORK=off go test -race ./swarms/swe -run TestDependencySurface
GOWORK=off go test ./... -run '^$'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add go.mod go.sum vendor swarms/swe/dependency_test.go
git commit -m "build: upgrade harness rig and cli contracts"
```

### Task 2: Convert leaf tools into immutable per-binding definitions

**Files:**
- Modify: `swarms/swe/agents.go:18-184`
- Modify: `swarms/swe/agents_test.go`
- Modify: `agents/operator/operator.go:60-130`
- Modify: `agents/operator/operator_test.go`
- Modify: `agents/reviewer/reviewer.go:55-120`
- Modify: `agents/reviewer/reviewer_test.go`
- Modify: `swarms/swe/confinement.go:64-102`
- Modify: `swarms/swe/confinement_test.go`
- Modify: `swarms/swe/skills_wiring_test.go`
- Modify: `swarms/swe/security.go`
- Modify: `swarms/swe/security_test.go`
- Modify: `swarms/swe/errors.go`
- Modify: `swarms/swe/errors_test.go`
- Modify: `swarms/swe/model.go`
- Modify: `swarms/swe/model_test.go`

**Step 1: Write failing binding-isolation tests**

Replace tests that expect `loop.ToolSet` with tests that bind each definition twice using different `tool.Bindings`:

- roots differ and every file/Bash/Skill tool uses its own root;
- permission checker instances differ;
- sandbox executor/read-only view instances differ;
- operator tools include write/edit; reviewer remains read-only;
- runtime workspace skills use the bound root and retain embedded-wins behavior;
- a missing workspace binding fails closed for workspace-required definitions.

Also assert produced tool names match declared `ProducedToolNames`; this catches stale bundle metadata.

**Step 2: Verify RED**

```bash
GOWORK=off go test -race ./agents/operator ./agents/reviewer ./swarms/swe -run 'Test.*Definition|Test.*Binding|Test.*Confinement|Test.*Skill'
```

Expected: compile failures because leaf builders return live `loop.ToolSet` and capture `deps.Root`.

**Step 3: Implement immutable factories**

Change `leafBuiltin.build` and agent package builders to return `[]tool.Definition`. Use `tool.NewDefinition`/`NewBundleDefinition` for SWE-specific tools and harness definitions for standard tools. Every factory reads `bindings.Workspace.Root` and constructs fresh mutable collaborators.

Add a `loop.PermissionFactory` per definition. It builds the current checker/posture from immutable policy and the session ceiling source. Add/update `loop.WithPolicyRevision` whenever permission/runtime-context policy changes.

Do not capture a live tool, permission checker, sandbox executor, file observation set, or session root in the rig definition.

**Step 4: Verify GREEN**

Run the Step 2 command plus:

```bash
GOWORK=off go test -race ./confine ./swarms/swe
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agents swarms/swe/agents.go swarms/swe/agents_test.go swarms/swe/confinement.go swarms/swe/confinement_test.go swarms/swe/skills_wiring_test.go
git commit -m "refactor(swe): define tools per loop binding"
```

### Task 3: Define the three-loop topology and managed delegation

**Files:**
- Modify: `swarms/swe/swarm.go:48-330`
- Modify: `swarms/swe/swarm_test.go`
- Modify: `swarms/swe/registry.go`
- Modify: `swarms/swe/registry_test.go`
- Modify: `swarms/swe/greeting.go`
- Modify: `swarms/swe/greeting_test.go`
- Modify: `swarms/swe/runtime_context.go`
- Modify: `swarms/swe/runtime_context_test.go`
- Delete: `swarms/swe/spawner.go`
- Delete: `swarms/swe/spawner_test.go`
- Modify: `swarms/swe/acceptance_test.go`

**Step 1: Write the topology and managed-Subagent tests**

Tests must prove:

- definitions are named `operator-primary`, `operator`, `reviewer`;
- primer display name/description remain the current operator identity/catalog text;
- only `operator-primary` is a primer and active primer;
- only `operator-primary` declares delegates;
- primer-minus-Subagent has the same tool policy/prompt identity as operator leaf;
- leaf definitions cannot start a delegate;
- managed start validates unknown agent and optional mode;
- `wait=true` returns the child final result;
- `wait=false` returns delegate/request IDs, followed by status, send follow-up, wait, and interrupt;
- quota/depth errors remain typed and no child is registered on refusal;
- the existing `Depth: 2` admits exactly one direct primary-to-leaf spawn (`Depth: 1`
  remains a negative regression);
- async requests resolve independently by request ID;
- restored delegates retain ownership and can receive follow-up.

Use harness event/controller observations. Do not test a new custom spawner abstraction.

**Step 2: Verify RED**

```bash
GOWORK=off go test -race ./swarms/swe -run 'TestOperatorTopology|TestManagedSubagent|TestAsyncDelegate|TestRestoredDelegate'
```

Expected: compile/test failure while `loop.Config`, `swarmSpawner`, and custom Subagent remain.

**Step 3: Build immutable definitions**

Create helpers returning `loop.Definition` via `loop.Define`. The primary uses:

```go
loop.WithDelegates(operator.Name, reviewer.Name)
loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged})
```

Use the existing operator/reviewer prompt and model selection. Preserve current single-mode behavior unless SWE already exposes named modes; do not add speculative modes. Apply `loop.WithRuntimeContext`, tool limits, and stable policy revision.

Delete `swarmSpawner`, its late `bind`, `subagentRunner`, custom catalog-to-tool wiring, and `tools.NewSubagent` from SWE. Keep the registry only if it remains the source of prompt/catalog/skill metadata; it no longer creates loop configs or runs children.

**Step 4: Verify GREEN**

Run Step 2 and all `swarms/swe` tests.

**Step 5: Commit**

```bash
git add swarms/swe
git commit -m "refactor(swe): declare managed delegate topology"
```

### Task 4: Compose one rig and delete manual persistence/checkpoint lifecycle

**Files:**
- Modify: `swarms/swe/persistence.go:21-418`
- Modify: `swarms/swe/persistence_test.go`
- Modify: `swarms/swe/persistence_integration_test.go`
- Modify: `swarms/swe/swarm.go`
- Modify: `swarms/swe/fingerprint_test.go`
- Modify: `swarms/swe/operator_eval_integration_test.go`

**Step 1: Write failing rig lifecycle tests**

Using actual fsstore integration fixtures, prove:

- one shared `SessionStoreFactory` provides stores to the single rig-builder path;
- each `Open` builds one immutable rig from that call's resolved client/config/root and
  mismatch flag; a later differing config is not served by a cached prior rig;
- new session ID is read from the returned controller, not minted by SWE;
- resume calls `Rig.RestoreSession` with the selected ID;
- idle native checkpoint produces `WorkspaceCheckpointed` without a watcher;
- restored workspace and topology are ready before first submit;
- active loop, direct model/effort change, ceiling, gates, and checkpoint policy survive restore;
- persistence paths outside the workspace pass; overlap fails at `rig.Define`;
- close/shutdown releases leases exactly once;
- list/catalog behavior remains unchanged.
- exported `swe.New` uses the same rig builder over one process-shared ephemeral memstore
  composite; it neither calls a legacy constructor nor loses workspace tools, and two
  headless sessions contend on the same checkout until clean handoff.

Add a deletion guard that rejects `watchSessionEvents`, `CheckpointWorkspace`, `session.New`, `session.Restore`, manual `Acquire`, and session lifecycle options in SWE production.

**Step 2: Verify RED**

```bash
GOWORK=off go test -tags integration -race ./swarms/swe -run 'TestRig|TestPersistence|TestWorkspace|TestNoManualLifecycle'
```

Expected: failures while manual factory/watcher code remains.

**Step 3: Implement rig ownership**

At factory construction, open fsstore and create shared session/workspace facades. During
each `Open`, resolve client/config/root plus `AllowConfigMismatch`, build definitions, and call
`rig.Define` with:

- `WithLoops`, `WithPrimers`, `WithActivePrimer`;
- `WithSessionStore`;
- `WithExclusiveWorkspace(workspaceStore, canonicalWorkingDir, fsstoreLeaser)` to preserve
  the existing edit-the-open-checkout behavior;
- explicit `WithSnapshots(SnapshotOnIdle, SnapshotBestEffort, 60s)`;
- `WithDelegationLimits` matching current caps;
- `WithFingerprintFields` and `WithCeilingFactory`;
- `WithAllowConfigMismatch` only for the existing explicit flag.
- the Task 0 rig-owned offload-GC option with SWE's current interval/timeout.

`Open(new)` calls `Rig.NewSession`; `Open(resume)` calls `Rig.RestoreSession`. Delete ID minting, session/root lease handling, journal/appender construction, per-session GC ticker, `watchSessionEvents`, checkpoint timeout, and manual checkpoint calls.

Keep only process-level fsstore close and catalog/list/replay reads not provided by the live
session contract. Delete `scheduleGC` only after the precondition's rig-owned replacement is
verified for new and restored sessions.

For `swe.New`, use a process-shared `storage/memstore` composite and corresponding session and
workspace stores, then invoke the same rig builder with exclusive current-checkout placement.
This fencing is process-local, which matches the headless API's scope; durable CLI sessions use
fsstore for cross-process exclusion. Delete the headless `session.New` path; update eval and
runtime-skill tests to prove parity, contention, and release/handoff.

**Step 4: Verify GREEN**

Run Step 2, then:

```bash
GOWORK=off go test -race ./swarms/swe
GOWORK=off go test -tags integration -race ./swarms/swe
```

Expected: PASS.

**Step 5: Commit**

```bash
git add swarms/swe/persistence.go swarms/swe/persistence_test.go swarms/swe/persistence_integration_test.go swarms/swe/swarm.go swarms/swe/fingerprint_test.go swarms/swe/operator_eval_integration_test.go
git commit -m "refactor(swe): let rig own session persistence"
```

### Task 5: Migrate the SWE-to-CLI session adapter

**Files:**
- Modify: `swarms/swe/agent.go:21-330`
- Modify: `swarms/swe/agent_test.go`
- Modify: `swarms/swe/acceptance_test.go`
- Modify: `swarms/swe/persistence_integration_test.go`

**Step 1: Write failing adapter tests**

Pin the approved CLI contract:

- `ActiveLoopID` directly returns `sess.ActiveLoop().ID()`; the approved CLI owns durable
  selection reconciliation and per-loop running state;
- focus is initialized/reopened from active but later active changes do not steal it (CLI-side assertion);
- `AcceptsImages(loopID)` reads the current loop model; heterogeneous loops and `Controller.Change` update immediately;
- replay preserves session order for all-loop Enduring history;
- replay and the returned live subscription fold `GateOpened`/`GateResolved` into one
  adapter-owned `(LoopID, ToolExecutionID)`-to-GateID index plus reverse GateID removal map
  before forwarding events;
- gate responses use that index with `SessionController.RespondGate`, including restored
  delegate-loop open gates, resolved-gate removal, and the same ToolExecutionID in two loops;
- `Close` calls `Shutdown` once and does not cancel a second root or persistence watcher.

**Step 2: Verify RED**

```bash
GOWORK=off go test -race ./swarms/swe -run 'TestSessionAgent|TestRootLoop|TestActiveLoop|TestAcceptsImages|TestReplay|TestClose'
```

Expected: compile failures for cached primary/static image fields and pointer to old `session.Session`.

**Step 3: Implement the adapter**

Store a `session.SessionController`, replay dependency, and concurrency-safe
forward/reverse gate indexes. Restore opens one unnarrowed cold replay to fold gates for every
loop and materializes the all-loop Enduring backlog returned to CLI. Remove `rootCtx`, cancel,
teardown ticker, cached image bool, restored primary ID, and direct constructor helpers.

Map methods to final contracts. Image capability must be target-loop-specific and dynamic.
`ActiveLoopID` is a direct controller query. Return one wrapping subscription that indexes gate
events and forwards them; do not open an adapter-owned second subscription. CLI owns
subscribe-before-baseline, active/focus/running folding.

**Step 4: Verify GREEN**

Run Step 2 and the migrated CLI interface compile assertion.

**Step 5: Commit**

```bash
git add swarms/swe/agent.go swarms/swe/agent_test.go swarms/swe/acceptance_test.go swarms/swe/persistence_integration_test.go
git commit -m "refactor(swe): adapt rig sessions to cli"
```

### Task 6: Update command wiring and preserve operator UX

**Files:**
- Modify: `cmd/swe/main.go:179-249`
- Modify: `cmd/swe/main_test.go`
- Modify: `swarms/swe/greeting.go`
- Modify: `swarms/swe/greeting_test.go`
- Modify: `swarms/swe/operator_eval_integration_test.go`

**Step 1: Write failing composition tests**

Assert:

- `--resume` selects `RestoreSession`; new and `/clear` use `NewSession`;
- banner/greeting and user-visible operator identity are unchanged despite internal `operator-primary` name;
- the opener satisfies the migrated CLI contract;
- process shutdown closes live session before the shared store;
- no SWE serve adapter is introduced. (**SUPERSEDED 2026-08-27:** `carbon serve` adds a
  serve COMPOSITION in `cmd/carbon`, not an adapter — no Carbon `Runner` wrapper exists,
  and the boundary guard in `cmd/carbon/main_test.go` still fails any `pkg/serve` import
  under `internal/`. See the wui design §6.)

**Step 2: Verify RED**

```bash
GOWORK=off go test -race ./cmd/swe ./swarms/swe -run 'TestRun|TestOpen|TestGreeting|TestOperatorRunner'
```

Expected: failures against old opener/agent methods.

**Step 3: Update composition**

Keep `cli.Run` and `SessionStoreFactory` as the process composition seam. Pass the migrated opener/adapter. Remove old comments and helpers describing primary `loop.Config`, spawner binding, manual lease/GC, or checkpoint watcher.

Do not add serve code. A future HTTP composition would pass the real rig to generic `serve.Handler[S,O]` without a SWE Runner wrapper.

**SUPERSEDED 2026-08-27.** That future arrived, as `carbon serve`. Read the sentence
above as a constraint on the shape any such composition must take, not as advance
authorisation to build one — this task prohibits serve code outright. The prohibition is
overridden by `looprig/docs/plans/2026-08-27-wui-web-ui-design.md` §6 on that document's
own argument. What survives unchanged is the constraint itself: the real rig is passed to
the generic `serve.Handler[S,O]`, there is no Carbon `Runner` wrapper, and the composition
lives only at the process root.

**Step 4: Verify GREEN**

Run Step 2 and `go test -race ./cmd/swe ./swarms/swe`.

**Step 5: Commit**

```bash
git add cmd/swe swarms/swe/greeting.go swarms/swe/greeting_test.go swarms/swe/operator_eval_integration_test.go
git commit -m "refactor(swe): open rig sessions from cli"
```

### Task 7: Restore and asynchronous delegation regression matrix

**Files:**
- Modify: `swarms/swe/acceptance_test.go`
- Modify: `swarms/swe/persistence_integration_test.go`
- Modify: `swarms/swe/runtime_skills_integration_test.go`
- Create: `swarms/swe/rig_restore_integration_test.go`

**Step 1: Add end-to-end regressions**

With actual fsstore, cover:

1. New session performs operator work, changes active loop/mode/model/effort, checkpoints workspace, shuts down, restores from a fresh store instance, verifies state before submit, and continues.
2. Synchronous delegate returns its final text to the primary.
3. Async start returns IDs; parent observes status, sends follow-up, waits by request ID, and interrupts a second request.
4. Two concurrent delegates resolve independently; one completion cannot satisfy the other's wait.
5. Restored delegate ownership allows follow-up and rejects sibling/unrelated IDs.
6. Optional mode is accepted only when declared by the delegate definition.
7. Gate routing, runtime skills, sandbox clamp, and workspace root remain loop-correct after restore.
8. Required/best-effort snapshot failures retain their documented admission behavior.

No sleeps: use event subscriptions, request IDs, blocked fake inference channels, and context deadlines.

**Step 2: Verify RED then GREEN**

Before the preceding production tasks, this suite must fail to compile or fail old-spawner semantics. After migration:

```bash
GOWORK=off go test -tags integration -race ./swarms/swe -run 'TestRigRestore|TestManagedDelegate|TestAsync|TestRuntimeSkills'
```

Expected: PASS.

**Step 3: Commit**

```bash
git add swarms/swe/acceptance_test.go swarms/swe/persistence_integration_test.go swarms/swe/runtime_skills_integration_test.go swarms/swe/rig_restore_integration_test.go
git commit -m "test(swe): cover restored rig and async delegates"
```

### Task 8: Delete legacy vocabulary and run final gates

**Files:**
- Delete any remaining compatibility-only files found by the searches below.
- Modify only files required by failures.

**Step 1: Add a deletion guard**

Add an AST/source guard under `swarms/swe` that fails on production uses/declarations of:

- `loop.Config`, `loop.ToolSet`;
- `session.New`, `session.Restore`, concrete `*session.Session`, `session.Option`,
  `session.Limits`, `session.ConfigFingerprintFields`, and session construction options
  including `WithLimits`, `WithCeiling`, and `WithConfigFingerprintFields`;
- `swarmSpawner`, `subagentRunner`, `RunSubagent`, custom `NewSubagent`;
- `watchSessionEvents`, idle `CheckpointWorkspace`, manual session lease/appender/GC wiring;
- `PrimaryLoopID`, static zero-argument `AcceptsImages`.

It must ignore comments/strings and respect import alias shadowing.

**Step 2: Verify searches**

```bash
rg -n 'loop\.Config|loop\.ToolSet|\*session\.Session|session\.(New|Restore|Option|Limits|ConfigFingerprintFields)|WithLimits|WithCeiling|WithConfigFingerprintFields|WithWorkspaceStore|watchSessionEvents|CheckpointWorkspace|swarmSpawner|subagentRunner|RunSubagent|PrimaryLoopID|AcceptsImages\(\)' --glob '*.go' --glob '!vendor/**'
```

Expected: no production hits; any test hit is an intentional negative fixture.

The migration inventory must explicitly update stale production/tests/comments in
`swarms/swe/errors.go`, `security.go`, `model.go`, `runtime_context_test.go`,
`operator_eval_integration_test.go`, `runtime_skills_integration_test.go`, and every test
currently constructing session options or direct sessions. Do not leave this work to a
search-only final task.

**Step 3: Run all gates**

```bash
GOWORK=off make fmt
GOWORK=off make lint
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
git diff --check
```

Expected: PASS. If localhost integration tests need unsandboxed loopback access, record that environment requirement; do not weaken tests.

**Step 4: Commit final cleanup if needed**

```bash
git add <only files changed by deletion guards/final fixes>
git commit -m "refactor(swe): remove legacy session wiring"
```

Do not create an empty commit.

---

## Completion criteria

- SWE constructs sessions only through one rig.
- `operator-primary` is the sole primer/delegator; operator/reviewer are delegate-free leaves.
- Tools, permissions, sandbox runners, observations, and runtime skills are fresh per bind and use the bound workspace.
- Harness manages Subagent actions, session/loop lifecycle, leases, restore, and native snapshots.
- SWE contains no idle checkpoint watcher, manual session factory, late spawner, or custom Subagent wrapper.
- Restored sync/async delegates, active/mode/model/effort, gates, ceiling, workspace, and CLI adapter state are verified before new admission.
- The migrated CLI version lands first; SWE introduces no serve endpoint or CLI-specific topology.
  (**SUPERSEDED 2026-08-27** as to the serve endpoint — see the wui design §6.)
