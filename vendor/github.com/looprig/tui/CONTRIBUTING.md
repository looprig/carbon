# Contributing to looprig/tui

Thanks for considering a contribution. `tui` is the interactive terminal
presentation layer in a multi-module Go ecosystem; it depends on
`github.com/looprig/harness` for runtime contracts, and harness never
imports `tui`. This file is the short guide for working in *this*
repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative
   source for the design, security, dependency, build, and code rules this
   module follows. PRs that contradict it will be asked to change.
2. Note the package boundary: the root package is the module's stable public
   facade; the coupled Bubble Tea application lives in
   `internal/presentation`. Internal packages never import the root facade.
3. Skim a couple of recent files in [`docs/plans/`](docs/plans/) for the
   design-doc style the project uses.
4. Open an issue for anything non-trivial so we can agree on direction
   before you spend the time.

## Design and security rules (the short version)

- **Strict typing everywhere.** No `any`/`interface{}` past a serialization
  boundary. Named types (`type UserID string`) over bare primitives when the
  value has domain meaning. All domain concepts are typed structs, never
  `map[string]interface{}`.
- **All errors are typed.** Sentinel or typed errors for public failures
  callers classify with `errors.As`; wrapped ordinary errors for contextual
  failures callers only report. Never swallow with `_`.
- **Security is first-class.** Validate at every boundary. Authenticate
  before authorize, authorize before act. Fail secure: on error or
  ambiguity, deny by default. Never log secrets, tokens, or PII. Use
  `crypto/rand` for anything security-sensitive.
- **Least privilege always.** Every component, goroutine, and service gets
  only the permissions it needs. Never pass a full config or god-object when
  a narrow interface suffices.
- **Prefer stdlib.** External packages require explicit user approval in
  the conversation that adds them. State what the package is, why stdlib is
  insufficient, and what the package adds. Once approved, the package is
  added to the approved list in `CLAUDE.md`. Never `go get` without that
  approval.
- **Open-Closed + Interface Segregation.** Small focused interfaces; never
  widen an existing interface. Liskov substitution: every implementation
  honors the full contract — no panicking, no unexpected errors, no
  silently doing less.
- **TUI stays a renderer.** This module does not implement command tools or
  couple to concrete tool/sandbox packages; it renders permission
  interactions supplied by `sessionadapter` and stays independent of them.

## Build, test, and secure

This module **vendors** its dependency tree (`vendor/`) and also
participates in the parent `../go.work` workspace for local
multi-module editing. Those two facts pull in opposite directions:
`-mod=vendor` (which the build needs for a reproducible, auditable
dependency tree) is incompatible with an active `go.work`. The Makefile
handles the build side of this for you by exporting `GOFLAGS=-mod=vendor`,
but that only wins once the workspace itself is out of the way — so run
every target with the workspace disabled:

```sh
GOWORK=off make secure
```

The Makefile is otherwise self-contained:

```sh
GOWORK=off make fmt             # gofmt the whole module in place
GOWORK=off make test            # go test -race ./...
GOWORK=off make test-integration # go test -tags integration -race, scoped to this module
GOWORK=off make lint            # fmt-check + vendor-check + vet + staticcheck + gosec
GOWORK=off make vuln            # go mod verify + govulncheck
GOWORK=off make secure          # lint + vuln — run this before every commit
GOWORK=off make vendor          # go mod vendor, then scrub + verify no VCS metadata leaked in
```

One more wrinkle: Go 1.26 can't resolve `go tool`-declared binaries (
staticcheck, gosec, govulncheck) from a vendor tree, so the Makefile
resolves those specific analysis tools via `TOOL_GOFLAGS := -mod=mod`
while everything else — the actual application build and tests — stays on
`-mod=vendor`. You don't need to set this yourself; it's baked into the
`lint`/`vuln` targets. `gosec` is additionally scoped to this module's own
package directories (not a plain `./...`) so it doesn't descend into the
nested `.worktrees/` checkouts, which are separate modules.

Build binaries with `CGO_ENABLED=0 go build -trimpath` so they never leak
local paths. Fuzz any parser of external input:
`go test -fuzz=FuzzXxx ./pkg -fuzztime=30s`.

## Tests

- **Table-driven tests, mandatory** when several cases share setup and
  assertion shape. Each subtest calls `t.Parallel()`. Cover the happy path,
  boundary values (zero/empty/max), error cases (invalid/missing/wrong
  type), and domain edge cases.
- A test that passes without `-race` but fails with it is **not passing**.
- Integration tests (tagged `//go:build integration`) live in
  `*_integration_test.go` files and are excluded from the default
  `go test ./...` run. Run them explicitly with
  `GOWORK=off make test-integration`.
- Never assume a test framework or script. The `Makefile` is the source of
  truth; if you change how tests run, update it.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules, open a PR per
  module and stack them; the `replace` directives let each module build
  against the others' local checkout.
- Write a clear description: what, why, the design alternative you
  rejected, and how you verified. `GOWORK=off make secure` output is
  welcome in the PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials. Don't add a new external
  dependency without prior approval (see `CLAUDE.md`).
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` `replace` directives
  unless the change is the point of the PR.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
