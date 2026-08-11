# storage

Neutral, stdlib-only storage contracts for durable session/workspace persistence.

`storage` defines four small primitives — **`Ledger`** (append-only, CAS-sequenced
record log), **`Leaser`** (single-writer epoch lease), **`KV`** (revision-CAS key/value),
and **`Blobs`** (content-addressed immutable bytes) — plus a typed error taxonomy, the
`AppendDefinite` ambiguity resolver, an in-memory reference backend (`memstore`), and a
backend conformance suite (`storetest`). Consumers depend only on these interfaces;
concrete backends (`fsstore`, `natsstore`, …) live in their own modules.

This module has **zero third-party dependencies** and will keep it that way.
