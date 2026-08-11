# pkg/foreign

`pkg/foreign` defines the **composition seams** for foreign-loop
backends: a session uses `Builder` to construct a fresh foreign loop
and `RestoredBuilder` to reconstruct one from journal-recovered state.
The concrete codex and claude backends live in the sibling
[`looprig/foreignloops`](https://github.com/looprig/foreignloops) module.

## What is foreign?

- **`Builder`** — the composition-root seam a session uses to construct
  a fresh foreign loop. Returns the `loop.Backend` and the minted
  `ForeignSID` the session records.
- **`RestoredBuilder`** — mirrors `Builder` but carries a
  `RestoredForeign` seed (the recovered foreign session id, the
  committed turn count, and the committed conversation thread) and
  returns no sid because the seed already holds it. A restored loop
  comes up idle, seeded with this state, and **resumes** (never
  re-creates) the recorded session on its next turn.
- **`RestoredForeign`** — the journal-recovered seed.
- **`EventPublisher`** — the narrow consumer of the session event
  fan-in a foreign loop holds. A session satisfies it via
  `PublishEvent` / `PublishEventChecked`.

The seams are deliberately narrow: a foreign loop sees only the
`loop.Backend` contract the session drives, the `EventPublisher` it
publishes through, and the `loop.BoundDefinition` it runs under. It
does not see the native loop's gate/commit/drain internals, which a
foreign loop has no analogue for.

## How to use

You don't call `Builder` directly — you register one with the rig, and
the session calls it when a loop selects the matching engine:

```go
import codexbackend "github.com/looprig/foreignloops/codex"
import claudbackend "github.com/looprig/foreignloops/claude"

r, err := rig.Define(
    rig.WithLoops(
        // a native loop:
        operator,
        // a codex-backed loop:
        codexLoop,  // loop.Define(... loop.WithEngine(loop.EngineForeignCodex) ...)
        // a claude-backed loop:
        claudeLoop, // loop.Define(... loop.WithEngine(loop.EngineForeignClaude) ...)
    ),
    rig.WithForeignBuilders(
        codexbackend.Builder(codexbackend.Options{ /* ... */ }),
        claudbackend.Builder(claudbackend.Options{ /* ... */ }),
    ),
    /* ... */
)
```

A session routes a `SubmitToLoop` to a foreign-backed loop the same way
it routes one to a native loop. The foreign backend owns its own
subprocess; the harness session owns the journal, the hub, and the
restore lifecycle.

## Sibling packages

- [`pkg/loop`](../loop/README.md) — `loop.EngineForeignClaude` /
  `EngineForeignCodex` select a foreign backend; `loop.Backend` is the
  contract a backend satisfies; `loop.BoundDefinition` is the bound
  recipe a backend runs under.
- [`pkg/event`](../event/README.md) — `ForeignSessionBound` is the
  durable event that records the late-bound foreign session id; restore
  recovers it.
- [`pkg/rig`](../rig/README.md) — `rig.WithForeignBuilders` registers
  the builders the session calls.
- [`github.com/looprig/foreignloops`](https://github.com/looprig/foreignloops)
  — the codex and claude backends behind these seams.

## How it is designed

```
       Loop definition (Engine = ForeignCodex | ForeignClaude)
                       │
                       │  session constructs the loop
                       ▼
            foreign.Builder / RestoredBuilder
                       │
                       │  returns loop.Backend + ForeignSID (Builder only)
                       ▼
            ┌────────────────────────────┐
            │ Foreign loop backend        │
            │ (looprig/foreignloops)      │
            │  • subprocess (codex exec)   │
            │  • JSONL decode             │
            │  • late-bound ForeignSID     │
            │  • durable resume            │
            └────────────┬───────────────┘
                         │
            publish  ────┴────►  EventPublisher  ──►  pkg/hub
                                                  ──►  pkg/event (ForeignSessionBound)
                         │
                         ▼
                  loop.Backend (the contract the session drives)
```

### Late-bound foreign session id

A foreign loop's session id is learned from the foreign process
(`codex exec --json` emits a `thread.started` event), not known at
construction. The session records the learned id as a durable
`ForeignSessionBound` event. Restore recovers that id and resumes the
same foreign session — a failed Codex start that produced an empty sid
retries `StartNew` until a nonempty session id has been bound.

### Restore is resume, not re-create

A restored foreign loop comes up **idle**, seeded with the
`RestoredForeign` thread and turn count, and **resumes** the recorded
foreign session on its next turn. It does not re-create the session:
the foreign backend's `codex exec resume` (or equivalent) is the
mechanism, and the harness session journal is what makes the resume
durable across harness restarts.

### The narrow seam

`Builder` and `RestoredBuilder` are deliberately small: the loop
context, the session/loop ids, the parent `loop.Provenance`, the
`EventPublisher`, the `loop.BoundDefinition`, an id generator, and the
event `Factory`. The seam carries no harness internals and no foreign
protocol vocabulary; a new foreign backend (a different CLI, an MCP
runtime, …) implements just `Builder`/`RestoredBuilder` and a
`loop.Backend`.
