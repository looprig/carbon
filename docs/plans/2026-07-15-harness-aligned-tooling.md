# Harness-Aligned Repository Tooling Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Give `tui`, `confinement`, and `carbon` consistent, self-contained Go quality and security checks before committing their current changes.

**Architecture:** Reuse the `harness` Makefile and Go tool-declaration pattern while preserving each repository's existing dependency distribution policy. `tui` remains vendored; `confinement` and `carbon` remain ordinary Go modules.

**Tech Stack:** Go 1.26.4, Make, `go vet`, Staticcheck, Gosec, Govulncheck, and Go race tests.

---

### Task 1: Align Carbon tooling

**Files:**

- Modify: `carbon/go.mod`
- Modify: `carbon/go.sum`
- Modify: `carbon/Makefile`
- Modify: `carbon/AGENTS.md`
- Modify: `carbon/CLAUDE.md`

1. Declare the three approved Go analysis tools.
2. Add `staticcheck`, `gosec`, module verification, vulnerability scanning, and
   `secure` Make targets.
3. Document the required pre-commit commands.
4. Refresh module metadata.
5. Run the secure checks, race tests, and trimmed build.

### Task 2: Align confinement tooling

**Files:**

- Modify: `confinement/go.mod`
- Modify: `confinement/go.sum`
- Modify: `confinement/Makefile`
- Modify: `confinement/AGENTS.md`
- Modify: `confinement/CLAUDE.md`

1. Declare the same approved Go analysis tools.
2. Add the same lint, vulnerability, and secure targets.
3. Document the required pre-commit commands.
4. Refresh module metadata.
5. Run the secure checks and race tests.

### Task 3: Complete TUI's vendored tooling path

**Files:**

- Modify: `tui/Makefile`

1. Add vendor refresh, scrub, and integrity targets matching the module's local
   replacements.
2. Keep application commands on `-mod=vendor`.
3. Resolve declared tool binaries with `-mod=mod` so Go 1.26.5 can execute them.
4. Run vendor integrity, secure checks, race tests, and a trimmed CGO-free
   build.

### Task 4: Commit independently

1. Review each repository's complete staged diff.
2. Commit `tui`, `confinement`, and `carbon` separately with repository-specific
   messages.
3. Confirm each repository is clean and record each commit ID.
