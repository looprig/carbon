# Carbon Agent Consolidation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace Carbon's planner, builder, and reviewer topology with one lean `carbon` primer that can delegate to itself and optionally launch Codex or Claude Code ACP children.

**Architecture:** Carbon will construct one `loop.Definition` directly from one catalog package, one full coding toolset, and one session access set/gate. Runtime configuration will have no `delegate_defaults`; the in-process `looprig/native` row is the deterministic default and ACP rows are explicit alternatives for `carbon`.

**Tech Stack:** Go, `github.com/looprig/harness`, `github.com/looprig/sandbox`, `github.com/looprig/tools`, ACP launch adapters, strict JSON model/MCP configuration.

---

### Task 1: Add the Carbon identity and prompt

**Files:**
- Create: `internal/catalog/carbon/generic_test.go`
- Create: `internal/catalog/carbon/generic.go`
- Delete after callers migrate: `internal/catalog/planner/planner.go`
- Delete after callers migrate: `internal/catalog/planner/planner_test.go`
- Delete after callers migrate: `internal/catalog/builder/builder.go`
- Delete after callers migrate: `internal/catalog/builder/builder_test.go`
- Delete after callers migrate: `internal/catalog/reviewer/reviewer.go`
- Delete after callers migrate: `internal/catalog/reviewer/reviewer_test.go`
- Delete after callers migrate: `internal/catalog/operator/operator.go`
- Modify: `internal/catalog/identity.go`
- Test: `internal/catalog/identity_test.go`

**Step 1: Write the failing Carbon catalog test**

Create a test that requires `Name == "carbon"`, a non-empty description, one well-formed `<identity product="Carbon">` prompt, and the approved intent/workflow/tools/safety/delegation/communication sections. Assert the prompt contains `Carbon`, `answer`, `change`, `verify`, `untrusted data`, `destructive`, and `delegate` once in the appropriate sections, rather than checking provider brand names.

```go
func TestIdentity(t *testing.T) {
    if Name != "carbon" { t.Fatalf("Name = %q, want generic", Name) }
    var root struct {
        XMLName xml.Name `xml:"identity"`
        Product string   `xml:"product,attr"`
    }
    if err := xml.Unmarshal([]byte(SystemPrompt), &root); err != nil {
        t.Fatalf("SystemPrompt is not XML: %v", err)
    }
    if root.Product != "Carbon" { t.Fatalf("product = %q", root.Product) }
}
```

**Step 2: Run the test to verify it fails**

Run: `GOWORK=off go test ./internal/catalog/carbon`

Expected: FAIL because the package does not exist.

**Step 3: Implement the minimal catalog package**

Define only `Name`, `Description`, and `SystemPrompt`. Put the complete approved prompt in `SystemPrompt`; do not split identity and role fragments or introduce a metadata wrapper.

```go
const Name = identity.AgentName("generic")
const Description = "Investigates, implements, tests, reviews, and verifies software-engineering work end to end."
const SystemPrompt = `<identity product="Carbon">...approved prompt...</identity>`
```

Remove the old shared `catalog.Identity` constant once assembly consumes `carbon.SystemPrompt`; retain no provider-branded prompt fragments.

**Step 4: Run catalog tests**

Run: `GOWORK=off go test ./internal/catalog/...`

Expected: PASS for the new Carbon catalog; old packages remain until Task 5 to keep intermediate builds coherent.

**Step 5: Commit**

```bash
git add internal/catalog
git commit -m "feat: define generic coding agent"
```

### Task 2: Remove `delegate_defaults` from model configuration

**Files:**
- Modify: `internal/app/modelconfig.go`
- Modify: `internal/app/modelconfig_normalize.go`
- Modify: `internal/app/modelconfig_digest.go`
- Modify: `internal/app/productionmodels.go`
- Modify: `internal/app/modelconfig_decode_test.go`
- Modify: `internal/app/modelconfig_validate_test.go`
- Modify: `internal/app/modelconfig_digest_test.go`
- Modify: `internal/app/modelconfig_native_test.go`
- Modify: `internal/app/productionmodels_test.go`
- Modify: model-config fixtures throughout `internal/app/*_test.go`

**Step 1: Write failing schema and normalization tests**

Add tests proving a valid version-2 file needs no `delegate_defaults` and a file that still supplies it fails strict decoding. Replace three-role ordering/default tests with assertions about models, primer selection, native ACP profiles, and Claude's optional small model only.

```go
func TestDecodeRejectsRemovedDelegateDefaults(t *testing.T) {
    _, err := decodeModelConfig([]byte(`{"version":2,"delegate_defaults":{},"models":[]}`))
    if err == nil { t.Fatal("decode accepted removed delegate_defaults") }
}
```

**Step 2: Run the focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'TestDecodeRejectsRemovedDelegateDefaults|TestNormalizeModelConfig'`

Expected: FAIL because the field is still decoded and normalization still requires three roles.

**Step 3: Delete default configuration plumbing**

Remove:

- `modelConfigFile.DelegateDefaults` and `delegateDefaultConfig`;
- `modelConfigDelegateRoleOrder` and `normalizedDelegateDefault`;
- `normalizedModelConfig.DelegateDefaults` and its validation block;
- `configuredDelegateDefault`;
- `productionModels.Defaults` and its compilation/string output;
- delegate-default fields from the normalized digest representation.

Keep `claude_code_small_model` validation based on whether gateway-backed Claude ACP can be exposed, not whether Claude happened to be a configured default. Update test fixtures by deleting the JSON field rather than replacing it.

**Step 4: Run configuration tests**

Run: `GOWORK=off go test ./internal/app -run 'ModelConfig|ProductionModels'`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/modelconfig.go internal/app/modelconfig_normalize.go internal/app/modelconfig_digest.go internal/app/productionmodels.go internal/app/*model*_test.go
git commit -m "refactor: remove delegate default configuration"
```

### Task 3: Compile one Carbon runtime catalog with an in-process default

**Files:**
- Modify: `internal/app/runtimecatalog.go`
- Modify: `internal/app/acpcatalog.go`
- Modify: `internal/app/acpproduction.go`
- Modify: `internal/app/runtimecatalog_test.go`
- Modify: `internal/app/acpcatalog_test.go`
- Modify: `internal/app/acpproduction_test.go`
- Modify: `internal/app/fingerprint_test.go`

**Step 1: Write failing runtime-catalog tests**

Require the production catalog to contain entries only for `carbon`, exactly one default entry, and that default to be `looprig/native`. Verify both ACP harnesses remain non-default explicit choices when configured.

```go
entries := compiled.RuntimeCatalog.EntriesFor(carbon.Name)
if got := countDefaults(entries); got != 1 { t.Fatalf("defaults = %d", got) }
if def := defaultEntry(entries); def.AgentHarness != looprigRuntimeHarness {
    t.Fatalf("default harness = %q, want %q", def.AgentHarness, looprigRuntimeHarness)
}
for _, old := range []identity.AgentName{"planner", "builder", "reviewer"} {
    if got := compiled.RuntimeCatalog.EntriesFor(old); got != nil { t.Fatalf("legacy entries = %v", got) }
}
```

Add a Harness-level preparation integration assertion through Carbon's assembled `StartAgent`: omitted runtime selectors resolve to `looprig/native`; explicit `agent_harness: "codex"` and `"claude-code"` resolve to their ACP profiles.

**Step 2: Run focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'RuntimeCatalog|ACPCatalog|ACPProduction'`

Expected: FAIL because production still compiles three roles and derives defaults from `delegate_defaults`.

**Step 3: Simplify catalog compilation**

Make the compiler fixed to `carbon.Name`; remove `AgentTypes` and `Defaults` inputs and `normalizedAgentTypes`. Produce raw ACP rows without assigning them product defaults, combine them with the ordinary `looprig/native` row, mark only that row `Default: true`, then call `loop.NewRuntimeCatalog` once. Do not retain an ACP-only catalog whose temporary default is later repaired.

The ordinary row uses configured delegate-capable model options and existing routing clients. If no delegate-capable model exists, use the configured primer model as the single in-process option so plain Carbon delegation always exists. ACP rows remain conditional on their configured model/native profiles and executable preflight.

**Step 4: Run catalog and fingerprint tests**

Run: `GOWORK=off go test ./internal/app -run 'RuntimeCatalog|ACPCatalog|ACPProduction|Fingerprint'`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/runtimecatalog.go internal/app/acpcatalog.go internal/app/acpproduction.go internal/app/*catalog*_test.go internal/app/acpproduction_test.go internal/app/fingerprint_test.go
git commit -m "refactor: compile carbon runtime catalog"
```

### Task 4: Collapse tools and access to one Carbon capability set

**Files:**
- Modify: `internal/app/toolsets.go`
- Modify: `internal/app/access.go`
- Modify: `internal/app/access_test.go`
- Modify: `internal/app/access_assembly_test.go`
- Modify: `internal/app/access_acceptance_test.go`
- Modify: `internal/app/toolsets_hostreads_test.go`
- Modify: `internal/app/toolsets_hostwrites_test.go`
- Modify: `internal/app/process_tools_test.go`
- Modify: `internal/app/permission_review_test.go`

**Step 1: Write failing single-access tests**

Replace executor-separation assertions with a test that `sessionAccess` exposes one non-nil `set`, one `gate`, and one `policyRev`; repeated loop IDs memoize within that set; different Carbon loop IDs receive different executors; and `Close` closes the set exactly once.

Add a tool roster test requiring Carbon to expose the builder's complete tool set, including read/search, write/edit, supervised Bash/process tools, web/fetch, task/user interaction, and optional Skill.

**Step 2: Run focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'SessionAccess|CarbonTool|AcceptanceProfileGateBehavior'`

Expected: FAIL because access and tools are role-specific.

**Step 3: Implement one access set and tool builder**

Reduce `sessionAccess` to:

```go
type sessionAccess struct {
    profileName string
    workspace   string
    configRev   string
    diagnostics []string
    set         *sandbox.ExecutorSet
    gate        loop.AccessGate
    policyRev   string
    closeOnce   sync.Once
    closeErr    error
}
```

Build one executor set from the selected Carbon profile and one `roleGate` from the same profile. Rename `rolePolicyRevision` to `agentPolicyRevision` (or inline it if used once) and identify it as `carbon`. Compute the access digest from one effective profile; remove planner/reviewer restrictions and aliases.

Rename `builderToolDefinitions` to `carbonToolDefinitions`, delete planner/reviewer/operator variants, and retain the full implementation roster with no branching by role.

**Step 4: Run access/tool tests**

Run: `GOWORK=off go test ./internal/app -run 'Access|Tool|Process|PermissionReview'`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/access.go internal/app/toolsets.go internal/app/*access*_test.go internal/app/*tool*_test.go internal/app/permission_review_test.go
git commit -m "refactor: use one generic access and tool set"
```

### Task 5: Replace swarm assembly with one direct Carbon definition

**Files:**
- Create: `internal/app/assembly.go`
- Create or rename: `internal/app/assembly_test.go`
- Delete: `internal/app/agents.go`
- Delete: `internal/app/swarm.go`
- Delete or rename: `internal/app/swarm_test.go`
- Delete: `internal/catalog/planner/`
- Delete: `internal/catalog/builder/`
- Delete: `internal/catalog/reviewer/`
- Delete: `internal/catalog/operator/`
- Modify: `internal/app/persistence.go`
- Modify: `internal/app/skills_catalog.go`
- Modify: `internal/app/runtime_skills_test.go`
- Modify: `internal/app/skills_wiring_test.go`
- Modify: `internal/app/managed_delegation_test.go`
- Modify: `internal/app/acceptance_test.go`

**Step 1: Write failing assembly tests**

Require `carbonDefinition` to return one definition named/displayed `carbon`, with `carbon` as its only delegate, managed delegation, quick/deep modes, Carbon prompt, full tools, runtime context, access gate, and policy revision. Require session assembly to expose `carbon` as the sole primer.

```go
definition, err := carbonDefinition(client, model, cfg, access, nil)
if err != nil { t.Fatal(err) }
if definition.Name() != carbon.Name { t.Fatalf("name = %q", definition.Name()) }
if got := definition.Delegates(); !slices.Equal(got, []identity.AgentName{carbon.Name}) {
    t.Fatalf("delegates = %v", got)
}
```

**Step 2: Run focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'CarbonDefinition|ActivePrimer|ManagedAgent'`

Expected: FAIL because the direct Carbon constructor does not exist.

**Step 3: Implement direct assembly**

Move the genuinely shared session-construction functions from `swarm.go` into `assembly.go`. Replace the roster build with one function that directly creates the Skill loader, Carbon tools, prompt, and options. Keep at most one optional `extras []tool.Definition` argument as a test probe seam; do not recreate a name-keyed map for one agent.

Set:

```go
const activePrimerName = carbon.Name
const agentKind = "carbon:carbon"
```

Use `loop.WithDelegates(carbon.Name)` and the existing managed-delegation limits. Rename `swarmStores` and related provider identifiers to `sessionStores` where they describe storage rather than agent topology.

Delete old catalog packages, `agents.go`, old assembly functions, legacy aliases, and tests whose only purpose was three-role differentiation.

**Step 4: Run assembly, delegation, skills, acceptance, and persistence tests**

Run: `GOWORK=off go test ./internal/app -run 'Carbon|Agent|Delegat|Skill|Acceptance|Persist'`

Expected: PASS.

**Step 5: Commit**

```bash
git add -A internal/catalog internal/app
git commit -m "refactor: assemble one generic agent"
```

### Task 6: Restrict ACP and MCP role surfaces to Carbon

**Files:**
- Modify: `internal/app/acpchildren.go`
- Modify: `internal/app/acpchildren_test.go`
- Modify: `internal/app/acpchildren_task31_test.go`
- Modify: `internal/app/subagent_e2e_test.go`
- Modify: `internal/app/agent_tools_integration_test.go`
- Modify: `internal/app/mcp.go`
- Modify: `internal/app/mcpconfig.go`
- Modify: `internal/app/mcp_test.go`
- Modify: `internal/app/mcpconfig_test.go`
- Modify: `internal/app/mcp_integration_test.go`

**Step 1: Write failing integration tests**

Add tests proving:

- `acpPostureFor("generic") == driver.PostureWorkspaceWrite`;
- planner/builder/reviewer postures are rejected;
- empty MCP `roles` exposes a binding to Carbon;
- `roles: ["carbon"]` is accepted;
- every former role name is rejected;
- Carbon can start an in-process child without runtime selectors and explicit Codex/Claude children with selectors.

**Step 2: Run focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'ACP|MCP|AgentTools|Subagent'`

Expected: FAIL on legacy role surfaces.

**Step 3: Implement the Carbon-only surfaces**

Change ACP posture selection to one accepted name and one result. Set MCP's allowed/default role list to `carbon` and remove role fan-out. Update event assertions and child fixtures to attribute all loops to Carbon while still distinguishing runtime harness/profile.

**Step 4: Run ACP/MCP tests**

Run: `GOWORK=off go test ./internal/app -run 'ACP|MCP|AgentTools|Subagent'`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/acpchildren.go internal/app/acpchildren*_test.go internal/app/subagent_e2e_test.go internal/app/agent_tools_integration_test.go internal/app/mcp*.go
git commit -m "refactor: expose generic ACP and MCP roles"
```

### Task 7: Update fingerprints, persistence, docs, and remove dead terminology

**Files:**
- Modify: `internal/app/fingerprint_test.go`
- Modify: `internal/app/persistence.go`
- Modify: `internal/app/persistence_test.go`
- Modify: `internal/app/persistence_integration_test.go`
- Modify: `internal/app/roster_contract_test.go`
- Modify: `internal/app/legacy_guard_test.go`
- Modify: `CLAUDE.md`
- Modify: `CONTRIBUTING.md`
- Modify: relevant current documentation outside `docs/plans/`

**Step 1: Write failing contract tests**

Replace the old roster contract with source-level assertions that production contains `internal/catalog/carbon`, has no imports of the deleted role packages, exposes `agentKind == "carbon:carbon"`, and does not contain live `delegate_defaults`, `leafBuiltin`, or `swarmDefinitions` plumbing. Historical design documents may retain old terms.

**Step 2: Run focused tests to verify failure**

Run: `GOWORK=off go test ./internal/app -run 'Fingerprint|Persistence|Roster|Legacy'`

Expected: FAIL until remaining identity and documentation references are updated.

**Step 3: Finish the clean break**

Update fingerprints and persistence assertions to Carbon. Remove compatibility aliases and obsolete helpers. Rewrite contributor/architecture guidance around one agent, one access authority, Carbon self-delegation, in-process default selection, and explicit ACP alternatives. Do not rewrite historical plan documents.

Run `rg -n 'planner|builder|reviewer|delegate_defaults|leafBuiltin|swarmDefinitions' --glob '!docs/plans/**' --glob '!*.sum'` and classify every remaining hit: delete live product-role references; keep ordinary English uses such as a third-party builder type only when they are not Carbon agent identities.

**Step 4: Run package tests**

Run: `GOWORK=off go test ./internal/app ./internal/catalog/... ./cmd/carbon`

Expected: PASS.

**Step 5: Commit**

```bash
git add -A
git commit -m "docs: describe single generic agent"
```

### Task 8: Verify the completed consolidation

**Files:**
- Modify only if verification exposes a defect in the requested behavior.

**Step 1: Format and inspect the diff**

Run: `gofmt -w internal/app internal/catalog cmd/carbon`

Run: `git diff --check`

Expected: no formatting errors or whitespace warnings.

**Step 2: Run focused behavioral tests with race detection**

Run: `GOWORK=off GOCACHE=/private/tmp/carbon-generic-gocache go test -race ./internal/catalog/... ./internal/app -run 'Carbon|Agent|Delegat|RuntimeCatalog|ModelConfig|Access|MCP|ACP|Fingerprint'`

Expected: PASS.

**Step 3: Run the full race-enabled suite**

Run: `GOWORK=off GOCACHE=/private/tmp/carbon-generic-gocache go test -race ./...`

Expected: PASS. If the known `TestProcessToolsSiblingLoopsCannotAccessEachOthersHandles` temporary-directory cleanup race recurs, rerun it three times and report it separately; do not conceal any new failure.

**Step 4: Run lint and integration checks**

Run: `GOWORK=off GOCACHE=/private/tmp/carbon-generic-gocache make lint`

Run: `GOWORK=off GOCACHE=/private/tmp/carbon-generic-gocache make test-integration`

Expected: PASS, subject only to explicitly documented external-service prerequisites.

**Step 5: Verify removal and worktree state**

Run: `rg -n 'internal/catalog/(planner|builder|reviewer|operator)|delegate_defaults|leafBuiltin|swarmDefinitions' --glob '!docs/plans/**'`

Expected: no live product-code hits.

Run: `git status --short`

Expected: clean.

**Step 6: Commit any verification-only correction**

If verification required a correction:

```bash
git add <exact corrected files>
git commit -m "fix: complete generic agent consolidation"
```
