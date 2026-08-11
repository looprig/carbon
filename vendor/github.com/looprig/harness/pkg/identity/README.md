# pkg/identity

`pkg/identity` holds the **shared correlation types** used across the
loop, command, and event packages: the `Coordinates` quartet, the
`Cause` causal edge, and the `Agency` audit enum. It sits below
`pkg/event` and `pkg/command` and imports only `github.com/looprig/core/uuid`,
so embedding these types never forms an import cycle.

## What is identity?

- **`Coordinates`** — the four nesting ids that locate any action in
  the hierarchy: `SessionID ▸ LoopID ▸ TurnID ▸ StepID`. Declared once
  and embedded wherever the full quartet is needed (`event.Header`,
  `Cause`, `GateRoute`). Named `Coordinates` — not `Scope` — because
  `event.Scope` is already the delivery-scope enum.
- **`AgentName`** — the immutable attribution name a loop runs under
  (e.g. `"operator"`, `"code reviewer"`). Stamped on the loop's
  `LoopStarted` at creation and never changes for the life of the loop,
  so the durable record carries a stable answer to "which agent
  produced this?". The zero value (empty string) means `UNSET` — a
  plain loop started without an attribution name, or a record persisted
  before `AgentName` existed.
- **`Agency`** — `AgencyMachine` (the fail-secure default: "our code
  did it") or `AgencyUser` ("a human did it"). It is an audit/observability
  record per action (who approved, who interrupted), **not** the gate
  decision. The zero value is `AgencyMachine` so a missing value never
  falsely claims a human acted.
- **`Cause`** — the direct causal edge: "the thing that caused this".
  Carried in the same id vocabulary as `Coordinates`, plus a
  `CommandID`, `EventID`, `ToolExecutionID`, and a copy of the causing
  command's `Agency` so an event can surface agency without chasing
  the command. Most fields are omitted per event/command; only the set
  ones are populated.

## How to use

You usually don't construct these directly — the runtime stamps them
on every event and command. You read them when you handle an event:

```go
for delivery := range sub.Events() {
    header := delivery.Event.EventHeader()
    sid := header.Coordinates.SessionID
    lid := header.Coordinates.LoopID
    tid := header.Coordinates.TurnID
    cmdID := header.Cause.CommandID   // the command that caused this event
    who  := header.Cause.Agency        // AgencyMachine by default; AgencyUser if a human
    _ = sid; _ = lid; _ = tid; _ = cmdID; _ = who
}
```

A `loop.Define` names a loop with an `AgentName`, and that name is the
attribution stamped on every event the loop emits:

```go
operator, _ := loop.Define(
    loop.WithName(identity.AgentName("operator")),
    /* ... */
)
```

Restore treats an empty stored `AgentName` as distinct from a configured
non-empty one — it does not silently accept the legacy zero as a match —
so a name change is never resumed unnoticed.

## Sibling packages

- [`pkg/event`](../event/README.md) — embeds `Coordinates` and `Cause`
  in `event.Header`; uses `Agency` for audit.
- [`pkg/command`](../command/README.md) — embeds them in `command.Header`
  and `command.GateRoute`.
- [`pkg/loop`](../loop/README.md) — `loop.WithName` takes an
  `AgentName`; `loop.WithDelegates` takes `[]AgentName`.
- [`pkg/hub`](../hub/README.md) — the hub's `event.Factory` stamps
  `Header` on synthesized session events.
- [`pkg/journal`](../journal/README.md) — records carry `Coordinates`
  for routing.

## How it is designed

```
                   identity (this package)
                          │
          ┌───────────────┼───────────────┐
          │               │               │
          ▼               ▼               ▼
   pkg/event        pkg/command       pkg/loop
   (Header)         (Header, GateRoute) (WithName, WithDelegates)
          │
          │  imports only core/uuid
          ▼
       no cycle
```

The package is intentionally a leaf: it depends only on `core/uuid`.
Every consumer embeds its types rather than re-declaring them, so the
correlation vocabulary is one set of fields, named once, and an
`Agency` value is always the same `uint8` no matter which package
reads it.

### Why `Agency` defaults to `Machine`

A missing audit value should never be mistaken for "a human did it".
`AgencyMachine = 0` is the fail-secure default: a record that fails to
set `Agency` reads as "our code did it", never as a forged human
attribution. `AgencyUser` is set explicitly by the path that handled a
human's action (e.g. the session setting it on an `Interrupt` command
constructed from an HTTP `POST /interrupt`).

### Why `AgentName` is a named type, not a bare string

A loop's attribution name has domain meaning; bare strings lose that.
The named type is also the restore-comparison key: a stored empty name
is treated as distinct from a configured non-empty one, which only
reads correctly if the two are the same named type all the way through
the journal, the comparison, and the loop definition.
