# SWE Migration to Harness Rig Design

**Date:** 2026-07-11  
**Status:** Implemented. The "no serve composition" constraint below is SUPERSEDED
by `looprig/docs/plans/2026-08-27-wui-web-ui-design.md` §6 (2026-08-27), which adds
`carbon serve`. That design argues the change on its own merits; nothing in this
document authorised it. See the "Serve" row and the "Do not add" list below.  
**Depends on:** the reviewed harness rig/session/workspace branch and the approved CLI migration plan

## Goal

Replace SWE's manual `loop.Config` and `session.New`/`session.Restore` composition with one immutable harness `rig.Rig`. Harness owns session lifecycle, topology, managed delegation, persistence, workspace placement, and checkpoint boundaries. SWE continues to own agent prompts, model selection, skills, permission policy, and OS-sandbox construction.

This is a breaking migration. Old adapters are deleted rather than wrapped.

## Current-state inventory

| Area | Current owner and sites | Migration disposition |
|---|---|---|
| Session construction | `swarms/swe/agent.go:56-118` calls three direct constructors | Delete; `SessionStoreFactory` calls `Rig.NewSession` or `Rig.RestoreSession` |
| Persistence | `swarms/swe/persistence.go:70-418` mints IDs, acquires leases, creates appenders, schedules GC, restores, and replays | Reduce to opening fsstore facades, defining a rig, catalog/list reads, and a replay adapter |
| Workspace checkpoints | `persistence.go:316-350` watches `SessionIdle` and calls `CheckpointWorkspace` | Delete. Configure `rig.WithSnapshots` and let native boundaries checkpoint |
| Agent composition | `swarms/swe/swarm.go:90-330` builds `loop.ToolSet`, `loop.Config`, limits, fingerprints, ceiling, and a custom Subagent | Replace with `loop.Define` and `rig.Define` |
| Delegation | `swarms/swe/spawner.go` late-binds a live `session.Session`; `swarm.go` injects `tools.NewSubagent` | Delete both. Declare delegates and managed delegation on the primer definition |
| Leaf tools | `swarms/swe/agents.go`, `agents/operator`, `agents/reviewer` build live tools and permission checkers | Convert to immutable `tool.Definition` factories and `loop.PermissionFactory` |
| Sandbox/read guard | `swarms/swe/confinement.go`, `confine`, runtime-skill wiring use a fixed root and per-spawn ceiling | Rebuild per loop bind from `tool.Bindings.Workspace` and the session ceiling source |
| TUI adapter | `swarms/swe/agent.go` caches primary ID and image capability, owns teardown/replay | Wrap `session.SessionController`; expose current active selection, independent focus, uniform all-loop replay, and dynamic per-loop image capability |
| CLI composition | `cmd/swe/main.go:179-249` opens `SessionStoreFactory` and passes an opener to CLI | Preserve this composition seam; update it to the migrated CLI adapter contract |
| Serve | No SWE production serve composition exists | Do not add one. Future composition uses generic `serve.Handler[S,O]` directly. **SUPERSEDED 2026-08-27:** `carbon serve` composes `serve.Handler[S,O]` over the real rig in `cmd/carbon`, and `internal/` still may not import `pkg/serve`. The forward-looking sentence in this cell constrained the shape of such a future; it did not authorise building one. The argument is in the wui design §6. |

## Target ownership

```text
SWE configuration
  -> immutable loop definitions (operator-primary, operator, reviewer)
  -> immutable rig definition (topology + stores + workspace + policy)
  -> Rig.NewSession / Rig.RestoreSession
  -> session.SessionController
  -> SWE's narrow CLI adapter
```

Harness owns:

- session IDs and session single-writer leases;
- journal/appender/catalog lifecycle and restore replay;
- loop construction and restore;
- active-loop durability;
- delegation depth/quota, ownership, request correlation, follow-up, status, wait, and interrupt;
- workspace placement, restore, mutation coordination, and snapshot scheduling;
- session shutdown and collaborator draining.

SWE owns:

- prompts, greeting, modes, models, and runtime-context content;
- tool and permission definitions;
- sandbox posture and runner construction at bind time;
- skill catalog and runtime-skill policy;
- CLI replay/view adaptation and process-level fsstore close.

## Topology

Use three definitions:

| Definition | Primer | Delegates | Purpose |
|---|---:|---|---|
| `operator-primary` | yes, active | `operator`, `reviewer` | Coding-loop topology key; display identity remains `operator` |
| `operator` | no | none | Delegate leaf for implementation/investigation |
| `reviewer` | no | none | Read-only critique leaf |

The separate key prevents definition-wide delegation from accidentally giving a spawned
operator another Subagent capability. A blocking harness prerequisite adds immutable display
name and description metadata to definitions: the primer key is `operator-primary`, while its
display name remains `operator`. `LoopStarted` exposes both values and CLI renders the display
name. Display aliases need be unique only within one parent's allowed delegate catalog; the
primer and operator leaf are deliberately allowed to share `operator`. Managed delegate
catalog entries use the target definition's display name and existing
operator/reviewer description. Tests compare primer-minus-managed-Subagent with the operator
leaf so the two cannot drift.

```go
primary, err := loop.Define(
    loop.WithName("operator-primary"),
    loop.WithDisplayName("operator"),
    loop.WithDescription(operator.Description),
    loop.WithInference(client, model),
    loop.WithSystem(operatorSystem),
    loop.WithTools(primaryToolDefinitions...),
    loop.WithPermissionFactory(primaryPermissionFactory),
    loop.WithRuntimeContext(runtimeContext),
    loop.WithPolicyRevision(policyRevision),
    loop.WithDelegates("operator", "reviewer"),
    loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
    loop.WithModes(planMode, buildMode),
    loop.WithInitialMode("build"),
)
```

The example is illustrative: mode names and default mode must preserve current SWE behavior. Do not add plan/build modes merely because harness supports them. If SWE retains one mode, omit `WithModes` and `WithInitialMode`; dynamic `Controller.Change` still supports model/effort changes.

## Managed Subagent

`loop.WithDelegates` plus `DelegationManaged` causes harness to bind the single managed Subagent tool. SWE deletes `swarmSpawner`, `subagentRunner`, `RunSubagent`, the custom Subagent definition, and the post-construction `bind(sess)` cycle.

The model-facing envelope supports:

- start with agent, message, optional mode, and `wait`;
- send follow-up with delegate ID and `wait`;
- wait for one request ID;
- status for one or all owned children;
- interrupt one owned child.

`wait=true` preserves today's synchronous result behavior. `wait=false` returns delegate/request IDs, after which the same Subagent tool performs follow-up/status/wait/interrupt. Parent-scoped `DelegateController` attenuation prevents sibling, ancestor, or unrelated access. Leaf definitions have no delegates, so they never receive Subagent.

## Tools, permissions, sandbox, and skills

`loop.Define` stores `tool.Definition`, not live `tool.InvokableTool` or `loop.ToolSet`. Each definition factory receives `tool.Bindings` for the current session/loop.

Migration rules:

1. File/Bash/skill definitions use `bindings.Workspace.Root`; no closure captures the root selected before the session exists.
2. Workspace-mutating tools use the harness workspace coordinator supplied in the binding. Prefer harness `tools.Files(readGuard)` and `tools.Bash(...)` definitions where they cover SWE behavior.
3. A `loop.PermissionFactory` creates a fresh checker per bind. It consumes only immutable SWE policy plus the bound loop/session capabilities.
4. Sandbox executors and read-only views are fresh per bound loop. The same effective-mode source feeds the permission posture and sandbox runner.
5. Runtime skills retain embedded-wins and human-gated workspace loading, but their workspace root comes from the binding.
6. Opaque permission/runtime-context collaborators receive a stable `WithPolicyRevision`; changing produced tool names or mode tools changes the harness topology fingerprint.

No resolved tool or permission instance is shared between primer, delegate, session, or restore.

## Rig composition

`SessionStoreFactory` opens one fsstore backend and constructs:

- `sessionstore.Store` for journal/catalog/leases;
- `workspacestore.Store` for workspace snapshots;
- shared store facades used by immutable rigs.

`Config`, provider/model catalog, working directory, and the resume-only mismatch flag arrive
at `Open`, not at `NewSessionStoreFactory`. Each `Open` therefore builds one immutable rig from
that resolved input and immediately calls `NewSession` or `RestoreSession`. `/clear` builds a
new rig with the same process config. Do not cache a rig across differing configs. This keeps
one composition path without pretending the rig is process-global.

SWE currently edits the process working directory. Preserve that behavior with an
**exclusive** fixed-root placement; silently switching to per-session roots would make the
TUI edit a hidden copy instead of the checkout the user opened. The fsstore leaser fences a
second session/process from owning the same checkout:

```go
r, err := rig.Define(
    rig.WithLoops(primary, operatorLeaf, reviewerLeaf),
    rig.WithPrimers("operator-primary"),
    rig.WithActivePrimer("operator-primary"),
    rig.WithSessionStore(sessionStore),
    rig.WithExclusiveWorkspace(workspaceStore, workspaceRoot, fsstoreLeaser),
    rig.WithSnapshots(rig.SnapshotPolicy{
        Trigger:  rig.SnapshotOnIdle,
        Priority: rig.SnapshotBestEffort,
        Timeout:  60 * time.Second,
    }),
    rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: operatorSpawnQuota}),
    rig.WithFingerprintFields(fields),
    rig.WithCeilingFactory(ceilingFactory),
)
```

The snapshot policy is explicit even though best-effort idle is the resolved default. It exactly replaces the current idle watcher. There is no second watcher and no call to `CheckpointWorkspace` on `SessionIdle`.

`workspaceRoot` is the canonical process working directory resolved before `rig.Define`.
Tests must pin exclusive-root contention, clean handoff, and the restore behavior for a
non-empty attached checkout. Do not silently select shared placement. Shared placement is
fuzzy and cannot use required snapshots. Per-session placement is a future explicit product
mode, not this compatibility migration.

Persistence paths remain outside the managed workspace; `rig.Define` rejects overlap.

## Creation, restore, and shutdown

New:

```go
sess, err := r.NewSession(ctx)
```

Restore:

```go
sess, err := r.RestoreSession(ctx, id)
```

The rig mints IDs for new sessions, acquires/releases leases, restores topology and workspace, installs snapshot policy before admission, and reconstructs active loop/mode/model/effort/delegates. SWE must not pre-acquire a session lease or rebuild a primary `loop.Config` for restore.

`sessionAgent.Close` calls `SessionController.Shutdown` once. It does not cancel an extra session root, stop a checkpoint watcher, release the session lease, or stop per-session GC. Process shutdown closes the shared fsstore only after live sessions have shut down.

Session offload-blob GC and workspace-snapshot GC are distinct. **Blocking prerequisite:**
the reviewed harness release must move the existing lease-guarded session offload collector
under rig/session lifecycle before SWE deletes `scheduleGC`. The current final rig surface
hides the journal lease, so SWE cannot safely keep that ticker beside `Rig.NewSession` and
`Rig.RestoreSession`; deleting it without a harness replacement would regress collection.
If that prerequisite is not in the selected harness tag, stop the migration and land the
harness follow-up first. Workspace snapshot GC remains manual and is not added by this
migration.

## CLI adapter

SWE implements the approved CLI contract over `session.SessionController`:

- `ActiveLoopID`: a direct `sess.ActiveLoop().ID()` query;
- focus: owned by CLI; initial/reopen focus follows active, later active changes do not steal focus;
- per-loop running state and active-selection reconciliation are owned by CLI from the single
  stream returned by SWE;
- image capability: query `sess.Loop(loopID).Model().Caps.AcceptsImages` for the focused submission target each time;
- `Submit` routes to active; `SubmitToLoop` routes to focus;
- restored replay materializes all-loop Enduring history for uniform CLI projections;
- gate conveniences use an adapter-owned index keyed by `(LoopID, ToolExecutionID)`: replay
  and the forwarded live stream fold `GateOpened`, with a reverse `GateID` index for
  `GateResolved`, then call `SessionController.RespondGate` with the indexed ID;
- replay uses the journal-backed enduring stream and does not create a second live subscription;
- `Close` maps to `Shutdown`.

The adapter does not open a second subscription. Its subscription wrapper updates the gate
index before forwarding each event. Restore performs one unnarrowed cold replay to seed gates
and materialize Enduring history from every loop, then returns that all-loop backlog to CLI.
Tests cover
opened-before-request ordering, identical tool-execution IDs in different loops, and restored
delegate-loop open/resolved gates.

The CLI dependency/version migration lands first. SWE does not temporarily emulate removed
`PrimaryLoopID` or static `AcceptsImages`.

## Headless API

The exported `swe.New` path remains supported for evals and tests. It uses the same definition
and rig builder over a process-shared ephemeral `storage/memstore` composite,
`sessionstore.Store`, and `workspacestore.Store`, with exclusive current-checkout placement.
The shared leaser provides process-local contention across two headless sessions; it is not
presented as cross-process fencing. The returned owner shuts down the session; the in-memory
store needs no process teardown. There is no direct `session.New` fallback and no second
topology builder.

## Error handling

- Rig definition errors fail before opening a live session.
- New/restore errors remain typed and return no partial session.
- Unknown delegate/mode and quota/depth failures surface through the managed Subagent result.
- Workspace busy/lost/recovery and checkpoint faults remain public typed errors.
- Tool binding or permission factory failures abort session construction transactionally.
- Config mismatch rejects restore unless the existing explicit SWE escape hatch maps to `rig.WithAllowConfigMismatch`.
- Because mismatch policy is immutable rig configuration, `Open` builds the rig after reading
  `SessionSelector`; it never mutates or reuses a differently configured rig.

## Deletions and non-goals

Delete, do not deprecate:

- direct `session.New`, `session.Restore`, `loop.Config`, `loop.ToolSet`, and session option wiring;
- `swarmSpawner`, `subagentRunner`, `RunSubagent`, and late `bind`;
- SWE's custom Subagent tool/catalog adapter;
- `watchSessionEvents`, checkpoint timeout/watcher teardown, and idle `CheckpointWorkspace`;
- manual ID, session lease, journal/appender, and lease-release wiring;
- per-session offload GC wiring, but only after the blocking harness rig-owned-GC prerequisite lands;
- cached primary model image capability and old primary-loop adapter vocabulary.

Do not add:

- a SWE serve endpoint (none existed at the time of this migration). **SUPERSEDED 2026-08-27**
  by `carbon serve`, which the wui design §6 argues for directly rather than claiming
  any permission from this list;
- a second session factory alongside rig;
- automatic workspace-snapshot GC;
- SWE-specific behavior to CLI;
- migration shims that keep old APIs alive.

## Acceptance criteria

- One rig-builder path creates an immutable rig for each resolved new/restore open.
- The public headless `swe.New` path uses the same rig builder over ephemeral stores.
- Only `operator-primary` can delegate; operator/reviewer children cannot.
- A direct child succeeds under `Depth: 2`; the display identity remains `operator` and
  managed catalog descriptions remain intact.
- Sync and async managed Subagent flows, follow-up, status, wait, interrupt, mode validation, quota, and restore are covered.
- Tool/permission/sandbox instances are fresh per loop and use the correct bound root.
- Idle checkpoint and restore work without a watcher.
- Active loop, mode, model, effort, ceiling, gates, delegates, and workspace restore before first admitted work.
- CLI active/focus/status/image behavior matches the approved CLI plan.
- Searches find no old session constructors, `loop.Config`, `loop.ToolSet`, spawner, custom Subagent, watcher, or manual checkpoint lifecycle.
