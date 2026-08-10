# Fingerprint Test Compile Repair Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Restore coderig test compilation without weakening the fingerprint-field assertion.

**Architecture:** Keep the test's whole-value comparison, but use deep equality because the Harness-owned value now contains a map. Reconcile the module graph with the dependency version already required by sibling modules.

**Tech Stack:** Go 1.26, standard-library `reflect`, Go modules.

---

### Task 1: Repair the fingerprint comparison

**Files:**
- Modify: `internal/app/fingerprint_test.go`

**Step 1: Verify the failing test**

Run: `GOWORK=off go test -mod=mod ./internal/app -run '^TestOperatorFingerprintFields$'`

Expected: build failure at `fingerprint_test.go` because `rig.ConfigFingerprintFields` contains a map and cannot be compared with `!=`.

**Step 2: Implement the minimal repair**

Import `reflect` and replace:

```go
if got != tt.want {
```

with:

```go
if !reflect.DeepEqual(got, tt.want) {
```

**Step 3: Reconcile module metadata**

Run: `GOWORK=off go mod tidy`

Expected: only `github.com/looprig/inference` changes from `v0.3.0` to `v0.3.1-0.20260718005749-13e4d7f173b3`.

**Step 4: Verify the focused test**

Run: `GOWORK=off go test -race ./internal/app -run '^TestOperatorFingerprintFields$'`

Expected: PASS.

### Task 2: Repair the command test double

**Files:**
- Modify: `cmd/coderig/main_test.go`

**Step 1: Verify the failing command test build**

Run: `GOWORK=off go test -race ./...`

Expected: build failure because `orderingAgent` lacks the TUI Agent interface's
`RespondGate` method.

**Step 2: Implement the minimal repair**

Import `encoding/json` and Harness's `gate` package, then add a no-op
`RespondGate(context.Context, gate.ID, string, map[string]json.RawMessage) error`
method to `orderingAgent`.

**Step 3: Verify the command package**

Run: `GOWORK=off go test -race ./cmd/coderig`

Expected: PASS.

### Task 3: Verify coderig

**Step 1: Run full verification**

Run: `GOWORK=off go test -race ./...`

Run: `GOWORK=off go build ./...`

Expected: PASS.

**Step 2: Inspect scope**

Run: `git diff --check && git diff --stat && git status --short`

Expected: only `go.mod`, `internal/app/fingerprint_test.go`, and
`cmd/coderig/main_test.go` are modified, plus these approved planning
documents.
