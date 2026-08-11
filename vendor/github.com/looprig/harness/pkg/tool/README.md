# pkg/tool

`pkg/tool` defines the **dependency-free contract surface** for the tools
subsystem: the `BaseTool`/`InvokableTool` interfaces every tool
implements, the `ToolResult` value tools return, the **tool-owned
preparation boundary** (`CallPreparer`, `Request`, `Requirement`,
`RuleCandidate`), and the optional capability interfaces the runner
probes for via type assertion.

It imports only `github.com/looprig/core` (plus stdlib). It must never
import `pkg/loop`, the runtime internals, or a concrete tools module, so
all of them can depend on it without a cycle. The standard tool
implementations (bash, web, …) live in the sibling
[`looprig/tools`](https://github.com/looprig/tools) module.

### Task tracking and delegation ownership

Optional task tracking is a standard-tool concern. Consumers that want it may
select the `TaskDefinitions()` bundle from
[`github.com/looprig/tools`](https://github.com/looprig/tools), which exposes the
four task model-facing names from one selected definition. Each bound Loop gets
its own task graph, while modes within that Loop share the graph.

Agent collaboration is intentionally different: `ListAgents`, `MessageAgent`,
`StartAgent`, and `StopAgent` are Harness-owned model-facing control tools.
Harness derives their schemas, and the `StartAgent` capability catalog, from the
frozen delegate topology and binds them to the parent-scoped delegate
controller, so their authority cannot be supplied by optional task tracking.
Harness therefore does not import `github.com/looprig/tools`. The four tools are
automatically injected as one bundle into each applicable Loop; consumers must
not add them manually. Agents coordinate task work through agent messages
rather than shared task memory.

## What is tool?

- **`BaseTool`** — `Info(ctx) (*ToolInfo, error)`. The minimal contract;
  never widened (new behavior is added via separate optional interfaces,
  never folded into `BaseTool`).
- **`InvokableTool`** — `BaseTool` plus
  `InvokableRun(ctx, argsJSON) (*ToolResult, error)`. The runner injects
  an "error: empty result" block when the result is nil or empty.
- **`Definition`** — a design-time binding of a tool name to a factory
  that builds live `InvokableTool` values from `tool.Bindings`. Loops
  hold `Definition` values; the runtime `Bind`s them at loop start.
- **`NewEvidenceDefinition`** — a sealed, read-only definition with
  frozen `ToolInfo`. Its `EvidenceFactory` receives only invocation
  `SessionID`, `LoopID`, and an optional root-only
  `ReadWorkspaceBinding`; the API does not expose mutation,
  observations, delegation, gates, grants, control, or extra tools.
- **`ToolMiddleware`** — wraps each invocation (observability, retry,
  redaction, …) with a `func(next InvokableTool) InvokableTool` shape.
- **`CallPreparer`** — the tool-owned preparation boundary. Decodes and
  validates the untrusted `argsJSON`, normalizes commands/URLs/paths,
  resolves canonical resource identities, and produces a typed `Request`
  plus an optional opaque `PreparedArtifact` the tool reads back at
  execution time.
- **`Request` / `Requirement` / `RuleCandidate`** — the typed prepared
  access request. `ValidateRequest` re-checks every invariant at the
  start of evaluation and at both durable codec boundaries.
- **Optional capability interfaces** — `Sequential` (must not run
  concurrently with other calls in the batch), `ReadGuard` consumer,
  `DelegateController` requirement, generic asynchronous process contracts,
  etc. The runner probes for each via type assertion; a tool implementing none
  is still a valid `InvokableTool`.

## How to use

### As a tool author

```go
type MyTool struct{}

func (MyTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
    return &tool.ToolInfo{
        Name:   "my_tool",
        Desc:   "Does a thing",
        Schema: json.RawMessage(`{"type":"object","properties":{...}}`),
    }, nil
}

func (t MyTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
    // parse + validate args, execute, return a result
    return tool.TextResult("done"), nil
}
```

Evidence collectors use the narrower factory boundary:

```go
definition := tool.NewEvidenceDefinition(
    "workspace-status",
    tool.RequiresWorkspaceRead,
    []tool.ToolInfo{statusInfo},
    func(ctx context.Context, bindings tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
        return []tool.InvokableTool{
            newStatusTool(bindings.ReadWorkspace.Root),
        }, nil
    },
)
```

The read-workspace pointer is a per-build copy. Evidence factories must still
treat the binding as invocation-scoped and must not retain it after returning.

If your tool is effectful, implement `CallPreparer` so the gate gets a
typed request to decide on:

```go
func (t MyTool) PrepareCall(ctx context.Context, executionID string, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
    var args myArgs
    if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
        return tool.Request{}, nil, /* typed error */
    }
    // normalize paths/commands/URLs, resolve canonical identity, ...
    return tool.Request{
        ToolName: "my_tool",
        Summary:  "my_tool(" + args.Path + ")",
        Requirements: []tool.Requirement{{
            Kind:        "fs.write",
            Match:       "Write(" + absPath + ")",
            Description: "Write " + absPath,
            GrantClass:  "fs.write.v1",
            GrantTarget: absPath,
        }},
    }, tool.TokenArtifact{Token: absPath}, nil
}
```

### As a rig composer

```go
operator, _ := loop.Define(
    loop.WithTools(
        tool.NewDefinition("read", tool.RequiresNothing, /* factory */),
        tool.NewDefinition("bash", tool.RequiresSandbox, /* factory */),
    ),
    /* ... */
)
```

The standard tool implementations live in
[`looprig/tools`](https://github.com/looprig/tools); a consumer wires
those factories and binds a `gate.Evaluator` (and a sandbox) at the
composition root.

### Asynchronous processes

`AsyncProcessRunner` is the shell-agnostic, enforcement-capable seam for
supervised processes. `PrepareProcess` validates grants and reserves enforcement
resources without spawning. The caller reads the returned preparation's
authoritative `WorkspaceAccess`, acquires a matching lifetime workspace lease,
and then calls its single-use `Start`. `WorkspaceAccess` keeps its path slices
private: construct it with `NewWorkspaceAccess`, and read defensive copies
through `WritePaths` and `WriteTrees`. Closing an unstarted preparation releases
its reservation.

The `Start` context governs setup through handoff only. The returned `Process`
lives independently until wait, close, its deadline, or runner shutdown.
Process deadlines are opt-in: a zero `ProcessRequest.Deadline` means no process
deadline and is never replaced by a runner default. Session or runner shutdown
still terminates the process.

`Process.StreamMode` distinguishes stream topology without fallback ambiguity.
Pipe mode exposes distinct stdout and stderr readers. PTY mode exposes combined
terminal bytes on stdout and a non-nil, closed-empty stderr; unavailable PTY
support fails preparation or start instead of falling back to pipes. Process
methods other than `Wait` are concurrency-safe; the supervisor is the sole
caller of `Wait` and calls it exactly once. Returned stdin supports concurrent
writes and close: close is idempotent, delivers EOF at most once, and makes
later writes fail.

`ProcessResult` carries a typed terminal reason and timestamps but never an OS
PID; model-facing handles belong to the supervising tools module. A process may
optionally implement `ProcessActivitySource`. Every activity invalidates the
complete bound observation cache, malformed activity is conservatively broad,
and the activity channel closes before `Wait` returns.

## Sibling packages

- [`pkg/gate`](../gate/README.md) — the three-state evaluator that
  decides `Deny`/`Gated`/`Allow` on the typed `Request` produced here.
- [`pkg/loop`](../loop/README.md) — `loop.WithTools` takes
  `tool.Definition` values; `loop.WithMiddlewares` takes
  `tool.ToolMiddleware` values.
- [`pkg/identity`](../identity/README.md) — `identity.AgentName` used in
  delegation tooling.
- [`github.com/looprig/tools`](https://github.com/looprig/tools) — the
  standard tool implementations.
- [`github.com/looprig/sandbox`](https://github.com/looprig/sandbox) —
  satisfies the `gate.AccessSource`/`GrantIssuer` seams the gate needs to
  decide the requirements tools produce.

## How it is designed

```
       tool.Definition (design-time)
                │
                │  loop.Bind (at loop start)
                ▼
       tool.InvokableTool (live)
                │
   ┌────────────┴────────────┐
   │  Optional: CallPreparer  │
   │  PrepareCall(argsJSON)   │
   │   → tool.Request          │
   │   → PreparedArtifact       │
   └────────────┬────────────┘
                │
                ▼
       pkg/gate.Evaluator
                │
                │  Deny / Gated (one combined approval) / Allow
                ▼
       InvokableRun(ctx, argsJSON)
                │
                ▼
       *tool.ToolResult  (content blocks)
```

### Ownership: tools prepare, the gate decides, the sandbox enforces

This split is invariant in the codebase (see `CLAUDE.md`):

- **Tools own preparation.** Each tool decodes and validates its own
  untrusted arguments, normalizes commands/URLs/paths, and produces the
  typed `Request`. Invalid input fails during preparation and never
  reaches the gate.
- **`pkg/gate` owns the three-state decision.** It never parses tool
  arguments; it consumes the typed `Request` and applies
  deny-before-allow, one combined approval, response routing.
- **The sandbox owns enforcement.** It satisfies
  `gate.AccessSource`/`GrantIssuer` without importing harness; harness
  never imports a sandbox package.

### Sealed `PreparedArtifact`

`PreparedArtifact` is `interface{ preparedArtifact() }` — the method is
unexported, so only types in this package can satisfy it. A type in
another package cannot masquerade as a `PreparedArtifact`; concrete
artifacts therefore live in this package alongside the seal.
`TokenArtifact` is the minimal single-string case; richer preparers
declare their own concrete artifact type here.

### Strict typing, no `any`

`ToolResult.Content` is `[]content.Block` (a sealed interface); the
runner injects an "error: empty result" block when the result is nil or
empty. There is intentionally no `Terminate` field in v1 — a turn ends
only via the model emitting no more tool calls or an abort.
