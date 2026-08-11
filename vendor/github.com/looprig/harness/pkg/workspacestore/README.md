# pkg/workspacestore

`pkg/workspacestore` captures and restores a session's **workspace tree**
as immutable, content-addressed snapshots over a `storage.Blobs`
backend. Snapshot a working directory to a `Ref`, record the `Ref` in
the session journal, and later materialize it on any host to resume.
An agent's files survive the compute they ran on.

## What is workspacestore?

- **`Store`** — the facade over a `storage.Blobs` backend. Holds only
  that backend and its resolved `Options`; every operation carries its
  own state, so a `Store` is as safe for concurrent use as the backend
  it wraps. Construct it only via `Open`.
- **`Open(b storage.Blobs, opts...) (*Store, error)`** — validates the
  backend, resolves the options (`SpoolDir`, `MaxEntries`, `MaxBytes`)
  from defaults plus overrides.
- **`Ref`** — the immutable, content-addressed snapshot name. The
  canonical form is `v1:sha256:<64 lowercase hex>`; obtain one only from
  `ParseRef` or from the store, never by string surgery. The `v1:`
  prefix is the evolution seam — a future format would parse under
  `v2:` without invalidating stored history.
- **`Snapshot` / `Materialize` / `Extract` / `Archive` / `GC`** — the
  operations: snapshot a tree to a `Ref`, materialize a `Ref` into a
  directory, extract an archive stream with bomb guards, archive a
  ref's bytes, and collect unreachable snapshots.
- **`Options`** — `WithSpoolDir(dir)` for the snapshot spool temp file;
  `WithMaxEntries(n)` and `WithMaxBytes(n)` are the **decompression-bomb
  guards** `Materialize` enforces.

## How to use

A consumer wires a `*workspacestore.Store` into a rig with a workspace
placement:

```go
import (
    "github.com/looprig/harness/pkg/workspacestore"
    "github.com/looprig/storage"
    // import a backend, e.g.
    // fsstore "github.com/looprig/fsstore"
)

backend, err := fsstore.Composite(rootDir)  // *storage.Composite
if err != nil { return err }

wsStore, err := workspacestore.Open(backend.Blobs,
    workspacestore.WithSpoolDir("/var/tmp/harness-spool"),
    workspacestore.WithMaxEntries(1<<20),  // 1 M entries
    workspacestore.WithMaxBytes(8<<30),    // 8 GiB
)
if err != nil { return err }

r, err := rig.Define(
    rig.WithSessionStore(sessionStore),
    rig.WithExclusiveWorkspace(wsStore, "/repo", backend.Leaser),
    /* ... */
)
```

A live session exposes the workspace through the `SessionController`
contract:

```go
ref, err := session.CheckpointWorkspace(ctx)        // snapshot the live tree
err     = session.RestoreWorkspace(ctx, ref)        // materialize a prior ref
```

A `Ref` can also be parsed from a stored string when you need to pass
one through a wire:

```go
ref, err := workspacestore.ParseRef("v1:sha256:" + hexDigest)
if err != nil { /* *InvalidRefError: names the rejected value and the rule */ }
```

## Sibling packages

- [`pkg/rig`](../rig/README.md) — `rig.WithExclusiveWorkspace` /
  `WithSessionWorkspaces` / `WithSharedWorkspace` configure the
  placement; `WithSeedSnapshot` materializes one before any loop
  starts.
- [`pkg/session`](../session/README.md) — `CheckpointWorkspace` /
  `RestoreWorkspace` on `SessionController`.
- [`pkg/sessionstore`](../sessionstore/README.md) — wired alongside
  this one in a rig; the session journal records the `Ref`s `Snapshot`
  produces.
- `github.com/looprig/storage` — `storage.Blobs`, the content-addressed
  immutable bytes primitive.
- `github.com/looprig/fsstore` / `looprig/natsstore` / `looprig/rclonestore`
  — the backend modules that produce a `*storage.Composite` whose
  `Blobs` field this store runs on.

## How it is designed

```
                   live workspace tree (a directory)
                            │
                            │  store.Snapshot
                            ▼
                  ┌──────────────────────┐
                  │ spool archive temp     │  (SpoolDir)
                  │ sha256 the bytes       │
                  └──────────┬───────────┘
                             │
                             ▼
                   workspacestore.Ref
                  "v1:sha256:<64 hex>"
                             │
                             │  Blobs.Put("workspaces/<hex>", archive)
                             ▼
                  storage.Blobs backend
                             │
                             │  store.Materialize
                             ▼
                  ┌──────────────────────┐
                  │ extract archive        │  MaxEntries / MaxBytes bomb guards
                  │ into target directory  │
                  └──────────────────────┘
```

### Ref grammar is the only way in

A `Ref` is opaque. `ParseRef` is the only constructor that takes a
string and returns one; the store's `Snapshot` is the only way to mint
one from a tree. There is no string surgery at any call site, so every
`Ref` in circulation is grammar-valid and its blob key is derivable
without re-validation.

### Bomb guards

`Materialize` enforces two independent ceilings against a hostile
archive:

- **`MaxEntries`** (default 2²⁰ entries) — caps how many archive
  entries `Materialize` will read before failing closed with an
  `*ArchiveLimitError`. A bomb that inflates to countless tiny
  entries cannot exhaust inodes.
- **`MaxBytes`** (default 8 GiB) — caps the cumulative bytes
  `Materialize` will **write** while extracting. It is enforced against
  bytes actually written, never a header's declared size, so a lying
  size field cannot breach it.

### GC

`GC` collects snapshots that are no longer reachable from the session
journal. Reachability is determined by replaying the journal for ref
references; an unreachable ref's blob is deleted from `Blobs`. The GC
is the only thing that deletes blobs — a `Ref` is otherwise immutable
for the life of its session.

### Canonical paths

This package uses `internal/pathutil` for canonical-path resolution
when the workspace root needs symlink resolution. `internal/pathutil`
resolves the deepest existing prefix of a path through symlinks and
appends any missing suffix, so a snapshot of `~/repo` and a snapshot
of `/Users/me/repo` produce the same canonical root.
