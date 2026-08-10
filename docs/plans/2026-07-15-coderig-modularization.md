# CodeRig Modularization Implementation Plan

> Execute this plan without commits or pushes until the user explicitly requests them.

**Goal:** Rename SWE to CodeRig, move reusable session, tool, and confinement machinery into their proper modules, and reduce CodeRig to explicit Loop definitions plus one Rig assembly path.

**Architecture:** Harness remains the core runtime and owns only core delegation and workspace contracts. The optional tools module supplies one definition per standard tool. Confinement bridges those definitions to sandbox enforcement. CLI owns the session-to-TUI adapter. CodeRig imports these modules and owns only coding prompts, Loop rosters, modes, model defaults, application flags, and Rig assembly.

**Technology:** Go 1.26, looprig harness, inference, storage backends, Bubble Tea CLI, looprig sandbox.

---

## Task 1: Relax repository rules that caused ceremony

**Files:**

- Modify: `swe/CLAUDE.md`
- Modify: `cli/CLAUDE.md`
- Modify: `harness/CLAUDE.md`

1. Replace sentence-grammar single-responsibility rules with cohesion guidance.
2. Replace mandatory interface-first rules with consumer-owned boundary guidance.
3. Require typed errors only when callers need stable classification or recovery.
4. Use table-driven tests only when cases share structure.
5. Remove the arbitrary function line threshold.
6. Preserve security boundaries, race testing, formatting, and validation rules.
7. Verify the documents no longer contain the removed absolute language with `rg`.

## Task 2: Make harness independent of optional tools

**Files:**

- Create: `harness/internal/workspaceobservations/observations.go`
- Create: `harness/internal/delegationtool/*.go`
- Modify: `harness/internal/sessionruntime/session.go`
- Modify: `harness/internal/sessionruntime/delegation.go`
- Modify: affected harness tests

1. Add failing dependency tests that forbid production imports of `github.com/looprig/harness/pkg/tools` and `github.com/looprig/tools` from harness runtime packages.
2. Move the workspace observation implementation behind `tool.WorkspaceObservations` into harness internal code.
3. Move the Subagent implementation needed by automatic delegation injection into harness internal code.
4. Replace sessionruntime imports of the public tools package.
5. Run focused sessionruntime and dependency tests with `-race`.

## Task 3: Create the standalone tools module

**Files:**

- Create: `tools/go.mod`
- Create: `tools/CLAUDE.md`
- Create: `tools/Makefile`
- Move: `harness/pkg/tools/*` to `tools/`
- Modify: tool implementation imports and tests

1. Move standard tool implementations and their tests into module `github.com/looprig/tools`.
2. Exclude the internalized Subagent and workspace observation implementations.
3. Keep permission checking, posture, approval persistence, matching, and policy fingerprinting with the standard tools.
4. Add a module dependency test that permits harness contracts but forbids sandbox and confinement imports.
5. Run `go test -race ./...` in the tools module.

## Task 4: Replace `Files` with individual definitions

**Files:**

- Modify: `tools/definitions.go`
- Modify: `tools/readfile.go`
- Modify: `tools/writefile.go`
- Modify: `tools/editfile.go`
- Modify: related tests

1. Add failing tests proving `ReadFile`, `WriteFile`, and `EditFile` definitions each produce exactly one tool.
2. Export only interface-based observation dependencies, or read observations directly from `tool.Bindings.Workspace`.
3. Implement independent definition constructors.
4. Delete `Files` and its bundle tests.
5. Verify a read-only Loop binds only ReadFile and never constructs mutators.
6. Run focused file tool tests and the full module race suite.

## Task 5: Add standard-tool consumer documentation

**Files:**

- Create: `looprig/docs/consumers/tools.md`
- Modify: `looprig/docs/consumers/README.md`
- Modify: `looprig/docs/consumers/packages.md`
- Modify: relevant quickstart examples

1. Show how to select individual standard tools.
2. Show how to implement `tool.InvokableTool` and expose it through `tool.NewDefinition`.
3. Explain binding requirements, permissions, capabilities, audit summaries, cancellation, and testing.
4. State clearly that the tools module is optional.
5. Verify every example against current public signatures.

## Task 6: Move the session adapter into CLI

**Files:**

- Create: `tui/sessionadapter/agent.go`
- Create: `tui/sessionadapter/agent_test.go`
- Modify: `tui/runtime/run.go` comments that name SWE
- Later delete: `swe/swarms/swe/agent.go`

1. Move the generic adapter tests to CLI and make them fail before the implementation is present.
2. Implement `sessionadapter.New` for fresh sessions.
3. Implement `sessionadapter.Restore` for cold replay and initialization cleanup.
4. Preserve gate correlation, visibility filtering, subscription teardown, and idempotent close.
5. Run `go test -race ./sessionadapter ./tui ./cli`.

## Task 7: Create the confinement module

**Files:**

- Create: `confinement/go.mod`
- Create: `confinement/CLAUDE.md`
- Create: `confinement/Makefile`
- Create: `confinement/factory.go`
- Create: `confinement/posture.go`
- Create: `confinement/factory_test.go`

1. Add tests for role and security limit clamping, nil security limit, per-Loop memoization, concurrent access, guarantee masks, strict failure, gated fallback, and unconfined acknowledgement.
2. Implement the effective mode source and sandbox mode adapter.
3. Implement the standard posture table.
4. Implement one executor per Loop binding and return Bash, Grep, and permission views over it.
5. Keep strict failure as the default and gated fallback explicit.
6. Run the full module race suite.

## Task 8: Rename the repository and product to CodeRig

**Files:**

- Rename directory: `swe/` to `coderig/`
- Modify: `coderig/go.mod`
- Rename: `coderig/cmd/swe/` to `coderig/cmd/coderig/`
- Rename or flatten: `coderig/swarms/swe/`
- Modify: `coderig/Makefile`
- Modify: live package comments, prompts, errors, banners, tests, and README
- Modify local git remote URL

1. Rename the working directory and module path to `github.com/looprig/coderig`.
2. Rename the command and binary to `coderig`.
3. Rename live product identity, error prefixes, prompt identity, fingerprints, and current revision strings.
4. Leave historical plan documents unchanged.
5. Update the local `origin` URL without pushing.
6. Run `rg` excluding historical plans to prove no current SWE names remain.

## Task 9: Simplify CodeRig Loop definitions

**Files:**

- Delete: CodeRig Registry implementation and tests
- Delete: CodeRig ModelCatalog implementation and tests
- Delete: `agents/internal/leafrig`
- Modify: operator and reviewer Loop construction
- Modify: greeting and model construction

1. Add focused tests for the explicit operator and reviewer definitions.
2. Replace Registry lookup with direct definitions and a small ordered display slice where needed.
3. Import individual definitions from `github.com/looprig/tools`.
4. Import confinement Factory instead of local executor and posture machinery.
5. Use `tools.PolicyFingerprint` instead of local policy hashing.
6. Delete economy, standard, and premium tiers.
7. Declare meaningful Loop modes and `inference.Effort` directly on definitions.
8. Verify the reviewer binds no mutating tool and the operator receives the intended roster.

## Task 10: Unify CodeRig session construction

**Files:**

- Modify: CodeRig assembly and persistence files
- Modify: command entry point
- Delete: duplicate headless construction seams and tests that only preserve them

1. Add a test that new and restored sessions share the same Loop and Rig definition path.
2. Create one injected store bundle and one `Open` path.
3. Make production supply fsstore and tests supply memstore.
4. Use `sessionadapter.New` or `sessionadapter.Restore` only after the same Rig assembly completes.
5. Keep a headless convenience wrapper only if it delegates directly to the shared path.
6. Run CodeRig unit and integration race tests.

## Task 11: Update live ecosystem references

**Files:**

- Modify: `looprig/profile/README.md`
- Modify: current central consumer documents
- Modify carefully: current `web` and `www` content only where changes do not overwrite user work

1. Replace live SWE repository and command references with CodeRig.
2. Add tools and confinement to the package map.
3. Do not edit historical plan documents.
4. Inspect dirty website diffs before touching overlapping files.
5. If a live reference overlaps unresolved user changes, report it instead of overwriting it.

## Task 12: Cross-module verification

1. Run `gofmt` for changed Go packages.
2. Run `go test -race ./...` in harness, tools, CLI, confinement, and CodeRig.
3. Run each available lint target.
4. Run dependency-boundary tests.
5. Run `git status --short` in every affected repository.
6. Confirm there are no commits, pushes, or unrelated modified files.

## Task 13: Rename the TUI module, terminal adapter, and session security limit

**Files:**

- Rename repository directory: `cli/` to `tui/`
- Modify module path: `github.com/looprig/cli` to `github.com/looprig/tui`
- Flatten: `tui/tui/` into the module root
- Rename: `tui/cli/` to `tui/runtime/`
- Rename: `tui/sessionadapter/` to `tui/sessionadapter/`
- Modify: `tui/docs/specs/session-adapter.md`
- Rename: `harness/pkg/security limit/` to `harness/pkg/security/`
- Modify: Harness command, event, session, tool-binding, Rig, and runtime APIs
- Modify: `tools`, `confinement`, `coderig`, `looprig`, `web`, and `www` consumers

1. Add compile-time tests for the desired root `tui`, `runtime.Run`,
   `sessionadapter.Adapter`, and `security.Limit` APIs and verify they fail while
   the old packages remain.
2. Rename the repository and module to `tui`, flatten the current TUI package,
   move the process-level runner to `runtime`, and update the local origin URL.
3. Rename `sessionadapter` to `sessionadapter`; rename its exported concrete type
   from `Agent` to `Adapter` without changing replay, gate folding, shutdown, or
   controller behavior.
4. Rename Harness package `security limit` to `security`; expose `Level`, `Limit`,
   `LimitSource`, `New`, and `NewBounded` with the same atomic clamping behavior.
5. Rename current public vocabulary from security limit to security limit in
   Session, Rig, tool bindings, commands, events, errors, and application config.
6. Decode legacy `SetSecurityCeiling` commands and `SecurityCeilingChanged`
   events so existing journals remain restorable; emit only the new names.
7. Update all live module imports, specs, consumer documentation, and site copy.
8. Inspect the TUI package dependency graph. Keep presentation state together
   unless another package creates a reusable leaf dependency without exporting
   internal Bubble Tea state.
9. Run `gofmt`, live stale-name searches, full race suites, lint/vet, site builds,
   and repository diff audits. Do not commit or push.
