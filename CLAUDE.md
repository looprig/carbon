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

Do not add an open-ended agent registry. The primer loop may expose a bounded picker over models.json entries tagged primer-capable (uses: ["primer", ...]); delegate roles remain fixed via delegate_defaults. The primer loop's declared inference transports span both the primer-capable roster and configured gateway-backed delegate models, since native delegate loops are ordinary Loop instances subject to the same transport-declaration and restore rules as the primer. Do not reintroduce a confinement bridge, a security-limit ordinal, or any in-session authority-mutation surface.

## Model catalogue and credentials

- All fixed `~/.looprig/coderig/...` paths in this file (`models.json`, `mcp.json`, `workspaces/<hash>/permissions.json`, the default session-store root) are relative to the resolved CodeRig home: `Config.HomeDir` when set (must be absolute, used exactly as given; validated once at construction, fail closed otherwise), else `~/.looprig/coderig`. One resolver (`internal/app/home.go`'s `looprigHome`) is the single place this is computed; there is no CLI flag or environment variable for it. This directory is CodeRig-specific — harness's sessionstore/workspacestore have no notion of "which product" is calling them, so a different looprig-platform agent product gets its own home, never this one (a prior product, `swe`, shared the bare `~/.looprig` directory before being retired; CodeRig does not repeat that).
- The `planner`, `builder`, and `reviewer` roster and its role policy remain fixed in code. Production model data is external configuration, loaded once at the composition boundary from `~/.looprig/coderig/models.json`. The file may also carry an optional top-level `permission_review` section that can enable classifier-based automatic permission review; see "Permission review" below for what it does and does not override.
- The model catalogue is operator-managed and read-only to CodeRig: the loader never creates, rewrites, or changes the mode of the file. On Unix, the file must be owner-only (`0600`), must be a regular file, and must not be a symlink.
- Inline API keys are permitted only in this machine-wide-per-product file because it is outside repositories and owner-only. Never put provider keys in `.env`, provider-key environment variables, command-line arguments, logs, fingerprints, permission files, or child environments.
- Native permission persistence is separate and remains per workspace at `~/.looprig/coderig/workspaces/<sha256(canonical-workspace)>/permissions.json`. The global model catalogue is not a permission store.
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
  today, but there are two seams: a caller embedding `coderig.Config` can set
  the field directly, and an operator can add a `permission_review` section
  to `~/.looprig/coderig/models.json` (`{"model": "<alias>", "strict": <bool>}`) —
  presence alone enables it, and `model` must name a configured alias with
  both `tools` and `structured_output_with_tools` capabilities or the
  catalogue fails closed with a typed `*ModelConfigError`. The programmatic
  seam always wins: if `PermissionReviewEnabled` is already `true` before the
  catalogue loads, the file can only leave it enabled, never disable or
  reconfigure it — a models.json section has no way to force it back off.
  When disabled, `newPermissionReviewRegistration` returns the zero
  (`enabled: false`) registration and `options()` returns `nil` — the
  assembled rig is byte-for-byte the same as if permission review did not
  exist.
- **Access-profile gate** — regardless of how `PermissionReviewEnabled`
  became true (the programmatic seam or models.json), permission review only
  ever takes effect when the session's `AccessProfile == AccessTrusted`. Any
  other profile (`readonly`, `unconfined`, or the empty default) leaves it
  **silently disabled** — the same byte-for-byte-identical-to-disabled rig as
  `PermissionReviewEnabled == false`, no error, no log. This is enforced once,
  in `newPermissionReviewRegistration`, so both the interactive and headless
  composition paths get it automatically.
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

## MCP servers

CodeRig can optionally compose MCP servers from an operator-managed
`<home>/mcp.json` (design doc
`docs/plans/2026-08-05-coderig-mcp-and-permission-review-design.md` Part 1).
Loading and validation live in `internal/app/mcpconfig.go`; assembly
(transports, bindings, the Manager, adoption, and lifecycle) lives in
`internal/app/mcp.go` and is composed inside `openRuntimeAgent`
(`internal/app/swarm.go`), so new, restore, interactive, and headless
sessions all get it the same way. CodeRig wires
`github.com/looprig/mcp`'s `pkg/harness` Manager/Bindings/Adopter and its
transport factories directly; it adds no policy-translation layer of its
own.

- **File and schema** — `<home>/mcp.json` uses the exact Claude Code
  `mcpServers` schema plus one looprig extension field, `roles`:
  `{"mcpServers": {"<binding>": {"type", "command", "args", "env", "url",
  "headers", "roles"}}}`. `type` is `stdio`, `http`, or `sse`; when omitted
  it is inferred from shape — `command` present infers `stdio`, `url`
  present infers `http` — but `sse` is never inferred (it shares the `url`
  shape with `http`), so a server that wants it must say `"type": "sse"`
  explicitly. Decoding is strict (unknown fields and duplicate keys are
  rejected, matching `models.json`'s own decode discipline), and binding
  names must satisfy `mcp/pkg/client.Name` validation — they become both the
  `mcp__<binding>__<tool>` tool prefix and the `mcp:<binding>:<tool>`
  permission identity.
- **Hygiene** — identical to `models.json`'s, because headers and env values
  may carry credentials the same way models.json's inline `api_key` fields
  do: ≤ 1 MiB, regular file, no symlink, owner-only `0600` on Unix, read
  once at the composition boundary, never created, rewritten, or
  mode-changed by CodeRig. An absent file disables the feature entirely —
  zero MCP assembly, a byte-for-byte identical rig to one with no `mcp.json`
  at all.
- **Roles extension** — `roles` is optional and drawn from `planner`,
  `builder`, `reviewer` (the fixed `internal/catalog` loop identities);
  empty or absent means all three. A binding's visibility selects by loop
  **name**, not loop ID, since bindings are built before the session mints
  loop IDs — a runtime-spawned delegate of a role shares its name and
  inherits its visibility. Unknown role names are a config error.
- **Fail-closed posture — two different failure modes, not one** — an
  invalid or insecure `mcp.json`, including a stdio `command` that does not
  resolve on `$PATH` (checked via `exec.LookPath` at construction), fails
  **session construction** itself with a typed, secret-free error — the same
  fail-closed treatment as a bad `models.json` alias, not a degraded server.
  Separately, a server whose config resolves and parses fine but whose
  connection or initialize handshake fails at `Start` is **optional-binding
  degradation**: the session still opens, that one server's tools are
  simply absent, and an integration status event explains why. Do not
  conflate the two — a config-shape problem never lets a session open
  quietly missing a server, but a live connectivity problem never blocks
  the session either.
- **Permissions** — every MCP tool call carries `tool.invoke` with identity
  `mcp:<binding>:<tool>` and routes through the same product access source
  and role gates every other tool does; `newProductAccessSource` already
  answers `AccessGated` for any non-empty `tool.invoke` scope
  (`internal/app/access.go`), so MCP tools get "ask" with no dedicated
  wiring. The command-safety permission classifier (see "Permission review"
  above) reviews shell commands only — MCP invocations are outside its
  evidence domain and are never auto-approved by it.
- **Applies to every access profile** — unlike permission review, MCP
  composition is not trusted-profile-gated: `openRuntimeAgent` builds the
  MCP assembly (`newMCPSessionAssembly`) unconditionally for every session,
  and neither it nor `internal/app/mcp.go` ever branches on
  `AccessProfile`/`AccessTrusted`. A configured `mcp.json` composes the same
  way under `readonly`, `unconfined`, and `trusted`.
- **Fingerprint and restore** — MCP identity folds into the rig's
  configuration fingerprint through harness's purpose-built seam:
  `mgr.ConfigDigest()` supplies `rig.ConfigFingerprintFields.ExternalCapabilityRev`
  (`internal/app/persistence.go`'s `agentFingerprintFields`) before
  `rig.NewSession`. The digest is secret-free by the mcp module's own
  contract — binding name, transport kind, redacted origin, capability/
  filter/limits/compat digests, and role-visibility digest, never a header
  or env **value** — so changing the server set, a URL, or role visibility
  is now correctly a **rejected drift** on restore by default, the same
  pattern as the "Restore behavior" bullet above: escaping it requires the
  caller's `SessionSelector.AllowConfigMismatch` on that specific resume,
  not a new permission-review- or MCP-specific path. (A harness-level bug
  found during this feature's implementation had this drift category
  wrongly classified as informational, silently re-adopting a changed
  server set on restore; harness fixed it to classify an opaque,
  direction-unknowable digest change as fail-secure `Warn`, matching every
  other rejected-drift category here.) Stable header/env values never move
  the digest. An absent `mcp.json` contributes the seam's empty
  no-external-capability value, so today's sessions restore completely
  unaffected.
- **Security** — treat `mcp.json` exactly like `models.json`: headers and
  env values may carry credentials and must never be logged, placed in an
  error message, or allowed to reach the fingerprint. The file is
  operator-managed and read-only to CodeRig — same posture, same file
  hygiene, same secret-bearing-file treatment as `~/.looprig/coderig/models.json`
  (see "Security" below).
- **Known gap** — the exported library construction path (`New()`/
  `newWithClient`) does not compose MCP; only `SessionStoreFactory.Open` →
  `openRuntimeAgent` does, and `cmd/coderig` exclusively uses that path.
  This is a real, pre-existing gap flagged in code comments
  (`runtime_controls.go`, `swarm.go`), not something this feature closes.

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
- Treat `~/.looprig/coderig/models.json` as a secret-bearing owner-only file. Never copy its inline API keys into a repository, `.env`, shell command, fingerprint, permission file, log, or ACP child environment.
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
