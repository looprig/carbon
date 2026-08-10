# Global Model Configuration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace Carbon's frozen production model rows and fixed credential environment variables with one securely loaded machine-wide `~/.looprig/models.json`, while preserving the existing three-agent roster, permission-based native sandbox, source-aware ACP gateway/native isolation, durability, and TUI behavior.

**Architecture:** Carbon reads and validates the global file once at the process-composition boundary, converts it into secret-free model descriptors plus credential-bound provider clients, and compiles the existing immutable primer and source-aware ACP runtime dependencies. Raw API keys never leave the loader/compiler. Native permission files remain per-workspace under `~/.looprig/workspaces/<hash>/permissions.json`; gateway and native ACP children remain non-interactive and posture-only, with separate proxy and harness-login environments.

**Tech Stack:** Go 1.26.x, standard-library `encoding/json`, `io`, and `os`, root `go.work`, Carbon `internal/app`, Inference model/gateway, LLM provider validation/client factory, Harness runtime catalogue, ACP launch/driver, Tools permission store, Sandbox profiles.

---

## Scope and Non-Goals

This is a delta plan from the current Carbon `main` checkout. The following are
already implemented and must not be rebuilt:

- the `planner`, `builder`, and `reviewer` roster;
- builder as the active primer;
- managed Subagent runtime selection;
- ACP Claude Code/Codex child builders, gateway routes, and native-profile seams;
- native sandbox profiles, gates, executor sets, permission persistence, and
  planner/reviewer read-only restrictions;
- TUI footer switching and runtime-control surfaces;
- child restore/tombstone behavior.

This plan does not implement OAuth, harness subscription-login discovery or
login configuration,
Bedrock SigV4, Phala attestation policy, interactive ACP approvals, live config
reload, a config-writing CLI, or multiple switchable native primer clients.

The first implementation supports exactly:

- API-key providers constructible through `llm/auto.New`;
- no-auth providers constructible through `llm/auto.New`, such as LM Studio;
- one `primer_default` shared by all three native primers;
- any number of delegate-capable gateway models;
- optional enabled native ACP profiles, with omitted models treated as
  harness-managed and explicit non-empty aliases preflighted independently;
- configured role-specific delegated defaults;
- one configured Claude Code small-model alias for gateway-backed Claude
  profiles.

If a requested provider needs a credential type other than API key or none,
return a typed configuration error. Do not invent a fallback or silently omit
the row.

## Execution Rules

Use @superpowers:test-driven-development for every behavior change and
@superpowers:verification-before-completion before every commit.

Use the current canonical module checkouts listed by
`/Users/ipotter/code/looprig/go.work`. Do not create feature-base ancestry checks
against the obsolete hashes in the superseded plan. Do not edit component
`go.mod`/`go.sum`, add `replace` directives, run `go work sync`, or pin sibling
pseudo-versions.

Before every task:

1. Run `git status --short` in Carbon and every module the task touches.
2. Preserve all pre-existing user changes.
3. Add only task-owned files to a checkpoint commit.
4. Never print, log, or place a real `api_key` in a test fixture. Use obvious
   sentinels such as `test-secret-do-not-log`.

The user must explicitly authorize implementation commits. The commit commands
below are checkpoint suggestions, not standing authorization.

## Canonical JSON Contract

Implementation and tests must use this exact schema vocabulary:

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
      "api_key": "test-secret-do-not-log",
      "uses": ["primer", "delegate"],
      "capabilities": {"tools": true, "thinking": true},
      "efforts": ["none", "low", "medium", "high", "max"],
      "default_effort": "high"
    }
  ]
}
```

Do not add aliases for the old fixed model names. The file is authoritative.

---

### Task 1: Establish a Clean Baseline and Pin Current Behavior

**Files:**

- Verify: `/Users/ipotter/code/looprig/go.work`
- Verify: `carbon/internal/app/acpcatalog.go`
- Verify: `carbon/internal/app/acpproduction.go`
- Verify: `carbon/internal/app/model.go`
- Verify: `carbon/internal/app/permissions.go`

**Step 1: Record repository state**

Run:

```bash
cd /Users/ipotter/code/looprig/carbon
git status --short
git log --oneline -5
cd /Users/ipotter/code/looprig
go work edit -json
```

Expected: commands succeed. Save the status output in execution notes. Do not
clean, stash, reset, or rewrite unrelated work.

**Step 2: Confirm the old production boundaries**

Run:

```bash
cd /Users/ipotter/code/looprig/carbon
rg -n 'frozenACPGatewayDefinitions|ANTHROPIC_API_KEY|OPENAI_API_KEY|defaultModel =|CLAUDE_CODE_ACP_NATIVE_MODELS|CODEX_ACP_NATIVE_MODELS' internal/app
```

Expected before implementation: matches in `acpcatalog.go`,
`acpproduction.go`, and `model.go`. Record them; later tasks must remove the
production matches.

**Step 3: Run the focused baseline**

Run:

```bash
GOWORK=off go test -race ./internal/app -run 'Test(CompileACPCatalog|ProductionACP|DefaultPermissionsPath|Access)'
```

If `GOWORK=off` cannot resolve the already-published component versions, rerun
without `GOWORK=off` and record why. Expected: PASS before edits.

No commit for this task.

---

### Task 2: Securely Locate and Read the Global Model File

**Files:**

- Create: `carbon/internal/app/modelconfig.go`
- Create: `carbon/internal/app/modelconfig_test.go`

**Step 1: Write path-resolution tests**

Add table-driven tests for a helper with the effective contract:

```go
func defaultModelConfigPath() (string, error)
```

Tests must override the process home using the same safe test mechanism already
used by `permissions_test.go`. Assert the result is exactly:

```text
<home>/.looprig/models.json
```

Also assert this path does not contain `workspaces` and differs from
`defaultPermissionsPath(workspace)`.

**Step 2: Run the test and prove red**

```bash
go test ./internal/app -run 'TestDefaultModelConfigPath' -count=1
```

Expected: compile failure because the helper does not exist.

**Step 3: Implement path resolution**

Use `os.UserHomeDir()` and `filepath.Join(home, ".looprig", "models.json")`.
Wrap home lookup failure in a typed Carbon configuration error. Do not read
`HOME` directly, expand `~`, or fall back to the current directory.

**Step 4: Write secure-open tests**

Define:

```go
const maxModelConfigBytes = 1 << 20

func readModelConfigFile(path string) ([]byte, bool, error)
```

The boolean means the file exists. Tests must cover:

- absent file returns `(nil, false, nil)`;
- regular `0600` file returns exact bytes and `true`;
- directory rejected;
- final-path symlink rejected;
- named pipe/non-regular file rejected where supported;
- file larger than 1 MiB rejected without unbounded allocation;
- Unix mode `0640`, `0604`, and `0666` rejected;
- Unix mode `0600` accepted;
- read errors are wrapped without including file contents.

Skip only the permission-bit cases on platforms that do not expose Unix mode
semantics. Do not skip ordinary file-type/size behavior.

**Step 5: Implement secure open in this exact order**

1. `os.Lstat(path)`.
2. Return absent only for `errors.Is(err, fs.ErrNotExist)`.
3. Reject `ModeSymlink` and every non-regular type.
4. On Unix, reject `mode.Perm() & 0077 != 0`.
5. `os.Open(path)` without modifying it.
6. Call `file.Stat()` and require both results to be regular and
   `os.SameFile(lstat, stat)`; this detects a final-path replacement race.
7. Read through `io.LimitReader(file, maxModelConfigBytes+1)`.
8. Reject when the result exceeds the limit.
9. Close the descriptor on every path.

Do not create directories, chmod the file, follow a symlink, or write defaults.

**Step 6: Verify green**

```bash
go test -race ./internal/app -run 'Test(DefaultModelConfigPath|ReadModelConfigFile)' -count=1
```

Expected: PASS.

**Step 7: Checkpoint**

```bash
git add internal/app/modelconfig.go internal/app/modelconfig_test.go
git commit -m "feat(app): securely load global model configuration"
```

---

### Task 3: Strictly Decode and Validate the Schema

**Files:**

- Modify: `carbon/internal/app/modelconfig.go`
- Modify: `carbon/internal/app/modelconfig_test.go`

**Step 1: Add private wire types**

Keep every type unexported so no raw credential can enter `app.Config`:

```go
type modelConfigFile struct {
    Version              int                              `json:"version"`
    PrimerDefault        string                           `json:"primer_default"`
    ClaudeCodeSmallModel string                           `json:"claude_code_small_model"`
    DelegateDefaults     map[string]delegateDefaultConfig `json:"delegate_defaults"`
    Models               []modelTargetConfig              `json:"models"`
}

type delegateDefaultConfig struct {
    Harness string `json:"harness"`
    Model   string `json:"model"`
    Effort  string `json:"effort"`
}

type modelTargetConfig struct {
    Alias         string                  `json:"alias"`
    Provider      string                  `json:"provider"`
    APIFormat     string                  `json:"api_format"`
    BaseURL       string                  `json:"base_url"`
    Model         string                  `json:"model"`
    APIKey        string                  `json:"api_key"`
    Uses          []string                `json:"uses"`
    Capabilities  modelCapabilitiesConfig `json:"capabilities"`
    Efforts       []string                `json:"efforts"`
    DefaultEffort string                  `json:"default_effort"`
}
```

Capabilities initially contain only `tools`, `thinking`, `images`,
`prompt_caching`, `structured_output`, and `structured_output_with_tools`.
Unknown capability fields must fail strict decoding.

**Step 2: Write strict JSON tests**

Test:

- empty input;
- malformed JSON;
- two top-level JSON values;
- unknown top-level, row, default, and capability fields;
- duplicate object keys at every nesting depth;
- version missing, zero, or not `1`;
- invalid UTF-8;
- a minimal valid no-auth LM Studio file;
- a valid API-key file.

Go's ordinary `encoding/json` accepts duplicate keys. Add a token pre-pass,
`rejectDuplicateJSONKeys`, that recursively tracks keys for each object. It must
walk arrays and nested objects and return a bounded error containing the key
name but not its value. Then decode again with `json.Decoder`, call
`DisallowUnknownFields`, and require EOF after the first value.

**Step 3: Prove red, implement, and prove green**

```bash
go test ./internal/app -run 'TestDecodeModelConfig' -count=1
```

Expected before implementation: FAIL. Implement the token pre-pass and strict
typed decode. Rerun; expected: PASS.

**Step 4: Write semantic validation tests**

Use one valid fixture and mutate one field per table row. Require rejection of:

- empty/whitespace-padded aliases, provider, format, model, use, or default;
- duplicate aliases;
- empty or duplicate `uses`;
- use other than `primer` or `delegate`;
- no models;
- missing or non-primer `primer_default`;
- missing tools capability for any used Carbon model;
- empty, duplicate, invalid, `xhigh`, or `ultra` efforts;
- default effort absent from the effort list;
- non-`none` effort without thinking capability;
- structurally invalid or insecure base URL;
- unsupported provider/format combination;
- missing API key for API-key provider;
- API key supplied to a no-auth provider;
- provider requiring special credentials unsupported by this schema;
- delegate default role other than planner/builder/reviewer;
- missing role default;
- invalid harness other than codex/claude-code;
- default model not delegate-capable;
- default effort not admitted by that model;
- Claude default without a valid delegate-capable small model;
- configured Claude small model that cannot support tools.

Require exactly one default for all three roles. Do not silently synthesize a
role default from array order.

**Step 5: Implement normalization**

Define a private normalized representation that contains the secret-bearing
client-construction input but provides a separate explicit secret-free
projection. Sort models by alias, uses lexically, efforts by the neutral effort
order, and delegate defaults by fixed role order. Reject duplicates before
sorting so duplicates cannot disappear.

Construct `model.Model` using `model.CustomModel` and capability options. Call
both structural model validation and the LLM provider truth-table validation.
Never clamp or reinterpret effort.

**Step 6: Add secret-free digest tests**

Assert:

- reordering JSON models/uses/efforts does not change the digest;
- changing alias/provider/format/base/model/use/capability/effort/default does;
- changing API-key bytes alone does not change the digest;
- changing credential presence changes admission and therefore the digest or
  validation result;
- the digest and every formatted normalized value exclude the sentinel secret.

Use a dedicated secret-free struct for `json.Marshal`; never marshal the wire
config and never hash an API key.

**Step 7: Verify and checkpoint**

```bash
go test -race ./internal/app -run 'Test(Decode|Validate|Normalize|Digest)ModelConfig' -count=1
git add internal/app/modelconfig.go internal/app/modelconfig_test.go
git commit -m "feat(app): validate global model configuration"
```

Expected test result: PASS.

---

### Task 4: Construct Credential-Bound Clients and Runtime Sources

**Files:**

- Create: `carbon/internal/app/productionmodels.go`
- Create: `carbon/internal/app/productionmodels_test.go`
- Modify: `carbon/internal/app/acpcatalog.go`
- Modify: `carbon/internal/app/acpcatalog_test.go`

**Step 1: Define the one-way compilation result**

Create a private result containing no raw configuration object:

```go
type productionModels struct {
    PrimerClient inference.Client
    PrimerModel  model.Model
    ACP           []ACPGatewaySource
    Defaults      map[identity.AgentName]configuredDelegateDefault
    ClaudeSmall   loop.ModelAlias
    ConfigRev     string
}
```

Do not add an API-key field, the decoded wire type, or file bytes.

**Step 2: Write client-construction tests**

Inject a narrow client factory:

```go
type configuredClientFactory func(model.Model, auth.APIKey) (inference.Client, error)
```

Tests must prove:

- factory called once per configured row with the exact descriptor and key;
- no-auth row passes an empty key only when provider auth is none;
- construction failure names only the row alias/provider;
- returned primer client/model match `primer_default`;
- only delegate-capable rows become `ACPGatewaySource` values;
- source efforts/defaults are exact;
- no result or `%v`, `%+v`, `%#v` formatted error contains the sentinel key.

Production uses `auto.New`. Tests use fakes and perform no network I/O.

**Step 3: Replace the frozen ACP input**

Delete `frozenACPGatewayDefinitions` and the branch that expands a provider
client into fixed aliases. Change catalogue input so production supplies
already validated gateway sources. A suitable shape is:

```go
type ACPCatalogInput struct {
    SubagentTypes []identity.AgentName
    GatewayTargets []ACPGatewaySource
    Defaults map[identity.AgentName]configuredDelegateDefault
    ClaudeSmall loop.ModelAlias
}
```

If tests still need native-auth types for lower-level compatibility, keep them
out of the production loader and do not advertise them in production. Prefer
deleting dead production discovery rather than maintaining two sources of
truth.

**Step 4: Make defaults configuration-driven**

Remove `markACPDefaults`'s implicit Claude-first behavior and model-array-order
fallback. Each role default must resolve the configured harness/model/effort.
Compilation fails when executable preflight later removes the configured
default harness; it must not silently choose another harness.

For Claude entries, use the configured `ClaudeSmall` alias. Remove the fixed
`sonnet-5` requirement.

**Step 5: Red/green catalogue tests**

Replace six-alias tests with arbitrary aliases such as `fixture-a` and
`fixture-b`. Assert:

- every configured delegate target is selectable through both harnesses before
  executable preflight;
- configured role defaults resolve exactly;
- Claude small-model alias resolves exactly;
- same alias plus different effort yields distinct concrete target aliases;
- duplicate aliases/defaults remain rejected;
- `GatewayTarget` returns the exact client and authoritative effort;
- no old alias exists unless the fixture configured it.

Run:

```bash
go test -race ./internal/app -run 'Test(ProductionModels|CompileACPCatalog|ACPGateway)' -count=1
```

Expected: PASS.

**Step 6: Checkpoint**

```bash
git add internal/app/productionmodels.go internal/app/productionmodels_test.go internal/app/acpcatalog.go internal/app/acpcatalog_test.go
git commit -m "feat(app): compile configured model targets"
```

---

### Task 5: Wire One Loaded Configuration into Primers and ACP Production

**Files:**

- Modify: `carbon/internal/app/config.go`
- Modify: `carbon/internal/app/model.go`
- Modify: `carbon/internal/app/model_test.go`
- Modify: `carbon/internal/app/acpproduction.go`
- Modify: `carbon/internal/app/acpproduction_test.go`
- Modify: `carbon/internal/app/persistence.go`
- Modify: `carbon/internal/app/persistence_test.go`
- Modify: `carbon/internal/app/swarm.go`
- Modify: `carbon/internal/app/swarm_test.go`

**Step 1: Add a single process-composition loader**

Create:

```go
func loadProductionModels() (productionModels, error)
```

It resolves the global path, securely reads, strictly decodes, validates,
normalizes, constructs clients, and returns the secret-free runtime result.

Do not call it from tests that inject a client. Do not load the file once for
primers and again for ACP. Production `New` and the persisted session factory
must load exactly once per top-level open operation and pass the same result to
both paths.

**Step 2: Remove the hard-coded primer**

Delete production dependence on `defaultModel = chutesKimiK26()` and
`LLM_API_KEY`. Keep helper model constructors only where unrelated tests still
need them; otherwise remove them with their stale tests.

Change production construction to pass `PrimerClient` and `PrimerModel` into
the existing definition assembly. All three primers use this same configured
default in this release.

`LoopRuntimeOptions` must advertise only the current primer model because the
runtime cannot yet switch inference clients safely. Do not claim every
`uses:["primer"]` row is selectable. A future multi-client runtime change can
widen that surface.

**Step 3: Replace ACP environment credential construction**

Delete reads of `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`, and delete
`productionACPClient`. Build the ACP composition from the already compiled
`productionModels.ACP`, role defaults, and Claude small alias.

Continue to read only ACP executable path configuration from its existing
process boundary. Provider API keys remain bound inside clients and must not be
added to either native or gateway ACP child environment allowlists.

**Step 4: Wire configured native ACP profiles**

Remove `CLAUDE_CODE_ACP_NATIVE_MODELS` and `CODEX_ACP_NATIVE_MODELS` as model
configuration sources. Instead, compile the optional `native_acp` profiles from
`models.json`: an absent or disabled profile contributes no native route, an
enabled profile with omitted `models` is harness-managed, and an explicit
non-empty list is an alias allowlist. Native defaults set `source: "native"` and
must name an explicit alias when the profile is constrained.

Preflight gateway routes through the loopback proxy and native routes through
`DialNative` with no proxy. Keep native login/process variables in the native
allowlist only; provider keys and model-selection environment variables never
enter either child environment. Update tests so one failed native alias does
not remove other ready aliases.

**Step 5: Fold the secret-free digest into durability**

Add `ModelConfigRev string` to `app.Config` only if the digest cannot flow
directly through the existing runtime-catalogue revision. It must contain the
secret-free digest, never file bytes or credentials.

Tests must prove:

- changing a non-secret model field rejects restore through the existing config
  mismatch mechanism;
- changing only API-key bytes retains the same fingerprint;
- missing configured target on restore never falls back;
- loading/configuration failure happens before persistence is opened or mutated.

**Step 6: Verify focused production assembly**

```bash
go test -race ./internal/app -run 'Test(ModelConfig|ProductionModels|ProductionACP|NewWithClient|Swarm|Fingerprint|Restore)' -count=1
```

Expected: PASS.

**Step 7: Checkpoint**

```bash
git add internal/app/config.go internal/app/model.go internal/app/model_test.go internal/app/acpproduction.go internal/app/acpproduction_test.go internal/app/persistence.go internal/app/persistence_test.go internal/app/swarm.go internal/app/swarm_test.go
git commit -m "feat(app): load production models from global config"
```

---

### Task 6: Prove Permission and Secret Boundaries End to End

**Files:**

- Modify: `carbon/internal/app/access_acceptance_test.go`
- Modify: `carbon/internal/app/acpchildren_test.go`
- Modify: `carbon/internal/app/acpchildren_task31_test.go`
- Modify: `carbon/internal/app/subagent_e2e_test.go`
- Modify: `carbon/internal/app/fingerprint_test.go`
- Modify: `carbon/internal/app/dependency_test.go`
- Modify only if a defect is found: `carbon/internal/app/permissions.go`
- Modify only if a defect is found: `carbon/internal/app/toolsets.go`

**Step 1: Pin filesystem separation**

Add an acceptance test asserting:

```text
model config: <home>/.looprig/models.json
permissions:  <home>/.looprig/workspaces/<sha256(canonical-root)>/permissions.json
```

Assert the paths are distinct. Assert loading models neither creates nor
modifies the permission file. Assert storing an interactive approval neither
creates nor modifies `models.json`.

**Step 2: Pin native permission behavior**

Reuse the assembled session access path, not isolated helper-only tests. Prove:

- builder still reaches the current gate/store/executor grant path;
- planner and reviewer remain read-only under readonly, trusted, and
  unconfined selected profiles;
- persisted workspace approvals still load from the hashed workspace path;
- headless mode still loads only its explicit absolute read-only permission
  file;
- no model field changes any access profile or permission decision.

Do not redesign or relocate `permissions.json`.

**Step 3: Pin ACP posture-only behavior**

Prove ACP child environment contains only allowed process values and its
ephemeral loopback gateway binding. It must contain none of:

- sentinel provider key;
- `models.json` path;
- raw model configuration JSON;
- native permission file path/content;
- `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or obsolete native-model variables.

Prove `session/request_permission` continues to deny outside the configured
role posture and never writes a workspace permission rule.

**Step 4: Add a broad redaction test**

Use one sentinel key and drive it through loading, normalization, client factory
failure, catalogue compilation, gateway construction, ACP preflight failure,
formatted errors (`%v`, `%+v`, `%#v`), fingerprint fields, and durable event
serialization fixtures. Search every captured byte slice/string and fail if the
sentinel occurs.

Do not test by logging the real decoded struct; that would create the leak the
test is meant to prevent.

**Step 5: Verify**

```bash
go test -race ./internal/app -run 'Test(.*Permission.*|.*Secret.*|.*Redact.*|.*ACP.*Environment.*|.*Fingerprint.*)' -count=1
```

Expected: PASS.

**Step 6: Checkpoint**

```bash
git add internal/app/access_acceptance_test.go internal/app/acpchildren_test.go internal/app/acpchildren_task31_test.go internal/app/subagent_e2e_test.go internal/app/fingerprint_test.go internal/app/dependency_test.go
git commit -m "test(app): prove model and permission boundaries"
```

Add `permissions.go` or `toolsets.go` to the commit only if a failing acceptance
test exposed a real defect and the minimal fix belongs there.

---

### Task 7: Remove Stale Catalogue Assumptions and Document Operations

**Files:**

- Modify: `carbon/CLAUDE.md`
- Modify: `carbon/CONTRIBUTING.md`
- Modify: current Carbon user documentation that describes Kimi, fixed ACP
  aliases, or credential environment variables
- Modify: `carbon/.gitignore` only if documentation tooling creates local test
  fixtures inside the repository (production config is outside it and needs no
  ignore rule)

**Step 1: Update contributor architecture**

Document:

- role policy remains fixed in code;
- global model data lives at `~/.looprig/models.json`;
- inline keys are permitted only because the file is outside repositories and
  owner-only;
- the loader never writes the file;
- permission storage remains per workspace;
- ACP children are posture-only;
- production must not reintroduce frozen model rows or fixed provider-key env
  variables.

Replace the stale statement that `LLM_API_KEY` is the only environment-sourced
value.

**Step 2: Add an operator example with fake values**

Provide one no-auth LM Studio row and one API-key row using an unmistakably fake
key. Include:

```bash
mkdir -p ~/.looprig
$EDITOR ~/.looprig/models.json
chmod 600 ~/.looprig/models.json
```

Do not provide a command that embeds a real key in shell history.

**Step 3: Stale-reference search**

Run:

```bash
rg -n 'frozenACPGatewayDefinitions|ANTHROPIC_API_KEY|OPENAI_API_KEY|CLAUDE_CODE_ACP_NATIVE_MODELS|CODEX_ACP_NATIVE_MODELS|Kimi-K2\.6|fable-5|sonnet-5|opus-5|gpt-5\.6-(sol|terra|luna)' internal cmd CLAUDE.md CONTRIBUTING.md
```

Expected: no production fixed-catalogue or credential-environment matches.
Historical test fixture names are permitted only when the test is explicitly
testing arbitrary configured data; prefer neutral fixture aliases.

**Step 4: Verify docs and tests**

```bash
gofmt -w internal/app/modelconfig.go internal/app/modelconfig_test.go internal/app/productionmodels.go internal/app/productionmodels_test.go
git diff --check
go test -race ./internal/app ./cmd/carbon
```

Expected: all commands exit 0.

**Step 5: Checkpoint**

```bash
git add CLAUDE.md CONTRIBUTING.md
# Add each additional documentation or source file by its exact path only after
# confirming it appears in this task's reviewed diff.
git commit -m "docs: document global model configuration"
```

Review `git diff --cached --name-only` before committing and unstage unrelated
files without discarding them.

---

### Task 8: Full Verification and Manual Acceptance

**Files:** None unless verification exposes a defect.

**Step 1: Run Carbon quality gates**

```bash
cd /Users/ipotter/code/looprig/carbon
make test
make lint
make secure
```

Expected: every command exits 0. If any command fails, stop, diagnose using
@superpowers:systematic-debugging, fix only the owning defect, and rerun the
entire failed command.

**Step 2: Run full race suites in changed modules**

At minimum:

```bash
go test -race ./...
```

If implementation changes another module, run `go test -race ./...` from that
module too. Do not infer success from Carbon tests alone.

**Step 3: Manual no-auth primer acceptance**

Create a temporary owner-only `~/.looprig/models.json` using a locally running
LM Studio exact served model ID and `uses` containing `primer`. Back up any
pre-existing user file without printing it. Start Carbon and verify all three
primers open, builder is active, and planner/reviewer remain read-only.

This step is destructive to local configuration if performed carelessly. Use a
recoverable backup and restore it immediately after the run. Do not commit or
capture the file.

**Step 4: Manual API-key ACP acceptance**

With an explicitly supplied test credential and configured ACP executable:

- start one Codex child;
- start one Claude Code child;
- exercise one same-dialect target;
- exercise one cross-dialect target;
- select a non-default admitted effort;
- verify child environment/logs/events do not reveal the key;
- verify no ACP action changes the native permission file.

Skip only when credentials/executables are unavailable, and record the skipped
release gate. Do not call the release verified if these live gates were skipped.

**Step 5: Restore acceptance**

Create a session, start a child, close it cleanly, rotate only the API key, and
restore successfully. Then change a non-secret target field and verify restore
reports configuration drift rather than silently substituting a target.

**Step 6: Final repository audit**

```bash
git status --short
git diff --check
git log --oneline -8
```

Confirm every changed file belongs to this plan, no credential/config fixture is
tracked, and no component dependency file changed unintentionally.

## Completion Criteria

The work is complete only when all of the following are evidenced:

- no production frozen model table remains;
- no production provider API-key environment read remains;
- `~/.looprig/models.json` is securely and strictly loaded once;
- one configured primer default and configured delegate defaults execute;
- raw keys are absent from every secret-free/durable/model-facing boundary;
- permission file location and native permission behavior are unchanged;
- ACP children remain isolated, posture-only, and unable to persist approvals;
- restore detects non-secret config drift and tolerates key rotation;
- focused, full race, lint, and security verification pass;
- required live connector acceptance is complete or explicitly recorded as an
  unmet release gate.
