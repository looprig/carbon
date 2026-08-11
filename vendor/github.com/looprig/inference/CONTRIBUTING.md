# Contributing to looprig/inference

Thanks for considering a contribution. `inference` is the provider-neutral
inference seam for the looprig ecosystem: it defines the `Client`/`Request`/
`Response` invocation contract and the domain packages around it — `model`
(model identity, origin, API format, capabilities, context limits, sampling,
effort), `stream` (pull-based stream frames and readers), `codec` (request
encoding / response decoding), `contextcount` (context counting and
compatibility checks), `usage` (normalized token-usage), `auth` (generic
authenticators), `route` (wire-API routers), `transport` (the HTTP execution
layer), `failure` (provider-neutral API/network/binding errors), and `wire`
(protocol framing). Provider-specific integrations (cloud auth, request
signing, model catalogues) live outside this module, in `looprig/llm`.

This module only depends on `github.com/looprig/core` (via a local
`replace`) and the Go standard library.

## Before you write code

For anything beyond a small, mechanical fix, open an issue or discuss first
so we can agree on direction before you spend the time. The package
boundaries this module follows are documented in
[`docs/plans/2026-07-16-package-boundaries-design.md`](docs/plans/2026-07-16-package-boundaries-design.md);
non-trivial design changes should get a similar dated design doc in
`docs/plans/` (`YYYY-MM-DD-<topic>-design.md`) before or alongside the
implementation, so future contributors can see why the code is shaped the
way it is.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make fmt        # gofmt the whole module in place
make fmt-check  # fail if any tracked file is not gofmt-clean
make test       # go test -race ./...
make lint       # fmt-check + go vet + staticcheck + gosec
make vuln       # go mod verify + govulncheck
make secure     # lint + vuln
```

This module does **not** vendor its dependencies — per the Makefile, it
depends only on `core` via a local `replace` and the standard library, so
there is no `vendor/` tree to maintain or refresh.

Build with `CGO_ENABLED=0 go build -trimpath` so binaries never leak local
paths.

## Tests

- Use table-driven tests when several cases share setup and assertion
  shape; use a focused test when a single scenario is clearer.
- A test that passes without `-race` but fails with it is not passing —
  `make test` always runs with `-race`.
- Cover the happy path, boundary values (zero/empty/max), error cases
  (invalid/missing/wrong type), and domain-specific edge cases.
- Never assume a test framework or script beyond what's already in use; the
  `Makefile` is the source of truth, and if you change how tests run,
  update it.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR.
- Don't commit secrets, tokens, or credentials.
- `inference` is a shared module that other looprig repos build against via
  local `replace` directives, so a new external dependency needs prior
  discussion before it lands in `go.mod` — prefer the standard library.
- Don't update the `Makefile` or `go.mod` unless the change is the point of
  the PR.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
