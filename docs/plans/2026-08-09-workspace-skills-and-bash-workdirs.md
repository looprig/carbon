# Workspace Skills and Bash Workdirs Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remove Carbon's embedded `code-style` skill, make gated workspace skills unconditional, and safely accept absolute in-workspace Bash working directories.

**Architecture:** Bash will normalize absolute workdirs to workspace-relative input before passing them through the existing symlink-aware containment primitive; filesystem access declarations remain unchanged. Carbon will always construct a workspace-aware Skill tool backed by an empty embedded allow-set, remove the embedded catalog and launch switch, and fingerprint the fixed enabled posture truthfully.

**Tech Stack:** Go 1.26, standard library `path/filepath`, LoopRig tools/harness/sandbox contracts, table-driven unit and integration tests.

---

### Task 1: Reproduce absolute Bash workdir handling

**Files:**
- Modify: `../tools/bash/preparecall_test.go`
- Modify: `../tools/bash/bash_test.go`

**Step 1: Write the failing preparation test**

Add a focused test that prepares this reported shape:

```go
args := fmt.Sprintf(`{"command":"find .","workdir":%q,"access":{"read":[{"scope":"tree","path":"."}]}}`, root)
req, _ := prepareBash(t, NewBash(root), args)
```

Assert `req.WorkingDirectory == root`, the tree-read scope is `tree:<root>`, and neither contains `root/root`.

**Step 2: Add containment cases**

Add table cases proving an absolute in-workspace subdirectory is accepted while an absolute sibling outside the workspace and a symlink escape are rejected.

**Step 3: Add an execution test**

Run Bash with an absolute in-workspace subdirectory and assert the command observes that directory.

**Step 4: Run tests to verify RED**

Run:

```bash
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-tools-workdir-gocache go test -race ./bash -run 'TestBashPrepareCallAbsoluteWorkdir|TestBashAbsoluteWorkdir'
```

Expected: FAIL because the current resolver re-anchors the absolute workdir under the workspace root.

### Task 2: Normalize Bash workdirs safely

**Files:**
- Modify: `../tools/bash/bash.go`

**Step 1: Implement the minimal normalization**

Update `resolveSpawnDir` so an absolute workdir is converted with `filepath.Rel(root, workdir)` before calling `workspace.ContainedPath`. Keep empty and relative handling unchanged. Do not change access-declaration path semantics.

**Step 2: Update the schema and comments**

Describe `workdir` as relative or absolute within the workspace. Document that the existing containment primitive remains authoritative.

**Step 3: Format and verify GREEN**

Run:

```bash
gofmt -w bash/bash.go bash/preparecall_test.go bash/bash_test.go
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-tools-workdir-gocache go test -race ./bash
```

Expected: PASS.

### Task 3: Specify unconditional workspace skills in Carbon

**Files:**
- Modify: `cmd/carbon/main_test.go`
- Modify: `internal/app/skills_wiring_test.go`
- Modify: `internal/app/runtime_skills_test.go`
- Modify: `internal/app/runtime_skills_integration_test.go`
- Modify: `internal/app/fingerprint_test.go`
- Modify: `internal/app/persistence_test.go`

**Step 1: Change CLI expectations**

Delete `wantRuntimeSkills` assertions, assert zero flags still produce the normal configuration, and add `--runtime-skills` as a removed/unknown flag case.

**Step 2: Change skill wiring expectations**

Assert Carbon's effective system prompt has no `<available_skills>` block and no `code-style`, while `skillDefinitionFor` is always non-nil, requires a workspace, and builds `Skill`.

**Step 3: Change fingerprint expectations**

Assert `agentFingerprintFields(Config{})` always reports `RuntimeSkills: true`. Remove false/true configuration variants from persistence tests.

**Step 4: Change integration setup**

Run the workspace-skill integration with `Config{}` on both new and restored sessions, proving the default product posture remains human-gated end to end.

**Step 5: Run tests to verify RED**

Run:

```bash
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-carbon-workdir-gocache go test -race ./cmd/carbon ./internal/app -run 'TestParseFlags|TestCarbon.*Skill|TestAgentFingerprintFields|TestCompactionWiring'
```

Expected: FAIL against the current optional flag/config and embedded catalog.

### Task 4: Remove embedded code-style and the runtime-skills switch

**Files:**
- Delete: `internal/app/skills/code-style/SKILL.md`
- Delete: `internal/app/skills.go`
- Delete: `internal/app/skills_catalog.go`
- Delete: `internal/app/skills_test.go`
- Modify: `internal/app/assembly.go`
- Modify: `internal/app/config.go`
- Modify: `internal/app/persistence.go`
- Modify: `cmd/carbon/main.go`
- Modify: `CLAUDE.md`

**Step 1: Remove embedded skill composition**

Delete `genericSkills`, `SkillsFS`, allow-map/catalog assembly, and prompt injection. Construct an empty embedded loader with no allow-map and always build `Skill` with `skill.WithWorkspaceRoot(bind.Workspace.Root)`.

**Step 2: Remove configuration and CLI branching**

Delete `Config.RuntimeSkills`, the CLI field/flag, and every conditional skill-definition branch. Keep `tool.RequiresWorkspace` on the unconditional Skill definition.

**Step 3: Make fingerprint posture constant**

Set `rig.ConfigFingerprintFields.RuntimeSkills` to `true` in `agentFingerprintFields`, documenting that it is fixed product behavior.

**Step 4: Update current contributor documentation**

State that Carbon always has the human-gated workspace Skill tool and ships no embedded skill catalog. Historical design plans remain historical.

**Step 5: Format and verify GREEN**

Run focused non-integration tests and expect PASS.

### Task 5: Cross-repository verification

**Files:**
- Verify only.

**Step 1: Verify tools**

Run:

```bash
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-tools-workdir-gocache go test -race ./...
```

Expected: PASS.

**Step 2: Verify Carbon unit suite**

Run:

```bash
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-carbon-workdir-gocache go test -race ./...
```

Expected: PASS.

**Step 3: Verify the workspace-skill integration**

Run:

```bash
GOWORK=<task-go-work> GOCACHE=/private/tmp/looprig-carbon-workdir-gocache go test -tags integration -race ./internal/app -run TestRuntimeSkillsWorkspaceLoadGatedEndToEnd
```

Expected: PASS with a real combined human gate.

**Step 4: Run repository security checks**

Run the documented formatting, lint, and security commands for both repositories. Any environmental exception must be reported separately from code failures.

**Step 5: Inspect diffs and status**

Confirm no dependency files, generated files, unrelated user changes, or embedded `code-style` references remain in current production/test code.
