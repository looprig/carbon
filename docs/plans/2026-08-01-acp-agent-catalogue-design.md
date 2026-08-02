# CodeRig ACP Agent Catalogue and Three-Agent Roster Design

**Date:** 2026-08-01

**Revised:** 2026-08-02

**Status:** Approved

## Goal

CodeRig has exactly three named agents—`planner`, `builder`, and `reviewer`—and
starts every session with `builder` active. All three are native primer Loops and
legal delegated roles. The existing TUI loop footer remains the agent switcher.

Delegated children run through ACP using Claude Code or Codex. Gateway-backed
and native primer model availability is machine configuration, not a compiled Go
table. CodeRig loads it once from:

```text
~/.looprig/models.json
```

The file may contain API keys in this iteration. OAuth and harness-native login
discovery are deferred. CodeRig validates the file, binds credentials directly
to provider clients, discards them from all public and durable projections, and
compiles one immutable, secret-free runtime catalogue.

Workspace permission decisions remain separate and workspace-specific:

```text
~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json
```

This design composes Harness's generic Subagent runtime-selection work and does
not redesign Task/Todo or the permission system.

## Decisions

- The fixed role roster and role policies remain in CodeRig source.
- Model rows, provider targets, credentials, effort choices, and model defaults
  come from `~/.looprig/models.json`; no production model row is hard-coded.
- Model configuration is machine-wide and shared by all workspaces for the same
  OS account. Permission approvals remain isolated per canonical workspace.
- `models.json` supports API-key and no-auth targets. OAuth, subscription-login
  discovery, Bedrock SigV4, and other credential mechanisms are deferred until
  they receive explicit schema and client-construction support.
- A missing `models.json` is valid: CodeRig has no configured gateway models and
  reports a bounded capability error when no usable primer/default exists.
- A present but malformed or insecure `models.json` fails startup. CodeRig never
  partially accepts a malformed file.
- All three native primers use one configured `primer_default` in this release.
  The schema marks other models as primer-capable for future selection, but the
  TUI must advertise only choices that the current runtime can actually switch.
- Delegated children are ACP-backed only. Claude Code and Codex may use any
  configured model marked for delegation when its connector and gateway route
  pass preflight.
- ACP children remain posture-only in this release. They do not consult or
  modify CodeRig's interactive permission file. A role/harness combination is
  admitted only if the connector can enforce its non-interactive posture.
- Provider identity, endpoint, exact model ID, and credentials are private
  routing data. The Subagent surface exposes only harness, alias, and effort.
- Restore never silently substitutes a model, credential mode, effort, or
  harness when configuration has changed.

## Ownership

| Owner | Responsibility |
|---|---|
| CodeRig | Role roster and prompts, global model-config schema and loader, client construction, defaults, access profiles, and catalogue compilation |
| Harness | Parent-scoped Subagent selection, child lifecycle, durability, quotas, and protocol-neutral builder registry |
| `foreignloops/driver/acp` | Adapt ACP sessions to the foreign Loop backend contract |
| `acp/launch` | Launch/configure Claude Code or Codex and apply its role posture |
| Inference/LLM | Validate model/provider combinations, construct supported clients, translate gateway ingress, and enforce target effort |
| Tools/Harness gate | Interactive permission decisions and persisted workspace rules for native Loops |
| TUI | Loop focus, primer switching, runtime controls, and permission diagnostics |

Harness must not read `models.json`, import ACP, or know provider configuration.
ACP packages must not own CodeRig roles or credentials. CodeRig loads the file at
its process-composition boundary and compiles the existing Harness runtime
catalogue plus fixed gateway targets from the same normalized input.

## Filesystem Layout and Trust Boundaries

`~/.looprig/models.json` is user-owned machine configuration. It is deliberately
outside every repository so it cannot be committed accidentally or exposed as
ordinary workspace content. CodeRig resolves the home directory with
`os.UserHomeDir`; it never interprets `~` itself.

When the file exists, CodeRig requires:

- a regular file, not a directory, device, or named pipe;
- no symbolic link at the final path;
- on Unix, no group or other permission bits (`mode & 0077 == 0`), normally
  mode `0600`;
- a bounded size of at most 1 MiB;
- UTF-8 JSON with exactly one top-level value and no trailing data;
- schema version `1` and no unknown fields.

CodeRig only reads this file. It does not create, rewrite, chmod, or migrate it.
Those actions require a separate explicit configuration command in the future.

The existing permission file remains:

```text
~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json
```

Interactive native Loops use a writable workspace permission store. Headless
runs accept only an explicitly configured absolute read-only permission file.
Model loading must not alter either behavior or reuse permission storage APIs.

## Model Configuration Schema

The canonical shape is:

```json
{
  "version": 1,
  "primer_default": "local-builder",
  "claude_code_small_model": "claude-small",
  "delegate_defaults": {
    "planner": {"harness": "codex", "model": "openai-planner", "effort": "high"},
    "builder": {"harness": "claude-code", "model": "claude-builder", "effort": "high"},
    "reviewer": {"harness": "claude-code", "model": "claude-reviewer", "effort": "medium"}
  },
  "models": [
    {
      "alias": "openai-planner",
      "provider": "openai",
      "api_format": "openai-responses",
      "base_url": "",
      "model": "gpt-5.6-sol",
      "api_key": "REDACTED-EXAMPLE",
      "uses": ["primer", "delegate"],
      "capabilities": {"tools": true, "thinking": true},
      "efforts": ["none", "low", "medium", "high", "max"],
      "default_effort": "high"
    },
    {
      "alias": "local-builder",
      "provider": "lmstudio",
      "api_format": "openai",
      "base_url": "http://127.0.0.1:1234/v1",
      "model": "exact-loaded-model-id",
      "uses": ["primer", "delegate"],
      "capabilities": {"tools": true},
      "efforts": ["none"],
      "default_effort": "none"
    }
  ]
}
```

`api_key` is omitted for no-auth providers. An empty string is treated as
absent. The raw decoded configuration is private to the loader/compiler and is
never returned through `app.Config` or stored on `model.Model`.

Each model row has these rules:

- `alias`: non-empty stable selector; unique across all rows.
- `provider`: a provider supported by the current LLM client factory.
- `api_format`: a format accepted for that provider.
- `base_url`: optional provider default or an HTTPS URL; HTTP is allowed only
  for loopback, using the existing `model.Model.Validate` rules.
- `model`: exact provider model ID; non-empty.
- `api_key`: required exactly when the provider requires API-key auth.
- `uses`: non-empty unique subset of `primer` and `delegate`.
- `capabilities`: conservative user assertions. `tools` must be true for every
  CodeRig primer or delegate. Thinking must be true if any non-`none` effort is
  advertised.
- `efforts`: non-empty, unique, valid neutral efforts. Unsupported `xhigh` and
  `ultra` are rejected rather than clamped.
- `default_effort`: must occur exactly once in `efforts`.

The loader rejects duplicate keys where detectable by typed decoding rules,
duplicate aliases, unknown uses, invalid defaults, unsupported provider auth,
missing Claude small-model requirements, and defaults referring to models not
admitted for that use.

## Agent Roster and Native Permissions

The three product definitions remain `planner`, `builder`, and `reviewer`; all
are primers and delegates, and `builder` is initially active.

- Planner is read/research oriented and cannot mutate the workspace.
- Builder owns edits, command execution, and verification under the selected
  native access profile and permission gate.
- Reviewer can inspect and run admitted checks but cannot mutate the workspace.

Native Loop enforcement uses the current direct sandbox profile, executor set,
Harness gate, and Tools permission store. Planner and reviewer use the
intersection with CodeRig's read-only profile under every selected user access
profile. This design does not replace these current permission-based features.

The model file never grants authority. Changing a model, provider, or API key
cannot widen a role's tools, sandbox profile, permission rules, or delegation
limits.

## Catalogue Compilation and Data Flow

Startup proceeds in this order:

1. Resolve and securely open `~/.looprig/models.json`.
2. Strictly decode and validate the complete schema.
3. Convert every row into a secret-free `model.Model`.
4. Validate provider/format/auth requirements through the LLM module.
5. Bind each API key directly to its `inference.Client`.
6. Build primer dependencies from the configured `primer_default`.
7. Build gateway-backed `ACPGatewaySource` rows for delegate-capable models.
8. Preflight configured ACP executables and remove unavailable harness choices.
9. Compile one immutable Harness runtime catalogue and fixed-target gateway
   registry.
10. Compute a secret-free digest and enter session construction.

No step after decoding needs the raw key. Errors identify a bounded field,
alias, or provider but never include file contents, credentials, provider
response bodies, or child environments.

The catalogue remains frozen for the process/session lifetime. Editing the file
does not mutate a running session. A new process or explicit future reload is
required.

## ACP Runtime and Permission Semantics

Each delegated child selects one immutable tuple:

```json
{
  "description": "Review the restore path",
  "prompt": "Inspect restore behavior and report correctness risks.",
  "subagent_type": "reviewer",
  "agent_harness": "claude-code",
  "model": "claude-reviewer",
  "effort": "medium",
  "run_in_background": true
}
```

The selected alias resolves to a client-bound fixed gateway target. The child
receives only its loopback gateway URL and ephemeral gateway token. It never
receives the configured provider API key or the path/content of `models.json`.

Claude Code requires a configured `claude_code_small_model`. That alias must be
delegate-capable and gateway-compatible whenever a Claude Code gateway profile
is advertised. CodeRig materializes it alongside the selected main target.

ACP permissions remain non-interactive and posture-only. CodeRig maps planner,
builder, and reviewer to the connector's known sandbox/approval posture and
registers `session/request_permission` to deny requests outside that posture.
ACP children neither read nor persist `permissions.json`. Full parity with
native interactive grants is deferred.

## Durability and Fingerprints

The durable model-configuration digest includes only normalized secret-free
fields: version, aliases, providers, formats, endpoints, exact model IDs, uses,
capabilities, efforts, defaults, and whether required credentials were present.
It must never include API-key bytes or a hash of those bytes.

ACP child identity remains secret-free: role, harness, credential mode, model
alias, small-model alias where applicable, normalized effort, runtime-profile
name, and ACP resume/session ID.

On restore, a missing alias, changed target descriptor, unavailable harness, or
configuration digest mismatch never silently falls back. Existing Harness
configuration-mismatch policy applies. An individual ACP child that cannot be
resumed restores as a closed tombstone without preventing primer/sibling
restore.

Rotating only an API-key value does not change the digest. Changing key presence
does, because it changes whether a route is executable.

## Failure Behavior

- Missing `models.json`: return an empty model configuration; startup succeeds
  only if the requested operation can run without a configured primer/model.
- Insecure file type or permissions: fail startup with a bounded configuration
  error naming the path, never file contents.
- Invalid JSON/schema/model/default: reject the entire file; no partial routes.
- Unsupported provider credential mechanism: reject that configuration with a
  typed unsupported-auth error.
- Missing required API key: reject the row/file; never advertise it.
- No usable `primer_default`: fail CodeRig session construction before opening
  persistence or starting a Loop.
- Missing ACP executable/preflight failure: omit that harness; native primers
  remain usable.
- No delegated profiles: Subagent reports bounded no-runtime capability.
- Gateway/ACP launch failure: release resources and return a bounded error.
- Restore cannot resolve prior identity: configuration mismatch or child
  tombstone as dictated by the existing Harness restore boundary.

## Verification

Tests must prove:

- the three-role roster and builder-active topology remain unchanged;
- the global path is exactly `~/.looprig/models.json` and permissions remain in
  the hashed workspace directory;
- missing model config, strict decoding, size/type/mode checks, duplicate and
  unknown-field rejection, and no partial acceptance;
- API-key/no-auth validation and supported-provider client construction;
- secrets never appear in models, catalogue entries, digests, formatted errors,
  events, child environments, gateway aliases, or journals;
- arbitrary configured aliases replace the old frozen six-alias table;
- primer and delegate use/default validation;
- Claude small-model validation;
- exact effort admission and target-authoritative gateway enforcement;
- same- and cross-dialect routing through both available ACP harnesses;
- native permission-store behavior and planner/reviewer restrictions do not
  regress;
- ACP children remain posture-only and cannot persist interactive approvals;
- restore detects secret-free configuration drift while API-key rotation alone
  does not invalidate identity.

Run focused tests and full race suites for every changed Go module. Before
release, exercise one no-auth local primer, one API-key gateway model through
Codex, one through Claude Code, and one cross-dialect route.
