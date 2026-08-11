# CLAUDE.md — storage

`storage` is a **stdlib-only leaf module**. It defines storage contracts; it depends on
nothing outside the Go standard library, and it must stay that way.

## Dependencies

- **No third-party dependencies, ever.** There is no scenario in this module where stdlib
  is insufficient. Do not `go get` anything. Do not add a `require` line beyond the module
  itself. If a task seems to need an external package, the task is wrong for this module.
  (`make secure`'s staticcheck/gosec/govulncheck are PATH-resolved dev-tool binaries, not
  go.mod dependencies — this does not contradict the rule above.)

## Code rules (same discipline as the consuming repos)

- **Strict typing.** No `any`/`interface{}` except at explicit serialization boundaries,
  narrowed immediately. Named types (`type UserID string`) over bare primitives when the
  value carries domain meaning. No untyped magic numbers/strings.
- **All errors are typed.** Every distinct failure mode is a concrete struct with an
  `Error()` method (and `Unwrap()` when it carries a cause). Never return
  `errors.New(...)`/`fmt.Errorf(...)` from a package-level API. Callers classify with
  `errors.As` — never by string.
- **Contracts first.** Write the interface, then the implementation. Keep interfaces small
  and segregated; a caller never depends on methods it does not use.
- Return errors explicitly; never swallow with `_`.
- Functions over ~30 lines invite an SRP check before growing further.

## Security

- Validate all names/keys at the boundary (`ValidateName`) before they reach any backend
  location. The name grammar is canonical by construction — no two valid names alias one
  location.
- `crypto/rand` for anything security-sensitive; never `math/rand`.
- Every I/O method takes a `context.Context`; callers set deadlines. No unbounded blocking.
- Fail secure: on error or ambiguity, deny/deny-by-default; never fall through.

## Testing

- **Table-driven tests, mandatory**, each with `t.Parallel()`. Cover happy path, boundary
  values (zero/empty/max), error cases, and domain edge cases (absent ledger, nil/empty
  payloads, unknown kinds).
- **Always `-race`:** `GOWORK=off go test -race ./...`. A test that needs `-race` to fail
  is not passing.
- **Fuzz any parser** of external input (`go test -fuzz=FuzzXxx -fuzztime=30s ./...`).
- `gofmt`-clean and `go vet`-clean at all times.

## Build & workspace

- **Every Go command runs with `GOWORK=off`** — there is a `go.work` at `~/code` that must
  not capture this module.
- `make check` (fmt-check + vet + `-race` test) before every commit.
