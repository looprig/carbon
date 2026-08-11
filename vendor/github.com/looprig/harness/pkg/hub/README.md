# pkg/hub

`pkg/hub` is the session-level **event fan-in**: a publish/subscribe hub
with a federated-quiescence model. Loops publish events through the
narrow `eventPublisher` contract; consumers (TUI, CLI, durable journal,
HTTP SSE) subscribe with an `event.EventFilter`. The hub aggregates loop
activity into one `sessionState` so a headless run can `WaitIdle` without
any session goroutine.

## What is hub?

A `*hub.Hub` is owned by a `Session`. It exposes:

- **To loops** — only `PublishEvent` / `PublishEventChecked` (the
  `eventPublisher` interface). Loops never see subscribers, state, or
  waiters.
- **To consumers** — `SubscribeEvents`, which returns an
  `*EventSubscription` carrying one bounded egress channel of
  `event.Delivery` values.
- **To the session** — `ExpectTurn` / `CancelExpectTurn` / `StopSession`
  / `WaitIdle` for the quiescence model.

The hub is constructed with `hub.New(sessionID, opts...)`. Default options
install a nop journal appender, a real-clock/real-uuid `event.Factory`,
and a nop fault reporter — i.e. a hub that publishes and fans out but
persists nothing. The composition root injects the real trio via
`WithAppender` / `WithFactory` / `WithFaultReporter`.

## How to use

Consumers reach the hub through `Session.SubscribeEvents`:

```go
sub, err := session.SubscribeEvents(nil)  // nil = all events
if err != nil { return err }
defer sub.Close()
for delivery := range sub.Events() {
    fmt.Println(delivery.Event.Scope(), delivery.Event.Class())
    if delivery.Event.EndsTurn() { /* this turn is over */ }
    if delivery.JournalSeq > 0    { /* an enduring event was persisted */ }
}
_ = sub.Err()  // nil on Close; *SubscriptionLossError on hub-forced loss
```

A subscriber filters by passing an `event.EventFilter`:

```go
onlyTurns := event.EventFilter{
    MatchTurns: true,
}
sub, _ := session.SubscribeEvents(onlyTurns)
```

A headless runner blocks on quiescence without a session goroutine:

```go
// internally, the session:
hub.ExpectTurn(turnID)
hub.WaitIdle(ctx)  // returns when the active set drains and the durable edge completes
```

## Sibling packages

- [`pkg/event`](../event/README.md) — `event.Event`, `event.EventFilter`,
  `event.Delivery`, the lifecycle and scope mixins the hub routes by.
- [`pkg/identity`](../identity/README.md) — the `event.Header` producer
  identity the hub stamps on synthesized session events.

## How it is designed

The hub is a **single fan-in point** with three concurrency domains, each
under its own lock:

```
   Loops ──PublishEvent──►  ┌────────────────────────────────────────┐
                            │ Hub                                     │
                            │                                         │
                            │  publishMu   ── admission seal (abort)   │
                            │  activityMu  ── active-set ↔ durable edge │
                            │  mu          ── subs, state, waiters     │
                            │                                         │
                            │  appender (durable write, OUTSIDE mu)    │
                            │  factory  (event.Header stamper)         │
                            │  reporter (fault escalation seam)        │
                            │  idleBoundary (native durable completion) │
                            └──────┬─────────────────────────────────┬──┘
                                   │                                 │
                                   ▼                                 ▼
                          ┌────────────────┐               ┌────────────────┐
                          │ Subscribers     │               │ WaitIdle        │
                          │ (TUI / SSE /    │               │ waiters         │
                          │  journal / test)│               │ (headless run)  │
                          └────────────────┘               └────────────────┘
```

- **`publishMu`** is the construction-abort admission seal. `AbortSession`
  closes admission atomically and returns `publishDrained` so the session
  retains journal ownership until every already-admitted publisher has
  left the appender path.
- **`activityMu`** orders every active-set mutation with its derived
  durable edge. It may be held across I/O, unlike `mu`; no state is read
  or written under this lock alone.
- **`mu`** guards `subs`, `state`, and `waiters` together. One lock keeps
  the subscriber-set snapshot consistent with the active/phase
  transition. The critical section only copies subscribers; `mu` is
  always released before durable I/O, workspace boundaries, reporting,
  or delivery.

### Bounded egress and the overflow policy

Each subscription owns **one bounded egress channel** (default 256). A
slow subscriber never blocks a publisher or another subscriber, so
delivery is a non-blocking send into this buffer; on overflow the
class-aware policy applies:

- **Ephemeral** events are **dropped** (a `TokenDelta` is expendable).
- **Enduring** events **fail the subscription** with a typed
  `*SubscriptionLossError`. The subscriber learns it lost the stream —
  so it can re-subscribe and re-sync — rather than silently missing an
  authoritative event.

`SessionStopped` is an event, not a stream terminator: a subscription
ends only on `Close`, loss, or hub teardown.

### Federated quiescence

`WaitIdle` blocks until the active set is empty **and** the durable edge
that corresponds to that transition has completed. Idle-boundary
generations close the fast-path window between an in-memory Active→Idle
transition and its native durable completion: a generation prevents an
older overlapping boundary from clearing a newer pending edge. A sticky
`waiterFailure` survives until the owning recoverable operation clears
its exact generation token, so stale recovery cannot erase a later fault.
