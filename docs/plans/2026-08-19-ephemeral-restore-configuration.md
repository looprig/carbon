# Ephemeral Restore Configuration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Let Carbon restore durable conversations under current model, skill, MCP, ACP-catalogue, and access-profile configuration while failing clearly when a durable active ACP harness is unavailable; default new Carbon invocations to trusted and remove stale shell ACP overrides.

**Architecture:** Harness exposes two policy-neutral composition hooks: its existing manifest `RestoreDecider`, plus a new runtime-restore resolver invoked only after exact durable runtime reconstruction fails. Carbon installs both policies, accepting its ephemeral drift categories and resolving only to the current default of the same persisted ACP harness. Harness preserves fail-closed defaults, lifecycle tombstoning, and typed active-runtime errors; MCP and other accepted drift use the existing audited configuration-adoption path.

**Tech Stack:** Go 1.26 modules (`harness`, `carbon`), Harness manifests/restore deciders/runtime catalogues, Carbon ACP composition, standard Go tests, zsh configuration.

---

### Task 1: Add a composer-owned runtime-restore resolver interface to Harness

**Files:**
- Modify: `../harness/pkg/session/decider.go`
- Modify: `../harness/pkg/rig/options.go`
- Modify: `../harness/internal/sessionruntime/restore_constructor.go`
- Test: `../harness/internal/sessionruntime/restore_runtime_test.go`
- Test: `../harness/internal/sessionruntime/restore_runtime_policy_test.go`

**Step 1: Write the failing resolver plumbing test**

Define a test resolver and assert that `rig.WithRuntimeRestoreResolver` reaches the restore constructor and is invoked only after exact reconstruction fails. Assert that omitting the option preserves current exact, fail-closed behavior.

**Step 2: Run the focused test and verify RED**

Run:

```bash
GOWORK=off go test ./internal/sessionruntime ./pkg/rig -run 'Test.*RuntimeRestoreResolver' -count=1
```

Expected: compile failure because the resolver interface and Rig option do not exist.

**Step 3: Add the policy-neutral public interface and Rig option**

Add bounded request and result types in `pkg/session`:

```go
type RuntimeRestoreRequest struct {
    AgentName identity.AgentName
    Harness   loop.AgentHarnessName
    Profile   loop.RuntimeProfileName
    Mismatch  RestoreRuntimeMismatchKind
    Catalog   loop.RuntimeCatalog
}

type RuntimeRestoreResolver interface {
    ResolveRuntimeRestore(context.Context, RuntimeRestoreRequest) (loop.Resolved, error)
}
```

Use the narrowest types that preserve these semantics if import layering requires the request to live in `pkg/loop` instead. Add `rig.WithRuntimeRestoreResolver`, validate nil, and thread the immutable resolver to `restoreRuntimeBinding`. The resolver does not receive raw provider errors, executable paths, credentials, or conversation content.

**Step 4: Write failing exact-first/same-harness fallback tests**

Cover:

- exact persisted target still wins when available;
- missing old model/effort/source resolves the current default for the same agent name and harness when remap is allowed;
- a different harness is never selected;
- absent harness/profile remains `RestoreRuntimeUnavailable`;
- remap disabled preserves current fail-closed behavior.

Use real `loop.RuntimeCatalog` entries rather than a mocked resolver.

**Step 5: Run the focused runtime tests and verify RED**

```bash
GOWORK=off go test ./internal/sessionruntime -run 'TestRestoreRuntimeBinding.*Remap' -count=1
```

Expected: failures showing the old exact selector remains mandatory.

**Step 6: Implement exact-first resolver delegation**

Refactor `restoreRuntimeBinding` so it first executes the existing exact validation. Only on an exact-selection compatibility failure and with a configured resolver, call the interface. Harness validates the returned `loop.Resolved`: agent name and harness must match the durable request, the entry must exist in the supplied current catalogue, and the result must be internally consistent. Harness then overrides the bound runtime with that authorized current selection.

Do not call the resolver for malformed durable events. A missing resolver, resolver rejection, different-harness result, missing catalogue entry, or invalid native/adapter shape remains a typed runtime mismatch.

**Step 7: Run focused and package tests**

```bash
GOWORK=off go test ./internal/sessionruntime ./pkg/session -count=1
```

Expected: PASS.

**Step 8: Commit the Harness change**

```bash
git add pkg/session/decider.go pkg/rig/options.go internal/sessionruntime/restore_constructor.go internal/sessionruntime/restore_runtime_test.go internal/sessionruntime/restore_runtime_policy_test.go
git commit -m "feat: expose runtime restore policy to composers"
```

### Task 2: Preserve an actionable active-runtime failure in Harness

**Files:**
- Modify: `../harness/pkg/session/errors.go`
- Modify: `../harness/internal/sessionruntime/restore_constructor.go`
- Test: `../harness/pkg/session/errors_test.go`
- Test: `../harness/internal/sessionruntime/restore_tombstone_test.go`

**Step 1: Write failing error-text and active-child tests**

Assert that an unavailable active adapter returns a typed `RestoreRuntimeMismatchError` through `RestoreError`, with bounded text equivalent to:

```text
session: restore runtime unavailable: used harness "codex" is not currently configured or runnable
```

Also assert that an unavailable inactive child is still tombstoned and does not reject the session.

**Step 2: Run the focused tests and verify RED**

```bash
GOWORK=off go test ./pkg/session ./internal/sessionruntime -run 'Test.*(Active.*Runtime|RestoreRuntimeMismatchError).*' -count=1
```

Expected: active failure is currently collapsed to `session: loop exited`, and the mismatch has no harness field.

**Step 3: Preserve the typed cause**

Add a bounded `Harness loop.AgentHarnessName` field to `RestoreRuntimeMismatchError`. Populate it from validated durable runtime identity or the bound runtime identity. When the failed/tombstoned plan is active, return its runtime mismatch as the `RestoreLoopFailed` cause rather than replacing it with `SessionLoopExited`.

Keep public text category-only except for the validated harness name. Never include model aliases, targets, executable paths, provider errors, or credentials.

**Step 4: Run focused and full Harness verification**

```bash
GOWORK=off go test ./pkg/session ./internal/sessionruntime -count=1
GOWORK=off go test ./... -count=1
```

Expected: PASS.

**Step 5: Commit the diagnostic change**

```bash
git add pkg/session/errors.go pkg/session/errors_test.go internal/sessionruntime/restore_constructor.go internal/sessionruntime/restore_tombstone_test.go
git commit -m "fix: explain unavailable active runtime restores"
```

### Task 3: Install Carbon's selective ephemeral-configuration restore policy

**Files:**
- Create: `internal/app/restore_policy.go`
- Create: `internal/app/restore_policy_test.go`
- Modify: `internal/app/persistence.go`
- Test: `internal/app/mcp_fingerprint_test.go`
- Test: `internal/app/permission_review_test.go`

**Step 1: Write failing policy table tests**

Construct typed `event.DriftAssessment` values and assert Carbon accepts:

- `DriftModel`, `DriftPrompt`, `DriftTopology`, and `DriftTool`;
- `DriftExternal` for all MCP changes;
- `DriftRuntimeSkills`;
- `DriftRuntime` only for `field == "catalog_rev"`;
- Carbon access-profile app-field drift;
- access-profile-derived permission/confinement drift, excluding permission-review fields.

Assert Carbon rejects workspace, trust, agent kind/name/adapter, hook policy, runtime profile/identity, and permission-review policy drift.

Every accepted result must set `Source: policy` and a bounded audit message. Runtime remapping is owned separately by Carbon's runtime resolver, not smuggled through the manifest decision.

**Step 2: Run the policy test and verify RED**

```bash
GOWORK=off go test ./internal/app -run TestCarbonRestorePolicy -count=1
```

Expected: compile failure because the policy does not exist.

**Step 3: Implement the policy**

Implement `carbonRestoreDecider.DecideRestore`. Reject any assessment containing a non-ephemeral Warn change. For permission changes, accept only the access-profile-derived base/posture change; reject `review_configured` and `review_policy_rev`. Accept confinement changes because the explicitly selected current Carbon access profile owns that sandbox posture. Preserve workspace and identity rejection.

**Step 4: Wire both Carbon policies into every Carbon rig**

Add `rig.WithRestoreDecider(carbonRestoreDecider{})` and `rig.WithRuntimeRestoreResolver(carbonRuntimeRestoreResolver{})` to the common option list in `buildRigWithRegistrationAndACP`. Keep `SessionSelector.AllowConfigMismatch` as the existing explicit manifest override; when set, it must not produce duplicate singleton options. The runtime resolver remains explicit Carbon composition policy in either case.

**Step 5: Update integration tests from rejection to adoption**

Change MCP fingerprint restore tests to prove changed/removed MCP configuration restores and emits configuration adoption. Add access-profile restore tests proving readonly-to-trusted and trusted-to-readonly both restore under the selected current profile. Preserve tests proving permission-review policy changes, workspace changes, and unrelated security boundaries reject.

**Step 6: Run focused Carbon tests**

```bash
go test ./internal/app -run 'Test(CarbonRestorePolicy|MCPConfigFingerprintRestoreBehavior|.*Access.*Restore)' -count=1
```

Expected: PASS against the workspace Harness changes.

**Step 7: Commit the Carbon restore policy**

```bash
git add internal/app/restore_policy.go internal/app/restore_policy_test.go internal/app/persistence.go internal/app/mcp_fingerprint_test.go internal/app/permission_review_test.go
git commit -m "feat: adopt ephemeral configuration on restore"
```

### Task 4: Prove used-versus-unused ACP restore behavior end to end

**Files:**
- Modify: `internal/app/acpchildren_test.go`
- Modify: `internal/app/subagent_e2e_test.go`
- Modify: `internal/app/rig_restore_integration_test.go`

**Step 1: Write failing integration tests**

Cover four scenarios with real durable session events and fake ACP builders/catalogues:

1. Removing an ACP profile never used by the session restores successfully.
2. Removing a used inactive ACP harness tombstones that child and restores the active Carbon loop.
3. Removing a used active ACP harness rejects with the actionable harness-named runtime error.
4. Keeping the harness while changing its model/effort/source restores using the current same-harness default.

Also prove MCP digest drift in each scenario does not alter the ACP result.

**Step 2: Run the focused tests and verify RED**

```bash
go test ./internal/app -run 'Test.*Restore.*ACP' -count=1
```

Expected: at least the model remap and clear active failure assertions fail before Tasks 1-3 are present.

**Step 3: Make the minimum Carbon ACP adjustments**

Update `resolveACPBoundRuntime` only as needed to consume the Harness-remapped bound identity. It must select current model aliases and launcher configuration for the already-authorized same harness. Do not introduce a second independent fallback that can disagree with Harness.

**Step 4: Run focused and package tests**

```bash
go test ./internal/app -run 'Test.*Restore.*ACP' -count=1
go test ./internal/app -count=1
```

Expected: PASS.

**Step 5: Commit the ACP restore behavior**

```bash
git add internal/app/acpchildren.go internal/app/acpchildren_test.go internal/app/subagent_e2e_test.go internal/app/rig_restore_integration_test.go
git commit -m "feat: restore ACP loops against current harness config"
```

### Task 5: Default Carbon to trusted

**Files:**
- Modify: `internal/app/access.go`
- Modify: `internal/app/access_test.go`
- Modify: `internal/app/acpchildren_test.go`
- Modify: `cmd/carbon/main_test.go`

**Step 1: Change tests to require trusted by default**

Update direct default assertions, empty-profile ACP posture expectations, and CLI parsing expectations. Add an explicit readonly case to ensure the safer profile remains selectable.

**Step 2: Run tests and verify RED**

```bash
GOWORK=off go test ./internal/app ./cmd/carbon -run 'Test(DefaultAccessProfile|ParseFlags|ACP.*Profile)' -count=1
```

Expected: failures showing the default is still readonly.

**Step 3: Change the default and documentation comment**

Set:

```go
const DefaultAccessProfile = AccessTrusted
```

Update the comment to state this is Carbon's operator-convenience default, while unconfined still requires separate acknowledgement.

**Step 4: Run focused tests**

```bash
GOWORK=off go test ./internal/app ./cmd/carbon -run 'Test(DefaultAccessProfile|ParseFlags|ACP.*Profile)' -count=1
```

Expected: PASS.

**Step 5: Commit the default change**

```bash
git add internal/app/access.go internal/app/access_test.go internal/app/acpchildren_test.go cmd/carbon/main_test.go
git commit -m "feat: default Carbon sessions to trusted access"
```

### Task 6: Remove stale ACP environment overrides from zsh

**Files:**
- Modify outside repository: `/Users/ipotter/.zshrc`

**Step 1: Re-read the exact target lines and request filesystem approval**

Confirm the file still contains only these obsolete exports:

```zsh
export CLAUDE_CODE_ACP_EXECUTABLE="$(command -v claude-code-acp)"
export CODEX_ACP_EXECUTABLE="$(command -v codex-acp)"
```

**Step 2: Remove the exports with an approved patch**

Retain the `~/.looprig/bin` PATH entry. Replace the obsolete CodeRig ACP comment with a short note that Carbon launcher paths live in `~/.looprig/carbon/models.json`, or remove the now-empty comment block.

**Step 3: Verify a fresh zsh resolves no overrides**

```bash
zsh -lic 'test -z "$CLAUDE_CODE_ACP_EXECUTABLE" && test -z "$CODEX_ACP_EXECUTABLE"'
```

Expected: exit 0. Do not print any other environment values.

### Task 7: Full verification and repository-boundary review

**Files:**
- Review only: `../harness/go.mod`, `go.mod`, root `go.work`, `repositories.mk`

**Step 1: Run formatting**

```bash
gofmt -w <all modified Go files>
```

**Step 2: Run Harness standalone verification**

```bash
cd /Users/ipotter/code/looprig/harness
GOWORK=off go test ./... -count=1
```

Expected: PASS.

**Step 3: Run Carbon workspace verification against modified Harness**

```bash
cd /Users/ipotter/code/looprig/carbon
go test ./... -count=1
```

Expected: PASS.

**Step 4: Check standalone Carbon without masking dependency publication**

```bash
GOWORK=off go test ./... -count=1
```

Expected before a Harness release: either PASS if Carbon required no new Harness API, or fail specifically because the required Harness release is not yet published/pinned. Do not add a local `replace` directive and do not vendor.

**Step 5: Review repository-local diffs and status separately**

```bash
git -C /Users/ipotter/code/looprig/harness status --short
git -C /Users/ipotter/code/looprig/carbon status --short
git -C /Users/ipotter/code/looprig status --short
```

Expected: only reviewed files in their owning repositories; never stage nested repositories in the outer workspace.

**Step 6: Perform release sequencing only if explicitly requested**

Harness is tier 3 and Carbon is tier 6. If publication is requested, release Harness first, verify its remote tag, then update Carbon's exact dependency, run `GOWORK=off go mod tidy` and standalone tests, and only then release Carbon. Otherwise leave version pins, tags, pushes, root metadata, and remotes untouched.
