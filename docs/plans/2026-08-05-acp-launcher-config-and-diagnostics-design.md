# ACP Launcher Configuration and Availability Diagnostics Design

**Date:** 2026-08-05

**Status:** Approved

## Goal

Carbon's agent collaboration tools (`StartAgent`, `MessageAgent`, `ListAgents`,
`StopAgent`) and the full ACP delegation pipeline to Claude Code and Codex are
implemented and released. In practice, enabling a claude-code or codex runtime
row today requires an operator to know two undocumented environment variables
(`CLAUDE_CODE_ACP_EXECUTABLE`, `CODEX_ACP_EXECUTABLE`), install the matching
ACP adapter binaries, and interpret total silence when anything is missing:
`NewACPComposition` drops every unavailable harness row from the runtime
catalogue without a trace, so `StartAgent` simply never advertises the harness
and nothing explains why.

This design closes the enablement gap with two changes:

1. ACP executable locations become first-class machine configuration in
   `~/.looprig/models.json`, with environment-variable override and PATH
   discovery fallback.
2. Startup produces bounded, secret-free diagnostics naming each harness that
   was configured but dropped, and why, on the same presentation channel the
   permission-store diagnostics already use.

## Non-goals

- No change to the delegation tools, the runtime catalogue contract, the
  preflight probes themselves, or restore semantics.
- No `carbon doctor` command, no TUI runtime browser, no adapter
  auto-installation.
- No relaxation of the fail-closed rules for a malformed `models.json`.

## Current state (verified 2026-08-05)

- `internal/app/acpproduction.go` reads the two executable env vars directly
  into `ACPChildrenConfig.Executables`. An empty or invalid path fails
  `preflightACPExecutable` and the harness is silently omitted
  (`acpchildren.go`, "Missing executables simply omit that harness").
- `filterACPPreflightCatalog` silently discards rows whose gateway or native
  preflight failed, whose advertised-model closure removed every alias, or
  whose configured default/small model became unresolvable.
- The only existing startup diagnostics channel is
  `sessionAccess.diagnostics` (permission-store load notices), surfaced by
  `RuntimeAgent.SessionPresentation()` as
  `tui.SessionPresentation.PermissionDiagnostics`.
- `modelConfigFile` (schema version 2) has no launcher section; the CLAUDE.md
  architecture note already classifies ACP executable locations as "launcher
  settings, not model credentials".

## Design

### 1. `acp_launchers` block in `models.json`

A new optional top-level object keyed by harness name:

```json
"acp_launchers": {
  "claude-code": { "executable": "/Users/me/.local/bin/claude-code-acp" },
  "codex":       { "executable": "/Users/me/.cargo/bin/codex-acp" }
}
```

Rules:

- Allowed keys are exactly `claude-code` and `codex`; an unknown harness key
  fails validation like any other unknown field (fail closed on malformed
  configuration is preserved).
- `executable` is required and non-empty within an entry, must be a clean
  absolute path (`filepath.IsAbs` and `filepath.Clean` idempotent), and is
  bounded like other string fields. Whether the file exists, is regular, and
  is executable remains a *runtime availability* question answered by the
  existing `preflightACPExecutable`, not a validation failure: a configured
  path that does not currently resolve produces a diagnostic and drops the
  harness, exactly as a missing env var does today.
- The block is placed at top level, not inside `native_acp`, because the same
  adapter binary serves both credential modes: gateway-backed children and
  native-auth children launch the same executable with different environments.
- The field is optional and additive, so the schema version stays 2. The v1
  to v2 increment existed because `description` became required; adding an
  optional section breaks no existing valid file.
- Launcher paths are machine-local settings. They are excluded from the
  model-configuration digest (`ModelConfigRev`) and from every durable
  fingerprint field. Availability already reaches the configuration
  fingerprint correctly: a harness row appearing or disappearing changes the
  compiled runtime catalogue digest. A merely *moved* executable that still
  passes preflight must not reject a restore.
- Paths never appear in secret-free projections, agent-visible errors, or
  durable events. They may appear in local diagnostics text only in the
  reduced form described in section 3.

### 2. Executable resolution precedence

`newProductionACPCompositionWithPreflight` resolves each harness executable in
this order, taking the first non-empty candidate:

1. The environment variable (`CLAUDE_CODE_ACP_EXECUTABLE` or
   `CODEX_ACP_EXECUTABLE`) — retained as the explicit per-process override and
   for backward compatibility.
2. The `acp_launchers.<harness>.executable` path from `models.json`.
3. `exec.LookPath` on the fixed well-known adapter name: `claude-code-acp`
   for claude-code, `codex-acp` for codex. Only these exact names are probed;
   the bare `claude` and `codex` CLIs do not speak ACP over stdio and are
   never guessed at. A relative `LookPath` result is resolved to an absolute
   path before use so the existing clean-absolute-path invariant holds.

Resolution is pure selection: the chosen candidate still passes through the
unchanged `preflightACPExecutable` stat check and the live ACP
initialize/session preflight. The resolution *source* (env, config, path) is
recorded alongside the choice so diagnostics can say where the failing path
came from.

### 3. Availability diagnostics

`ACPComposition` gains a `Diagnostics []string` field populated by
`NewACPComposition`. One bounded line per configured-but-unavailable harness
or per materially reduced harness, built from fixed categories:

- no executable resolved:
  `acp: claude-code unavailable: no executable (set acp_launchers in models.json or CLAUDE_CODE_ACP_EXECUTABLE)`
- resolved executable failed the stat check:
  `acp: codex unavailable: configured executable not runnable (from acp_launchers)`
- live preflight failed:
  `acp: codex unavailable: preflight failed (gateway)` /
  `acp: claude-code unavailable: preflight failed (native)`
- partial closure:
  `acp: claude-code: 2 configured models not advertised by the adapter`

Rules:

- Category text is fixed and secret-free. Diagnostics never include stderr
  content, provider messages, URLs, tokens, or full filesystem paths; the
  provenance suffix names only the *source* (`acp_launchers`, the env var
  name, or `PATH`), matching the `boundedACPChildError` philosophy.
- A harness that is configured nowhere (no enabled `native_acp` entry and no
  gateway target naming it) produces no diagnostic. Silence is only wrong
  when configuration asked for something that startup could not deliver.
- Diagnostics are advisory presentation only. They do not change catalogue
  compilation, fingerprints, preflight behavior, or failure modes.

Plumbing reuses the existing channel end to end:

- `withProductionACPChildren` carries `composition.Diagnostics` into `Config`
  (a new `Config.ACPDiagnostics []string` alongside the composition pointer).
- `buildSessionAccess`'s result already owns presentation diagnostics;
  `sessionAccess.diagnostics` is extended by appending the ACP lines at
  runtime-agent construction, so `RuntimeAgent.SessionPresentation()` and the
  TUI notice surface them with zero new presentation API. Headless
  construction carries them the same way; embedders read them off the
  presentation as today.

## Ownership

| Owner | Responsibility |
|---|---|
| `modelconfig.go` (+ validate/normalize/digest) | Parse and validate `acp_launchers`; exclude it from the digest |
| `acpproduction.go` | Resolution precedence, provenance capture, PATH discovery |
| `acpchildren.go` | Emit categorized diagnostics where rows are dropped or reduced |
| `config.go` / `runtime_controls.go` | Thread diagnostics to the existing presentation surface |
| docs (`access-profiles.md` area / README) | Document the block, the adapters, and the env overrides |

Harness, acp, and foreignloops modules are untouched.

## Security

- Launcher paths are configuration, not credentials, but live in the same
  owner-only `models.json`; the existing file-mode and symlink checks already
  cover them.
- PATH discovery executes nothing by itself; discovery output feeds the same
  preflight that already gates process launch.
- Diagnostics obey the existing bounded, secret-free error discipline and are
  never written to durable events.

## Verification

- `models.json` with an `acp_launchers` block parses; unknown harness keys,
  relative paths, and empty executables fail validation with typed errors.
- A v2 file without the block remains valid and byte-identical in digest to
  today.
- Precedence: env var beats config beats PATH, proven per harness with a fake
  preflight.
- Changing only a launcher path (both preflight-passing) leaves
  `ModelConfigRev` and the session fingerprint unchanged; removing the
  executable changes the runtime catalogue digest exactly as an env-var
  removal does today.
- Each diagnostic category is produced by its scenario and carries no path,
  stderr, or provider content; an unconfigured harness produces none.
- TUI presentation shows ACP diagnostics through the existing
  `PermissionDiagnostics` surface (integration-level assertion at the
  presenter seam).
