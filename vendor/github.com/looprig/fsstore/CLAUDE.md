# CLAUDE.md — fsstore

`fsstore` implements storage's storage primitives (Ledger, Leaser, KV, Blobs) over the
local filesystem. It is a concrete backend, not a contract module.

## Dependencies

- **stdlib + `github.com/looprig/storage` only.** No other third-party dependency, ever.
  Do not `go get` anything else. If a task seems to need an external package, the task is
  wrong for this module.
- `staticcheck`, `gosec`, and `govulncheck` used by `make secure` are PATH-resolved
  dev-tool binaries, not `go.mod` dependencies — they do not contradict the rule above.

## Code rules

- **Strict typing.** No `any`/`interface{}` except at explicit serialization boundaries,
  narrowed immediately. Named types over bare primitives when the value carries domain
  meaning. No untyped magic numbers/strings.
- **All errors are typed.** Every distinct failure mode is a concrete struct (or a sentinel
  for a context-free leaf). Never return bare `errors.New(...)`/`fmt.Errorf(...)` from a
  package-level API. Callers classify with `errors.As` — never by string.
- Return errors explicitly; never swallow with `_`.
- Contracts first: write the interface, then the implementation.

## Security

- Fail secure: on error or ambiguity, deny/deny-by-default; never fall through.
- Validate all names/keys at the boundary before they reach any filesystem location.
- `crypto/rand` for anything security-sensitive; never `math/rand`.
- `filepath.Clean` and verify the result stays within the store root before opening any
  path derived from a name/key.

## Testing

- **Table-driven tests, mandatory**, each with `t.Parallel()`. Cover happy path, boundary
  values (zero/empty/max), error cases, and domain edge cases.
- **Fuzz every parser of external input** (the on-disk frame codec, path derivation).
- **Always `-race`.**
- **Every Go command uses `GOWORK=off`** — the `~/code/go.work` must not capture this
  module. Run `make check` before every commit.
