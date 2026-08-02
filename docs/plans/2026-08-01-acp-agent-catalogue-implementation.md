# CodeRig ACP Agent Catalogue Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build CodeRig's three-agent native-LM-Studio primer roster and ACP-only delegated-child catalogue, allowing Claude Code or Codex to run every admitted direct-provider/local model at an exact per-child effort.

**Architecture:** Harness owns protocol-neutral Subagent selection and child lifecycle; `acp/loop` adapts ACP sessions to Harness backends; `acp/launch` owns safe Claude Code/Codex process configuration; Inference supplies exact effort translation plus an owned fixed-target gateway per child; CodeRig owns roles, targets, curated defaults, and composition. The TUI reuses its existing loop footer and adds only an optional active-primer setter.

**Tech Stack:** Go 1.26.5, root Go workspace, Harness Loop/Rig/session APIs, Inference codecs and gateway, ACP client/launch, LM Studio's OpenAI-compatible API, and Bubble Tea TUI.

---

## Execution rules

Approved design:

- `coderig/docs/plans/2026-08-01-acp-agent-catalogue-design.md`

Generic Subagent design and detailed Harness tasks:

- `harness/docs/plans/2026-07-31-subagent-tool-parity-design.md`
- `harness/docs/plans/2026-07-31-subagent-tool-parity-implementation.md`

Use @superpowers:using-git-worktrees before implementation and
@superpowers:test-driven-development for every behavior change. The Harness,
CodeRig, Tools, and Sandbox work must start from the committed
`feat/long-running-commands` heads, or from branches that already contain them:

```text
harness  aaefa01c
coderig  0b9b020
tools    a182af7
sandbox  9bca5d4
```

Do not copy the old main-branch raw-JSON Subagent execution boundary into new
code. The prepared artifact produced by `PrepareCall` is authoritative.

The root `/Users/ipotter/code/looprig/go.work` is the only local cross-module
resolution mechanism. Do not add temporary pseudo-versions, branch versions,
or local `replace` directives to component `go.mod` files. If feature worktrees
are used, replace the affected `use` paths in the root `go.work` with those
worktree paths and remove the corresponding main-checkout paths so a module
path occurs exactly once. Restore canonical paths after integration.

Preserve all pre-existing dirty files, especially:

- `harness/go.mod`
- `harness/go.sum`
- `inference/transport/client.go`
- `llm/vendor/github.com/looprig/inference/transport/client.go`

Task/Todo work is already in progress/landed and is out of scope. Do not rename,
redesign, or reformat it while implementing this plan.

Each task ends with a suggested local commit checkpoint because the planning
workflow expects small reversible commits. Do not perform a commit, tag, module
version adoption, or push until the user explicitly authorizes it.

### Task 1: Validate the development workspace and feature bases

**Files:**

- Verify: `/Users/ipotter/code/looprig/go.work`
- Verify: `harness/docs/plans/2026-07-31-subagent-tool-parity-*.md`
- Verify: the four long-running-command worktrees listed above

**Step 1: Verify the workspace parses**

Run:

```bash
cd /Users/ipotter/code/looprig
go work edit -json
```

Expected: one `Use` entry for every managed module and no `Replace` entries.

**Step 2: Verify the prepared-call bases**

Run one ancestry check per affected repository after creating the feature
branches:

```bash
git -C <feature-harness> merge-base --is-ancestor aaefa01c HEAD
git -C <feature-coderig> merge-base --is-ancestor 0b9b020 HEAD
git -C <feature-tools> merge-base --is-ancestor a182af7 HEAD
git -C <feature-sandbox> merge-base --is-ancestor 9bca5d4 HEAD
```

Expected: all commands exit 0.

**Step 3: Point the root workspace at feature worktrees**

Use `go work edit -dropuse` for each affected main checkout and `-use` for its
feature worktree. Do not run `go work sync`; it may rewrite component
`go.mod`/`go.sum` files.

**Step 4: Record clean/dirty baselines**

Run `git status --short` in root, Inference, LLM, Harness, ACP, TUI, Tools,
Sandbox, and CodeRig. Save the output in the execution notes; do not clean or
reset any repository.

### Task 2: Extend the neutral effort contract without lossy clamping

**Files:**

- Modify: `inference/model/effort.go`
- Modify: `inference/model/effort_test.go`
- Modify: `inference/codec/anthropicapi/encode.go`
- Modify: `inference/codec/anthropicapi/encode_test.go`
- Modify: `inference/codec/anthropicapi/server_decode.go`
- Modify: `inference/codec/anthropicapi/server_decode_test.go`
- Modify: `inference/codec/openairesponses/encode.go`
- Modify: `inference/codec/openairesponses/encode_test.go`
- Modify: `inference/codec/openairesponses/server_decode.go`
- Modify: `inference/codec/openairesponses/server_decode_test.go`
- Modify only if supported by the native wire: corresponding OpenAI Chat and Gemini effort tests/codecs

**Step 1: Write the failing closed-enum tests**

Pin this internal vocabulary:

```go
const (
    EffortNone   Effort = ""
    EffortLow    Effort = "low"
    EffortMedium Effort = "medium"
    EffortHigh   Effort = "high"
    EffortXHigh  Effort = "xhigh"
    EffortMax    Effort = "max"
)
```

Test that `EffortXHigh.Valid()` is true and `Effort("ultra").Valid()` is
false. Keep `EffortNone` as the internal zero value; external `none` mapping
belongs at catalogue/serialization boundaries.

**Step 2: Run the model test and observe failure**

```bash
cd /Users/ipotter/code/looprig/inference
go test -race ./model -run TestEffort
```

Expected: FAIL because `EffortXHigh` is not defined/valid.

**Step 3: Implement the enum addition**

Add only `EffortXHigh` and include it in `Valid`. Do not add `ultra`.

**Step 4: Write failing Anthropic round-trip tests**

Test request encode and server decode for `xhigh`. Verify `none`/zero omits
`thinking` and `output_config.effort`, and `ultra` is rejected by ingress
decode. Preserve `max` as `max`.

**Step 5: Implement Anthropic exact mapping**

Add `xhigh` to `effortValue` and `parseEffort`. Do not clamp it to `high`.

**Step 6: Write failing Responses round-trip tests**

Cover `none`, `low`, `medium`, `high`, `xhigh`, and `max` exactly. Explicitly
replace the old assertion that `max` clamps to `high`.

**Step 7: Implement Responses exact mapping**

Map all supported values verbatim. Unknown values remain fail-closed/omitted on
egress and rejected on ingress.

**Step 8: Keep dialect capability truthful**

Do not extend OpenAI Chat or Gemini merely to make a common test green. If their
native API cannot represent `xhigh`/`max` exactly, keep the codec limited and
ensure the CodeRig target catalogue never routes an exact-effort profile through
that egress format.

**Step 9: Verify**

```bash
go test -race ./model ./codec/anthropicapi ./codec/openairesponses
go test -race ./codec/...
```

Expected: PASS.

**Checkpoint:** `feat(inference): add exact xhigh effort translation`

### Task 3: Let a fixed gateway target enforce the selected effort

**Files:**

- Modify: `inference/gateway/target.go`
- Modify: `inference/gateway/resolver.go`
- Modify: `inference/gateway/handler.go`
- Modify: `inference/gateway/handler_test.go`
- Modify: `inference/gateway/matrix_test.go`
- Modify: `inference/gateway/mux_test.go`

**Step 1: Write failing target validation/clone tests**

Add an optional, presence-preserving field:

```go
type Target struct {
    ID             string
    Client         inference.Client
    Model          model.Model
    EnforcedEffort *model.Effort
}
```

Tests must prove:

- `nil` means preserve the harness request;
- a pointer to `model.EffortNone` means explicitly force no effort;
- `xhigh` and `max` validate;
- `ultra` rejects `NewMux`/`Fixed` construction;
- resolver returns deep-copy the pointer.

**Step 2: Run the gateway tests and observe failure**

```bash
go test -race ./gateway -run 'Test(Target|Mux|Fixed).*Effort'
```

Expected: FAIL because `EnforcedEffort` is absent.

**Step 3: Implement validation and cloning**

Copy the effort value into fresh storage in every Target clone. Never retain a
caller pointer.

**Step 4: Write failing handler precedence tests**

Drive real Anthropic and Responses ingress requests into a fake target client.
Assert:

```text
target nil + incoming high     => high
target none + incoming high    => none
target xhigh + incoming low    => xhigh
target max + incoming omitted  => max
```

Also prove temperature, top-p, max tokens, and stop values from the harness are
unchanged.

**Step 5: Implement narrow enforcement**

After route resolution/model replacement and before feature validation, clone
or create `decoded.Request.Override` and replace only its `Effort` when
`target.EnforcedEffort != nil`.

**Step 6: Verify both ingress-to-egress directions**

Add matrix cases for Anthropic ingress to Responses egress and Responses ingress
to Anthropic egress at `xhigh`; add an explicit-none case. Assert the upstream
JSON, not merely the neutral fake request.

**Step 7: Verify**

```bash
go test -race ./gateway ./codec/anthropicapi ./codec/openairesponses
```

Expected: PASS.

**Checkpoint:** `feat(gateway): enforce fixed-target effort`

### Task 4: Add direct Anthropic/OpenAI targets and bounded LM Studio discovery

**Files:**

- Modify: `llm/llm.go`
- Modify: `llm/provider.go`
- Modify: `llm/provider_test.go`
- Modify: `llm/validate_test.go`
- Create: `llm/providers/anthropic/client.go`
- Create: `llm/providers/anthropic/client_test.go`
- Create: `llm/providers/openai/client.go`
- Create: `llm/providers/openai/client_test.go`
- Create: `llm/providers/lmstudio/catalog.go`
- Create: `llm/providers/lmstudio/catalog_test.go`
- Modify: `llm/auto/auto.go`
- Modify: `llm/auto/auto_test.go`
- Modify: `llm/auto/counter.go` and tests if context policy needs the new providers
- Modify: `llm/internal` dependency tests as required

**Step 1: Write failing provider-policy tests**

Add `ProviderAnthropic = "anthropic"` and `ProviderOpenAI = "openai"` to the
known-provider policy. Pin:

- Anthropic: API-key auth, Anthropic format only, canonical
  `https://api.anthropic.com/v1` endpoint;
- OpenAI: API-key auth, OpenAI Responses format for the new GPT targets,
  canonical `https://api.openai.com/v1` endpoint;
- provider identity remains independent of ACP harness identity;
- OpenRouter is not involved in either construction.

**Step 2: Implement provider constants/policy**

Update every closed provider switch. Unknown providers must continue failing
closed.

**Step 3: Write failing direct-client wire tests**

Using `httptest.Server`, require:

- Anthropic `POST /v1/messages`, `x-api-key`, the pinned
  `anthropic-version` header, no bearer header, and Anthropic codec body;
- OpenAI `POST /v1/responses`, bearer authorization, and Responses codec
  body;
- credentials never appear in errors or model descriptors.

**Step 4: Implement provider clients**

Compose `inference/transport.Client` with the existing codecs/routes. Keep
provider-specific headers inside the provider package. Do not add a generic
header bag to `model.Model`.

**Step 5: Wire `auto.New`**

Dispatch the two new providers to their constructors after the existing
fail-closed auth check. Do not change OpenRouter behavior; CodeRig simply does
not select it.

**Step 6: Write failing LM Studio catalogue tests**

Build a bounded GET client for `<base>/models`. Test:

- loopback HTTP only;
- context cancellation and a finite client timeout;
- response body and model-count bounds;
- exact configured alias matching against returned IDs;
- zero or multiple matches return typed errors;
- discovered IDs are diagnostic but secrets/responses are not logged.

The resolver API should be small and injectable, for example:

```go
type Catalog interface {
    Resolve(context.Context, Alias) (string, error)
}
```

**Step 7: Implement LM Studio discovery**

Do not fuzzy-match arbitrary substrings. Put alias-to-accepted-ID patterns in
CodeRig; keep this package responsible for safe list/fetch/exact resolution.

**Step 8: Verify**

```bash
cd /Users/ipotter/code/looprig/llm
go test -race ./providers/anthropic ./providers/openai ./providers/lmstudio ./auto ./...
```

Expected: PASS under the root workspace; no `go.mod` change.

**Checkpoint:** `feat(llm): add direct cloud targets and LM Studio discovery`

### Task 5: Execute the generic Harness Subagent runtime-selection plan

**Files:**

- Follow exact Harness files in Tasks 1-8 and 12 of
  `harness/docs/plans/2026-07-31-subagent-tool-parity-implementation.md`
- Modify: `harness/docs/plans/2026-07-31-subagent-tool-parity-design.md`
- Modify: `harness/docs/plans/2026-07-31-subagent-tool-parity-implementation.md`

**Step 1: Reconfirm the hard-cut API tests**

Before implementation, tests must require `description`, `prompt`, and
`subagent_type`; remove legacy `agent`, `message`, and `wait`; preserve managed
`start|send|wait|interrupt|status`.

**Step 2: Implement Tasks 1-3 from the Harness plan**

Add secret-free runtime selector/catalogue values, the parent-scoped resolver,
`EngineAdapter`, stable runtime profile identity, and sealed bind-time
engine/model/effort overrides.

External effort parsing must be presence-aware:

```text
omitted  -> use catalogue default
"none"   -> model.EffortNone with EffortSet=true
"xhigh"  -> model.EffortXHigh
""       -> reject
"ultra"  -> reject
```

**Step 3: Implement Tasks 4-5 from the Harness plan against prepared calls**

`PrepareCall` performs the one untrusted JSON decode, validates the complete
branch and runtime tuple, and stores a typed prepared artifact. `InvokableRun`
reads only `loop.PreparedCallFromContext`; it must never decode `argsJSON`
again.

**Step 4: Implement Tasks 6-7**

Revalidate authorization in the parent-scoped controller, pin the resolved
harness/model/effort/profile on the child, and persist/restore only secret-free
identity. Omitted/defaulted values must be persisted explicitly after
resolution.

**Step 5: Implement Task 8**

Return stable JSON results and bounded model-safe errors. Canary tests must
prove executable paths, tokens, URLs, provider bodies, prompts, descriptions,
and raw UUIDs do not leak.

**Step 6: Preserve ACP optionality**

Run native-only tests proving a Harness consumer that registers no adapter
profiles has no ACP-related fields in Subagent schema/description and links no
ACP package.

**Step 7: Verify focused and full Harness suites**

```bash
cd <feature-harness>
go test -race ./pkg/tool ./pkg/loop ./pkg/rig
go test -race ./internal/delegationtool ./internal/sessionruntime
go test -race ./...
```

Expected: PASS using the root workspace. Do not modify the pre-existing dirty
`go.mod`/`go.sum` as part of this task.

**Checkpoint sequence:** use the individual Harness plan's Task 1-8 checkpoint
commits when authorized; do not squash until review.

### Task 6: Make ACP connectors carry model, effort, and role posture truthfully

**Files:**

- Modify: `acp/launch/contracts.go` only if a small common configuration contract is needed
- Modify: `acp/launch/claudecode.go`
- Modify: `acp/launch/claudecode_test.go`
- Modify: `acp/launch/claude_connector.go`
- Modify: `acp/launch/claude_connector_test.go`
- Modify: `acp/launch/codex.go`
- Modify: `acp/launch/codex_test.go`
- Modify: `acp/launch/codex_connector.go`
- Modify: `acp/launch/codex_connector_test.go`
- Modify: `acp/launch/errors.go`
- Modify: `acp/docs/connectors/inference-gateway.md`

**Step 1: Write failing effort-token tests**

Define one validated ACP-facing effort value with external tokens
`none|low|medium|high|xhigh|max`. Reject empty, whitespace, control characters,
TOML fragments, environment fragments, and `ultra` before command/session
construction.

**Step 2: Write failing Claude connector tests**

Pin:

- `ANTHROPIC_MODEL` and `ANTHROPIC_SMALL_FAST_MODEL` receive validated
  gateway-facing aliases when configured;
- a model option is selected only from the session's advertised values;
- an advertised `thought_level` select option receives the exact effort when
  available;
- `none` means no thought-level selection, not "use high";
- the owned gateway remains the authority if the adapter lacks a thought-level
  selector;
- existing absolute-path, environment allowlist, `CLAUDECODE` rejection, and
  no-ambient-credential tests remain green.

**Step 3: Implement Claude configuration**

Keep executable configuration in `Configure` and session option application in
the session-level connector. Do not invent ACP RPCs.

**Step 4: Write failing Codex connector tests**

Require deterministic, safely encoded launch overrides for model and
`model_reasoning_effort`. Preserve posture fields and the fixed custom gateway
provider. A changed tuple requires a new connector/process/session.

**Step 5: Implement Codex configuration**

Never call the unreliable `session/set_model` extension. Validate before
building argv; do not interpolate unchecked strings.

**Step 6: Verify**

```bash
cd /Users/ipotter/code/looprig/acp
go test -race ./launch ./client ./protocol ./transport/stdio
```

Expected: PASS.

**Checkpoint:** `feat(acp): configure exact model and effort per harness`

### Task 7: Implement `acp/loop` as the only ACP-to-Harness adapter

**Files:**

- Create: `acp/loop/config.go`
- Create: `acp/loop/builder.go`
- Create: `acp/loop/backend.go`
- Create: `acp/loop/session.go`
- Create: `acp/loop/updates.go`
- Create: `acp/loop/restore.go`
- Create: `acp/loop/errors.go`
- Create: `acp/loop/*_test.go`
- Modify: `acp/internal/boundary/deps_test.go`
- Modify: `acp/CLAUDE.md`
- Create: `acp/loop/README.md`

**Step 1: Write compile-time boundary tests**

Pin that `acp/loop.Backend` implements `harness/pkg/loop.Backend` and that its
live/restore builders satisfy Harness's generic adapter builder contracts. Permit
Harness/Inference imports only in `acp/loop`; keep `acp/client`, `protocol`,
`transport`, and `launch` free of Harness imports.

**Step 2: Define the injected configuration**

The package receives no provider secret or CodeRig catalogue. Use narrow
consumer-owned factories, conceptually:

```go
type GatewayFactory interface {
    NewGateway(context.Context, loop.RuntimeProfileName) (launch.ModelProxy, error)
}

type ConnectorFactory interface {
    Connector(loop.RuntimeProfileName) (launch.HarnessAdapter, SessionConfigurer, error)
}
```

The exact public types should follow the generic profile values landed in
Harness. A profile resolves to one owned gateway plus one connector; it never
contains raw executable paths or keys in durable values.

**Step 3: Write lifecycle tests**

Prove order and unwind:

```text
resolve profile -> construct gateway -> launch.Dial (starts gateway) ->
session/new -> apply model/mode/effort -> first prompt
```

Every failure closes created resources exactly once. Close cancels the ACP
prompt/process before closing the gateway. Race close, cancellation, and child
death.

**Step 4: Implement live backend construction**

Translate Harness user blocks to ACP prompt blocks, ACP session updates to
Harness chunks/tool-call observations, terminal stop reasons to Loop terminal
events, and `Interrupt` to ACP cancel. Preserve ordered single-turn behavior and
bounded queues.

**Step 5: Implement restore construction**

Restore starts a fresh ACP process/session from the persisted runtime profile
and reconstructs the child conversation through the Harness-provided folded
messages. Do not attempt to resume a provider/CLI session ID. The Harness journal
is authoritative.

**Step 6: Pin tool ownership**

ACP children use their harness's execution tools. `ReplaceExternalTools` and
native Harness tool injection must fail structurally for adapter-backed loops.
The parent Harness Loop alone owns the managed Subagent tool.

**Step 7: Add fuzz and sanitization tests**

Fuzz malformed ACP updates. Ensure adapter errors presented to the model contain
only bounded categories, not argv, environment, token, provider response, or
prompt content.

**Step 8: Verify**

```bash
go test -race ./loop ./launch ./client
go test -race ./...
```

Expected: PASS with the root workspace and no temporary `go.mod` pin.

**Checkpoint:** `feat(acp): adapt ACP sessions to Harness loops`

### Task 8: Build CodeRig's immutable model and runtime catalogue

**Files:**

- Create: `coderig/internal/app/model_catalog.go`
- Create: `coderig/internal/app/model_catalog_test.go`
- Create: `coderig/internal/app/acp_catalog.go`
- Create: `coderig/internal/app/acp_catalog_test.go`
- Create: `coderig/internal/app/acp_runtime.go`
- Create: `coderig/internal/app/acp_runtime_test.go`
- Create: `coderig/internal/app/runtime_dependencies.go`
- Create: `coderig/internal/app/runtime_dependencies_test.go`
- Modify: `coderig/internal/app/config.go`
- Modify: `coderig/internal/app/models.go`
- Modify: `coderig/internal/app/models_test.go`
- Modify: `coderig/internal/app/model.go`
- Modify: `coderig/internal/app/model_test.go`
- Modify: `coderig/internal/app/dependency_test.go`

**Step 1: Write failing alias/target tests**

Pin exactly these stable aliases and private target descriptors:

```text
fable-5, sonnet-5, opus-5
gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna
deepseek-v4-flash, gemma-4-31b
```

Assert private providers are Anthropic/OpenAI/LM Studio, never `claude-code`,
`codex`, or OpenRouter. Exact provider IDs, endpoints, and clients must not be
part of model-facing/durable catalogue projections.

**Step 2: Implement cloud descriptors and clients**

Construct direct first-party targets with the LLM provider packages. Load
`ANTHROPIC_API_KEY` and `OPENAI_API_KEY` only in the process composition
boundary; bind them to clients immediately and never store them in `Config`,
models, fingerprints, errors, or child environments.

**Step 3: Write failing LM Studio startup-discovery tests**

Given a fake model list, resolve DeepSeek-V4-Flash-0731 and Gemma 4 31B IT to
one exact served ID each. DeepSeek must be the native default. Zero/ambiguous
matches fail with typed bounded errors.

**Step 4: Implement native model set**

Replace Kimi/Chutes default construction. Build one LM Studio client and two
secret-free descriptors from the discovered exact IDs. Advertise only `none`
effort until exact local effort support is proven.

**Step 5: Write failing Cartesian-product tests**

For every available ACP harness, assert all eight model aliases are present.
Cloud effort sets are exactly the approved lists, local effort sets are
capability-derived, and `ultra` never appears. Missing credentials/models remove
only the affected targets; missing executable preflight removes only the
affected harness.

**Step 6: Write failing curated-profile tests**

Pin:

```text
intelligence: fable-5/medium default, gpt-5.6-sol/high alternate
build:        sonnet-5/high default, gpt-5.6-luna/max alternate
review:       opus-5/medium default, gpt-5.6-terra/high alternate
```

Profiles are recommendations/defaults, not restrictions. Explicit selection can
still reach every admitted model through either harness.

**Step 7: Implement one catalogue compiler**

From one immutable input, generate:

- Harness parent-scoped runtime options;
- secret-private factories for `acp/loop` profiles;
- fixed gateway Targets with `EnforcedEffort`;
- fingerprint-safe catalogue identity;
- runtime-control model choices for native primers.

Reject duplicate aliases, defaults, runtime profile names, route identities, and
unsupported effort intersections during construction.

**Step 8: Verify**

```bash
cd <feature-coderig>
go test -race ./internal/app -run 'Test(ModelCatalog|ACPCatalog|RuntimeDependencies|CuratedProfiles|LMStudio)'
```

Expected: PASS.

**Checkpoint:** `feat(coderig): add native and ACP runtime catalogues`

### Task 9: Replace the operator roster with planner, builder, and reviewer

**Files:**

- Delete: `coderig/internal/catalog/operator/operator.go`
- Delete: obsolete operator tests
- Create: `coderig/internal/catalog/planner/planner.go`
- Create: `coderig/internal/catalog/planner/planner_test.go`
- Create: `coderig/internal/catalog/builder/builder.go`
- Create: `coderig/internal/catalog/builder/builder_test.go`
- Modify: `coderig/internal/catalog/reviewer/reviewer.go`
- Modify: `coderig/internal/catalog/reviewer/reviewer_test.go`
- Modify: `coderig/internal/catalog/identity.go`
- Modify: `coderig/internal/app/toolsets.go`
- Modify: `coderig/internal/app/toolsets_test.go`
- Modify: `coderig/internal/app/access.go`
- Modify: `coderig/internal/app/access_test.go`
- Rewrite: `coderig/internal/app/swarm.go`
- Rewrite: `coderig/internal/app/swarm_test.go`
- Modify: CodeRig tests referring to `operator-primary` or `operator`

**Step 1: Write failing prompt identity tests**

Require exactly `planner`, `builder`, and `reviewer`; XML fragments must be
well formed and include the approved mission/boundary terms. Delete expectations
for `operator-primary` and `operator`.

**Step 2: Implement the three role prompts**

Keep shared identity guidance in `catalog.Identity`. Keep role-specific behavior
in each package. State tool/capability truthfully and include shared delegation
guidance without claiming a child can exceed session depth/quota limits.

**Step 3: Write failing tool-roster/access tests**

Pin:

- planner: read/glob/grep, read-only terminal, WebSearch, Fetch, Task, AskUser,
  optional Skill, managed Subagent; no Write/Edit;
- builder: full read/write/edit/search/terminal/web/Task/AskUser/Skill plus
  managed Subagent;
- reviewer: read/glob/grep and test/check terminal, Task, AskUser, optional
  Skill, managed Subagent; no Write/Edit;
- planner/reviewer terminal access cannot mutate through the sandbox profile.

**Step 4: Implement cohesive role toolsets/access**

Reuse standard tool definitions and existing executor/gate architecture. Do not
introduce a generic agent registry or a new policy-translation bridge.

**Step 5: Write failing topology tests**

Require three definitions, each with all three delegate names and managed
delegation. Keep the existing explicit depth/quota safety policy unless a
separate test-approved decision changes it.

**Step 6: Implement definition assembly**

Build each definition once with native LM Studio inference and a portable
runtime profile resolver for ACP children. Apply CodeRig context policy and
fingerprints consistently to all three.

**Step 7: Verify**

```bash
go test -race ./internal/catalog/... ./internal/app -run 'Test(Role|Tool|Access|Swarm|Topology)'
```

Expected: PASS.

**Checkpoint:** `feat(coderig): replace operator roster with three agents`

### Task 10: Assemble three primers and ACP-only delegated children

**Files:**

- Modify: `coderig/internal/app/persistence.go`
- Modify: `coderig/internal/app/persistence_test.go`
- Modify: `coderig/internal/app/persistence_integration_test.go`
- Modify: `coderig/internal/app/managed_delegation_test.go`
- Modify: `coderig/internal/app/rig_restore_integration_test.go`
- Modify: `coderig/internal/app/swarm.go`
- Modify: `coderig/internal/app/runtime_controls.go`
- Modify: `coderig/internal/app/runtime_controls_test.go`
- Modify: `coderig/internal/app/fingerprint_test.go`
- Modify: `coderig/internal/app/errors.go` or create focused ACP/model error files

**Step 1: Write failing primer assembly tests**

Require:

```go
rig.WithPrimers("planner", "builder", "reviewer")
rig.WithActivePrimer("builder")
```

Assert all three `LoopStarted` events are roots and `ActiveLoopID` is builder.

**Step 2: Implement primer assembly**

Replace operator constants in rig options, agent kind, greeting, persistence,
and tests. Do not create primer/leaf duplicate definitions.

**Step 3: Write failing ACP-only delegation tests**

With fake connectors/gateways, start two sibling children:

```text
planner -> claude-code / fable-5 / medium
reviewer -> codex / gpt-5.6-terra / high
```

Assert `EngineAdapter`, distinct runtime profiles, distinct owned gateway/ACP
lifecycles, common workspace placement, and no native child fallback. Repeat with
cross-provider routes and explicit `none`/`xhigh`.

**Step 4: Wire generic resolver and adapter builders**

Pass the compiled parent-scoped catalogue and `acp/loop` live/restore builders
through Rig options. No CodeRig production import may reference Foreignloops or
direct Claude/Codex drivers. Existing Harness compatibility package names may
appear only at the generic seam until renamed upstream.

**Step 5: Update native runtime controls**

For primer Loops, `/model` lists DeepSeek default and Gemma alternative, and
`/effort` lists only enforceable local values. Adapter-backed children expose
their pinned tuple as read-only unless Harness later supports an exact
process-restart mutation contract.

**Step 6: Pin restore/fingerprint behavior**

Restore all primers, active builder/selected primer, independent native runtime
state, and ACP child secret-free profile identity. Missing current profile fails
restore; no harness/model/effort fallback.

**Step 7: Verify**

```bash
go test -race ./internal/app -run 'Test(Primer|ManagedDelegation|RuntimeCatalog|Restore|Fingerprint)'
```

Expected: PASS.

**Checkpoint:** `feat(coderig): compose ACP-only delegated agents`

### Task 11: Reuse the TUI footer as the primer switcher

**Files:**

- Modify: `tui/internal/presentation/agent.go`
- Modify: `tui/api.go`
- Modify: `tui/internal/presentation/commands.go`
- Modify: `tui/internal/presentation/screen.go`
- Modify: `tui/internal/presentation/screen_test.go`
- Modify: `tui/internal/presentation/loopbar_test.go`
- Modify: `tui/sessionadapter/adapter.go`
- Modify: `tui/sessionadapter/adapter_test.go`
- Modify: `coderig/internal/app/runtime_controls.go`
- Modify: `coderig/internal/app/runtime_controls_test.go`

**Step 1: Write failing optional-capability tests**

Add a narrow optional interface, not a method on the mandatory `tui.Agent`:

```go
type ActivePrimerController interface {
    SetActivePrimer(context.Context, uuid.UUID) error
}
```

The session adapter delegates to `SessionController.SetActiveLoop`. Test timeout,
error propagation, and no call for an unknown/non-primer Loop.

**Step 2: Write failing footer visibility tests**

Feed three root `LoopStarted` events followed by `LoopIdle`. Assert planner,
builder, and reviewer remain visible while unrelated idle child loops retain the
existing hide policy. Use `runtimeProjection.loop(id).primer`; do not create a
second registry.

**Step 3: Implement primer-aware bar entries**

Extend the assembled `loopBarEntry` with a primer bit or pass primer IDs into
the filter. Preserve stable creation order, cap priority, hit testing, and
current focus glyphs.

**Step 4: Write failing selection tests**

For both pointer click and `Ctrl+N`/`Ctrl+P`:

- selecting a primer changes focus and asynchronously requests active-primer
  change;
- selecting a child changes focus only;
- an unavailable optional capability retains today's focus-only behavior;
- another primer's running turn continues in the background;
- `ActiveLoopChanged` remains the event-authoritative state update and does not
  cause a second setter call.

**Step 5: Implement one selection path**

Have click and keyboard call the same `selectLoop` function. That function
updates focus immediately and returns a bounded setter command only for a
primer. Report setter failure as a non-secret notice without rolling back focus.

**Step 6: Keep slash commands unchanged**

Assert `/agent` is absent. Do not add a new tray or duplicate Mode/Model/Effort
controls.

**Step 7: Verify**

```bash
cd /Users/ipotter/code/looprig/tui
go test -race ./internal/presentation ./sessionadapter ./...
```

Expected: PASS.

**Checkpoint:** `feat(tui): switch primers through the loop footer`

### Task 12: Update CodeRig CLI/configuration boundaries and remove stale integrations

**Files:**

- Modify: `coderig/cmd/coderig/main.go`
- Modify: `coderig/cmd/coderig/main_test.go`
- Modify: `coderig/internal/app/config.go`
- Modify: `coderig/internal/app/model.go`
- Modify: `coderig/internal/app/dependency_test.go`
- Modify: `coderig/CLAUDE.md`
- Modify: `coderig/CONTRIBUTING.md`
- Modify: current CodeRig README/specs that describe operator/Kimi/catalogue policy

**Step 1: Write failing configuration-boundary tests**

Pin environment/flags for LM Studio base and absolute ACP executable paths. The
ACP harnesses are optional capabilities; malformed configured paths fail startup,
while omitted paths simply omit that harness. Cloud credentials omit their
targets when absent unless an explicitly required profile depends on them.

**Step 2: Implement process-level dependency construction**

Read environment once, validate it, bind secrets to provider clients, run
bounded connector preflight, and return secret-free runtime dependencies to
session assembly. Never forward `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` to ACP
children; they receive only the local gateway token.

**Step 3: Add dependency guards**

Production CodeRig must not import:

```text
github.com/looprig/foreignloops
direct Claude/Codex driver packages
OpenRouter constructors
```

It may import `github.com/looprig/acp/loop`, ACP launch config types at the
composition boundary, Inference gateway, and the direct provider constructors.

**Step 4: Update contributor guidance**

Replace stale statements that forbid the now-approved fixed model/runtime
catalogue or describe operator-only topology. Preserve the new SOLID and root
`go.work` rules verbatim.

**Step 5: Verify stale names**

```bash
rg -n 'operator-primary|Kimi-K2\.6|OpenRouter|foreignloops|/agent' \
  internal cmd CLAUDE.md CONTRIBUTING.md README.md docs --glob '!docs/plans/2026-0[67]-*'
```

Expected: only intentional historical/negative-test references.

**Checkpoint:** `refactor(coderig): finalize ACP-only runtime composition`

### Task 13: Cross-module acceptance and live connector verification

**Files:**

- Add integration tests to the owning modules above
- Add cross-repository tests to `tests/` only if behavior cannot be proven in one owner
- Modify: relevant Makefiles only for existing test targets; do not add dependency pins

**Step 1: Run formatting and diff checks**

```bash
gofmt -w <changed-go-files>
git diff --check
```

Run `git diff --check` separately in every changed repository and root.

**Step 2: Run focused race suites in dependency order**

```text
Inference -> LLM -> Harness -> ACP -> TUI -> CodeRig -> tests
```

Run with the root workspace active. Expected: PASS with no `go.mod`/`go.sum`
changes caused by local dependency coordination.

**Step 3: Run full module checks**

From each changed module:

```bash
go test -race ./...
make lint
make secure
```

Use only targets that exist. Preserve and report unrelated pre-existing
failures; do not fix them opportunistically.

**Step 4: Run real local-model checks**

With LM Studio explicitly started and the two models loaded:

- discovery uniquely resolves DeepSeek-V4-Flash-0731;
- discovery uniquely resolves Gemma 4 31B IT;
- builder primer defaults to DeepSeek;
- switching one primer to Gemma affects only that Loop;
- all three primer conversations persist independently.

If LM Studio is unavailable, report this live check as unexecuted; unit tests do
not pretend it passed.

**Step 5: Run real ACP checks**

With explicitly configured absolute connector paths and credentials, exercise:

- Claude Code -> Anthropic Fable/Sonnet/Opus (same dialect);
- Codex -> OpenAI Sol/Terra/Luna (same dialect);
- Claude Code -> OpenAI target (cross dialect);
- Codex -> Anthropic target (cross dialect);
- both harnesses -> LM Studio target;
- exact `none`, `xhigh`, and `max` enforcement where catalogued;
- concurrent siblings with different tuples;
- cancel, process death, restore, and cleanup.

Capture only secret-free model/harness aliases and outcome categories.

**Step 6: Audit dependency/version state**

```bash
git status --short
git -C inference status --short
git -C llm status --short
git -C harness status --short
git -C acp status --short
git -C tui status --short
git -C coderig status --short
```

Confirm no temporary `go.mod` pins/replaces and no Task/Todo edits.

**Step 7: Request code review**

Use @superpowers:requesting-code-review. Review SOLID boundaries, lifecycle
ownership, exact-effort truthfulness, secret handling, restore determinism,
and TUI focus/active separation.

### Task 14: Release/adoption phase (separately authorized)

**Files:**

- Modify component `go.mod`/`go.sum` only after provider modules are published
- Modify vendor trees only in consuming release commits
- Modify: root `repositories.mk` after published tags exist

**Step 1: Stop for authorization**

Do not tag, push, or change released sibling versions during implementation.
Ask the user before this task.

**Step 2: Release bottom-up**

When authorized, publish/adopt in dependency order:

```text
Inference -> LLM -> Harness -> ACP -> TUI -> CodeRig -> tests/root inventory
```

Only here should consumers replace their published sibling versions and refresh
vendor trees. Verify each consumer with `GOWORK=off` after its dependencies are
published.

**Step 3: Restore canonical root workspace paths**

Point `go.work` back to the normal component directories, validate with
`go work edit -json`, and ensure no duplicate module paths remain.

**Step 4: Final verification**

Use @superpowers:verification-before-completion. Report exact test commands,
live checks, skipped checks, commit IDs, published versions, and final token-free
status output.
