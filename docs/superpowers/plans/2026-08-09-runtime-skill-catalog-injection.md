# Runtime Skill Catalog Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Make workspace skills discoverable by injecting validated skill metadata into Carbon's per-turn `<runtime_context>` block without changing the static Carbon prompt or bypassing Skill permissions.

**Architecture:** Extend Carbon's existing `defaultRuntimeContextProvider` with a narrow, injected catalog seam and render the catalog after date/cwd/Git data. Keep workspace metadata parsing and path-safety authority in `tools/skill`, exposing only a metadata-oriented API if the current package does not provide the required workspace reader. The existing `Skill` tool remains the only body-loading path and continues to enforce access approval and TOCTOU-safe snapshots.

**Tech Stack:** Go 1.26.4, `github.com/looprig/carbon`, `github.com/looprig/tools/skill`, `harness/pkg/loop.RuntimeContextProvider`, Go `io/fs`/filesystem APIs, package tests.

## Global Constraints

- The catalog belongs in the per-turn `<runtime_context>` block, not `carbon.SystemPrompt`.
- Catalog entries contain frontmatter metadata only; never include skill bodies.
- Listing a skill must not grant permission to load or execute it.
- Existing Skill context-load, filesystem-read, containment, access-gate, and TOCTOU protections remain authoritative.
- Runtime context generation is non-fatal: missing, malformed, inaccessible, or unsafe skill inputs degrade to an empty/partial catalog.
- Names must be validated before path construction; reject traversal, symlink escapes, and paths outside `<workspace>/.skills/`.
- Entries are deterministic: canonical-name sort and duplicate suppression.
- Preserve `maxRuntimeContextBytes` and always emit a closing `</runtime_context>` tag.
- Do not add CLI flags, persistent registries, automatic skill-body loading, or unrelated refactors.

---

## File Structure

- Modify: `carbon/internal/app/runtime_context.go` — add injected skill-catalog seam, production wiring support, discovery invocation, and bounded runtime rendering.
- Modify: `carbon/internal/app/runtime_context_test.go` — add runtime rendering and degradation tests using temporary fixture workspaces and injected seams.
- Inspect/possibly modify: `tools/skill/` — expose a narrow workspace metadata reader only if existing parser/containment APIs cannot be reused; keep parser and safety rules in this package.
- Inspect/possibly modify: `tools/skill/*_test.go` — test any new metadata API independently of Carbon runtime rendering.
- Modify if needed: `carbon/internal/app/assembly.go` — pass the authoritative workspace-root/catalog seam when constructing `NewRuntimeContextProvider`; do not alter static system prompt assembly.
- Modify if needed: `carbon/internal/app/assembly_test.go` — assert the static Carbon system prompt remains unchanged and does not contain `<available_skills>`.

---

### Task 1: Establish the metadata contract in `tools/skill`

**Files:**
- Inspect: `tools/skill/skill_loader.go` and the parser/metadata files in `tools/skill/`
- Modify only if required: `tools/skill/<metadata implementation>.go`
- Test only if required: `tools/skill/<metadata implementation>_test.go`

**Interfaces:**
- Consumes: a workspace root and the existing `SKILL.md` parsing/validation behavior.
- Produces: a narrow metadata record containing canonical `Name` and bounded-source `Description`, plus a workspace reader/discovery function usable by Carbon without exposing skill bodies or private parser types.

- [ ] **Step 1: Inspect the existing parser and workspace helpers completely.**

  Confirm the names of the existing metadata type, frontmatter parser, name validation, and containment helpers. Do not add a second parser if an exported API can safely reuse the existing implementation.

- [ ] **Step 2: Write a focused failing test for metadata-only workspace discovery.**

  Use a temporary fixture with:
  - one valid `.skills/alpha/SKILL.md` containing a name, description, and body;
  - one malformed skill;
  - one empty-description skill;
  - one traversal-like directory name or symlink candidate.

  Assert that the API returns only the valid canonical name and description, never the body, and rejects unsafe candidates.

- [ ] **Step 3: Run the focused tools/skill test and observe the expected failure.**

  Run:

  ```bash
  go test ./tools/skill -run 'Test.*Workspace.*Metadata|Test.*Skill.*Metadata' -count=1
  ```

  Expected: FAIL because the metadata discovery API or behavior is not yet present.

- [ ] **Step 4: Implement the smallest metadata/discovery API.**

  The implementation must:
  - inspect only direct `.skills/<directory>/SKILL.md` candidates;
  - validate the candidate before constructing a path;
  - reject symlink/path escapes;
  - parse frontmatter using the package's authoritative parser;
  - omit malformed, unreadable, empty-name, and empty-description entries;
  - return metadata only, never body bytes;
  - avoid returning raw filesystem errors to the runtime renderer.

- [ ] **Step 5: Run the focused tools/skill tests and verify they pass.**

  Run the same command from Step 3. Expected: PASS with no body leakage.

- [ ] **Step 6: Run the existing tools/skill tests.**

  Run:

  ```bash
  go test ./tools/skill -count=1
  ```

  Expected: PASS; existing Skill loading, permission preparation, and TOCTOU tests remain green.

---

### Task 2: Add runtime catalog rendering with an injected seam

**Files:**
- Modify: `carbon/internal/app/runtime_context.go`
- Test: `carbon/internal/app/runtime_context_test.go`

**Interfaces:**
- Consumes: the metadata reader from Task 1, `defaultRuntimeContextProvider`, and the existing clock/cwd/Git seams.
- Produces: `Blocks(context.Context) []content.Block` output containing a deterministic, metadata-only `<available_skills>` section when valid entries exist.

- [ ] **Step 1: Add a test seam and write the failing rendering test.**

  Add a fixture provider seam to the test instance rather than reading the live repository. Create two valid metadata records in reverse order and assert the rendered block contains:

  ```xml
  <available_skills>
  <skill>
  <name>alpha</name>
  <description>Alpha description.</description>
  </skill>
  <skill>
  <name>zeta</name>
  <description>Zeta description.</description>
  </skill>
  </available_skills>
  ```

  Also assert that a representative skill body string is absent.

- [ ] **Step 2: Run the focused runtime-context test and observe the expected failure.**

  Run:

  ```bash
  go test ./carbon/internal/app -run 'TestDefaultRuntimeContextProvider.*Skill|Test.*Runtime.*Catalog' -count=1
  ```

  Expected: FAIL because `Blocks` currently renders only date/cwd/Git data.

- [ ] **Step 3: Implement catalog rendering minimally.**

  Extend `defaultRuntimeContextProvider` with a narrow catalog function or metadata-reader field. In `Blocks`, append the catalog after `writeGit` and before the existing size-bound/closing-tag logic. Render only validated metadata, use deterministic name ordering, and keep the catalog absent when no valid entries exist.

- [ ] **Step 4: Add safe text rendering and duplicate handling.**

  Ensure names and descriptions cannot inject closing tags or arbitrary control content. Suppress duplicate canonical names deterministically. Do not render raw discovery errors.

- [ ] **Step 5: Run the focused runtime-context test and verify it passes.**

  Run the command from Step 2. Expected: PASS.

- [ ] **Step 6: Run all existing runtime-context tests.**

  Run:

  ```bash
  go test ./carbon/internal/app -run 'Test(DefaultRuntimeContextProvider|NewRuntimeContextProvider)' -count=1
  ```

  Expected: PASS, including all existing date/cwd/Git degradation and output-bound tests.

---

### Task 3: Wire production workspace discovery without changing the static prompt

**Files:**
- Modify: `carbon/internal/app/runtime_context.go`
- Modify if needed: `carbon/internal/app/assembly.go`
- Test: `carbon/internal/app/runtime_context_test.go`
- Test if needed: `carbon/internal/app/assembly_test.go`

**Interfaces:**
- Consumes: the Task 1 metadata reader and the Task 2 runtime provider seam.
- Produces: production `NewRuntimeContextProvider()` behavior that discovers the authoritative current workspace's `.skills/` directory while retaining cwd as fallback where the existing composition requires it.

- [ ] **Step 1: Write a failing production-constructor wiring test.**

  Use an injected or test-only constructor seam so the test supplies a temporary workspace containing one valid skill and verifies the resulting provider includes that skill. Do not change the process working directory globally in a parallel test.

- [ ] **Step 2: Run the focused wiring test and observe the expected failure.**

  Run:

  ```bash
  go test ./carbon/internal/app -run 'Test(NewRuntimeContextProvider|RuntimeContext).*Workspace|Test.*Skill.*Wiring' -count=1
  ```

  Expected: FAIL because production construction currently wires only clock, cwd, and Git.

- [ ] **Step 3: Wire the catalog reader at the composition boundary.**

  Pass the authoritative session/rig workspace root when available. If the current runtime provider constructor cannot receive that root without a broad interface change, use a narrow root resolver seam and retain cwd as the documented fallback. Do not put catalog data into `carbon.SystemPrompt` or the loop definition fingerprint.

- [ ] **Step 4: Add a static-prompt regression assertion.**

  Preserve the existing expectation that the initial Carbon system prompt has zero `<available_skills>` sections. If the test needs clarification, assert that runtime catalog content appears only in provider output and not in `definition.FingerprintInitial().EffectiveSystem`.

- [ ] **Step 5: Run focused wiring and assembly tests.**

  Run:

  ```bash
  go test ./carbon/internal/app -run 'Test(NewRuntimeContextProvider|RuntimeContext|CarbonDefinition)' -count=1
  ```

  Expected: PASS.

---

### Task 4: Add omission, security, and bounds coverage

**Files:**
- Modify: `carbon/internal/app/runtime_context.go` only if a test exposes a missing bound or safety rule
- Test: `carbon/internal/app/runtime_context_test.go`
- Test if needed: `tools/skill/*_test.go`

**Interfaces:**
- Consumes: the completed runtime catalog provider and metadata API.
- Produces: regression coverage for all spec acceptance criteria without weakening existing behavior.

- [ ] **Step 1: Add tests for malformed and inaccessible entries.**

  Assert that one invalid entry is omitted while valid neighboring entries remain present, and that missing `.skills` produces the normal runtime block without an available-skills section or error.

- [ ] **Step 2: Add tests for unsafe paths and untrusted text.**

  Assert that traversal-like names, symlink escapes, tag-like descriptions, control characters, and body text cannot escape the catalog's bounded rendering or forge runtime tags.

- [ ] **Step 3: Add tests for deterministic ordering and duplicates.**

  Supply duplicate canonical names and unsorted metadata; assert exactly one entry per name in sorted order.

- [ ] **Step 4: Add tests for entry and total-output bounds.**

  Supply many entries and oversized descriptions. Assert the rendered output is at most `maxRuntimeContextBytes`, contains `</runtime_context>`, and uses deterministic omission/truncation without partial malformed skill entries where the implementation promises complete entries.

- [ ] **Step 5: Run the focused security and bounds tests.**

  Run:

  ```bash
  go test ./carbon/internal/app ./tools/skill -run 'Test.*(Skill|Runtime|Catalog|Workspace|Bound|Unsafe|Malformed|Duplicate)' -count=1
  ```

  Expected: PASS.

---

### Task 5: Full verification and review handoff

**Files:**
- No planned production changes; only fix issues found by verification or review.

**Interfaces:**
- Consumes: all completed implementation and tests.
- Produces: verified Carbon runtime skill catalog injection with no regressions.

- [ ] **Step 1: Run the complete Carbon module tests.**

  Run:

  ```bash
  go test ./...
  ```

  Expected: PASS. If the workspace runner cannot start commands, record that limitation explicitly rather than claiming success.

- [ ] **Step 2: Run formatting and static checks for changed Go files.**

  Run:

  ```bash
  gofmt -w carbon/internal/app/runtime_context.go carbon/internal/app/runtime_context_test.go tools/skill/*.go
  go vet ./carbon/... ./tools/skill/...
  ```

  Expected: no formatting diff and no vet findings. Limit the `gofmt` file list to files actually changed.

- [ ] **Step 3: Inspect the final diff.**

  Verify that:
  - no static Carbon prompt injection was added;
  - no skill bodies are rendered in runtime context;
  - no access gate or Skill execution path was bypassed;
  - no raw filesystem errors or secrets are rendered;
  - output remains bounded and closed.

- [ ] **Step 4: Request code review.**

  Provide the reviewer the base/head SHAs, this spec, the changed files, focused test output, and the security invariants above. Fix Critical and Important findings before completion; verify each fix individually.

- [ ] **Step 5: Report completion with observed evidence.**

  Include exact commands and observed results, changed files, the runtime block behavior, and any unverified checks caused by environment limitations.
