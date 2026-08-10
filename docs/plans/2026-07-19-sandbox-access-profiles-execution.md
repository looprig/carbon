# Sandbox Access Profiles Cross-Repository Implementation Plan

> **Execution workflow:** Use subagent-driven development. A fresh implementer
> owns each task. Formal spec-compliance and code-quality reviewers run only at
> phase boundaries, across the whole phase delta.

**Goal:** Replace the mode, posture, security-limit, and layered-permission
system with consumer-defined access profiles, one generic combined gate,
prepared capability requests, OS-enforced per-spawn grants, and CodeRig-owned
product profiles.

**Architecture:** `sandbox` stays standalone and owns immutable profiles,
executors, grants, enforcement guarantees, and the local egress proxy. Harness
owns dependency-free access evaluation and gate routing. Tools own preparation,
normalization, permission rules, and the workspace file. MCP and TUI consume the
new Harness contracts. CodeRig directly assembles the modules and owns product
policy. The confinement bridge is retired.

**Technology:** Go, macOS Seatbelt, Linux namespaces/Landlock/nftables,
authenticated HTTP forward and HTTPS CONNECT proxying, JSON permission files,
Git worktrees, race tests, integration tests, fuzz tests, Staticcheck, Gosec,
Govulncheck, and Astro for published documentation.

## Fixed execution decisions

- Use branch `feat/sandbox-access-profiles` in every affected repository.
- This is a greenfield hard cut. Do not add compatibility aliases, adapters,
  deprecated copies, dual codecs, migration readers, or renamed presets.
- Use isolated sibling worktrees. Do not develop in the existing dirty trees.
- Preserve all pre-existing changes, including the unrelated dirty Harness plan
  and the website `.DS_Store`.
- Use no new third-party dependency. Stop and ask before adding one.
- Code tasks are test-first. Each implementer writes a failing focused test,
  verifies the failure, implements the minimum coherent change, runs focused and
  repository tests, self-reviews the diff, and commits.
- Do not run formal reviewer subagents after individual tasks. At each phase
  boundary, run one spec-compliance review, fix and re-review until clean, then
  run one code-quality review, fix and re-review until clean.
- Do not advance a phase with an open finding or failing required check.
- Commits stay local. Do not push, merge, archive a remote repository, or open a
  pull request without a later explicit user request.

## Repository set and bases

| Worktree name | Source repository | Base | Role |
|---|---|---|---|
| `sandbox` | `/Users/ipotter/code/looprig/sandbox` | `main` | Profiles and OS enforcement |
| `harness` | `/Users/ipotter/code/looprig/harness` | `main` | Prepared request and gate runtime |
| `tools` | `/Users/ipotter/code/looprig/tools` | `main` | Tool preparation and workspace rules |
| `mcp` | `/Users/ipotter/code/looprig/mcp` | `main` | External-tool preparation consumer |
| `tui` | `/Users/ipotter/code/looprig/tui` | `main` | Combined approval presentation |
| `coderig` | `/Users/ipotter/code/looprig/coderig` | `main` | Product policy and direct assembly |
| `confinement` | `/Users/ipotter/code/looprig/confinement` | `main` | Retired bridge |
| `tests` | `/Users/ipotter/code/looprig/tests` | `main` | Cross-module and platform integration coverage |
| `profile` | `/Users/ipotter/code/looprig/www/looprig` | current pinned submodule commit | Public `.github` profile docs |
| `www` | `/Users/ipotter/code/looprig/www` | `build/landing-site` | Website and profile submodule pointer |

The live code dependency sweep found no required behavior change in `core`,
`eval`, `flow`, `foreignloop`, `inference`, `llm`, `storage`, or `fsstore`.
Those unchanged sibling modules remain source links. The standalone `tests`
repository is an affected consumer because it owns cross-module and real
platform integration coverage. Vendored copies do not make other repositories
implementation consumers.

## Subagent and commit protocol

For every task, the controller gives a fresh implementer only that task's plan
section, repository worktree paths, relevant contributor instructions, and the
last accepted upstream commit IDs. The implementer must not edit another task's
files unless compilation requires it and the controller agrees.

Every implementation report must include:

1. the failing test and the observed failure;
2. files changed and obsolete files deleted;
3. focused, full, integration, fuzz, and security commands actually run;
4. remaining platform checks that require CI;
5. the local commit hash; and
6. a short self-review of authority changes and fail-closed behavior.

Implementers may commit only after the repository's required security check.
For repositories with `make secure`, run it immediately before every commit.
For `sandbox`, run `go vet ./...`, `go mod verify`, race tests, and
`git diff --check` immediately before every commit.

At a phase boundary:

1. Freeze implementation work.
2. Give a fresh spec reviewer the authoritative access spec, this plan, and the
   complete multi-repository phase diff.
3. Assign findings to a fresh fix implementer, rerun affected checks, and ask
   the same reviewer to re-review.
4. After spec approval, give a fresh code-quality reviewer the complete phase
   diff and test evidence.
5. Fix and re-review until clean.
6. Record accepted commit IDs and only then begin the next phase.

The per-task implementer sequence intentionally differs from the default
subagent-development workflow: formal reviews are aggregated at phase
boundaries as requested. Task-level tests and self-review are still mandatory.

## Phase 0: Create the integration workspace and freeze the contract

### Task 0.1: Create matching feature worktrees

**Files:** No repository file changes.

1. Confirm every source tree's branch, status, and base commit. Record them in
   the execution log. Stop if the branch already exists or a target worktree is
   occupied; do not delete or reset anything.
2. Create `/Users/ipotter/code/looprig-worktrees/sandbox-access-profiles/`.
3. Add a worktree named after each repository using branch
   `feat/sandbox-access-profiles`. Base the `profile` worktree on the currently
   pinned submodule commit and the `www` worktree on `build/landing-site`; base
   every other worktree on `main`.
4. Add integration-root symlinks for the unchanged sibling modules `core`,
   `eval`, `inference`, `storage`, `fsstore`, and `llm`. These make existing
   `replace ../module` directives resolve without branching unchanged repos.
5. Verify every affected worktree reports the same branch name and a clean
   status before transferring approved planning changes.

Use explicit paths. The setup is conceptually:

```bash
integration_root=/Users/ipotter/code/looprig-worktrees/sandbox-access-profiles
feature_branch=feat/sandbox-access-profiles

git -C /Users/ipotter/code/looprig/sandbox worktree add \
  -b "$feature_branch" "$integration_root/sandbox" main
git -C /Users/ipotter/code/looprig/harness worktree add \
  -b "$feature_branch" "$integration_root/harness" main
git -C /Users/ipotter/code/looprig/tools worktree add \
  -b "$feature_branch" "$integration_root/tools" main
git -C /Users/ipotter/code/looprig/mcp worktree add \
  -b "$feature_branch" "$integration_root/mcp" main
git -C /Users/ipotter/code/looprig/tui worktree add \
  -b "$feature_branch" "$integration_root/tui" main
git -C /Users/ipotter/code/looprig/coderig worktree add \
  -b "$feature_branch" "$integration_root/coderig" main
git -C /Users/ipotter/code/looprig/confinement worktree add \
  -b "$feature_branch" "$integration_root/confinement" main
git -C /Users/ipotter/code/looprig/www/looprig worktree add \
  -b "$feature_branch" "$integration_root/profile" HEAD
git -C /Users/ipotter/code/looprig/www worktree add \
  -b "$feature_branch" "$integration_root/www" build/landing-site
```

Create the unchanged-module symlinks individually after verifying each target
does not exist. Never use a recursive delete to repair this workspace.

### Task 0.2: Transfer and reconcile approved specifications

**Files:**

- Transfer to `coderig/docs/specs/access-profiles.md`
- Transfer to `coderig/docs/specs/coderig-assembly.md`
- Transfer to `coderig/docs/plans/2026-07-19-consumer-defined-sandbox-access-design.md`
- Transfer this execution plan
- Preserve for Phase 1: `sandbox/README.md`, `sandbox/SPEC.md`

Use `apply_patch` in the clean worktrees to reproduce the approved dirty spec
changes. Do not alter the original source trees. Keep the Sandbox README/SPEC
edits uncommitted until the implementation makes their current-behavior claims
true.

The contract review must explicitly pin:

- access bindings for sandbox kinds plus CodeRig-owned `tool.invoke` and
  `context.load` kinds;
- the post-approval structural grant issuer and typed v1 enforcement classes;
- grant binding to execution, command, canonical cwd, executor, profile, route,
  guarantees, class, target, and expiry;
- a distinct read-boundary guarantee and no production null fallback;
- no legacy secret globs, workspace carveouts, or implicit mode policy;
- required executor scratch root, positive limit, ownership, and cleanup;
- `ReadOnly` as CodeRig's default and explicit Unconfined acknowledgement;
- the exact v1 family catalog: `git log`, `git status`, `git diff`, `git show`,
  and `git push`;
- macOS target proxy support in v1 and honest Linux failure/broad fallback;
- Linux proxy bridging, SOCKS/SSH, transparent TCP, and TLS termination as v2;
- `mcp`, public profile docs, and `www` in the repository impact; and
- hard-cut sequencing with no compatibility layer.

### Task 0.3: Establish clean baselines

Run in dependency order from the integration worktrees:

```bash
cd "$integration_root/sandbox" && GOWORK=off go test -race ./...
cd "$integration_root/harness" && GOWORK=off go test -race ./...
cd "$integration_root/tools" && GOWORK=off go test -race ./...
cd "$integration_root/mcp" && GOWORK=off go test -race ./...
cd "$integration_root/tui" && GOWORK=off go test -race ./...
cd "$integration_root/coderig" && GOWORK=off go test -race ./...
cd "$integration_root/confinement" && GOWORK=off go test -race ./...
cd "$integration_root/www" && npm run build
```

If a baseline fails, record the exact pre-existing failure and stop before
implementation. Do not fold unrelated repairs into this feature.

### Phase 0 boundary

Run the aggregated spec review, then the documentation/code-quality review.
After approval, commit only the reconciled CodeRig planning/spec files with:

```text
docs: freeze sandbox access profile hard-cut contract
```

Do not commit the transferred Sandbox README/SPEC until Phase 1.

## Phase 1: Replace the Sandbox contract and enforcement

### Task 1.1: Replace modes and public policy with immutable profiles

**Files:**

- Rewrite `sandbox/policy.go`
- Add `sandbox/profile.go`
- Add `sandbox/profile_test.go`
- Add or rewrite internal effective-policy compiler files and tests
- Delete `sandbox/policyfor.go`, `sandbox/presets.go`, `sandbox/foreign.go`
- Delete their mode/preset/foreign tests

Write failing tests for fixed access numeric values, zero-value rejection,
canonical roots, each `AccessFor` kind/scope, `Restrict`, immutable copies,
unconfined consistency, fingerprint determinism, read-boundary requirements,
and unknown-value failure. Remove `Mode`, `PolicyFor`, policy options, preset
names, `ExternalDecl`, and old public mutable policy types rather than adapting
them.

Focused verification:

```bash
GOWORK=off go test -race . -run 'Test(NewProfile|AccessFor|Restrict|ProfileFingerprint)'
```

### Task 1.2: Add executor ownership and real post-approval grants

**Files:**

- Rewrite `sandbox/executor.go`, `sandbox/executor_test.go`
- Rewrite `sandbox/grant.go`, `sandbox/grant_test.go`
- Add `sandbox/executor_set.go`, `sandbox/executor_set_test.go`
- Add isolated-HOME and cleanup integration tests

Implement per-key executor memoization, independent grant keys, owner-only
isolated HOME directories, required scratch/limit options, idempotent close, and
partial-construction cleanup. Replace `PlanGrants`/`DescribeGrant` and opaque
`Delta:"net"` with post-decision `IssueGrant`. Make the executor compile and
apply each verified structured delta to the per-spawn policy. Remove dynamic
mode execution, external execution, `ReadOnlyView`, and any wrapper that cannot
own per-spawn proxy credentials and cleanup.

Implement the complete v1 grant-class set:
`filesystem.path.read.v1`, `filesystem.tree.read.v1`,
`filesystem.host.read.v1`, `filesystem.path.write.v1`,
`filesystem.tree.write.v1`, `filesystem.host.write.v1`,
`network.proxy-target.v1`, `network.broad.v1`, and `command.start.v1`.
The command class binds the exact normalized command and authorizes one spawn.

Tests must cover replay, cross-key use, cross-command use, cwd drift, profile and
route drift, expiry, unsupported class/target combinations, grant revocation,
and proof that verified deltas actually change the child policy.

### Task 1.3: Implement the authenticated egress proxy and route model

**Files:**

- Add `sandbox/egress_route.go`, `sandbox/egress_route_test.go`
- Add `sandbox/proxy.go`, `sandbox/proxy_test.go`
- Add focused direct/upstream proxy integration tests

Implement a loopback-only random listener, per-execution authentication, HTTP
absolute-form forwarding, HTTPS CONNECT, normalized host/port matching, direct
DNS address checks and rebinding defense, metadata/private/link-local denial,
explicit direct routing, fail-closed HTTP/HTTPS upstream chaining, route
selection, credential redaction, cancellation, bounded idle handling, and typed
denial recording. Never inherit `NO_PROXY` as a child bypass and never fall back
to direct egress after an upstream failure.

Focused verification:

```bash
GOWORK=off go test -race . \
  -run 'Test(Egress|Proxy|CONNECT|NetworkTarget|Upstream|Route|Credential|Rebinding|NOProxy)' \
  -count=20
```

### Task 1.4: Adapt platform backends and conformance tests

**Files:**

- Rewrite `sandbox/backend.go`, `sandbox/backend_seatbelt.go`
- Rewrite `sandbox/backend_linux.go`, `sandbox/net_linux.go`,
  `sandbox/landlock_linux.go`, `sandbox/namespace_linux.go`, and stage-2 policy
  transport as required
- Replace production behavior in `sandbox/backend_null.go`
- Adapt acceptance/conformance tests and `sandbox/sandboxtest/`
- Replace mode-named SBPL fixtures under `sandbox/testdata/`
- Reconcile `sandbox/README.md`, `sandbox/SPEC.md`, `sandbox/doc.go`

On macOS, remove broad file-read and process-exec preambles, allow only tested
runtime necessities and configured roots, deny direct remote egress, and allow
the exact local proxy port. Report target-network and read-boundary guarantees
only when achieved.

On Linux, compile explicit profile roots and preserve the valuable namespace,
Landlock, seccomp, cgroup, nftables, and init mechanisms. Do not report
target-network in v1: rung 1 lacks a parent-proxy bridge and rung 2 is port-only.
Reject target grants or use a visibly broad exact-command grant only when the
backend can enforce its stated class. Other platforms reject `Sandboxed`; direct
execution is only explicit `Unconfined`.

Verification:

```bash
GOWORK=off go test -race ./...
GOWORK=off go test -race . \
  -run 'Test(AcceptanceMatrixDarwin|Seatbelt.*(Read|Proxy|Network|Grant))'
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off \
  go test -c -o /tmp/coderig-sandbox-linux.test .
GOWORK=off go vet ./...
GOWORK=off go mod verify
git diff --check
```

Linux CI must separately run the race suite with rung-1 user/network namespaces
enabled and with a rung-2 Landlock-v4 environment. Record those as required
pre-merge checks if they are unavailable locally.

### Task 1.5: Add Sandbox integration coverage in the tests module

**Files:**

- Add focused integration-tag tests under `tests/`
- Modify `tests/go.mod` and `tests/go.sum` only as required to consume the local
  Sandbox worktree
- Update `tests/Makefile` only if a distinct platform-integration target is
  required

Keep module-owned unit and conformance tests in `sandbox`. Put real
cross-module/process/platform integration scenarios in `tests`: construct
profiles through the public `ExecutorSet` API, verify owned HOME/TMPDIR cleanup,
exercise exact Path versus recursive Tree grants and post-approval symlink-swap
rejection, prove combined target-network and broad-DNS behavior where the host
can enforce it, and assert unsupported platform/profile combinations fail
closed. Do not duplicate private backend compiler tests.

On macOS, run the native Seatbelt integration cases. On Linux CI, run the same
integration suite once with rung-1 user/network namespaces enabled and once in
a Landlock-v4 rung-2 environment. Tests must skip only when their documented OS
or kernel prerequisite is absent; they must not silently turn an enforcement
failure into a skip.

Verification:

```bash
cd "$integration_root/tests"
GOWORK=off go test -tags integration -race ./...
GOWORK=off go vet -tags integration ./...
make check
```

Cross-compile any platform-specific test packages locally. Record the two Linux
runtime variants as required pre-merge CI checks when executing on macOS.

### Phase 1 boundary

Review the complete Sandbox and tests-module phase delta against the access spec
and the achieved platform guarantees, then run code-quality review. Require
README/SPEC examples and integration tests to match the hard-cut API. Commit
review fixes only after both repositories' full check sets pass.

## Phase 2: Replace Harness permission and security contracts

### Task 2.1: Add typed preparation and multi-source access evaluation

**Files:**

- Add `harness/pkg/tool/preparation.go` and tests
- Add `harness/pkg/gate/access.go`, `evaluator.go`, and tests
- Modify `harness/pkg/gate/gate.go`, `payload.go`, `prompt.go`, `response.go`
- Add request/action decoder fuzz targets `FuzzDecodeRequest` and
  `FuzzDecodeApprovalAction`

Define validated `Requirement`, `RuleCandidate`, `Request`, `RuleMatcher`,
`RuleWriter`, `GrantIssuer`, `AccessBinding`, and `ApprovalAction` contracts.
Requirements carry all normalized matching fields plus optional paired
`grant_class`/`grant_target` fields; empty means the direct tool enforces its
resource, while a populated pair requests a post-decision executor grant.
Every `command.execute` requirement carries `command.start.v1` and the exact
normalized command target. A saved exact, wildcard, or family rule may satisfy
the decision, but grant issuance remains exact-command and single-spawn.
Route each kind to exactly one access source. Evaluate all requirements, apply
deny-before-allow precedence, and return one combined set of unmet capabilities.
Expose exactly `Approve`, `Approve always for this workspace`, and `Deny`.

`Approve always` must atomically persist every displayed reusable candidate
before grants are minted; persistence failure blocks execution. Once approval
writes nothing. Gate must not import Sandbox, parse tool arguments, or define a
permission-file format.

### Task 2.2: Execute one prepared artifact through one combined gate

**Files:**

- Modify `harness/pkg/tool/tool.go`
- Modify `harness/pkg/loop/tool_context.go`, `deps.go`, `definition.go`
- Modify `harness/internal/loopruntime/contracts.go`, `runner.go`, `toolset.go`,
  `gate.go`
- Adapt focused runtime and definition tests

The runner mints the execution ID, prepares once, evaluates once, opens at most
one gate, resolves the action, issues fresh grants, and executes the prepared
artifact. Effectful tools without preparation fail closed. Pure tools may return
an empty request. Carry issued tokens in the prepared execution contract rather
than the old ambient grant context.

### Task 2.3: Replace durable approval wires and remove security limits

**Files:**

- Modify `harness/internal/sessionruntime/gates.go`, session construction,
  restore, and journal handling
- Modify `harness/pkg/command/approve.go`, `deny.go`, `marshal.go`
- Modify `harness/pkg/event/gate.go`, `tool.go`, `marshal.go`
- Modify `harness/pkg/session/session.go`, `harness/pkg/rig/options.go`,
  lifecycle/errors, and `harness/pkg/loop/bound_overrides.go`
- Delete `harness/pkg/tool/permission_request.go` and its JSON/tests
- Delete `harness/pkg/tool/grants.go` and tests
- Delete `harness/pkg/command/security_limit.go` and tests
- Delete `harness/pkg/event/security_limit.go` and tests
- Delete `harness/pkg/security/`
- Delete `harness/internal/sessionruntime/security_limit.go` and tests

Remove `PermissionPrompter`, concrete sealed request types, `ApprovalScope`,
session scope, `ToolPolicy`, `EffectChecker`, old gate check/grant methods,
ambient grants, `SecurityLimit`, `SetSecurityLimit`,
`SecurityLimitChanged`, factories, projections, legacy wire tags, and effect
ordinals. Audit stores requirement and candidate descriptions, never tokens.

### Task 2.4: Document and verify the final Harness API

**Files:**

- Add `harness/pkg/gate/README.md`
- Update package docs and `harness/CLAUDE.md` ownership statements
- Refresh `harness/vendor/`

Verification:

```bash
GOWORK=off go test -race ./pkg/gate ./pkg/tool
GOWORK=off go test -race ./pkg/loop ./pkg/command ./pkg/event
GOWORK=off go test -race ./internal/loopruntime ./internal/sessionruntime ./pkg/rig
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off go test ./pkg/gate -run='^$' -fuzz=FuzzDecodeRequest -fuzztime=30s
GOWORK=off go test ./pkg/gate -run='^$' \
  -fuzz=FuzzDecodeApprovalAction -fuzztime=30s
GOWORK=off make vendor
GOWORK=off make secure
```

### Phase 2 boundary

Review the complete Harness delta, including deletion of old durable types and
the absence of Sandbox imports. Then run code-quality review over preparation,
gate concurrency, restore behavior, and audit secrecy.

## Phase 3: Replace Tools permission logic and prepare every effect

### Task 3.1: Replace PermissionChecker with one hardened workspace store

**Files:**

- Rewrite `tools/permission/store.go`
- Add `tools/permission/rule.go`, `rule_json.go`, `diagnostic.go`, and tests
- Delete or replace `check.go`, `grant.go`, `grant_remint.go`,
  `noninteractive.go`, `intent.go`, `posture.go`, `policyrev.go`, and obsolete
  tests

Implement schema version 2 with one capability and explicit enforcement class
per rule. Accept one explicit path. Add an interprocess lock; re-read/merge under
lock; validate owner, exact mode, regular type, link count, size, and symlink
status; write an owner-only temporary file; fsync file and directory; rename
atomically; and leave the old file intact on error. A failed workspace write
must block the approved call.

Remove hard approve, separate hard deny, posture, security-level selection,
unattended wrappers, user-global files, session rules, prefixes, grant deltas,
and hidden HOME discovery.

### Task 3.2: Implement token-aware Bash rule families

**Files:**

- Replace `tools/permission/match.go`
- Add `tools/permission/bashrule.go` and fuzz/table tests

Parse exact, `Bash(*)`, and `Bash(prefix:*)` rules into canonical tokens.
Segment at `&&`, `||`, `;`, `|`, `|&`, `&`, newline, and subshell boundaries.
Match every segment independently. Fall back to exact candidates for redirects,
expansion, substitutions, ambiguous quoting, unsupported syntax, unknown
prefixes, shells, interpreters, `find`, `xargs`, `env`, and package/task
runners. Never use normalized `strings.HasPrefix`.

Allow automatic families only through the injected CodeRig catalog. Load a
valid manual out-of-catalog allow family with a diagnostic; deny families need
no warning. Reject every wildcard/family record that carries a filesystem or
network delta.

### Task 3.3: Prepare file, context, and utility tools

**Files:**

- Modify `tools/internal/filemutation/writefile.go`, `editfile.go`
- Modify `tools/readfile/readfile.go`, `glob/glob.go`, `grep/grep.go`
- Modify `tools/skill/skill.go`
- Modify public definition wrappers and their tests

Validate and canonicalize once during preparation. Emit filesystem read/write
requirements and a canonical write scheduling key. Execute the typed prepared
artifact without reparsing raw JSON. Preserve Skill snapshot/TOCTOU behavior;
emit `context.load` and any applicable filesystem requirement.

### Task 3.4: Prepare Bash, Fetch, and WebSearch network requirements

**Files:**

- Modify `tools/bash/bash.go` and tests/schema
- Modify `tools/fetch/fetch.go` and tests
- Modify `tools/websearch/websearch.go`, `duckduckgo.go`, and tests

Bash always emits `command.execute` with `command.start.v1` and the exact
normalized command target, and emits only explicitly declared
filesystem/network deltas. It executes through the bound runner with issued
grants, marking every command-backed requirement with its exact grant class and
target; omitted gated access remains OS-blocked. Command `Allow` needs no token,
command `Deny` never mints one, and a gated command plus its deltas produces one
combined prompt rather than a second command-start prompt.

Fetch normalizes method, scheme, host, port, path, and redirects once and emits
the shared network requirement. WebSearch widens `SearchProvider` so each
provider exposes its endpoint requirements; DuckDuckGo declares
`https://html.duckduckgo.com:443`. Redirects and secondary targets fail closed.
Do not retain a separate Fetch permission layer.

### Task 3.5: Verify and document Tools

**Files:** Update `tools/README.md`, package docs, and `tools/CLAUDE.md` ownership
statements.

```bash
GOWORK=off go test -race ./permission ./bash ./fetch ./websearch
GOWORK=off go test -race ./readfile ./glob ./grep ./writefile ./editfile ./skill
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off go test ./permission -run='^$' -fuzz=FuzzBashRule -fuzztime=30s
GOWORK=off go test ./permission -run='^$' -fuzz=FuzzPermissionFile -fuzztime=30s
GOWORK=off make secure
```

### Phase 3 boundary

Review the complete Tools delta against preparation ownership, family matching,
permission hardening, and shared-network semantics. Then perform code-quality
review with special attention to parser fuzzing, filesystem races, rollback,
and redirects.

## Phase 4: Migrate independent Harness consumers

After Phases 2 and 3 are accepted, MCP and TUI tasks may run in parallel because
they edit separate repositories against stable contracts. Give each a fresh
implementation subagent. Do not start the phase reviews until both finish.

### Task 4.1: Migrate MCP external tools

**Files:**

- Modify `mcp/pkg/harness/tools.go`, `deps.go`, `tools_test.go`
- Replace fixtures using `ApprovalScope`, `PermissionPrompter`, or
  `NewExternalRequest`
- Refresh `mcp/vendor/`

Replace `adaptedTool.BuildRequest` with preparation that emits a stable,
redacted `tool.invoke` requirement scoped to the MCP binding/tool identity.
Remove session-scope policy. Keep SDK types behind the existing package boundary.

```bash
GOWORK=off go test -race ./pkg/harness
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off make vendor
GOWORK=off make secure
```

### Task 4.2: Replace TUI approval and access controls

**Files:**

- Modify `tui/api.go`, `tui/sessionadapter/adapter.go`
- Modify `tui/internal/presentation/agent.go`, `prompt.go`, `interaction.go`,
  `action.go`, `commands.go`, `sessioncore.go`, `screen.go`,
  `runtimecontrol.go`, `runtimeprojection.go`
- Adapt all focused presentation/sessionadapter/runtime tests
- Update `tui/README.md` and refresh `tui/vendor/`

Render one prompt containing every unmet capability and every exact persisted
candidate. Show exactly the three actions. Remove session approval, `/access`,
access trays, access metadata queries, `SecurityLimitChanged` folding,
`AccessID`, `AccessOption`, `AccessOptions`, and `SetAccess`.

Add synchronous session presentation metadata for workspace, fixed profile, and
permission diagnostics. Capture it at screen construction and session handoff
so diagnostics appear before a gate. Keep the first banner exactly as the
existing CodeRig identity/session banner; display the fixed profile in session
metadata/footer, not as a mutable control.

```bash
GOWORK=off go test -race ./internal/presentation ./sessionadapter ./runtime
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off make vendor
GOWORK=off make secure
```

### Phase 4 boundary

Run one spec review across both MCP and TUI deltas, then one code-quality review.
Verify the TUI never reconstructs rules and MCP never maps external invocation
to `command.execute`.

## Phase 5: Assemble CodeRig directly and retire Confinement

### Task 5.1: Add CodeRig product access, family, permission, and route policy

**Files:**

- Add `coderig/internal/app/access.go`, `access_test.go`
- Add `coderig/internal/app/egress.go`, `egress_test.go`
- Add `coderig/internal/app/permissions.go`, `permissions_test.go`
- Modify `coderig/internal/app/config.go`, `persistence.go`, fingerprint tests
- Modify `coderig/cmd/coderig/main.go`, `main_test.go`

Define the three exact profiles, `ReadOnly` default, explicit unconfined
acknowledgement, reviewer ceiling, product `tool.invoke`/`context.load` source,
and exact family catalog. Compute the default permissions path as
`~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json` only in
interactive CodeRig assembly. Headless accepts an explicit read-only path and
never searches HOME.

Capture HTTP/HTTPS proxy configuration in the parent, retain credentials only
in the route object, translate `NO_PROXY` only through explicit validated direct
routes, redact secrets, and fingerprint non-secret route identity/guarantees.

Replace `--security-mode` with `--access-profile
readonly|trusted|unconfined`. Remove `DefaultSecurityMode`, `ParseSecurityMode`,
and mutable security-limit configuration.

### Task 5.2: Replace Confinement with direct role assembly

**Files:**

- Rewrite `coderig/internal/app/toolsets.go`, `swarm.go`,
  `runtime_controls.go`, `session_browser.go`
- Add/adapt acceptance, managed delegation, new/restore, failure cleanup, and
  lifecycle tests
- Modify `coderig/go.mod`, `go.sum`, dependency tests, and `CLAUDE.md`

Construct one `sandbox.ExecutorSet` per role authority, key executors by Loop ID,
and pass the same effective profile pointer to the role's four sandbox access
bindings and executor set. Bind the executor structurally as the grant issuer.
Pass the CodeRig product source for `tool.invoke` and `context.load`. The
reviewer always uses `sandbox.Restrict(selected, reviewerCeiling)`.

Collapse new, restore, headless, and interactive construction onto one `Open`
path. Own all executor-set closers in the runtime agent; close partial assembly
on failure and close everything exactly once at shutdown. Carry synchronous
profile/workspace/diagnostic presentation metadata to TUI.

Fingerprint the access ABI, selected name, normalized selected/effective
profiles, reviewer ceiling, family policy revision, and sanitized route
identity/guarantees. Prove credentials never appear and every authority drift
rejects restore.

### Task 5.3: Retire the Confinement repository

**Files:**

- Delete `confinement/factory.go`, `posture.go`, tests, module spec, `go.mod`,
  `go.sum`, and `Makefile`
- Replace `confinement/README.md` with a retirement notice pointing to direct
  Sandbox profile/executor assembly
- Remove obsolete contributor instructions or replace them with the retirement
  boundary

Do not move translation, posture mapping, dynamic clamping, or gated
unsandboxed fallback into CodeRig under another name. Retiring repository
contents is in scope; changing the remote repository's archive setting is not.

### Task 5.4: Verify product behavior

```bash
cd "$integration_root/coderig"
GOWORK=off go test -race ./internal/app ./cmd/coderig
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
GOWORK=off make build
GOWORK=off make secure
```

Run CodeRig acceptance tests for all three profiles, reviewer restriction,
isolated HOME, `/dev/null`, root/home denial, exact and family approvals,
Git-over-HTTPS target reuse, Git-over-SSH broad fallback, organization proxy
failure, headless files, new/restore parity, banner/session ID preservation, and
clean executor shutdown.

### Phase 5 boundary

Run spec review across CodeRig and the retired Confinement delta, then
code-quality review. The review must reject local bridge wrappers that merely
rename Sandbox/Harness APIs and any lingering mutable access control.

## Phase 6: Publish final contracts and run the cross-repository gate

### Task 6.1: Update public profile documentation

**Files in `profile`:**

- `README.md`
- `docs/consumers/tools.md`
- `docs/consumers/larger-systems.md`
- `docs/consumers/rig.md`
- `docs/consumers/session.md`
- `docs/consumers/packages.md`
- related contributor/package documentation found by the final audit

Replace live examples of Confinement, sandbox modes, dynamic executors,
security-limit factories/commands/events, and old approval scopes with the final
public profile, gate, preparation, and direct-assembly APIs. Historical journal
entries may remain clearly historical.

Commit the profile docs, update the `www` feature worktree's `looprig` submodule
pointer to that commit, and run:

```bash
cd "$integration_root/www"
npm run build
```

Do not push either repository during this plan.

### Task 6.2: Refresh dependency metadata and vendor snapshots

In dependency order, run `go mod tidy` only where imports changed, inspect every
`go.mod`/`go.sum` delta, and refresh vendor trees only in Harness, MCP, and TUI.
No module may retain a live Confinement requirement. Do not edit vendored code by
hand.

### Task 6.3: Run the final live-reference audit

Search all live, non-vendored, non-historical source and public documentation:

```bash
rg -n --glob '!**/vendor/**' --glob '!**/docs/plans/**' \
  'github.com/looprig/confinement|confinement\.' "$integration_root"
rg -n --glob '!**/vendor/**' --glob '!**/docs/plans/**' \
  'SecurityLimit|SetSecurityLimit|SecurityLimitChanged|WithSecurityLimit' \
  "$integration_root"
rg -n --glob '!**/vendor/**' --glob '!**/docs/plans/**' \
  'sandbox\.(ZeroTrust|ReadOnly|Write|Trusted)|NewExecutorDynamic|PolicyFor' \
  "$integration_root"
rg -n --glob '!**/vendor/**' --glob '!**/docs/plans/**' \
  'ScopeSession|user-global|HardApprove|HardDeny|WithPosture' \
  "$integration_root"
```

Expected matches are limited to the Confinement retirement notice and explicit
"removed API" sections in final documentation. There must be no live code,
example, fixture, or current-behavior reference.

### Task 6.4: Run the final verification matrix

Run in dependency order:

```bash
cd "$integration_root/sandbox" && GOWORK=off go test -race ./...
cd "$integration_root/harness" && GOWORK=off go test -race ./... && GOWORK=off make secure
cd "$integration_root/tools" && GOWORK=off go test -race ./... && GOWORK=off make secure
cd "$integration_root/mcp" && GOWORK=off go test -race ./... && GOWORK=off make secure
cd "$integration_root/tui" && GOWORK=off go test -race ./... && GOWORK=off make secure
cd "$integration_root/coderig" && GOWORK=off make build && GOWORK=off make test && GOWORK=off make secure
cd "$integration_root/tests" && GOWORK=off make check
cd "$integration_root/www" && npm run build
```

Also run all affected integration-tag suites and the Phase 1 Linux checks. Run
the permission and gate fuzz targets for at least 30 seconds each. Save command,
exit status, and relevant output in the execution handoff.

### Phase 6 boundary

Run a final cross-repository spec-compliance review first and a final
cross-repository code-quality/security review second. Reviewers receive the full
feature-branch range from every repository, the reference-audit output, and the
verification matrix. Fix and re-review until both approve.

## Completion handoff

Completion requires:

- every affected repository on local branch `feat/sandbox-access-profiles`;
- all phase reviewers approved with no open findings;
- all required local checks passing and Linux CI status explicitly recorded;
- no uncommitted feature changes in the integration worktrees;
- all original source-tree changes still present and untouched;
- a table of repository, base commit, final commit, tests, and review result;
- no push, merge, PR, or remote archival performed without explicit approval.

At handoff, offer the user the next integration action. Do not assume whether
they want nine branches pushed, pull requests opened, or changes merged.
