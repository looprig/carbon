# Workspace Skills and Bash Workdirs Design

**Date:** 2026-08-09

## Goal

Remove CodeRig's embedded `code-style` skill, make human-gated workspace
skills an unconditional product capability, and allow Bash calls to use either
relative or absolute in-workspace working directories without weakening the
sandbox boundary.

## Current problems

CodeRig currently embeds `internal/app/skills/code-style/SKILL.md`, adds it to
Generic's system prompt, and exposes workspace `.skills/` only when the process
is launched with `--runtime-skills`.

The Bash schema describes `workdir` as workspace-relative, but the tools path
primitive intentionally anchors every input beneath the workspace. When a model
supplies the workspace's absolute path, that input is therefore appended to the
workspace root. A subsequent `access.read` declaration for `.` resolves beneath
the doubled path. The sandbox correctly rejects that unconfigured tree scope,
and Harness redacts the internal authorization error to `error: permission
denied`. The shell command never runs, and the user sees an authorization error
for what was actually a working-directory normalization problem.

## Design

### Workspace skills

CodeRig will ship no embedded skills. Delete the `code-style` document, its
embedded filesystem, its allow-list/catalog construction, and its system-prompt
catalog entry.

Generic will always receive the workspace-aware `Skill` tool. A requested
workspace skill remains untrusted and keeps the existing preparation flow:
CodeRig snapshots `.skills/<name>/SKILL.md`, emits `context.load` and
`filesystem.read` requirements, waits for the combined human gate, and returns
only the approved snapshot. Removing the launch switch does not bypass or widen
that gate.

Remove the `--runtime-skills` CLI flag and the `Config.RuntimeSkills` option.
The rig fingerprint will record runtime skills as enabled unconditionally,
truthfully reflecting the product's fixed behavior. Restoring a session whose
old fingerprint recorded runtime skills disabled is configuration drift and
continues through the existing mismatch handling.

Workspace skill names and descriptions will not be injected into the system
prompt. They are workspace-controlled, dynamic, and untrusted; the existing
Skill call remains the explicit load boundary.

### Bash workdirs

`workdir` will accept either a workspace-relative path or an absolute path that
resolves inside the workspace. An empty value still selects the workspace root.

For an absolute value, Bash will first express it relative to the configured
workspace root and then pass that relative form through the existing
symlink-aware `workspace.ContainedPath` check. This preserves one containment
authority and prevents absolute inputs from being re-anchored as path text.
Absolute paths outside the workspace, lexical `..` escapes, and symlink escapes
remain rejected during `PrepareCall`, before access evaluation or execution.

Filesystem access declarations retain their existing semantics. In particular,
absolute declaration paths may intentionally name host paths so the selected
profile can allow, gate, or deny them. This change applies only to Bash's
confined working directory.

The model-facing schema will describe the accepted relative-or-contained-
absolute contract. The prepared artifact retains the original `workdir` and
re-resolves it immediately before execution, preserving the existing
resolution-change check.

## Error handling

An invalid or outside-workspace `workdir` remains a tool-preparation error and
never reaches the access gate. A valid absolute in-workspace value normalizes to
the same authoritative working directory and access scopes as its relative
equivalent. Sandbox and grant enforcement remain unchanged and continue to fail
closed.

## Testing

In `tools`:

- reproduce the reported absolute-root plus `access.read tree "."` call and
  assert the request uses the single workspace root;
- accept an absolute in-workspace subdirectory;
- reject absolute outside-workspace paths, lexical escapes, and symlink escapes;
- run a command successfully from an absolute in-workspace directory.

In `coderig`:

- assert Generic has no embedded `code-style` catalog or asset;
- assert the workspace-aware Skill definition is always present and requires a
  workspace binding;
- run the existing gated workspace-skill integration with a zero `Config`;
- assert runtime-skills fingerprinting is always enabled;
- assert `--runtime-skills` is now an unknown flag.

Focused package tests run first, followed by each repository's full race-enabled
test suite in accordance with its contributor instructions.
