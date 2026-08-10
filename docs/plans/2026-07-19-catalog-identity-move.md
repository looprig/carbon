# Catalog Identity Move Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development to implement this plan task-by-task.

**Goal:** Move CodeRig's shared prompt identity from application assembly into the internal prompt catalog without changing prompt bytes or runtime behavior.

**Architecture:** `internal/catalog` owns static identity and role prompt content. `internal/app` imports that content and remains responsible only for composing the final Loop system prompts.

**Tech Stack:** Go 1.26, standard library XML testing, existing CodeRig catalog and app packages.

---

### Task 1: Establish catalog ownership with a failing test

**Files:**
- Create: `internal/catalog/identity_test.go`
- Reference: `internal/app/identity_test.go`

1. Copy the existing identity-content and XML-contract tests into package `catalog`.
2. Run `go test ./internal/catalog`.
3. Confirm the package fails to compile because `catalog.Identity` does not exist.

### Task 2: Move the shared identity prompt

**Files:**
- Create: `internal/catalog/identity.go`
- Delete: `internal/app/identity.go`
- Delete: `internal/app/identity_test.go`
- Modify: `internal/app/swarm.go`
- Modify: `internal/app/greeting_test.go`
- Modify: `internal/app/skills_catalog.go`

1. Move the `Identity` constant byte-for-byte into package `catalog`.
2. Import the catalog root in app assembly and reference `catalog.Identity` wherever prompts are composed or asserted.
3. Update comments to name `catalog.Identity`.
4. Remove the obsolete app-owned implementation and tests.
5. Run `gofmt` on changed Go files.
6. Run `go test ./internal/catalog ./internal/app` and confirm both packages pass.

### Task 3: Verify behavior and scope

**Files:**
- Verify all changed files above.

1. Run `go test -race ./...` and confirm the full repository suite passes.
2. Run `git diff --check` and inspect `git diff` to confirm the prompt text is unchanged and unrelated working-tree changes are preserved.

No commit is included because repository instructions require explicit user authorization before committing.
