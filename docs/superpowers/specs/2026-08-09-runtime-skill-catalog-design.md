# Runtime Skill Catalog Injection Design

## Status

Draft for user review.

## Goal

Make Carbon workspace skills discoverable to Carbon by injecting a compact `<available_skills>` catalog into the per-turn `<runtime_context>` block, while keeping skill instructions loaded on demand through the existing `Skill` tool.

## Context and Current Behavior

Carbon already wires a workspace-enabled `Skill` tool in `carbon/internal/app/assembly.go`:

```go
loader := skill.NewEmbeddedSkillLoader(nil, nil)
...
skill.NewSkill(loader, agent, skill.WithWorkspaceRoot(bind.Workspace.Root))
```

The `Skill` tool can load a non-embedded workspace skill from `<workspace>/.skills/<name>/SKILL.md`. Workspace loads are untrusted and protected by the existing preparation, permission, containment, and TOCTOU-safe snapshot flow. However, Carbon's static system prompt contains no `<available_skills>` catalog, so a model must already know a skill name before it can request it.

Carbon has a per-turn runtime context seam:

- `harness/pkg/loop.RuntimeContextProvider` exposes `Blocks(context.Context) []content.Block`.
- `carbon/internal/app/runtime_context.go` renders one bounded `<runtime_context>` text block.
- `carbon/internal/app/assembly.go` installs it with `loop.WithRuntimeContext(runtimeCtx)`.

The catalog belongs in this runtime block, not in the static system prompt.

## Design Decisions

### 1. Inject into runtime context, not the static system prompt

Keep `carbon.SystemPrompt` unchanged. Append the catalog inside the existing `<runtime_context>` block so it is refreshed per turn and excluded from immutable system-prompt/fingerprint content. The catalog contains metadata only; skill bodies remain on-demand.

Expected shape:

```xml
<runtime_context>
date: 2026-08-09
cwd: /Users/ipotter/code/looprig
git branch: main
git status: clean

<available_skills>
<skill>
<name>brainstorming</name>
<description>You MUST use this before any creative work...</description>
</skill>
</available_skills>
</runtime_context>
```

### 2. Discover workspace skills from `.skills/`

Inspect only direct child directories of `<workspace>/.skills/`. An entry is eligible only when it contains a regular `SKILL.md`. Validate names before path construction and reject traversal, symlink escapes, malformed paths, and files outside `.skills/`. Reuse existing `tools/skill` parser and workspace-safety contracts where possible.

### 3. Parse metadata only

Read frontmatter `name` and `description` using the authoritative `tools/skill` metadata/parser contract. Do not duplicate parsing rules. Omit malformed, unreadable, empty-name, or empty-description entries without failing the turn. Bound and safely render untrusted descriptions so they cannot forge runtime tags. Never include skill bodies.

### 4. Preserve the existing security model

Catalog discovery is informational and grants no permission. The existing `Skill.PrepareCall` flow remains authoritative for context-load permission, filesystem-read permission, containment, TOCTOU-safe snapshots, and execution.

### 5. Inject through a testable runtime seam

Extend `defaultRuntimeContextProvider` with a narrow injected catalog/discovery seam analogous to its clock, cwd, and Git seams. Production wiring uses the authoritative session/workspace root when available, with current cwd as fallback. Tests must not depend on the live checkout.

Missing workspace, missing `.skills`, discovery errors, and malformed individual skills degrade to an empty/partial catalog and never fail a turn or expose raw filesystem errors.

### 6. Deterministic and bounded output

Sort entries by canonical skill name, emit duplicate names once, and enforce explicit limits for entry count, name length, description length, and total runtime-context size. Preserve the existing `maxRuntimeContextBytes` bound and always close `</runtime_context>`. Truncation/omission must be deterministic.

## Proposed Components

### `carbon/internal/app/runtime_context.go`

Add the catalog seam, workspace metadata discovery/rendering, deterministic ordering, bounds, and fail-soft behavior. Keep date/cwd/Git behavior unchanged.

### `carbon/internal/app/runtime_context_test.go`

Add tests for valid rendering, ordering, malformed/missing/empty metadata omission, missing `.skills`, unsafe names/symlinks, body exclusion, duplicate handling, bounds, closing tags, and non-fatal degradation.

### `tools/skill`

If necessary, expose a narrow workspace metadata/discovery API that returns validated name/description records and never bodies. Keep parser and containment authority in `tools/skill`; add package-level tests for the API.

## Data Flow

1. The loop calls `RuntimeContextProvider.Blocks()` for a turn.
2. Carbon renders date, cwd, and Git state.
3. It resolves the session workspace root and scans direct `.skills` entries.
4. It parses validated metadata only, sorts and bounds entries.
5. It appends `<available_skills>` to the runtime block.
6. Carbon selects a listed name and invokes `Skill`.
7. The existing Skill tool performs the gated snapshot/load operation.

## Error Handling and Security

- Runtime context generation never returns an error.
- Missing/inaccessible `.skills` is an empty catalog.
- Bad entries are skipped individually.
- No bodies or raw filesystem errors are rendered.
- Names are validated before path construction.
- Symlink/path escapes are rejected.
- Catalog listing grants no access.
- Output is bounded and well-formed.
- Workspace content remains untrusted.

## Non-Goals

- Changing `carbon.SystemPrompt`.
- Automatically loading skill bodies.
- Automatically approving workspace skills.
- Adding CLI flags or persistent registries.
- Changing the Skill tool permission/TOCTOU model.
- Discovering skills outside the current workspace `.skills/` directory.

## Acceptance Criteria

1. Valid workspace skills produce a deterministic `<available_skills>` catalog inside `<runtime_context>`.
2. The catalog contains validated frontmatter metadata only.
3. Carbon's static prompt is unchanged.
4. Existing Skill permission and TOCTOU protections remain active.
5. Bad, unsafe, inaccessible, or oversized inputs degrade without failing a turn.
6. Output remains bounded and closes `</runtime_context>`.
7. Existing date/cwd/Git behavior remains intact.
8. Focused app and, if needed, tools/skill tests cover success, omission, ordering, security, bounds, and degradation.

## Verification Plan

- Run focused `carbon/internal/app` runtime-context tests.
- Run focused `tools/skill` tests if its metadata API changes.
- Run Carbon package tests.
- Verify a deterministic fixture contains names/descriptions but no bodies.
- Verify existing Skill tests still demonstrate gated, snapshot-based loading.

## Open Review Question

Confirm whether discovery should use process cwd or the authoritative session workspace root when they differ. Recommendation: use the session workspace root, with cwd as fallback.
