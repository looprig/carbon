# Contributing to looprig/fsstore

Thanks for considering a contribution. `fsstore` implements storage's storage
primitives (Ledger, Leaser, KV, Blobs) over the local filesystem — a concrete
backend, not a contract module.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`) first. It is the
   authoritative source for the design, security, dependency, and testing
   rules this module follows. PRs that contradict it will be asked to
   change.
2. **No third-party dependencies, ever.** This is the one rule to know
   before you open an editor: `fsstore` depends on the Go standard library
   and `github.com/looprig/storage` only. Never `go get` anything else, and
   never add a `require` for any other module. If a task seems to need an
   external package, the task is wrong for this module — bring it up before
   writing code, don't route around the rule.
3. Open an issue for anything non-trivial so direction is agreed before you
   spend the time.

## Design and security rules (the short version)

- **Strict typing.** No `any`/`interface{}` except at explicit
  serialization boundaries, narrowed immediately. Named types over bare
  primitives when the value carries domain meaning. No untyped magic
  numbers or strings.
- **All errors are typed.** Every distinct failure mode is a concrete
  struct (or a sentinel for a context-free leaf). Never return bare
  `errors.New(...)`/`fmt.Errorf(...)` from a package-level API. Callers
  classify with `errors.As`, never by string. Never swallow an error with
  `_`.
- **Contracts first:** write the interface, then the implementation.
- **Fail secure.** On error or ambiguity, deny/deny-by-default; never fall
  through.
- Validate all names/keys at the boundary before they reach any filesystem
  location. Use `filepath.Clean` and verify the result stays within the
  store root before opening any path derived from a name or key.
- `crypto/rand` for anything security-sensitive; never `math/rand`.

## Build, test, and secure

Every Go command in this module runs with `GOWORK=off` so the parent
`~/code/go.work` never captures it — the Makefile targets already do this
for you.

```sh
make test        # GOWORK=off go test -race ./...
make fmt         # gofmt the whole module in place
make fmt-check   # fails if any file is not gofmt-clean
make vet         # GOWORK=off go vet ./...
make check       # fmt-check + vet + test
make staticcheck # staticcheck ./...
make gosec       # gosec -quiet ./...
make vuln        # govulncheck ./...
make secure      # fmt-check + vet + staticcheck + gosec + vuln
```

`staticcheck`, `gosec`, and `govulncheck` are **not** module dependencies —
the no-third-party-dependency rule above forbids adding them to `go.mod`.
Instead each target resolves the binary from `PATH` (falling back to
`$(go env GOPATH)/bin`) and, if it isn't installed, prints an install hint
and skips gracefully rather than failing the build. Install them locally
with:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
go install github.com/securego/gosec/v2/cmd/gosec@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

Run `make check` before every commit, and `make secure` before opening a
PR that touches anything security- or path-handling-relevant.

## Tests

- **Table-driven tests, mandatory**, each subtest calling `t.Parallel()`.
  Cover the happy path, boundary values (zero/empty/max), error cases, and
  domain edge cases.
- **Fuzz every parser of external input** — notably the on-disk frame codec
  and path derivation from names/keys.
- **Always `-race`.** A test that passes without `-race` but fails with it
  is not passing.
- The `Makefile` is the source of truth for how tests run; if you change
  that, update it.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules (e.g. a contract
  change in `storage`), open a PR per module and stack them.
- Write a clear description: what, why, and how you verified it. `make
  check` (and `make secure` where relevant) output is welcome in the PR
  body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials.
- Don't add a new external dependency — see the rule above. Don't update
  `CLAUDE.md`, `Makefile`, or `go.mod` unless the change is the point of
  the PR.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
