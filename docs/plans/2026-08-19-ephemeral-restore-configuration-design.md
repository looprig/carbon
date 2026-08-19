# Ephemeral Restore Configuration Design

## Goal

Carbon sessions preserve durable conversation and loop history while adopting
the current process configuration on resume. Models, skills, MCP servers,
launcher paths, and unused ACP runtimes are ephemeral and do not make a session
unrestorable. A durable loop that actually used an ACP harness still requires
that harness to be available.

New Carbon processes default to the `trusted` access profile. The profile
selected for the current process applies to both new and restored sessions;
access-profile drift is audited but does not reject a Carbon restore.

## Restore policy

Harness remains policy-neutral. Its existing `session.RestoreDecider` exposes
manifest drift to the composition layer, and a new runtime-restore resolver
exposes a durable loop's validated runtime selection plus the current catalogue
when exact reconstruction fails. Both hooks keep their existing fail-closed
behavior when a composer does not install a policy.

Carbon installs application-specific implementations instead of using
Harness's defaults or the blanket `WithAllowConfigMismatch` escape hatch. The
manifest decider accepts drift in:

- model identity and the parent runtime catalogue;
- MCP external-capability identity;
- runtime skills, prompts, tools, and ordinary topology;
- Carbon's access-profile application field and its derived permission-policy
  revision.

The decider continues to reject changes in workspace identity, workspace trust,
confinement, agent kind/name/adapter, hook policy, and the selected active
runtime profile or adapter identity. This keeps filesystem and process identity
boundaries explicit while allowing Carbon's operator-selected access profile to
be current-process configuration.

Every accepted difference remains visible in Harness's typed drift assessment
and produces the existing durable `ConfigurationAdopted` audit event. Carbon
does not remove ephemeral fields from manifests: they remain useful diagnostic
history even though they no longer gate restore.

## Used ACP runtimes

Global ACP catalogue drift is not evidence that a session depended on the
changed entry. Harness already reconstructs durable child loops individually
and tombstones unavailable inactive children. Carbon therefore accepts global
catalogue drift and leaves the per-loop restored builder as the compatibility
boundary.

For each durable ACP child, Harness first attempts its exact, fail-closed
reconstruction. If the old model, effort, credential source, or target alias no
longer exists, it gives the validated mismatch and current catalogue to the
composer-installed runtime resolver. Carbon's resolver may return only the
current default selection for the same persisted harness; it may not cross to a
different harness. Launcher paths are never durable identity. The persisted ACP
session identifier is passed to the same harness's restored builder with the
current configuration.

If that harness has no admitted profile, no verified executable, or cannot
restore the durable child, Harness tombstones an inactive child. If the failed
child is the session's active loop, the entire restore fails. Harness will
preserve the typed runtime-mismatch cause for this active-loop failure so the
operator sees that a used ACP harness—not unrelated configuration drift—blocked
the resume.

## MCP and skills behavior

MCP servers are entirely ephemeral. A historical MCP invocation and result are
already durable conversation events; restoring them does not require the old
server. New turns receive only the MCP servers configured for the current
process. Adding, removing, or changing an MCP server never rejects restore.

Skills, system prompts, and tool schemas likewise come from the current Carbon
composition. Harness marks reconstructed model context stale when configuration
was adopted, so the context is rebuilt from durable conversation events under
the current prompt/tool/skill configuration without losing messages.

## Access-profile default and adoption

`DefaultAccessProfile` changes from `readonly` to `trusted`, and CLI tests/help
expect that default. Explicit `--access-profile readonly` and the separately
acknowledged `unconfined` profile remain available.

On resume, the current invocation's access profile is adopted even when it is
broader than the persisted profile. This is Carbon-specific policy and is
recorded as configuration adoption. It does not relax workspace-root,
confinement, or agent-identity checks.

## Operator diagnostics

Restore failures must state the actionable boundary. An active durable ACP loop
whose harness cannot be reconstructed reports that the session used an ACP
runtime that is now unavailable, including the bounded harness name when it is
safe to do so, and advises restoring the launcher/profile or starting a new
session. Diagnostics never include executable paths, credentials, provider
responses, or model content.

Configuration drift that Carbon accepts is not presented as a failure. The
durable adoption event remains the audit record.

## Local ACP configuration

The shell must not override `models.json` with npm shim symlinks. The two stale
`CLAUDE_CODE_ACP_EXECUTABLE` and `CODEX_ACP_EXECUTABLE` exports are removed from
`~/.zshrc`; Carbon's owner-only `~/.looprig/carbon/models.json` remains the
single launcher authority.

## Components

- `carbon/internal/app`: Carbon manifest decider and same-harness runtime
  resolver, plus focused integration tests.
- `carbon/cmd/carbon`: trusted default assertions and CLI behavior.
- `harness/internal/sessionruntime`, `harness/pkg/session`, and `harness/pkg/rig`:
  expose the policy-neutral runtime-restore request/decision hook, preserve the
  fail-closed default, and present the active child runtime-mismatch cause.
- `~/.zshrc`: remove obsolete ACP environment overrides.

No persisted event schema changes are required. Existing sessions use the
current manifest drift path and gain the new Carbon policy at their next
restore.

## Verification

Tests cover:

- model, catalogue, MCP, skill, and access-profile drift restoring successfully;
- workspace, confinement, and agent-identity drift remaining rejected;
- unused ACP removal restoring successfully;
- a used inactive ACP child being tombstoned while the session restores;
- a used active ACP child failing with an actionable unavailable-harness error;
- same-harness model/effort/source drift selecting the current fallback;
- the CLI defaulting to trusted while explicit readonly still works;
- ACP launcher resolution after the shell overrides are removed.

Run repository-native tests plus `GOWORK=off go test ./...` in every modified Go
module. Carbon's workspace integration tests run against the modified Harness;
standalone Carbon verification requires a published Harness version before any
Carbon dependency pin is updated, following the workspace release graph.
