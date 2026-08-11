# pkg/event

`pkg/event` defines the **sealed union** of rig, session, loop, turn,
step, and tool events. Enduring rig-control and workspace transitions are
durable replay inputs; ephemeral streaming events are never persisted.

Every concrete event embeds:

- a `Header` (producer identity: `Coordinates`, `Cause`, `Agency`);
- exactly one **lifecycle mixin** (`ephemeral`, `enduring`, or
  `terminal` — supplying `Class()` and `EndsTurn()`); and
- exactly one **scope mixin** (`sessionScoped` or `loopScoped` —
  supplying `Scope()`).

The compile-time assertions in `doc.go` pin the sealed union: every
concrete event type must satisfy `Event`, and the list is the
authoritative enumeration. Adding a new event without adding it there is
harmless, but removing a type or breaking its interface satisfaction
fails the build here first.

## What is event?

- A typed `Event` interface with `isEvent` (unexported, so only types in
  this package can implement it) plus `Class`, `Scope`, `EndsTurn`, and
  `EventHeader`.
- **Three lifecycle classes**:
  - `ClassEphemeral` — streaming events (`TokenDelta`, `ToolCallStarted`,
    `ToolCallCompleted`, …). Never persisted.
  - `ClassEnduring` — rig-control and workspace transitions
    (`ConfigurationAdopted`, `WorkspaceCheckpointed`,
    `ForeignSessionBound`, `CompactionStarted`, …). The durable replay
    inputs.
  - `ClassTerminal` — turn/step outcomes (`TurnDone`, `TurnFailed`,
    `TurnInterrupted`, `StepDone`). Mark `EndsTurn()` true when they end
    a turn.
- **Two scopes**:
  - `ScopeSession` — events the session as a whole owns (started, idle,
    stopped, workspace, restore, integration status).
  - `ScopeLoop` — events a particular loop owns (started, idle, mode
    changed, compaction, turn/step/tool lifecycle).
- A `Delivery` value: an `Event` paired with its `JournalSeq` (zero for
  an ephemeral event that was never persisted, the append sequence for an
  enduring one). The journal sequence rides alongside the event without
  ever entering the event codec, so a live SSE consumer can stamp
  `id:<journal_seq>` without the seq being part of the wire form.
- An `EventFilter` (match by class, scope, kind, or specific types) used
  by `Session.SubscribeEvents`.
- A `Reply` interface: a small, sealed set of command-resolution events
  (`TurnStarted`, `InputQueued`, `TurnRejected`, `TurnFoldedInto`,
  `InputCancelled`, `CompactWaiterResolved`, `CompactWaiterRejected`)
  whose `ReplyTo()` returns the embedded `Header.Cause.CommandID`. The
  set is asserted in tests so the seven stay sealed.

## How to use

You usually don't construct events directly — the runtime and the
session emit them. You read them through a subscription:

```go
sub, _ := session.SubscribeEvents(nil)
for delivery := range sub.Events() {
    switch ev := delivery.Event.(type) {
    case event.TurnStarted:
        // a turn began; ev.Header.Coordinates.TurnID is its id
    case event.TokenDelta:
        // streaming assistant text; ev.Chunk carries the delta
    case event.ToolCallStarted:
        // a tool call is about to run
    case event.ToolCallCompleted:
        // a tool call finished
    case event.PermissionRequested:
        // a gate is open; answer with session.RespondGate
    case event.TurnDone:
        // a turn completed; ev.Header carries the causation id
    case event.IntegrationStatus:
        // an external integration (MCP, …) reported state
    }
    if delivery.JournalSeq > 0 {
        // an enduring event was persisted at this journal sequence
    }
}
```

Filters narrow the stream:

```go
turnsOnly, _ := session.SubscribeEvents(event.EventFilter{
    MatchTurns: true,
    MatchScope: event.ScopeLoop,
})
```

## Sibling packages

- [`pkg/identity`](../identity/README.md) — `Coordinates`, `Cause`,
  `Agency`, embedded in `event.Header`.
- [`pkg/command`](../command/README.md) — the commands whose outcomes
  some events reply to (`event.Reply`).
- [`pkg/hub`](../hub/README.md) — the fan-in that delivers events.
- [`pkg/journal`](../journal/README.md) — the durable writer that
  sequences enduring events.

## How it is designed

```
              one concrete event type
                       │
   ┌───────────────────┴───────────────────┐
   │ Header (identity, cause, agency)       │
   │  exactly one lifecycle mixin            │
   │    ephemeral | enduring | terminal       │
   │  exactly one scope mixin                 │
   │    sessionScoped | loopScoped            │
   └───────────────────┬───────────────────┘
                       │
                       │  satisfies
                       ▼
                    Event
            ┌──────────┴───────────┐
            │  Class() / Scope()    │
            │  EndsTurn()           │
            │  EventHeader()        │
            └──────────┬───────────┘
                       │
        ┌──────────────┴──────────────┐
        ▼                             ▼
   fan-in: pkg/hub            durable: pkg/journal
   (class-aware overflow)    (only ClassEnduring persisted)
```

### Why the mixins

The "exactly one of each" rule is enforced from both sides:

- Embedding **two** of a kind makes the promoted selector ambiguous —
  the file does not compile.
- Embedding **zero** leaves the method missing — the type does not
  satisfy `Event`.

So any count other than one fails the compile-time assertions in `doc.go`
first.

### Drift, integration, and compaction events

A few event groups carry extra structure worth knowing about:

- **`DriftAssessment`** / **`ConfigurationAdopted`** — at restore time the
  rig runs a `RestoreDecider` against the assessment and records the
  decision as a durable `ConfigurationAdopted`. The assessment compares
  the live loop fingerprint to the journal-recorded one and classifies
  each change as `Info` or `Warn`.
- **`IntegrationStatus`** — an integration is any live external capability
  a session runs alongside its loops (MCP, a language server, a plugin
  host). Harness does not implement one; this event is the coarse,
  protocol-neutral way one reports how it's doing
  (`Starting`/`Ready`/`Degraded`/`Failed`/`Closed`).
- **`CompactionStarted` / `CompactionCommitted` / `CompactionRejected`**
  — per-loop context compaction lifecycle, with the context basis
  (revision, through-event-id) the compactor ran against.
