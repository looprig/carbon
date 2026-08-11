# fsstore

`fsstore` implements storage's four storage primitives — **Ledger**, **Leaser**, **KV**,
and **Blobs** — over the local filesystem, giving a durable, single-host backend for session
and workspace persistence. The concrete on-disk formats live here, behind storage's neutral
contracts: the ledger is an append-only frame log (length-prefixed, CRC-32C-checked frames)
with automatic torn-tail recovery on reopen; leases are `flock`-fenced lock files carrying a
durable, strictly-increasing epoch counter; KV is one revision-CAS'd file per key (atomic
temp-file + rename writes); and Blobs are content-addressed immutable byte objects. It depends
only on the Go standard library and `github.com/looprig/storage`.

The backend assumes **single-writer-per-name**: reads take no advisory lock and trust that no
one rewrites committed bytes concurrently. Enforcing that discipline is the **caller's
responsibility** — acquire the name's lease via the `Leaser` before writing its ledger or KV
entries. Path containment is lexical (validated names, `filepath.Clean`, a root-prefix check);
it does not resolve symlinks, so owning the store root directory is the deployment's
responsibility.

## Usage

`Open` wires the four backends under one root directory and returns them bundled as a
`*storage.Composite` (the primitives collide on method names, so they are composed as a
field-bundle, not a single all-four type — reach each as `store.Ledger`, `store.Leaser`,
`store.KV`, `store.Blobs`). Hand the bundle to a consumer such as `sessionstore.Open`:

```go
store, err := fsstore.Open(fsstore.Options{Root: "/var/lib/looprig"})
if err != nil {
    return err
}
defer store.Close() // releases in-process state; idempotent

sess, err := sessionstore.Open(store.Backend()) // or store.Composite
```

`Root` is required (an empty `Root` is rejected with an `*OptionsError`) and is created at
`0700` if absent. `Close` releases the ledger backend's in-process cache and is idempotent;
after `Close` the `Store` must not be reused.
