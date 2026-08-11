# pkg/journal

`pkg/journal` is the contract for one session's **single serialized
durable writer**. Every command, every enduring event, and every
fence flows through one `SessionJournal`; the log stays totally-ordered
and gap-free, and restore replays it.

This package owns the **contract** — `SessionJournal`, `JournalRecord`,
`EventRecord`, `CommandRecord`, `LeaseFence` — and the
`JournalEventAppender` adapter that the `pkg/hub` fan-in depends on. The
concrete backend lives in [`pkg/sessionstore`](../sessionstore/README.md)
over a `storage.Composite`, wired at the composition root.

## What is journal?

- **`SessionJournal`** — the single serialized writer for one session.
  One method: `Append(ctx, JournalRecord) (seq uint64, err error)`. `ctx`
  bounds the caller's willingness to wait; the implementation carries a
  per-append deadline independent of `ctx` so one stuck call cannot wedge
  the serialized writer forever. Appends are totally ordered: returned
  sequences are strictly monotonic across calls.
- **`JournalRecord`** — the sealed sum a writer persists: an
  `EventRecord` (an `Enduring` event), a `CommandRecord` (the intent log),
  or a `LeaseFence` (an internal fence). Sealed by the unexported
  `isJournalRecord` marker, so the serializer's switch over the sum is
  exhaustive and a foreign type can never masquerade as a record.
  `IdempotencyID()` is the stable per-record id a backend uses as its
  de-dup key so a redelivered append de-duplicates.
- **`JournalEventAppender`** — adapts a `SessionJournal` to the narrow
  "append one Enduring event" seam the `pkg/hub` fan-in depends on. The
  hub holds an unexported `eventAppender` interface; this type satisfies
  it structurally, so the composition root wires it in via
  `hub.WithAppender` **without the hub ever importing the journal
  package** (Dependency Inversion). After a successful append it
  best-effort notifies a `catalogUpdater` so the replay-free session
  index stays current.
- **Errors** — `NilJournalError`, `CommandRouteMismatchError`,
  `*InvalidRecordError`, plus the wrapped backend errors a
  `SessionJournal` implementation returns.

## How to use

You don't. A consumer doesn't hold a `SessionJournal` directly; the
composition root wires one into the hub:

```go
// internal/sessionruntime (illustrative):
appender := journal.NewJournalEventAppender(storeJournal,
    journal.WithCatalog(catalog),
)
hub := hub.New(sessionID,
    hub.WithAppender(appender),
    hub.WithFactory(factory),
    hub.WithFaultReporter(reporter),
)
```

A backend implements `SessionJournal`:

```go
type myJournal struct{ ledger storage.Ledger }

func (j myJournal) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
    payload, id, err := journal.MarshalRecord(rec)  // event/command codec
    if err != nil { return 0, err }
    return j.ledger.Append(ctx, id, payload)
}
```

The default in-tree implementation is `pkg/sessionstore`; the
sibling `looprig/fsstore`, `looprig/natsstore`, and `looprig/rclonestore`
modules provide the `storage.Composite` backends that
`pkg/sessionstore` runs on top of.

## Sibling packages

- [`pkg/event`](../event/README.md) — `EventRecord` wraps an `Enduring`
  event; `event.MarshalEvent` is the strict codec the writer calls.
- [`pkg/command`](../command/README.md) — `CommandRecord` wraps a
  `command.Command` plus its dispatch target; `command.MarshalCommand`
  is the strict codec.
- [`pkg/hub`](../hub/README.md) — the fan-in that owns the
  `eventAppender` seam; `JournalEventAppender` satisfies it.
- [`pkg/sessionstore`](../sessionstore/README.md) — the in-tree
  `SessionJournal` implementation over a `storage.Composite`.
- `github.com/looprig/storage` — `Ledger`, the append-only,
  CAS-sequenced primitive a backend wraps.

## How it is designed

```
                  Hub (pkg/hub)
                       │
                       │ AppendEvent (single seam)
                       ▼
            JournalEventAppender (this package)
                       │
                       │ Append (one serialized writer per session)
                       ▼
                SessionJournal
                       │
                       │ MarshalRecord (event/command codec)
                       ▼
                  storage.Ledger  (looprig/storage)
                       │
                       ▼
            totally-ordered, gap-free, de-duped log
                       │
                       ▼
                   restore replays it
```

### One writer, total order

A `Session` owns **exactly one** `SessionJournal`. Every command and
every enduring event flows through its single `Append`; the returned
sequences are strictly monotonic, so the log is a total order with no
gaps. Restore replays it from sequence zero; foreign-loop backends
recover their foreign session ids from it.

### Sealed record sum

`JournalRecord` is sealed by the unexported `isJournalRecord` marker.
Only `EventRecord`, `CommandRecord`, and `LeaseFence` implement it, so
the serializer's switch over the sum is exhaustive and a foreign type
cannot masquerade as a record. `IdempotencyID()` is the stable
per-record id a backend uses as its de-dup key — an event's `EventID`,
a command's `CommandID`, or a fence's epoch — so a redelivered append
de-duplicates instead of double-writing.

### Strict codec, fail-closed on ephemeral

The writer never persists an `Ephemeral` event. `event.MarshalEvent`
fails closed on one; the `EventRecord` wrapper does not re-check (the
codec is the single validation source). The same single-source pattern
applies to commands: `command.MarshalCommand` is the strict codec the
writer calls, and `ParseApprovalAction` is the single validation source
shared across the wire decoder and the session route.

### Dependency inversion

The hub holds an unexported `eventAppender` interface; this package's
`JournalEventAppender` satisfies it structurally. The hub never imports
`pkg/journal`. The composition root wires the concrete adapter; the hub
depends only on the one-method behavior.
