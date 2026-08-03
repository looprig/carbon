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

## Model catalogue and credentials

- The `planner`, `builder`, and `reviewer` roster and its role policy remain fixed in code. Production model data is external configuration, loaded once at the composition boundary from `~/.looprig/models.json`.
- The model catalogue is operator-managed and read-only to CodeRig: the loader never creates, rewrites, or changes the mode of the file. On Unix, the file must be owner-only (`0600`), must be a regular file, and must not be a symlink.
- Inline API keys are permitted only in this machine-wide file because it is outside repositories and owner-only. Never put provider keys in `.env`, provider-key environment variables, command-line arguments, logs, fingerprints, permission files, or child environments.
- Native permission persistence is separate and remains per workspace at `~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json`. The global model catalogue is not a permission store.
- ACP children may be gateway-backed or native-auth and receive posture metadata only. Gateway children use the loopback proxy; native children use the selected harness's existing login state. Neither receives provider API keys or a native `permissions.json`; CodeRig owns sandbox and permission enforcement.
- `native_acp` is optional. An absent or disabled profile contributes no native runtime. An enabled profile with omitted `models` is harness-managed and passes no model or effort selector; an explicit non-empty `models` list requires each native delegate default to name one configured alias.
- Defaults are source-aware: omitted `source` remains gateway-backed, while native defaults must set `source: "native"`. Native login environment variables are isolated by the native allowlist and must not be replaced with provider-key or model-selection environment variables.
- Production assembly must not reintroduce frozen model rows, fixed model aliases, provider-key environment reads, or native-model environment variables. The external catalogue is authoritative. Environment variables used to locate ACP executables are launcher settings, not model credentials.

## Permission review

CodeRig can optionally enable classifier-based automatic permission review
(`internal/app/permission_review.go`, design doc
`docs/plans/2026-07-27-permission-classifier-hustle-design.md` in the
`harness` repository §19-20). It composes exactly one
`github.com/looprig/classifiers/pkg/commandsafety` classifier over CodeRig's
shared inference client and registers it with `harness/pkg/rig` via
`rig.WithPermissionClassifiers`; harness/pkg/gate and pkg/hustle own the
actual review mechanism and neutral domain (see that module's own
`pkg/gate/README.md#permission-review` for the full story). CodeRig never
duplicates ceiling-comparison/eligibility logic locally.

- **Enable/disable** — `Config.PermissionReviewEnabled` (default `false`, so
  a zero `Config` never auto-approves anything). There is no CLI flag for it
  today; a caller embedding `coderig.Config` sets the field directly. When
  disabled, `newPermissionReviewRegistration` returns the zero
  (`enabled: false`) registration and `options()` returns `nil` — the
  assembled rig is byte-for-byte the same as if permission review did not
  exist.
- **Model capability requirements** — `Config.PermissionReviewModel` is
  required whenever `PermissionReviewEnabled` is true (rejected at
  construction with `*PermissionReviewConfigError` otherwise). CodeRig never
  reuses an operator Loop's current model for this; the classifier needs an
  explicitly named model that supports tool use and structured output
  together (`commandsafety.New` enforces this and fails construction if the
  named model doesn't qualify).
- **Evidence boundaries** — `permission_review_evidence.go` builds CodeRig's
  own `gate.EvidenceAccessEvaluator`/`gate.EvidenceContainmentVerifier`,
  bound to the session's selected `AccessProfile` as the one trusted
  security ceiling (`evidenceCeilingFor`), and passes
  `commandsafety.RequiredEvidenceKinds()` — never a hand-copied list — as the
  allowlist. All three install together via `rig.WithPermissionReviewEvidence`.
- **Human fallback** — every non-`allowed` classifier outcome (including any
  construction or capability error) leaves CodeRig's ordinary interactive or
  headless permission gate exactly as open as it is with review disabled.
  Permission review can only ever narrow to one auto-approval; it cannot
  deny, persist a rule, or widen the selected access profile.
- **Audit/privacy** — CodeRig adds no permission-review-specific logging or
  audit path of its own; the durable trail is entirely harness's internal,
  secret-free `PermissionReviewStarted`/`PermissionReviewCompleted` events.
  Never log secrets or raw command/evidence content in CodeRig's own code
  paths that touch this feature, matching the existing "Security" rules
  above.
- **Policy tuning** — `Config.PermissionReviewStrictPolicy` selects between
  two local decision policies (`permissionReviewPolicyFor`): the
  Codex-compatible default (`gate.DefaultPermissionReviewPolicy`) or a
  strictly tighter alternative that can only ever lower `MaximumAutoRisk`
  and raise per-risk minimum authorization floors, never loosen either.
  Ignored when `PermissionReviewEnabled` is false.
- **Restore behavior** — permission review's identity (classifier
  name/revision, policy revision, evidence catalog, security ceiling) is
  part of the rig's configuration fingerprint. Restoring a session that was
  opened with review disabled into a build where it is now enabled is a
  rejected drift (harness's `pkg/gate/README.md#restore-behavior`) unless
  the caller opts in via `SessionSelector.AllowConfigMismatch` on that
  specific resume (`internal/app/persistence.go`'s `buildRigWithRegistration`
  then passes Harness's blanket `rig.WithAllowConfigMismatch()`) — the same
  existing mechanism CodeRig already uses for every other rejected-drift
  case, not a new permission-review-specific path. Enabled→disabled and
  same-configuration restores are unaffected.

`make test-integration` (see "Commands" below) is the suite that actually
proves this feature works end to end against a real classifier call; run it
before any release touching permission review.

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
- Treat `~/.looprig/models.json` as a secret-bearing owner-only file. Never copy its inline API keys into a repository, `.env`, shell command, fingerprint, permission file, log, or ACP child environment.
- Fail closed when access, permission, identity, or durable policy state is uncertain.

## Code and tests

- Follow SOLID principles:
  - **Single Responsibility:** keep each package, type, and function focused on one cohesive reason to change.
  - **Open/Closed:** extend behavior through stable seams instead of repeatedly modifying unrelated callers.
  - **Liskov Substitution:** implementations must preserve the full behavioral contract of the interfaces they satisfy.
  - **Interface Segregation:** prefer small consumer-owned interfaces over broad capability bundles.
  - **Dependency Inversion:** product policy depends on abstractions owned at the consuming boundary; infrastructure details are injected by the composition root.
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

Do not use pinned local sibling-module versions in `go.mod` to coordinate development across Looprig repositories. Use the root `/Users/ipotter/code/looprig/go.work` workspace instead. Change a sibling version in `go.mod` only as explicit release/adoption work after the providing module has a published version.

The following development-only analysis tools are approved and declared through
the Go toolchain:

- `github.com/securego/gosec/v2/cmd/gosec`
- `golang.org/x/vuln/cmd/govulncheck`
- `honnef.co/go/tools/cmd/staticcheck`

Run `make secure` before every commit. It verifies formatting, runs `go vet`,
Staticcheck, Gosec, `go mod verify`, and Govulncheck.

Do not commit, push, or rename a remote repository unless the user explicitly asks.
