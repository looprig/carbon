# Carbon Identity Rename Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Publish a standalone CodeRig `v0.18.1`, rename the complete product and sole agent identity to Carbon, and publish Carbon `v0.1.0` without compatibility or migration code.

**Architecture:** Release every unpublished dependency in dependency order, then prove CodeRig resolves with `GOWORK=off` and no local replacements before tagging it. Preserve that Git ancestry while changing the local worktree, module, command, agent, state, MCP helper, ecosystem references, and remote to Carbon; publish a Carbon-specific MCP release before the final standalone Carbon release.

**Tech Stack:** Go 1.26.4+, Git annotated tags and worktrees, GitHub SSH remotes, GNU/BSD `sed`, `rg`, repository Makefiles, Go race/integration/security/cross-build gates.

---

## Execution rules

- Invoke `@superpowers:using-git-worktrees` before making release-train changes.
- Use `@superpowers:test-driven-development` for semantic Carbon behavior.
- Use `@superpowers:verification-before-completion` before every commit, tag,
  push, release claim, and final handoff.
- Never force-push, move a tag, delete an existing tag, reset user work, or use
  `git add -A` in a dirty repository.
- Push a dependency tag before writing it into a downstream `go.mod`.
- Use separate task-specific `GOCACHE` and `GOMODCACHE` directories under
  `/private/tmp` for standalone release proofs.
- Preserve CodeRig's existing untracked `docs/superpowers/` files. They move
  with the product worktree and are added only during the reviewed Carbon docs
  sweep.
- Preserve and review the existing LLM changes in
  `providers/openai/client.go` and `providers/openai/options.go`.
- Commands that access GitHub require network approval. Ask once with the
  narrowest useful Git/Go prefix and never work around a denied approval.

### Task 1: Record and verify the release baseline

**Files:**
- Verify: `/Users/ipotter/code/looprig/coderig/`
- Verify: `/Users/ipotter/code/looprig/{secrets,credentials,inference,llm,acp,harness,mcp,foreignloops,tools,tui}/`
- Verify: `/Users/ipotter/code/looprig/repositories.mk`

**Step 1: Record every relevant checkout**

Run from `/Users/ipotter/code/looprig`:

```bash
for repo in coderig inference llm acp harness mcp foreignloops tools tui; do
  git -C "$repo" status --short --branch
  git -C "$repo" rev-parse HEAD
  git -C "$repo" remote -v
  git -C "$repo" tag --sort=-v:refname | head -5
done
```

Expected: record the exact branch, HEAD, remotes, tags, and dirty paths. Stop if
there are unexpected production edits beyond the two known LLM files.

**Step 2: Verify the three new GitHub repositories**

Run:

```bash
git ls-remote git@github.com:looprig/secrets.git
git ls-remote git@github.com:looprig/credentials.git
git ls-remote git@github.com:looprig/carbon.git
```

Expected: each repository exists and has no conflicting `main` or target tag.

**Step 3: Verify target tags are unused**

Check local and remote refs for:

```text
secrets/v0.1.0
credentials/v0.1.0
inference/v0.9.0
llm/v0.13.1
acp/v0.2.0
harness/v0.22.0
mcp/v0.5.0
foreignloops/v0.2.1
tools/v0.9.0
tui/v0.14.0
coderig/v0.18.1
mcp/v0.6.0
carbon/v0.1.0
```

Expected: every target tag is absent. If one exists, stop and inspect it; never
replace it.

**Step 4: Record the approved design commits**

Run in `coderig/`:

```bash
git log -2 --oneline
git status --short
```

Expected: the Carbon design commits are present and only the pre-existing
`docs/superpowers/` files are untracked.

### Task 2: Initialize and release Secrets `v0.1.0`

**Files:**
- Existing module/new repository: `/Users/ipotter/code/looprig/secrets/`
- Verify: `secrets/go.mod`
- Test: `secrets/**/*_test.go`

**Step 1: Inspect the complete module**

Run:

```bash
rg --files secrets | sort
rg -n 'TODO|FIXME|api[_-]?key|token|secret' secrets
```

Expected: no committed credential value, generated local store, or unrelated
workspace file is present.

**Step 2: Run the release suite before Git initialization**

Run:

```bash
cd /Users/ipotter/code/looprig/secrets
GOWORK=off GOCACHE=/private/tmp/secrets-v010-gocache go test -race -count=1 ./...
GOWORK=off go vet ./...
```

Expected: PASS with zero failures.

**Step 3: Initialize the repository and commit the reviewed snapshot**

Run:

```bash
git init -b main
git remote add origin git@github.com:looprig/secrets.git
git add go.mod go.sum *.go contracttest local
git diff --cached --check
git commit -m "feat: establish secret storage module"
```

Expected: one root commit containing only the Secrets module.

**Step 4: Re-run verification on the committed tree**

Run the Step 2 commands again and verify `git status --short` is empty.

**Step 5: Publish and verify `v0.1.0`**

Run:

```bash
git push -u origin main
git tag -a v0.1.0 -m "secrets v0.1.0"
git push origin v0.1.0
git ls-remote origin refs/heads/main refs/tags/v0.1.0 'refs/tags/v0.1.0^{}'
```

Expected: remote `main` and the peeled annotated tag resolve to the verified
commit.

### Task 3: Initialize and release Credentials `v0.1.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/credentials/go.mod`
- Modify: `/Users/ipotter/code/looprig/credentials/go.sum`
- Existing module/new repository: `/Users/ipotter/code/looprig/credentials/`
- Test: `credentials/**/*_test.go`

**Step 1: Adopt the published Secrets module**

Run in `credentials/`:

```bash
GOWORK=off go mod edit -dropreplace=github.com/looprig/secrets
GOWORK=off go mod edit -require=github.com/looprig/secrets@v0.1.0
GOWORK=off GOMODCACHE=/private/tmp/credentials-v010-modcache go mod tidy
```

Expected: `go.mod` has no `replace` and requires Secrets `v0.1.0`.

**Step 2: Verify independently**

Run:

```bash
GOWORK=off GOMODCACHE=/private/tmp/credentials-v010-modcache GOCACHE=/private/tmp/credentials-v010-gocache go mod verify
GOWORK=off GOMODCACHE=/private/tmp/credentials-v010-modcache GOCACHE=/private/tmp/credentials-v010-gocache go test -race -count=1 ./...
GOWORK=off GOMODCACHE=/private/tmp/credentials-v010-modcache go vet ./...
```

Expected: PASS with zero failures.

**Step 3: Initialize and commit**

Run:

```bash
git init -b main
git remote add origin git@github.com:looprig/credentials.git
git add go.mod go.sum *.go catalog httpauth oauth refresh
git diff --cached --check
git commit -m "feat: establish credential lifecycle module"
```

Expected: one root commit with no local replacement.

**Step 4: Publish and verify `v0.1.0`**

Run the same branch/tag sequence as Secrets with message
`credentials v0.1.0`. Verify both the branch and peeled tag.

### Task 4: Release Inference `v0.9.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/inference/go.mod`
- Modify: `/Users/ipotter/code/looprig/inference/go.sum`
- Test: `inference/**/*_test.go`

**Step 1: Create an isolated release branch/worktree**

Use `@superpowers:using-git-worktrees` to create branch
`release/inference-v0.9.0` from current Inference `main`.

**Step 2: Adopt Credentials and Secrets**

Run:

```bash
GOWORK=off go mod edit -dropreplace=github.com/looprig/credentials
GOWORK=off go mod edit -dropreplace=github.com/looprig/secrets
GOWORK=off go mod edit -require=github.com/looprig/credentials@v0.1.0
GOWORK=off go mod edit -require=github.com/looprig/secrets@v0.1.0
GOWORK=off go mod tidy
```

Expected: no Credentials/Secrets replacement or `v0.0.0` requirement remains.

**Step 3: Run focused credential and transport tests**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/inference-v090-gocache go test -race ./auth ./transport ./retry -count=1
```

Expected: PASS.

**Step 4: Run the full release gates**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/inference-v090-gocache go mod verify
GOWORK=off GOCACHE=/private/tmp/inference-v090-gocache go test -race -count=1 ./...
GOWORK=off GOCACHE=/private/tmp/inference-v090-gocache go vet ./...
```

Expected: PASS.

**Step 5: Commit, merge to main, publish, and verify**

Commit only `go.mod` and `go.sum` as
`build(inference): publish credential dependencies`. Fast-forward or merge the
reviewed release branch into `main`, push `main`, create annotated `v0.9.0`,
push the tag, and verify remote refs.

### Task 5: Complete and release LLM `v0.13.1`

**Files:**
- Preserve/modify: `/Users/ipotter/code/looprig/llm/providers/openai/client.go`
- Preserve/modify: `/Users/ipotter/code/looprig/llm/providers/openai/options.go`
- Test: `llm/providers/openai/**/*_test.go`
- Modify: `llm/go.mod`
- Modify: `llm/go.sum`
- Refresh if present: `llm/vendor/`

**Step 1: Review the existing user diff**

Run:

```bash
git diff -- providers/openai/client.go providers/openai/options.go
rg -n 'WithRoundTripper|roundTripper' providers/openai
```

Expected: the diff only adds a non-nil checked `WithRoundTripper` option and
threads it to Inference transport construction.

**Step 2: Add or update the focused test first**

In the existing OpenAI option/client test file, add a test equivalent to:

```go
func TestWithRoundTripperRejectsNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("WithRoundTripper(nil) did not panic")
		}
	}()
	_ = WithRoundTripper(nil)
}
```

Also add a positive construction test using a stub `http.RoundTripper` and
assert that a request traverses that stub rather than the default transport.

**Step 3: Run focused tests**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/llm-v0130-gocache go test -race ./providers/openai -count=1
```

Expected: PASS after the preserved implementation is complete.

**Step 4: Commit the preserved round-tripper work**

Stage only the two production files and their focused test. Commit as:

```text
feat(openai): support caller-owned verified transports
```

**Step 5: Adopt the published dependency chain**

Run:

```bash
GOWORK=off go mod edit -dropreplace=github.com/looprig/core
GOWORK=off go mod edit -dropreplace=github.com/looprig/credentials
GOWORK=off go mod edit -dropreplace=github.com/looprig/inference
GOWORK=off go mod edit -dropreplace=github.com/looprig/secrets
GOWORK=off go mod edit -require=github.com/looprig/credentials@v0.1.0
GOWORK=off go mod edit -require=github.com/looprig/inference@v0.9.0
GOWORK=off go mod edit -require=github.com/looprig/secrets@v0.1.0
GOWORK=off go mod tidy
```

If `vendor/` exists, run `GOWORK=off go mod vendor` and inspect every vendored
diff.

**Step 6: Run full release verification**

Run `go mod verify`, `go test -race -count=1 ./...`, `go vet ./...`, and the
repository's Makefile lint/security gates with `GOWORK=off`.

Expected: PASS and a clean worktree after the dependency commit.

**Step 7: Publish `v0.13.1`**

Push `main`, create annotated `v0.13.1`, push it, and verify the remote branch
and peeled tag.

### Task 6: Release Harness `v0.22.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/harness/go.mod`
- Modify: `/Users/ipotter/code/looprig/harness/go.sum`
- Refresh: `harness/vendor/` if present
- Test: `harness/internal/sessionruntime/**/*_test.go`
- Test: `harness/pkg/foreign/**/*_test.go`

**Step 1: Adopt published direct dependencies**

Remove Harness's local Looprig replacements. Require Core `v0.5.0`, Eval
`v0.1.0`, Inference `v0.9.0`, and Storage `v0.3.1`; tidy with `GOWORK=off`.

**Step 2: Run focused collaboration tests**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/harness-v0220-gocache go test -race ./pkg/foreign ./internal/sessionruntime -run 'Test.*(Foreign|Message|Broker|Delivery)' -count=1
```

Expected: PASS.

**Step 3: Run full release gates**

Run Harness default race, integration race, vet, staticcheck/gosec, native
build, and supported cross-build gates. Refresh and inspect vendor metadata if
the repository vendors dependencies.

**Step 4: Commit and publish**

Commit dependency metadata as `build(harness): prepare v0.22.0 dependencies`,
push `main`, tag annotated `v0.22.0`, push, and verify.

### Task 7: Release ACP `v0.2.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/acp/go.mod`
- Modify: `/Users/ipotter/code/looprig/acp/go.sum`
- Refresh: `acp/vendor/`
- Test: `acp/client/**/*_test.go`
- Test: `acp/launch/**/*_test.go`
- Test: `acp/protocol/**/*_test.go`

**Step 1: Update direct requirements**

Require Harness `v0.22.0`, Inference `v0.9.0`, Storage `v0.3.1`, and current
Core. Run `GOWORK=off go mod tidy` and `GOWORK=off go mod vendor`.

**Step 2: Verify selector and steering behavior**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/acp-v020-gocache go test -race ./client ./launch ./protocol -run 'Test.*(Steer|Selector|Model|Effort|Restore)' -count=1
```

Expected: PASS.

**Step 3: Run full release gates and inspect vendor diff**

Run default race tests, integration tests, vet, staticcheck, security checks,
and native/cross builds. Reject unrelated vendor churn.

**Step 4: Commit and publish**

Commit dependency/vendor metadata, push `main`, tag annotated `v0.2.0`, push,
and verify.

### Task 8: Release MCP `v0.5.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/mcp/go.mod`
- Modify: `/Users/ipotter/code/looprig/mcp/go.sum`
- Test: `mcp/cmd/coderig-collab-mcp/**/*_test.go`
- Test: `mcp/pkg/collab/**/*_test.go`
- Test: `mcp/pkg/server/**/*_test.go`

**Step 1: Update published requirements and remove local replacements**

Require Harness `v0.22.0`, Inference `v0.9.0`, and Core `v0.5.0`. Tidy with
`GOWORK=off`.

**Step 2: Verify the final CodeRig helper**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/mcp-v050-gocache go test -race ./pkg/collab ./pkg/server ./cmd/coderig-collab-mcp -count=1
```

Expected: PASS and the executable is still named `coderig-collab-mcp`.

**Step 3: Run full release gates, commit, and publish**

Run MCP's full default/integration/security/cross-build gates. Commit release
metadata, push `main`, publish annotated `v0.5.0`, and verify remote refs.

### Task 9: Release Tools `v0.9.0`, Foreignloops `v0.2.1`, and TUI `v0.14.0`

**Files:**
- Modify: `/Users/ipotter/code/looprig/tools/go.mod`
- Modify: `/Users/ipotter/code/looprig/foreignloops/go.mod`
- Modify: `/Users/ipotter/code/looprig/tui/go.mod`
- Modify corresponding `go.sum` and `vendor/` metadata
- Test: `tools/bash/**/*_test.go`
- Test: `foreignloops/driver/acp/**/*_test.go`
- Test: `tui/**/*_test.go`

**Step 1: Release Tools**

Require Harness `v0.22.0` and Inference `v0.9.0`. Run the Bash focused tests,
full race/integration/security gates, commit metadata, push `main`, and publish
annotated `v0.9.0`.

**Step 2: Release Foreignloops**

Remove local replacements and require ACP `v0.2.0`, Harness `v0.22.0`,
Inference `v0.9.0`, Storage `v0.3.1`, and Core `v0.5.0`. Refresh vendor data,
run steering/restore focused tests and the full release suite, commit, push,
and publish annotated `v0.2.0`.

**Step 3: Release TUI**

Require Harness `v0.22.0`, Inference `v0.9.0`, Storage `v0.3.1`, and Core
`v0.5.0`. Run focused StartAgent/runtime presentation tests and the full
release suite, commit, push, and publish annotated `v0.14.0`.

**Step 4: Verify all runtime release refs**

Use `git ls-remote` for every branch and annotated tag from Tasks 6-9. Do not
start CodeRig adoption until all expected refs match the tested commits.

### Task 10: Make CodeRig standalone from the verified `v0.18.1` baseline

**Files:**
- Modify: `/Users/ipotter/code/looprig/coderig/go.mod`
- Modify: `/Users/ipotter/code/looprig/coderig/go.sum`
- Verify: `coderig/docs/plans/2026-08-09-carbon-identity-rename-design.md`
- Verify: `coderig/docs/plans/2026-08-09-carbon-identity-rename.md`

**Step 1: Remove every local replacement**

Run one `go mod edit -dropreplace` command for each Looprig replacement in
`coderig/go.mod`. Then set these requirements:

```text
github.com/looprig/acp v0.2.0
github.com/looprig/classifiers v0.1.2
github.com/looprig/core v0.5.0
github.com/looprig/credentials v0.1.0
github.com/looprig/foreignloops v0.2.1
github.com/looprig/fsstore v0.3.2
github.com/looprig/harness v0.22.0
github.com/looprig/inference v0.9.0
github.com/looprig/llm v0.13.1
github.com/looprig/mcp v0.5.0
github.com/looprig/sandbox v0.7.0
github.com/looprig/secrets v0.1.0
github.com/looprig/storage v0.3.1
github.com/looprig/tools v0.9.0
github.com/looprig/tui v0.14.0
```

Run `GOWORK=off go mod tidy`.

**Step 2: Prove the module metadata is standalone**

Run:

```bash
test -z "$(rg '^replace github.com/looprig/' go.mod || true)"
GOWORK=off GOMODCACHE=/private/tmp/coderig-v0180-modcache GOCACHE=/private/tmp/coderig-v0180-gocache go mod verify
GOWORK=off GOMODCACHE=/private/tmp/coderig-v0180-modcache GOCACHE=/private/tmp/coderig-v0180-gocache go test -race -count=1 ./...
```

Expected: no replacement output and all tests pass from remote modules.

**Step 3: Run the complete release gates**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/coderig-v0180-gocache go test -tags integration -race -count=1 ./...
GOWORK=off GOCACHE=/private/tmp/coderig-v0180-gocache make secure
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 GOWORK=off go build -trimpath ./...
```

Expected: PASS. A documented platform limitation is not a passing release gate
unless the repository's approved instructions explicitly permit it.

**Step 4: Commit only the release adoption**

Run:

```bash
git add go.mod go.sum
git diff --cached --check
git commit -m "build(coderig): prepare v0.18.1 release"
```

Do not add `docs/superpowers/`.

**Step 5: Re-run the standalone test from the committed tree**

Repeat Steps 2-3. Record the verified commit ID.

**Step 6: Push CodeRig and publish `v0.18.1`**

Run:

```bash
git push origin main
git tag -a v0.18.1 -m "coderig v0.18.1"
git push origin v0.18.1
git ls-remote origin refs/heads/main refs/tags/v0.18.1 'refs/tags/v0.18.1^{}'
```

Expected: branch and peeled tag match the verified release commit.

**Step 7: Prove remote-only consumption**

In a fresh `/private/tmp` directory outside the workspace, run a remote module
download/build or `go install github.com/looprig/coderig/cmd/coderig@v0.18.1`
with fresh module/build caches.

Expected: success without any sibling checkout.

### Task 11: Establish failing Carbon identity tests

**Files:**
- Rename/test: `coderig/internal/catalog/generic/generic_test.go`
- Modify: `coderig/internal/app/fingerprint_test.go`
- Modify: `coderig/internal/app/home_test.go`
- Modify: `coderig/internal/app/mcpconfig_test.go`
- Modify: `coderig/internal/app/collabmcp_test.go`
- Modify: `coderig/cmd/coderig/main_test.go`

**Step 1: Change product identity expectations before production code**

Update tests to require:

```go
if Name != "carbon" {
	t.Fatalf("Name = %q, want carbon", Name)
}
if root.Product != "Carbon" {
	t.Fatalf("product = %q, want Carbon", root.Product)
}
```

Change fingerprint expectations to `carbon:carbon`, home expectations to
`.looprig/carbon`, MCP role expectations to `carbon`, banner expectations to
`Carbon`, and the product's helper lookup expectation to
`carbon-collab-mcp`. MCP-module server tests change separately in Task 14.

**Step 2: Run focused tests and observe RED**

Run the relevant existing package tests without changing production code.

Expected: failures explicitly show old CodeRig/Generic values.

**Step 3: Commit test intent only**

Commit the failing test changes as:

```text
test: specify Carbon product identity
```

Do not push this commit to the CodeRig remote.

### Task 12: Move the worktree and change the Git remote

**Files/directories:**
- Rename: `/Users/ipotter/code/looprig/coderig/` to `/Users/ipotter/code/looprig/carbon/`
- Modify common repository configuration via `git -C carbon remote set-url`.
  The linked worktree's `carbon/.git` is a pointer to the durable common Git
  directory under `.worktrees/carbon-main-baseline`.
- Modify: `/Users/ipotter/code/looprig/go.work`

**Step 1: Record linked worktrees**

Run `git -C coderig worktree list --porcelain` and save the output in task
notes. Verify each non-prunable path before moving.

**Step 2: Move the product worktree**

From `/Users/ipotter/code/looprig`, rename `coderig` to `carbon`. Do not copy,
delete, or reinitialize `.git`.

**Step 3: Repair and verify worktrees**

Run `git -C carbon worktree repair` with the known linked paths as required,
then `git -C carbon worktree list --porcelain`.

Expected: the main worktree is `/Users/ipotter/code/looprig/carbon`; every
retained linked worktree has a valid Git directory.

**Step 4: Change only the product origin**

Run:

```bash
git -C carbon remote set-url origin git@github.com:looprig/carbon.git
git -C carbon remote -v
```

Expected: both fetch and push URLs name Carbon.

**Step 5: Point the workspace at Carbon**

Change `./coderig` to `./carbon` in root `go.work`. Run `go work sync` only
after the Carbon module declaration changes in Task 13.

### Task 13: Apply the mechanical Carbon rename

**Files/directories:**
- Rename: `carbon/cmd/coderig/` to `carbon/cmd/carbon/`
- Rename: `carbon/internal/catalog/generic/` to `carbon/internal/catalog/carbon/`
- Modify: all first-party files under `carbon/` returned by the reviewed searches
- Modify: `carbon/go.mod`

**Step 1: Capture candidate files before replacement**

Run in `carbon/`:

```bash
rg -l --hidden --glob '!**/.git/**' --glob '!**/.worktrees/**' --glob '!vendor/**' 'github\.com/looprig/coderig|coderig-collab-mcp|coderig:generic|coderig-access:generic|\.looprig/coderig|CodeRig|coderig' . | sort
rg -l --hidden --glob '!**/.git/**' --glob '!**/.worktrees/**' --glob '!vendor/**' '\bGeneric\b|"generic"|generic\.Name|generic\.SystemPrompt|internal/catalog/generic' . | sort
```

Review and retain these lists in task output.

**Step 2: Run ordered `sed` replacements**

Apply case-sensitive replacements, in this order, only to the reviewed
first-party candidate files:

```text
github.com/looprig/coderig       -> github.com/looprig/carbon
coderig-collab-mcp               -> carbon-collab-mcp
coderig-access:generic           -> carbon-access:carbon
coderig:generic                  -> carbon:carbon
~/.looprig/coderig               -> ~/.looprig/carbon
cmd/coderig                      -> cmd/carbon
bin/coderig                      -> bin/carbon
CodeRig                          -> Carbon
coderig                          -> carbon
```

Use BSD-compatible `sed -i ''` on macOS. Do not run a global lowercase
`generic` replacement.

**Step 3: Rename directories**

Rename the command and catalog directories listed above. Update package clauses
and imports from `generic` to `carbon`.

**Step 4: Replace product-agent symbols semantically**

Within `carbon/`, replace product uses of:

```text
Generic                 -> Carbon
generic.Name            -> carbon.Name
generic.Description     -> carbon.Description
generic.SystemPrompt    -> carbon.SystemPrompt
genericDefinition       -> carbonDefinition
genericToolDefinitions  -> carbonToolDefinitions
"generic" role/name     -> "carbon"
```

Leave ordinary phrases such as "generic error", generic interfaces, and the
third-party module `generic-list-go` untouched.

**Step 5: Format and run the focused GREEN tests**

Run:

```bash
gofmt -w cmd internal
GOWORK=off go test ./internal/catalog/carbon ./internal/app ./cmd/carbon -run 'TestIdentity|TestAgentKindFormat|Test.*Home|Test.*MCP.*Role|Test.*Banner' -count=1
```

Expected: PASS.

**Step 6: Commit the product rename**

Review `git diff --stat`, `git diff --check`, and every rename. Commit as:

```text
refactor: rename CodeRig and Generic to Carbon
```

### Task 14: Rename and release the Carbon collaboration helper as MCP `v0.6.0`

**Files/directories:**
- Rename: `mcp/cmd/coderig-collab-mcp/` to `mcp/cmd/carbon-collab-mcp/`
- Modify: `mcp/pkg/server/server.go`
- Modify: `mcp/pkg/server/server_test.go`
- Modify: `mcp/pkg/collab/protocol.go`
- Modify: product-specific MCP comments/docs/tests returned by `rg`

**Step 1: Confirm Carbon helper tests are RED**

Run the test changes from Task 11 against unchanged MCP production code.

Expected: failures name `coderig-collab-mcp`.

**Step 2: Rename the command and literals**

Use targeted `sed` replacement from `coderig-collab-mcp` to
`carbon-collab-mcp`, rename the directory, and update its command comment and
stderr prefix. Do not change endpoint/token environment variable contracts.

**Step 3: Run focused and full MCP verification**

Run:

```bash
GOWORK=off go test -race ./pkg/collab ./pkg/server ./cmd/carbon-collab-mcp -count=1
GOWORK=off go test -race ./... -count=1
```

Then run MCP integration, security, and cross-build gates.

Expected: PASS and no first-party `coderig-collab-mcp` occurrence remains.

**Step 4: Commit and publish MCP `v0.6.0`**

Commit as `refactor(mcp): rename collaboration helper for Carbon`, push MCP
`main`, create annotated `v0.6.0`, push, and verify remote refs.

**Step 5: Adopt MCP `v0.6.0` in Carbon**

Set Carbon's MCP requirement to `v0.6.0`, run `GOWORK=off go mod tidy`, run the
focused collaboration tests, and commit as
`build(carbon): adopt Carbon collaboration helper`.

### Task 15: Update workspace and ecosystem references

**Files:**
- Modify: `/Users/ipotter/code/looprig/go.work`
- Modify: `/Users/ipotter/code/looprig/repositories.mk`
- Modify: `/Users/ipotter/code/looprig/tests/dependency_boundary_test.go`
- Modify: `/Users/ipotter/code/looprig/tests/root_layout_test.go`
- Modify: `/Users/ipotter/code/looprig/www/src/components/TerminalDemo.astro`
- Modify: `/Users/ipotter/code/looprig/www/src/pages/roadmap.astro`
- Modify: `/Users/ipotter/code/looprig/www/looprig/docs/GLOSSARY.md`
- Modify: `/Users/ipotter/code/looprig/www/looprig/docs/consumers/larger-systems.md`
- Modify: `/Users/ipotter/code/looprig/www/looprig/docs/consumers/packages.md`
- Modify: `/Users/ipotter/code/looprig/www/looprig/profile/README.md`
- Modify: `/Users/ipotter/code/looprig/.github/profile/README.md`
- Modify: first-party files returned by the approved CodeRig-name search in
  `acp`, `classifiers`, `foreignloops`, `harness`, `inference`, `mcp`,
  `sandbox`, `tests`, `tools`, `tui`, and `www`

**Step 1: Update workspace and repository registry**

Change the `go.work` member to `./carbon`. Replace the `repositories.mk` CodeRig
entry with:

```text
"carbon|git@github.com:looprig/carbon.git|v0.1.0"
```

Keep it pending or use the exact planned tag only until Carbon `v0.1.0` is
published; repository verification must not run against a nonexistent tag.

**Step 2: Update executable product references with `sed`**

Across the reviewed first-party file list, replace CodeRig product names,
module URLs, paths, examples, and commands with Carbon forms. In reusable
modules, prefer neutral "product composition root" wording when the reference
is not truly Carbon-specific.

**Step 3: Review checked-out historical documents**

Apply the same product mapping to active first-party `docs/specs` and
verification records in the active checkout. Preserve genuinely historical
`docs/plans` records and the approved rename mapping when rewriting would
destroy former/current evidence; list those exclusions in the Task 16 audit.
Do not touch `.git` objects, vendored third-party sources, the archive tree, or
linked worktrees outside the active workspace.

**Step 4: Update boundary tests first, then production metadata**

Make tests expect `github.com/looprig/carbon` and directory `carbon`. Run:

```bash
GOWORK=off go test ./tests -run 'Test.*(Dependency|RootLayout)' -count=1
```

Expected: PASS after metadata changes.

**Step 5: Commit by repository**

Create narrow local commits in each affected repository. Push only release
repositories already authorized by this plan. Leave documentation-only sibling
commits local unless the user separately authorizes their remote push.

### Task 16: Exhaustive stale-identity audit

**Files:**
- Verify: `/Users/ipotter/code/looprig/carbon/`
- Verify: first-party active workspace repositories

**Step 1: Prove CodeRig tokens are absent**

Run from the workspace root:

```bash
rg -n -i --hidden \
  --glob '!**/.git/**' \
  --glob '!**/.worktrees/**' \
  --glob '!**/vendor/**' \
  --glob '!**/docs/plans/**' \
  --glob '!zarchive/**' \
  --glob '!go.work.sum' \
  'coderig|code[ _-]?rig|github\.com/looprig/coderig|\.looprig/coderig'
```

Expected: no first-party active runtime/source results. Classify every result rather than
blindly excluding it. The reviewed exclusions are the two rename-plan files
themselves (their former/current mapping is normative), historical reusable
Harness fingerprint vectors and the v1 replay fixture (their serialized
values are byte-level compatibility contracts), the preserved
`CODERIG_COLLAB_ENDPOINT`/`CODERIG_COLLAB_TOKEN` environment names (the MCP
wire contract), and the retired-module string in `tools/dependency_test.go`
(a regression guard that must continue to reject the former import path).
Historical plans outside the active Carbon design and implementation record,
and the archived `zarchive/` tree, are reviewed as documents but are not
runtime identity surfaces.

**Step 2: Audit Generic product-agent remnants**

Run targeted searches in Carbon:

```bash
rg -n --hidden --glob '!**/.git/**' --glob '!**/.worktrees/**' --glob '!vendor/**' '\bGeneric\b|"generic"|generic\.Name|internal/catalog/generic' carbon
```

Expected: no product-agent result. Ordinary generic terminology and
`generic-list-go` may remain only after explicit review.

**Step 3: Verify paths and Git identity**

Run:

```bash
test -d carbon
test ! -e coderig
git -C carbon remote -v
git -C carbon worktree list --porcelain
```

Expected: the active product directory and origin are Carbon; linked worktrees
are valid.

### Task 17: Full Carbon verification

**Files:**
- Verify: `/Users/ipotter/code/looprig/carbon/`
- Verify: `/Users/ipotter/code/looprig/mcp/`
- Verify: root workspace tests and website build

**Step 1: Run focused semantic suites**

Run Carbon catalog, fingerprint, persistence, MCP, collaboration, CLI, and
runtime-catalog tests with `-race -count=1`.

Expected: PASS.

**Step 2: Run Carbon standalone gates**

Run with fresh task-specific caches:

```bash
GOWORK=off go mod verify
GOWORK=off go test -race -count=1 ./...
GOWORK=off go test -tags integration -race -count=1 ./...
GOWORK=off make secure
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 GOWORK=off go build -trimpath ./...
```

Expected: PASS.

**Step 3: Run workspace and website verification**

Run root layout/dependency tests, `go work sync` followed by the workspace's
non-mutating verification targets, and the website's documented build/test
command.

Expected: PASS. Review `go.work.sum` separately and commit it only if the root
repository intentionally tracks the required change.

**Step 4: Run a remote-only Carbon module proof before tagging**

Because Carbon has not yet been tagged, create a disposable clone or archive of
the committed Carbon tree and verify it with `GOWORK=off` and fresh module
caches. Confirm `go.mod` contains no local replacements.

Expected: PASS.

### Task 18: Publish Carbon `v0.1.0`

**Files:**
- Verify only: `/Users/ipotter/code/looprig/carbon/`

**Step 1: Review the complete Carbon history boundary**

Run:

```bash
git status --short
git show-ref --verify refs/heads/main refs/heads/feat/carbon-v010-rebased
git merge-base --is-ancestor v0.18.1 HEAD
git log --oneline --decorate v0.18.1..feat/carbon-v010-rebased
git diff --stat v0.18.1..feat/carbon-v010-rebased
git tag --sort=-v:refname | head -20
```

Expected: `feat/carbon-v010-rebased` is a descendant of corrected CodeRig
`v0.18.1`, the rename commits are reviewed, and the active Carbon worktree is
clean except for no untracked files. The old baseline `main` ref remains
preserved until the explicit promotion below.

**Step 2: Promote the reviewed Carbon tip to `main`, then push without tags**

The old baseline worktree owns the local `main` ref, so promote it explicitly
without force-pushing or losing the pre-rename tip:

```bash
git -C /Users/ipotter/code/looprig/.worktrees/carbon-main-baseline branch baseline/pre-carbon-promotion main
git -C /Users/ipotter/code/looprig/.worktrees/carbon-main-baseline switch --detach main
git -C /Users/ipotter/code/looprig/carbon branch --ff-only main feat/carbon-v010-rebased
git -C /Users/ipotter/code/looprig/carbon switch main
test "$(git -C /Users/ipotter/code/looprig/carbon rev-parse HEAD)" = "$(git -C /Users/ipotter/code/looprig/carbon rev-parse main)"
```

Verify `git -C carbon log -1 --oneline` is the reviewed tip, then run:

Run exactly:

```bash
git push -u origin main
```

Do not use `--tags`, `--follow-tags`, or mirror push.

**Step 3: Create and push Carbon's first tag**

Run:

```bash
git tag -a v0.1.0 -m "carbon v0.1.0"
git push origin v0.1.0
git ls-remote origin refs/heads/main refs/tags/v0.1.0 'refs/tags/v0.1.0^{}'
```

Expected: Carbon remote `main` and the peeled tag match the verified commit.

**Step 4: Prove no CodeRig tags were pushed**

Run `git ls-remote --tags origin`.

Expected: Carbon remote contains `v0.1.0` and no CodeRig release tags.

**Step 5: Prove remote installation**

With new task-specific caches in a clean temporary directory, run:

```bash
GOWORK=off go install github.com/looprig/carbon/cmd/carbon@v0.1.0
```

Expected: success using remote modules only.

**Step 6: Final repository registry update**

If `repositories.mk` was held pending, set the Carbon entry to `v0.1.0`, run
the root repository's clone/status/verify target, and commit only the intended
root metadata.

**Step 7: Final report**

Report:

- every published module tag and verified commit;
- CodeRig `v0.18.1` remote proof;
- MCP `v0.5.0` and `v0.6.0` boundary;
- Carbon `v0.1.0` remote proof;
- exact default, race, integration, security, and cross-build results;
- stale-name audit results and reviewed exclusions;
- documentation-only sibling commits left local;
- any platform or network exception without presenting partial work as
  complete.
