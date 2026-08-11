# pkg/loop

`pkg/loop` defines **immutable loop recipes** and the **public contracts
for live loops**. Concrete loop actors and their construction remain
internal (see `internal/loopruntime`); consumers compose definitions into
a [`pkg/rig`](../rig/README.md) and interact with the returned
`loop.Handle` or `loop.Controller` values.

A **loop** is the agent: one goroutine owns its mutable state, drives one
inference client through turns and steps, and emits events. The package
holds the design-time types (`Definition`, `Mode`, `Engine`,
`CompactionPolicy`), the live identity/mutation surface (`Handle`,
`Controller`, `ModeCatalog`, `ExternalToolInstaller`), and the
runtime-supporting types the actor and the consumer both depend on
(`Backend`, `Provenance`, `RuntimeContextProvider`, `AccessGate`,
`ReadGuard`).

## What is loop?

- **`Definition`** — an immutable loop recipe. Built with `loop.Define`
  and a stream of `Option` values: name, inference client, model,
  system prompt, tools, access gate, middlewares, modes, delegation,
  compaction policy, structured output. `Define` validates every
  invariant and freezes the result.
- **`Handle`** — the read-only identity and inference view of a live loop
  (`ID`, `Mode`, `Model`).
- **`Controller`** — the trusted mutation surface (`SetMode`, `Change`,
  `Interrupt`). `Interrupt` is subtree-scoped: it cancels this loop's
  current turn and every loop below it in the delegate subtree.
- **`Backend`** — the narrow turn-engine contract `Session` drives. Both
  the native actor and a foreign loop satisfy it; it's the minimal subset
  the session uses (command submission, completion signal, committed-state
  snapshot).

## How to use

Consumers don't build loops directly; they compose definitions into a rig:

```go
operator, err := loop.Define(
    loop.WithName("operator"),
    loop.WithClient(inferenceClient),
    loop.WithModel(model.Model{ /* provider, name, sampling.effort */ }),
    loop.WithSystem("You are a careful coding agent."),
    loop.WithTools(/* tool.Definition values from looprig/tools */...),
    loop.WithAccessGate(gateEvaluator),
    loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
    loop.WithDelegates("reviewer", "architect"),
    loop.WithModes(loop.Mode{Name: "plan", /* ... */}),
    loop.WithInitialMode("plan"),
    loop.WithCompaction(/* optional CompactionPolicy */),
    loop.WithOutputSchema(/* optional inference.OutputSchema */),
    loop.WithPolicyRevision("2026-07-21.1"),
)
if err != nil { return err }

r, err := rig.Define(rig.WithLoops(operator), /* ... */)
```

Driving a live loop happens through the `Session` returned by the rig:

```go
handle := session.ActiveLoop()
handle.ID(); handle.Model(); handle.Mode()

ctl, ok := session.LoopController(handle.ID())
if ok {
    ctl.SetMode(ctx, "plan")
    ctl.Interrupt(ctx)  // subtree-scoped
}
```

## Sibling packages

- [`pkg/tool`](../tool/README.md) — `tool.Definition` values passed to
  `loop.WithTools`.
- [`pkg/gate`](../gate/README.md) — the `gate.Evaluator` bound via
  `loop.WithAccessGate`.
- [`pkg/identity`](../identity/README.md) — `identity.AgentName` used by
  `loop.WithName` and `loop.WithDelegates`.
- [`pkg/event`](../event/README.md) — `event.TurnIndex` and the events
  the loop emits.
- [`pkg/command`](../command/README.md) — the commands the actor accepts
  on its `Commands` channel.
- [`pkg/foreign`](../foreign/README.md) — `EngineForeignClaude` /
  `EngineForeignCodex` select a foreign-loop backend.
- [`pkg/hustle`](../hustle/README.md) — the parallel background work
  subsystem a loop can invoke through the hustle tool.
- `github.com/looprig/inference` — `inference.Client`, `model.Model`,
  `inference.OutputSchema`, context counters.
- `github.com/looprig/foreignloops` — the codex/claude backends behind
  `Engine`.

## How it is designed

A `Definition` is an **immutable recipe**. The private
`internal/loopruntime` package binds a definition into a live actor: one
goroutine that owns mutable state, accepts commands on a channel, and
spawns a fresh turn goroutine per accepted turn.

```
                Definition (immutable recipe)
                          │
                          │  rig binds it (Bind)
                          ▼
                internal/loopruntime.Loop
                          │
   ┌──────────────────────┴───────────────────────┐
   │ Loop actor goroutine                          │
   │  state: idle | running | shuttingDown          │
   │  commands chan  ◀── Submit/CancelQueued/...   │
   │  priorityCommands ◀── Interrupt/Shutdown      │
   │  gateReg ◀── pending gate registrations       │
   │  snapshots ◀── committed-state queries        │
   └──────┬───────────────────────────────────────┘
          │ per accepted turn
          ▼
   ┌──────────────────────────────────────────────┐
   │ Turn runner goroutine                         │
   │  turnState.msgs (staged; owned by turn)       │
   │  events chan (per-turn; owned by caller)     │
   │  for each step:                               │
   │    LLM stream  ─► emit TokenDelta              │
   │    tool batch ─► gate → sandbox → result      │
   │    commit group into loopState.msgs           │
   └──────────────────────────────────────────────┘
```

The full goroutine/channel/ack picture — including `StartTurn`,
`Interrupt`, and `Shutdown` semantics, and the rationale for there being
no turn queue in v1 — is in
[`docs/architecture/agent-loop.md`](../../docs/architecture/agent-loop.md).

### Engines

`Engine` selects the loop backend. The zero value is `EngineNative`
(harness's own actor). `EngineForeignClaude` and `EngineForeignCodex`
select a foreign-loop backend built by a `foreign.Builder` registered on
the rig. A foreign loop satisfies the same `Backend` contract the session
drives; restore recovers its foreign session id from the journal.

### Modes and tool limits

A `Mode` is a predeclared alternative to a definition's base inference
settings: model, effort, tools, tool limits, and an optional instructions
override. The implicit base mode is the empty `ModeName`. Default tool
limits are 25 iterations, 100 calls per turn, 8 parallel calls.

### Compaction and context policy

A `CompactionPolicy` opts a loop into per-loop token accounting: when
input occupancy crosses a threshold, the loop runs a hustle-style
compaction that produces a summary tied to the exact context basis, then
commits the compacted context. `ContextObservationPolicy` is the
mutually-exclusive alternative — observe occupancy without compacting.
Both require a `contextcount.ContextCounter` and a compatible
`inference.Client`; `Define` validates the binding.

### Concurrency contract

The actor's `Commands` channel is **unbuffered** — sends block until the
actor is ready. Callers must never close it; stop the actor with
`Shutdown`. The submit commands (`UserInput`, `SubagentResult`,
`CancelQueuedInput`) are **fire-and-forget**: their outcomes are
published as typed events, not replied on a per-command channel. Only the
control commands (`Interrupt`, `Shutdown`) carry an `Ack` channel, and
each must be non-nil and buffered(1) so the actor's send never stalls.
