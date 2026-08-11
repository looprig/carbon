# pkg/sessionstore

`pkg/sessionstore` is the **session-scoped facade** over a
`storage.Composite` backend. It is the in-tree `SessionJournal`
implementation that a [`pkg/rig`](../rig/README.md) is configured with;
it owns the durable event/command log, the replay-free session catalog,
and the workspace ref → blob offload threshold.

The storage primitives themselves live in the sibling
[`looprig/storage`](https://github.com/looprig/storage) module; the
concrete backend (filesystem, NATS, rclone) lives in one of
`looprig/fsstore`, `looprig/natsstore`, `looprig/rclonestore`. This
package is the **session-shaped adapter** between those generic
primitives and the `pkg/journal` contract.

## What is sessionstore?

- **`Open(b *storage.Composite, opts...) (*Store, error)`** — the
  constructor. Validates the composite (rejects a nil composite or any
  nil primitive — `Ledger`, `Leaser`, `KV`, `Blobs` — with a typed
  `*InvalidBackendError`, fail-closed). Resolves the options from
  defaults (512 KiB offload threshold) plus overrides.
- **`Store`** — the facade. Holds the assembled `*storage.Composite`
  plus resolved `Options`. Construct it only via `Open`.
- **`SessionJournal`** — `Store` satisfies `pkg/journal.SessionJournal`
  for the session's serialized writer: `Append(ctx, rec) (seq, err)`
  encodes the record, offloads large payloads to `Blobs`, and appends
  the envelope to the session's `Ledger`.
- **`Catalog`** — the replay-free session index. Projected from the
  event stream as events are appended; keyed by session id. Records
  `SessionMeta` (id, title, status, loop count, model, hustle usage,
  timestamps) and per-session derived state. A picker reads it without
  replaying any journal.
- **`Lease`** — the single-writer epoch lease a session acquires on
  open so two processes can't own the same session at once.
- **`Options`** — `WithOffloadThreshold(n)` is the only knob today: the
  payload size (bytes) above which a record is stored as an out-of-line
  blob instead of inline in the ledger. Default 512 KiB; non-positive
  values are ignored.

## How to use

A consumer wires a `*sessionstore.Store` into a rig:

```go
import (
    "github.com/looprig/harness/pkg/sessionstore"
    "github.com/looprig/storage"
    // import a backend, e.g.
    // fsstore "github.com/looprig/fsstore"
)

backend, err := fsstore.Composite(rootDir)  // *storage.Composite
if err != nil { return err }

store, err := sessionstore.Open(backend,
    sessionstore.WithOffloadThreshold(1<<20),  // 1 MiB
)
if err != nil { return err }

r, err := rig.Define(
    rig.WithSessionStore(store),
    /* ... */
)
```

`pkg/rig` and `pkg/session` reach the store through the rig; you don't
call `Append` yourself. The `Catalog` is reachable through the same
store for a session picker (a "recent sessions" list, a restore UI).

## Sibling packages

- [`pkg/journal`](../journal/README.md) — the `SessionJournal`
  contract this package implements.
- [`pkg/event`](../event/README.md) — the events the catalog projects.
- [`pkg/hustle`](../hustle/README.md) — hustle usage the catalog
  aggregates.
- [`pkg/workspacestore`](../workspacestore/README.md) — the workspace
  snapshot store wired alongside this one in a rig with a workspace
  placement.
- [`pkg/rig`](../rig/README.md) — `rig.WithSessionStore` takes a
  `*sessionstore.Store`.
- `github.com/looprig/storage` — `Ledger`, `Leaser`, `KV`, `Blobs`.
- `github.com/looprig/fsstore` / `looprig/natsstore` / `looprig/rclonestore`
  — the backend modules that produce a `*storage.Composite`.

## How it is designed

```
       *storage.Composite (looprig/storage)
            │
            │  sessionstore.Open (validates + wraps)
            ▼
       *sessionstore.Store
            │
   ┌────────┼────────────────┐
   │        │                │
   ▼        ▼                ▼
 Ledger   Blobs              KV
 (append) (offload ≥512KiB)  (catalog)
   │        │                │
   │        │                │
   ▼        ▼                ▼
 session journal          Catalog
 (pkg/journal)         (replay-free index)
```

### Layout

Every session's records share a leading name segment:

- ledger name: `sessions/<uuid>`
- blobs live under: `sessions/<uuid>/blobs/...`
- catalog key: `sessions/<uuid>/catalog`

The layout is the contract between `Open`, `Append`, `Replay`, and the
`Catalog`; it is enforced by the named constants in this package
(`sessionsPrefix`), not by string surgery at the call sites.

### Large-record offload

A record whose payload exceeds the offload threshold (default 512 KiB,
under storage's 1 MiB per-record ceiling) is stored as an out-of-line
blob; the ledger carries only the envelope framing the blob key. The
threshold sits comfortably under the per-record ceiling so envelope
framing never pushes a record over the limit.

### Catalog is derivable

The `Catalog` is a **replay-free** projection. It is best-effort by
construction: `UpdateOnEvent` never returns a non-nil error (the catalog
is derivable, so a failed index is logged and swallowed inside it). A
failed CAS — `storage.KV` has no unconditional Put, every Put is a
revision compare-and-swap — is retried up to `catalogMaxCASRetries`; a
pathologically contended key surfaces a typed `*CatalogConflictError`
rather than spinning forever. `RepairCatalog` rebuilds a catalog from
the journal under its own scan timeout.

### Fail-closed validation

`Open` rejects a nil composite or any nil primitive field with a typed
`*InvalidBackendError` that names the missing piece (`"composite"`,
`"Ledger"`, `"Leaser"`, `"KV"`, `"Blobs"`), so the composition root
knows exactly what was not wired and never dereferences a nil
primitive later.
