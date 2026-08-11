# pkg/command

`pkg/command` defines the **sealed command union** for the loop actor.
`Command` is a sealed interface: only types in this package can implement
it (the `isCommand` method is unexported). The actor accepts commands on
its `Commands` channel and on its priority `PriorityCommands` lane.

## What is command?

- A sealed `Command` interface with `CommandHeader()` returning `Header`
  (id, agency, route).
- **Submit commands** — fire-and-forget; their outcome is published as a
  typed event, never replied on a per-command channel:
  - `UserInput` — submit a user turn to a loop.
  - `SubagentResult` — deliver a delegate's result back to its parent.
  - `CancelQueuedInput` — drop a queued input without running it.
  - `ProvideUserInput` — answer a `UserInputRequested` gate.
  - `Compact` — request a per-loop context compaction.
- **Control commands** — carry a buffered(1) `Ack` channel so the actor's
  send never stalls:
  - `Interrupt` — `Ack chan bool`; true iff a running turn was cancelled.
  - `Shutdown` — `Ack chan error`; nil on clean exit.
- **Routing helpers** — `GateRoute` (loop + tool-execution addressing for
  a gate reply), `Route` (the loop-side match key), `ApproveAction`
  (exactly the three approval actions; the single validation source
  shared by the strict wire decoder and the session route).
- `Header` / `CommandName` / `CommandField` / `InvalidCommandError` for
  contract violations by internal callers.

## How to use

Consumers don't construct commands directly; they call `Session` methods
that produce them:

```go
session.Submit(ctx, blocks)                 // → command.UserInput
session.SubmitToLoop(ctx, loopID, blocks)  // → command.UserInput routed to loopID
session.Interrupt(ctx)                      // → command.Interrupt (priority)
session.Shutdown(ctx)                       // → command.Shutdown (priority)
session.RespondGate(ctx, gateResponse)      // → command.ApproveToolCall / DenyToolCall / ...
```

A loop backend (see `pkg/loop.Backend`) consumes commands off its
`CommandSink()`. The native actor's `Commands` channel is **unbuffered**;
priority commands travel on a separate bounded lane so an `Interrupt` or
`Shutdown` is never stuck behind a saturated submit lane.

## Sibling packages

- [`pkg/identity`](../identity/README.md) — `identity.Coordinates`,
  `identity.Cause`, `identity.Agency` embedded in `Header` and `GateRoute`.
- [`pkg/event`](../event/README.md) — the events the actor publishes in
  reply to submit commands; the `event.Reply` set whose `ReplyTo()` is
  the command id.
- [`pkg/gate`](../gate/README.md) — `gate.ApprovalAction`,
  `gate.GateResponse`, `gate.CloseReason`, mirrored verbatim by the
  approve/deny commands.

## How it is designed

```
   Consumer ──► Session ──► command.UserInput / Interrupt / Shutdown / ...
                                  │
                                  │  CommandSink (Commands chan)
                                  ▼
                       ┌──────────────────────┐
                       │ Loop actor            │
                       │  select:             │
                       │   priorityCommands ──┤  ◀── Interrupt, Shutdown
                       │   commands         ──┤  ◀── submit / control
                       │   gateReg          ──┤  ◀── pending gate registrations
                       │   snapshots       ──┤  ◀── committed-state queries
                       └──────────┬───────────┘
                                  │
                                  │ outcomes published as events
                                  ▼
                                pkg/event  (via pkg/hub)
```

### Sealed by construction

`Command` is `interface { isCommand(); CommandHeader() Header }`. The
`isCommand` method is unexported, so only types in this package can
implement it — the set of commands is closed at compile time. Adding a
new command requires a matching change to the actor's `select` and to
the session dispatch; nothing else can mint one.

### Strict wire decoding

The marshal/unmarshal pair (`marshal.go` / `validate.go`) is the **single
strict decoder** for commands crossing the wire (e.g. an HTTP gate
response). `ParseApprovalAction` is the one validation source shared by
`DecodeApprovalAction` and the session route, so anything but the three
exact actions fails closed — a malformed or token-bearing record can
neither be journaled nor restored.
