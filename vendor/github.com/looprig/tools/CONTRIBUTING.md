# Contributing to looprig/tools

Thanks for considering a contribution. `tools` provides the optional standard
tool implementations for looprig Loops — Bash, ReadFile, WriteFile, EditFile,
Glob, Grep, WebSearch, Fetch, Tasks (`TaskCreate`, `TaskUpdate`, `TaskGet`, and
`TaskList`), AskUser, and Skill — built against the
contracts the harness defines. This file is the short guide for working in
*this* repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative
   source for the ownership boundaries, design, dependency, testing, and
   style rules the whole module follows. PRs that contradict it will be
   asked to change.
2. Read [`docs/specs/module.md`](docs/specs/module.md) for why the module is
   shaped as independent per-tool definitions instead of a bundle, and skim
   [`docs/plans/`](docs/plans/) for the design-doc style the project uses
   (e.g. `2026-07-15-tool-package-layout-design.md`).
3. Open an issue for anything non-trivial so we can agree on direction
   before you spend the time.

## Design and security rules (the short version)

- **Preparation is owned by the tool, evaluation by the harness.** Every
  tool is a `tool.CallPreparer`: `PrepareCall` decodes, validates, and
  canonicalizes its arguments once and emits one typed `tool.Request`.
  Tools classify capabilities; they never decide Deny/Gated/Allow — that
  belongs to the harness gate evaluator. Execution consumes the typed
  prepared artifact only; a prepared tool without it fails closed.
- **`permission` is the shared rule library, not a tool.** Its canonical
  requirement-match encodings are the pinned contract between tool
  preparation and stored-rule matching. Rule-file load failures are errors.
- **Respect the module boundaries.** Export one `tool.Definition` per
  independently selectable capability by default — never bundle unrelated
  tools. `Tasks` is the deliberate related-family exception because its four
  operations share one Loop-local graph. Read workspace root, observations,
  coordinators, ceiling, and delegates from `tool.Bindings`. Depend on harness
  contracts, never harness internals. Never import sandbox or confinement
  directly; accept their behavior through the harness runner and permission
  interfaces.
- **Tasks are a deliberate related-family bundle.** `TaskDefinitions()` produces
  four model-facing operations backed by one Loop-local graph per definition
  build. Modes in one Loop share that graph; parent and child Loops do not.
  Harness owns and injects `Subagent`, so task coordination across Loops uses
  delegation messages rather than shared task memory.
- **No sibling tool-package imports.** Public tool packages stay independent
  of each other; the one allowlisted exception is `permission`. Shared
  mechanics live under `internal`. `dependency_test.go` at the module root
  enforces this — extend it, never weaken it, when adding packages.
- **Fail secure.** Invalid input fails during preparation, not later.
  Bash's one deliberate, recorded exception: the model-supplied command
  goes to `sh -c` (an argv list can't express shell features); the security
  boundary is the permission gate over the prepared command-backed request
  plus the injected confined runner, not the argv shape.
- **Prefer stdlib.** External packages require explicit user approval in
  the conversation that adds them. Once approved, the package is added to
  the approved list in `CLAUDE.md`. Never `go get` without that approval.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make test      # go test -race ./...
make fmt       # gofmt the whole module in place
make fmt-check # gofmt check only (fails if anything is unformatted)
make lint      # fmt-check + go vet + staticcheck + gosec
make vuln      # go mod verify + govulncheck
make secure    # lint + vuln
```

For the integration suite: `GOWORK=off go test -race -tags integration ./...`.

## Tests

- Use table-driven tests when cases share structure and focused tests for
  singular behavior.
- Cover malformed input, cancellation, containment, permission denial,
  audit output, and concurrency where relevant.
- A test that passes without `-race` but fails with it is not passing.
- README code snippets are compiled by `example_readme_test.go` — keep them
  in lockstep with any README change.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. If a change spans modules, open a PR per
  module and stack them.
- Write a clear description: what, why, the design alternative you
  rejected, and how you verified. `make secure` output is welcome in the
  PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials. Don't add a new external
  dependency without prior approval (see `CLAUDE.md`).
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` unless the change is
  the point of the PR.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
