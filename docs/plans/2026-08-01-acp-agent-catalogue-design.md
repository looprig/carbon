# CodeRig ACP Agent Catalogue and Three-Agent Roster Design

**Date:** 2026-08-01

**Status:** Approved

## Goal

Replace CodeRig's current operator/operator-primary/reviewer topology with three
named agents—`planner`, `builder`, and `reviewer`—that can each run as a primer
or be selected as a delegated role. Start every session with `builder` active,
while preserving the TUI's existing loop footer as the way a user moves among
the three primer conversations.

Native primer Loops use LM Studio. Delegated children use ACP only, through
either Claude Code or Codex, in one of two credential modes: the harness's
**own account** (its native subscription or login, serving that harness's own
model catalogue) or a **gateway-backed** target for any provider CodeRig holds
direct API credentials for. The ACP child adapter is the
`foreignloops/driver/acp` driver, so CodeRig takes a foreignloops dependency
for delegated children; it never constructs the direct-CLI foreignloops
drivers or MCP agent transport.

This design composes the generic Subagent runtime-selection work specified in
Harness. It does not redesign the Task/Todo tool.

Coordinated development uses the checked-in root `go.work`. Implementation must
not temporarily pin unpublished sibling commits or versions in component
`go.mod` files. Published dependency-version adoption remains explicit release
work after the providing module is released.

## Decisions

- The product has exactly three agent definitions: `planner`, `builder`, and
  `reviewer`.
- All three definitions are primers and legal Subagent targets.
- `builder` is the initial active primer.
- Primers are native Harness Loops backed by LM Studio in this iteration.
- Delegated children are ACP-backed Loops only.
- The supported ACP harnesses are `claude-code` and `codex`.
- Every ACP child runs in exactly one credential mode: `native-auth` (the
  harness's own subscription/login and its own model catalogue) or
  `gateway-backed` (a target CodeRig holds direct API credentials for).
- Gateway-backed targets may be any provider CodeRig owns direct API access
  to — Anthropic, OpenAI, OpenRouter, Bedrock, Phala, LM Studio, or another
  configured provider — subject to the Inference module having a client for
  it.
- A gateway-backed target is admitted only where a real credential and client
  exist; a native-auth child is confined to its own harness's models. Cross-
  harness reach into another vendor's subscription models is impossible and
  is never advertised.
- Harness aliases, model aliases, and effort are selected per child Loop.
- Provider identity is private routing metadata; it is not the ACP harness and
  is not exposed as a model-facing Subagent parameter.
- ACP remains optional to Harness and the TUI. CodeRig requires it only for
  delegated-child construction.
- The existing TUI loop footer remains the agent switcher. There is no new Agent
  tray and no `/agent` slash command in this iteration.
- The current Task/Todo work is out of scope.

## Terminology and Ownership

An **agent definition** is a CodeRig role: identity, system instructions,
capability requirements, modes, access posture, and delegation policy.

A **Loop** is one live or restored conversation executing an agent definition.
The same `builder` definition can therefore back one primer Loop and many child
Loops without making "agent" a first-class Harness runtime object.

An **agent harness** is the program driving an ACP child, currently Claude Code
or Codex. It is not a model provider.

A **model target** is the private destination selected by a stable model alias.
Its provider/client, target API format, exact provider model ID, URL, and
credentials stay behind the gateway registry.

Ownership remains layered:

| Module | Responsibility |
|---|---|
| CodeRig | Agent roster, personas, native primer model policy, curated profiles, admitted ACP/model/effort combinations, defaults, and access posture |
| Harness | Generic parent-scoped Subagent selection, child lifecycle, durability, quotas, and protocol-neutral adapter backend seam |
| `foreignloops/driver/acp` | Adapt `acp/client` and `acp/launch` into the foreignloops `loop.Backend` live/restore construction |
| `acp/launch` | Safely launch and configure Claude Code or Codex ACP processes and bind them to the gateway |
| Inference gateway | Resolve harness-facing aliases and translate ingress requests to the selected target client's API format |
| TUI | Present Loop conversations, footer focus, focused submission, and optional per-Loop runtime controls |

Harness must not import ACP or know Claude Code/Codex configuration formats.
ACP packages must not own CodeRig's role catalogue. CodeRig compiles one frozen
catalogue into both gateway routes and Harness Subagent runtime profiles so the
advertised choices cannot drift from executable choices.

## Agent Roster

### Planner

The planner investigates before execution. Its system prompt emphasizes:

- repository and architecture exploration;
- external research when repository evidence is insufficient;
- decomposition into bounded, independently verifiable work;
- explicit assumptions and evidence-backed synthesis;
- delegation to planner, builder, or reviewer when useful;
- no workspace mutation.

Its native tool posture is read/research oriented: ReadFile, Glob, Grep,
read-only terminal commands, WebSearch, Fetch, Task, AskUser, optional Skill,
and the managed Subagent tool. Its access gate must reject workspace mutation.

### Builder

The builder owns implementation. Its system prompt emphasizes:

- locating and fixing the root cause;
- making focused edits that fit surrounding code;
- executing commands and debugging failures;
- using research only when local evidence is insufficient;
- testing from narrow checks to broader verification;
- delegating investigation or review while retaining end-to-end ownership.

Its native tool posture includes ReadFile, WriteFile, EditFile, Glob, Grep,
terminal execution, WebSearch, Fetch, Task, AskUser, optional Skill, and the
managed Subagent tool.

### Reviewer

The reviewer independently checks work and reports findings without editing. Its
system prompt emphasizes:

- correctness, security, compatibility, and maintainability review;
- targeted tests, builds, static checks, and terminal inspection;
- validation of claimed behavior and failure cases;
- prioritized findings with concrete file/symbol evidence;
- no workspace mutation.

Its native tool posture includes ReadFile, Glob, Grep, read-only/test terminal
execution, Task, AskUser, optional Skill, and the managed Subagent tool. It does
not receive WriteFile or EditFile, and its access profile must enforce the same
boundary for terminal commands.

### Topology and recursion

CodeRig passes all three definitions to `rig.WithPrimers` and selects `builder`
with `rig.WithActivePrimer`. Each definition declares all three agent names as
legal delegates and uses managed delegation, so the Subagent tool is
structurally injected rather than manually wired.

The session's existing depth and total-spawn quotas remain hard backstops.
CodeRig initially retains the current bounded, shallow delegation policy unless
an implementation task deliberately changes the tested policy. A definition's
presence in both primer and delegate catalogues does not create a global agent
registry and does not bypass the parent-scoped delegate check.

## Native Primer Models

All three primer Loops use CodeRig's normal Harness backend and LM Studio
client. The session opens with:

- default: `deepseek-v4-flash`, resolved at startup to the loaded
  DeepSeek-V4-Flash-0731 server identifier;
- alternative: `gemma-4-31b`, resolved at startup to the loaded Gemma 4 31B IT
  server identifier.

The provider's exact served ID is not hard-coded from a model-card repository
name. CodeRig queries LM Studio's model endpoint, matches an explicit configured
alias to one unique loaded model, validates capabilities/context, and fails
startup on absence or ambiguity. DeepSeek Flash is the default for new primer
Loops; Gemma is selectable per primer through the existing `/model` runtime
control.

Local models initially advertise only `none` effort unless the loaded model,
Inference client, and request codec can truthfully enforce another effort. The
catalogue never presents decorative effort choices that collapse to the same
request.

ACP-backed primary Loops are intentionally deferred. The generic adapter seam
must not prevent that later extension, but CodeRig does not launch its three
primers through ACP in this release.

## Credential Modes

An ACP child reaches models one of two ways, fixed per child at start and
pinned for its lifetime.

**`native-auth`** — the child runs on the agent harness's own account: Claude
Code with its Claude subscription/login, Codex with its ChatGPT/Codex login.
The harness talks to its vendor directly with its own client identity, so:

- no gateway is started, bound, or borrowed for that child;
- the selectable models are exactly that harness's own catalogue, and only
  its vendor's models — a Claude Code child cannot reach an OpenAI model this
  way, and vice versa;
- effort is expressible only through what the connector itself advertises
  (the ACP thought-level config option, a Codex reasoning-effort launch
  setting); where the connector cannot express it, effort is not advertised
  for that tuple, per the Harness design's admission rule;
- the gateway-side guarantees do not apply: no target-authoritative effort,
  no strict alias enforcement, no gateway usage attribution.

**`gateway-backed`** — the child is pointed at its own loopback gateway and
CodeRig's own credentials serve the traffic. Any provider CodeRig holds
direct API access to may be a target: Anthropic, OpenAI, OpenRouter,
Bedrock, Phala, LM Studio, or another provider the Inference module has a
client for. Because the gateway translates dialects, a gateway-backed target
is reachable from either ACP ingress — this is where the cross-dialect matrix
lives (Codex ingress onto an Anthropic target, Claude Code ingress onto an
OpenAI-format target). All gateway guarantees apply: strict resolution,
target-authoritative effort, per-child isolation.

The distinction is credential ownership, not vendor. An Anthropic model
reached with CodeRig's own Anthropic API key is `gateway-backed` and
available through both harnesses; the same vendor's models reached through a
Claude subscription are `native-auth` and available only through Claude Code.

**Explicitly out of scope:** CodeRig never replays a harness's subscription
credential through the gateway, and never impersonates a first-party client
(forged user-agents, private beta headers, or any other cloaking) to make
subscription-bound models look like API traffic. Those techniques violate the
providers' subscription terms, break on every upstream client release, and
put the user's account at risk. A subscription model is reachable only by
running its own harness in `native-auth` mode. A combination that would
require impersonation is simply not admitted.

## ACP Harness Catalogue

CodeRig preflights these ACP connectors:

- `claude-code`, speaking the gateway's Anthropic-compatible ingress;
- `codex`, speaking the gateway's OpenAI Responses-compatible ingress.

Each child gets one ACP process/session. The selected harness, model alias, and
effort are fixed before the first child prompt and remain pinned for that child
Loop. Sibling children may use different tuples while sharing the session
workspace.

The initial CodeRig composition gives each **gateway-backed** child an owned
loopback gateway server with a fixed target. This is intentional: the gateway can authoritatively
enforce that child's selected effort even when an ACP harness sends its own
default, and no shared `(ingress, alias)` route can confuse two simultaneous
children that selected the same model with different efforts. `acp/launch`
already owns the corresponding start-before-child and close-after-child
lifecycle. A future shared multiplexed server may replace this optimization only
after its route identity includes the complete runtime profile. A
`native-auth` child gets no gateway at all: its connector is configured
without a proxy binding and runs on the harness's own login state, which
`acp/launch` must support as an explicit mode rather than requiring a
binding.

An unavailable connector is not advertised. If no ACP connector is available,
CodeRig can still run its native primers, but a Subagent start fails with a
bounded capability error because CodeRig registers no native delegated-child
profile.

CodeRig admits the following stable model aliases:

These are the **gateway-backed** aliases, admitted only where CodeRig holds
that provider's credential; each is reachable from both ACP harnesses:

| Alias | Display model | Private provider | Allowed efforts |
|---|---|---|---|
| `fable-5` | Claude Fable 5 | Anthropic | `low`, `medium`, `high`, `max` |
| `sonnet-5` | Claude Sonnet 5 | Anthropic | `low`, `medium`, `high`, `max` |
| `opus-5` | Claude Opus 5 | Anthropic | `low`, `medium`, `high`, `max` |
| `gpt-5.6-sol` | GPT-5.6 Sol | OpenAI | `none`, `low`, `medium`, `high`, `max` |
| `gpt-5.6-terra` | GPT-5.6 Terra | OpenAI | `none`, `low`, `medium`, `high`, `max` |
| `gpt-5.6-luna` | GPT-5.6 Luna | OpenAI | `none`, `low`, `medium`, `high`, `max` |
| `deepseek-v4-flash` | DeepSeek V4 Flash 0731 | LM Studio | capability-derived; initially `none` |
| `gemma-4-31b` | Gemma 4 31B IT | LM Studio | capability-derived; initially `none` |

The provider column is private routing metadata and is deliberately
open-ended: an operator holding OpenRouter, Bedrock, Phala, or another
provider credential registers those targets the same way, and they become
admissible aliases with no change to the model-facing surface. The Inference
module having a working client for that provider is the only gate.

A `native-auth` child instead selects from its own harness's model catalogue.
Those entries are single-harness by construction, carry no gateway target,
and advertise effort only where the connector expresses it. CodeRig
discovers them from the connector rather than hard-coding a vendor list, and
a harness with no usable login contributes no native-auth entries.

`ultra` and `xhigh` are deliberately excluded: `xhigh` is not a valid
`model.Effort`, and extending the closed neutral vocabulary is cross-repo
`inference/model` + codec work outside this design. These tables may widen to
`xhigh` only as explicit follow-up work after that vocabulary lands. Each
target's effective effort list is the intersection of model and gateway
support; ACP runtime support is not a factor because effort binds
gateway-side.

Claude Code additionally requires a small/fast model (`ClaudeModels.Small`).
CodeRig fixes it product-wide to `sonnet-5` at default effort. It is not
model-selectable: a claude-code child's owned gateway materializes the small
target alongside the selected main target, the catalogue validates it like
any admitted alias, and it is recorded in the child's durable runtime
identity.

Every **gateway-backed** target can be reached from both ACP ingress formats,
because CodeRig's own credential — not the harness's identity — serves it.
Conceptually the frozen catalogue compiles these routes, while each
child-owned fixed gateway materializes only its selected target:

```text
(Anthropic ingress, alias)        -> private target client + target API format
(OpenAI Responses ingress, alias) -> private target client + target API format
```

Native-auth models are not in this matrix at all: they are reachable only
from their own harness, and the catalogue admits them as single-harness
entries. So the full cross product exists only over the gateway-backed
aliases, and a deployment with no API credentials collapses to the diagonal —
Claude Code on its own models, Codex on its own. It is never a claim that the
providers are interchangeable. The gateway performs translation only when
ingress and target formats differ. For example, Sonnet through Claude Code is a
same-dialect gateway route, while Sonnet through Codex translates Responses to
Anthropic Messages. Conversely, an OpenAI model through Claude Code translates
Anthropic ingress to the OpenAI target format.

The target API format comes from the selected target descriptor, never from the
chosen ACP harness. `provider` therefore never becomes `claude-code` or `codex`.
Those are harness aliases; `anthropic`, `openai`, and `lmstudio` are private
target provenance.

The selected normalized effort is part of the immutable ACP runtime profile
and is enforced solely by the child's fixed gateway target through the
target-authoritative effort step (the named gateway work item in the Harness
design). No connector expresses effort. The gateway overwrites only the
neutral request's effort after ingress decode and before target
validation/encoding; other harness sampling inputs remain intact.
An explicit `none` therefore remains distinguishable from an omitted Subagent
effort even though internal `model.EffortNone` is the empty value. A tuple is admitted
only when the resulting target request is observably different and matches the
selection; unsupported or lossy mappings fail catalogue construction.

## Curated Profiles

CodeRig presents small role-oriented recommendations without restricting the
full allowed catalogue:

| Profile | Agent | Default | Alternate |
|---|---|---|---|
| `intelligence` | planner | Fable 5 / `medium` | GPT-5.6 Sol / `high` |
| `build` | builder | Sonnet 5 / `high` | GPT-5.6 Luna / `max` |
| `review` | reviewer | Opus 5 / `medium` | GPT-5.6 Terra / `high` |

These are product defaults and convenient presets, not providers and not ACP
harnesses. Harness selection remains independent: either recommended target may
run through Claude Code or Codex. An explicit Subagent selection may choose any
other admitted alias/effort combination compatible with the role.

The parent-scoped catalogue gives each role one deterministic default harness,
model, and effort. CodeRig may choose one ACP harness as the product default only
after connector preflight; omission never silently switches to another harness
after child creation.

## Subagent Surface

CodeRig consumes Harness's approved Subagent envelope. A child start selects:

```json
{
  "description": "Review the restore path",
  "prompt": "Inspect restore behavior and report correctness risks.",
  "subagent_type": "reviewer",
  "agent_harness": "claude-code",
  "model": "opus-5",
  "effort": "medium",
  "run_in_background": true
}
```

`subagent_type` is one of `planner`, `builder`, or `reviewer`.
`agent_harness`, `model`, and `effort` are optional request fields but appear in
the model-facing schema only when the parent has a genuine corresponding ACP
choice. Omission resolves deterministic role defaults. CodeRig does not expose
provider names, API formats, URLs, credentials, executable paths, or gateway
tokens through the tool.

The same `start`, `send`, `wait`, `interrupt`, and `status` control surface
manages ACP children. Follow-up sends reuse the child's pinned runtime tuple.
All controller authorization, ownership, request correlation, durability, and
depth/quota checks remain Harness responsibilities.

## ACP Tool and Access Semantics

Harness's managed Subagent tool controls the parent. It is independent from the
execution tools inside Claude Code or Codex.

An ACP child receives its role instructions and a CodeRig access posture mapped
to the chosen harness's known configuration. CodeRig admits a role/harness
combination only if the harness can enforce the role's required boundary:

- planner: research/read exploration without workspace mutation;
- builder: workspace edits and terminal execution under the session's gates;
- reviewer: reads and test/check execution without workspace mutation.

ACP is optional in the ecosystem but real when configured: CodeRig passes the
genuine harness parameters that the connector supports. Harness does not
pretend all native tool definitions have been exported into the ACP child. A
combination that cannot preserve required tool or access semantics is omitted
instead of being advertised optimistically.

Direct Claude/Codex CLI Loop integrations are not added to CodeRig. If legacy
configuration for such a path is found during implementation, remove it after
tests prove ACP parity. Process launching belongs only to `acp/launch`; child
Loop adaptation belongs only to `foreignloops/driver/acp`, and CodeRig never
constructs the direct-CLI foreignloops drivers.

Permission posture follows the Harness design's translation contract: the
role's access posture maps to the neutral posture vocabulary on the
foreignloops driver contract; the ACP driver applies it per connector (Claude
Code permission mode, Codex sandbox/approval posture) before the first prompt
and always registers the `session/request_permission` handler, denying
anything outside the posture. This release is policy-only — ACP children have
no interactive approval path. Note the deliberate asymmetry: CodeRig's native
Loops get full sandbox/gate enforcement, while ACP children are posture-only;
a role whose boundary the chosen harness posture cannot enforce is simply not
admitted for that harness.

## TUI Agent Switching

The TUI already renders Loop conversations in the footer, supports pointer
selection, and cycles focus with `Ctrl+N` / `Ctrl+P`. The composer already sends
to the focused Loop through `SubmitToLoop`.

CodeRig/TUI integration changes that existing surface rather than adding a new
one:

- keep all three primer Loops visible in the footer while idle;
- initialize focus and active selection to `builder`;
- selecting a primer focuses it and calls the optional active-primer capability,
  which delegates to `SessionController.SetActiveLoop`;
- selecting a delegated child remains focus-only and does not replace the
  active primer;
- preserve each primer's transcript, model, effort, mode, and running state;
- allow an old primer's turn to continue in the background after switching;
- route subsequent default input and active-loop status to the newly selected
  primer.

The TUI identifies primers from its existing `LoopStarted` runtime projection;
it does not require a second agent registry. The setter is a small optional
capability so third-party/single-loop agents continue to compile and behave as
today. There is no `/agent` slash command in this iteration.

## Restore and Durability

The active primer is already durable through `ActiveLoopChanged`. Restore must
recreate all three native primers, recover the saved active primer, and preserve
each Loop's independent runtime state.

ACP child durability records only secret-free stable identity:

- role/agent name;
- agent-harness alias;
- credential mode (`native-auth` or `gateway-backed`);
- model alias (and the fixed small-model alias for claude-code children);
- normalized effort;
- opaque runtime-profile name needed by the injected adapter builder;
- the agent-assigned ACP session identifier (`ACPSessionID`), journaled once
  known — it is the resume key.

Restore resolves that identity through the current frozen CodeRig catalogue,
starts a fresh ACP process, and resumes the child's own agent-side session
via `session/load` with the journaled `ACPSessionID` when the adapter
advertises the load capability. A failed or unavailable resume is never
session-fatal: that child restores as a closed tombstone Loop with a typed
restore incompatibility (per the Harness design's per-child degraded
restore), while the primers and sibling children restore normally. Missing
harnesses, routes, aliases, or incompatible configuration tombstone that
child explicitly rather than falling back silently. Journals
never contain provider credentials, gateway tokens, executable paths, raw
environment, or target URLs.

Catalogue, role prompt, model descriptor, access posture, and gateway-route
changes participate in existing configuration/policy fingerprints. A mismatch
does not silently reinterpret a prior child.

## Failure Behavior

- Missing LM Studio default/alternative model: fail CodeRig startup with the
  unmatched alias and discovered non-secret IDs.
- Missing ACP executable or failed ACP preflight: omit that harness from new
  child choices; fail startup only if policy requires at least one child harness.
- No ACP profiles available: primers remain usable; Subagent start reports that
  no delegated runtime is available.
- Unsupported model/effort/harness tuple: reject during Subagent preparation and
  revalidate in the controller.
- Missing gateway route or codec: fail catalogue construction before a child can
  be advertised.
- Missing or expired native-auth login for a harness: contribute no
  native-auth entries for it and report a bounded capability error if a child
  start names one; never fall back to a gateway-backed target, and never
  attempt to reuse that login's credential outside its own harness.
- Provider credential absent for a gateway-backed alias: omit that alias from
  the catalogue rather than registering an unusable route.
- ACP launch/session failure: return a bounded model-safe Subagent error and
  release the process/proxy resources.
- Restore identity cannot resolve or resume fails: restore that child as a
  closed tombstone with a typed incompatibility rather than falling back to a
  different harness, model, or effort; the session and its other Loops still
  restore.

## Verification

Tests must prove at least:

- exactly three agent definitions exist and replace operator identities;
- all three are primers and delegates, with `builder` active initially;
- native planner/reviewer cannot mutate and builder can use its execution tools;
- DeepSeek Flash is the native default and Gemma is selectable when discovered;
- no OpenRouter or direct Claude/Codex child path is constructed;
- both ACP harnesses can select every admitted gateway-backed model alias;
- native-auth entries are single-harness: a native-auth alias offered by one
  harness is never advertised or startable under the other;
- a composition with no provider credentials still yields working
  native-auth children, and one with no harness logins still yields working
  gateway-backed children;
- no request path ever sends a harness's own credential to the gateway, and
  no forged client identity headers are constructed anywhere;
- same-dialect and cross-dialect gateway routes select the correct target API
  format;
- `none` survives schema, preparation, runtime resolution, gateway codec,
  events, and restore where supported, and stays distinguishable from an
  omitted effort;
- `xhigh` and `ultra` are rejected and never advertised;
- curated profiles resolve to the approved default/alternate pairs without
  hiding the full catalogue;
- sibling children can run different harness/model/effort tuples concurrently;
- the Subagent schema omits ACP selectors when no ACP profile exists;
- all three idle primers remain visible in the footer;
- selecting a primer changes focus and active loop, while selecting a child
  changes focus only;
- switching primers preserves independent conversations and permits background
  work to finish;
- restored sessions recover the active primer and secret-free ACP child identity;
- module dependency tests preserve Harness/ACP/CodeRig ownership boundaries.

Run focused tests and full race suites in each changed Go module. Exercise at
least one real Claude Code ACP route, one real Codex ACP route, one cross-provider
translation in each direction, and both LM Studio primer aliases before release.

## External Model References

- Anthropic model overview: <https://platform.claude.com/docs/en/about-claude/models/overview>
- Anthropic effort controls: <https://platform.claude.com/docs/en/build-with-claude/effort>
- OpenAI model catalogue: <https://developers.openai.com/api/docs/models>
- DeepSeek API models: <https://api-docs.deepseek.com/quick_start/pricing/>
- DeepSeek V4 Flash weights: <https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash>
- Google Gemma 4 collection: <https://huggingface.co/collections/google/gemma-4>
- LM Studio Gemma 4 31B build: <https://huggingface.co/lmstudio-community/gemma-4-31B-it-GGUF>
