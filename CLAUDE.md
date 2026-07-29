# CodeRig contributor instructions

CodeRig is the reference coding Rig built from looprig modules. This repository owns coding behavior and product assembly. Reusable runtime, presentation, tools, sandbox, storage, and inference machinery belongs in the module that defines that abstraction.

## Architecture

- `internal/app/swarm.go` assembles the primary operator and the fixed leaf Loops.
- `internal/app/access.go` owns the three named product access profiles, the independent reviewer restriction, the product `tool.invoke`/`context.load` access source, and the secret-free access-config digest. `internal/app/egress.go` resolves the parent proxy environment into one validated session egress route. `internal/app/permissions.go` owns the automatic Bash-family catalog and the permission-file locations.
- `internal/app/toolsets.go` performs direct sandbox assembly: one `sandbox.ExecutorSet` per role authority, the combined `harness/pkg/gate` access gate per role (which resolves the calling loop's executor by Loop ID and binds it as the structural grant issuer), and the standard tool definitions bound to that set. There is no policy-translation bridge.
- `internal/catalog/operator` and `internal/catalog/reviewer` own role identity and prompts.
- `cmd/coderig` imports the private `internal/app` composition boundary. The module root has no Go package.
- The primary operator may delegate to a non-delegating operator or reviewer. Leaves do not receive delegation capability. The operator-primary and operator leaf share the operator profile but get separate executor instances (separate grants and scratch HOME) keyed by Loop ID; the reviewer always uses `sandbox.Restrict(selected, reviewerCeiling)` and its own executor set.
- Each Loop receives only the individual tools it needs. The reviewer has no file mutation tools.
- `github.com/looprig/tools` provides optional standard tools; `github.com/looprig/sandbox` provides profiles, executors, grants, and the egress proxy; `github.com/looprig/harness/pkg/gate` provides dependency-free access evaluation and prompt routing. CodeRig wires these directly.
- `github.com/looprig/tui/sessionadapter` adapts a session controller to the TUI. The composition-root `RuntimeAgent` also implements `tui.SessionPresenter`, supplying the session's fixed profile name, workspace root, and permission diagnostics.
- The access profile is FIXED at Open and never changes in-session; the TUI only displays it. New, restore, headless, and interactive construction share one `Open` path (`openRuntimeAgent`); interactive and headless differ only in the permission store (workspace vs read-only) and the gate evaluator (interactive vs headless). The runtime agent OWNS every executor-set closer: a partial-construction failure closes what it built, and shutdown closes each set exactly once. A changed selected profile, reviewer restriction, or egress route identity/guarantees changes the durable access-config digest and so rejects a restore.

Do not add a generic agent registry or model tier catalog. The roster is a small fixed set of Loop definitions. Runtime choices belong in Loop modes and model effort. Do not reintroduce a confinement bridge, a security-limit ordinal, or any in-session authority-mutation surface.

## Placement

Keep behavior here when it is specific to a coding Rig, such as prompts, role tool selection, coding modes, model defaults, and product flags.

Move behavior to its owning module when it is reusable across products. Examples include session adapters, standard tool implementations, sandbox profile/executor/grant enforcement, gate evaluation, persistence mechanics, and generic Loop or Rig lifecycle behavior.

Prefer direct assembly over local wrappers that only rename another module's API.

## Security

- Give each Loop the minimum tool set and the least-authority access profile it needs.
- Keep mutating, command, and network effects human-gated unless enforced guarantees justify automatic approval.
- Treat `Bash` as intentionally shell-based. Permission checks and OS confinement are its boundaries.
- Validate CLI input before constructing the Rig.
- Never log secrets or place them in audit summaries. Upstream proxy credentials live only inside the sandbox egress route and never enter the fingerprint, permission file, logs, or child environment.
- Fail closed when access, permission, identity, or durable policy state is uncertain.

## Code and tests

- Keep packages cohesive. Split code when ownership or invariants differ, not to satisfy an arbitrary size rule.
- Introduce interfaces at consumer boundaries or when multiple implementations justify them.
- Use typed errors when callers need to classify or recover. Wrapped ordinary errors are fine for contextual failures.
- Use table-driven tests when cases share setup and assertions. Focused tests are fine for singular behavior.
- Add integration tests for process, filesystem, network, or durable storage boundaries.
- Run `gofmt` on changed Go files and `go test -race ./...` before committing.

## Commands

```bash
make build
make test
make lint
make secure
```

`make test-integration` runs the `//go:build integration`-tagged suite,
including process/filesystem/network/durable-storage-boundary tests and the
live permission-review end-to-end tests. It is not run by `make test` or CI
by default; run it before any release touching permission review, restore,
or access-profile behavior.

The binary and command are both named `coderig`.

## Dependencies

Prefer the standard library. Ask before adding a new third-party dependency. Sibling looprig modules already in `go.mod` are approved architecture dependencies.

The following development-only analysis tools are approved and declared through
the Go toolchain:

- `github.com/securego/gosec/v2/cmd/gosec`
- `golang.org/x/vuln/cmd/govulncheck`
- `honnef.co/go/tools/cmd/staticcheck`

Run `make secure` before every commit. It verifies formatting, runs `go vet`,
Staticcheck, Gosec, `go mod verify`, and Govulncheck.

Do not commit, push, or rename a remote repository unless the user explicitly asks.
