# CodeRig MCP + HomeDir + Permission-Review Enablement Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Wire MCP servers from `<home>/mcp.json` into CodeRig via the existing `github.com/looprig/mcp` module, make the `~/.looprig` root configurable via `Config.HomeDir`, and enable the permission classifier via a `permission_review` section in `models.json`.

**Architecture:** All changes live in `coderig/internal/app` (plus one line in `cmd/coderig`). Design doc: `docs/plans/2026-08-05-coderig-mcp-and-permission-review-design.md` — read it fully before starting; every decision below is justified there. No harness/mcp/classifiers module changes.

**Tech Stack:** Go, `github.com/looprig/mcp` (`pkg/harness` as import alias `mcpharness`, `pkg/client`, `pkg/transport/{stdio,streamablehttp,sse}`), harness `pkg/rig` fingerprint seam, table-driven tests, `//go:build integration` for process-spawning tests.

**Working rules (repo conventions, non-negotiable):**
- TDD every task: failing test → verify fail → minimal code → verify pass → commit.
- `gofmt` changed files; `go test -race ./...` (package-scoped is fine per task) before each commit; `make secure` before the final commit of each phase.
- Commits: conventional style, **no Co-Authored-By trailer**.
- Never put header/env/credential values in error messages, digests, or logs.
- Run everything from `/Users/ipotter/code/looprig/coderig`; the root `go.work` resolves sibling modules — do NOT add replace directives or pin sibling versions in `go.mod`. You WILL need to `go get github.com/looprig/mcp@latest`-style add the dependency to `go.mod` (Task 10); with `go.work` present use `go mod tidy` and accept the workspace resolution.

---

## Phase 1 — `Config.HomeDir` and the single home resolver

### Task 1: home resolver

**Files:**
- Create: `internal/app/home.go`
- Test: `internal/app/home_test.go`

**Step 1: Write failing tests** (`home_test.go`, package `app`):

```go
func TestLooprigHome(t *testing.T) {
	t.Run("empty uses user home", func(t *testing.T) {
		got, err := looprigHome(Config{})
		if err != nil { t.Fatal(err) }
		home, _ := os.UserHomeDir()
		if got != filepath.Join(home, ".looprig") { t.Fatalf("got %q", got) }
	})
	t.Run("absolute override wins", func(t *testing.T) {
		dir := t.TempDir()
		got, err := looprigHome(Config{HomeDir: dir})
		if err != nil { t.Fatal(err) }
		if got != dir { t.Fatalf("got %q want %q", got, dir) }
	})
	t.Run("relative override rejected", func(t *testing.T) {
		if _, err := looprigHome(Config{HomeDir: "rel/path"}); err == nil {
			t.Fatal("want error for relative HomeDir")
		}
	})
}
```

**Step 2:** `go test ./internal/app/ -run TestLooprigHome -race` → FAIL (undefined `looprigHome`, undefined `Config.HomeDir`).

**Step 3: Implement.**
- Add to `Config` in `internal/app/config.go` (near the top, before the primer fields), with the doc comment from the design §Part 2 (relocates models.json, mcp.json, workspaces, default store root; empty = `~/.looprig`):

```go
	// HomeDir overrides the looprig base directory (default ~/.looprig). ...
	HomeDir string
```

- `internal/app/home.go`:

```go
// looprigHome resolves the looprig base directory: Config.HomeDir when set
// (must be absolute; fail closed otherwise), else ~/.looprig.
func looprigHome(cfg Config) (string, error) {
	if cfg.HomeDir != "" {
		if !filepath.IsAbs(cfg.HomeDir) {
			return "", fmt.Errorf("coderig: HomeDir must be absolute, got %q", cfg.HomeDir)
		}
		return cfg.HomeDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("coderig: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".looprig"), nil
}
```

**Step 4:** test → PASS. **Step 5:** `git commit -m "feat(app): Config.HomeDir and single looprig home resolver"`

### Task 2: thread the resolver through the four existing resolutions

**Files:**
- Modify: `internal/app/modelconfig.go` (~line 296-305, the function joining `home, ".looprig", "models.json"`)
- Modify: `internal/app/permissions.go` (~line 60-70, the workspaces path builder)
- Modify: `internal/app/persistence.go` (~line 51-58, `DefaultDataDir`)
- Modify: `cmd/coderig/main.go` (~line 242, the `DefaultDataDir` call site)
- Tests: extend the existing tests for each file (`modelconfig_test.go`, `permissions` tests, persistence tests) with a `HomeDir`-override case.

**Approach:** each of the three path builders currently calls `os.UserHomeDir()` directly. Change their signatures to accept the resolved home (a `string`), and have callers resolve once via `looprigHome(cfg)`. `DefaultDataDir()` is exported and called from `cmd/coderig/main.go`: add `DefaultDataDirIn(home string) (string, error)` and make `DefaultDataDir()` delegate with the default home (keeps the exported API compatible); `cmd/coderig` switches to resolving through the Config it already builds. Trace every caller of the three functions (grep) and thread the home value from the nearest place that has a `Config`. Where a caller has no Config (none expected — verify), stop and reconsider rather than plumbing globals.

**Steps:** failing test per file (e.g. models.json read from `Config{HomeDir: t.TempDir()}`), verify fail, implement, verify pass (`go test ./internal/app/ ./cmd/... -race`), then:
`git commit -m "feat(app): resolve models.json, permissions, and store root through HomeDir"`

---

## Phase 2 — permission_review in models.json

### Task 3: schema + decode

**Files:**
- Modify: `internal/app/modelconfig.go` (`modelConfigFile` struct ~line 31; decode is strict — the new field MUST be added to the struct or existing behavior can't see it, and unknown-field rejection stays intact)
- Test: `internal/app/modelconfig_permission_review_test.go`

**Step 1: Failing table-driven test.** Cases (follow the fixture style of `modelconfig_test.go` — write a temp file `0600` and load):
1. absent section → `PermissionReview == nil` on the parsed file, loader output leaves review disabled;
2. present `{"model": "haiku", "strict": true}` → parsed;
3. `{"strict": true}` without `model` → error naming `permission_review.model`;
4. unknown field inside the section → error (strict decode);
5. explicit `"permission_review": null` → treated as absent (decide: simplest is reject like other explicit-null fields in this file — match `nativeACPProfileConfig`'s precedent and REJECT with a typed error).

**Step 3: Implement.** Add to `modelConfigFile`:

```go
	PermissionReview *permissionReviewConfig `json:"permission_review"`
```

```go
type permissionReviewConfig struct {
	Model  string `json:"model"`
	Strict bool   `json:"strict"`
}
```

Give it an `UnmarshalJSON` with `DisallowUnknownFields` (copy the pattern from `delegateDefaultConfig.UnmarshalJSON`). Validation of `Model != ""` happens in Task 4's resolution step (typed `*ModelConfigError`-family error, naming the field, never a key).

**Steps 2/4/5:** fail → pass → `git commit -m "feat(app): parse optional permission_review section in models.json"`

### Task 4: resolve + enable in the loader

**Files:**
- Modify: `internal/app/models.go` (or wherever `loadModels`/`productionModels` resolves aliases — find the function that builds the `configured` value consumed by `SessionStoreFactory.Open` at `persistence.go:374`; it must gain `PermissionReview` output fields: `Enabled bool`, `Model model.Model`, `Strict bool`)
- Modify: `internal/app/persistence.go` (`Open`, ~line 385 area where `cfg.*` fields are copied from `configured`)
- Also find and update the headless path (`swarm.go:265-268` per the review) — both paths must compose identically.
- Test: extend `modelconfig_permission_review_test.go` + an assembly-level test in `internal/app/permission_review_test.go`'s style.

**Behavior (design §Part 3, exactly):**
- Section present → resolve `Model` alias against the catalogue; alias must exist and its capabilities must include `tools` AND `structured_output_with_tools`; on shortfall return a typed error naming the alias (`*ModelConfigError` family — reuse the existing capability-error type if one fits, e.g. the pattern behind `ModelConfigCapabilityError`).
- In `Open`: only if `cfg.PermissionReviewEnabled` is **false** do:

```go
	if !cfg.PermissionReviewEnabled && configured.PermissionReviewEnabled {
		cfg.PermissionReviewEnabled = true
		cfg.PermissionReviewModel = configured.PermissionReviewModel
		cfg.PermissionReviewStrictPolicy = configured.PermissionReviewStrict
	}
```

- Programmatic-enable-wins and the plain-bool "cannot force-disable" limitation get a sentence in `Config.PermissionReviewEnabled`'s doc comment.

**Test cases:** file enables when Config zero; programmatic enable + file present → programmatic model retained; missing alias → typed error; capability shortfall → typed error naming alias; absent section → disabled.

**Commit:** `git commit -m "feat(app): enable permission review from models.json permission_review section"`

### Task 5: Phase 2 docs + gate

- Update `CLAUDE.md` "Permission review" bullet **Enable/disable** to mention the models.json path (keep it accurate: still no CLI flag).
- Run `make secure` and `go test -race ./...`.
- `git commit -m "docs: permission_review models.json enablement in CLAUDE.md"`

---

## Phase 3 — mcp.json loader

### Task 6: schema types + strict decode

**Files:**
- Create: `internal/app/mcpconfig.go`
- Test: `internal/app/mcpconfig_test.go`

**Schema (design §1.1).** Wire types:

```go
type mcpConfigFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Type    string            `json:"type"`    // "stdio" | "http" | "sse" | "" (inferred)
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Roles   []string          `json:"roles"`   // looprig extension
}
```

Decode with `DisallowUnknownFields` + duplicate-key rejection (copy `modelconfig.go`'s decoder helpers; extract a shared helper if that avoids duplication — DRY, but do not restructure modelconfig beyond extraction). Output of validation is:

```go
type mcpServerSpec struct {
	name      string          // validated against client.Name rules
	kind      string          // "stdio" | "http" | "sse"
	command   string
	args      []string
	env       map[string]string
	url       string
	headers   map[string]string
	roles     []string        // normalized, sorted; empty means all three
}
```

**Validation rules to test (table-driven):**
- binding name must satisfy `client.Name.Validate()` (1-64 bytes, `[a-z0-9_-]`, alnum first byte) — call it, don't reimplement;
- `type` inference: `command` set → stdio; `url` set → http; both set or neither → error;
- explicit `type` must agree with the fields present;
- `url` must parse absolute, scheme `https`, or `http` only for loopback hosts (mirror the transport's rule; the transport re-validates — this check just fails earlier with a config-shaped error);
- `roles` ⊆ {planner, builder, reviewer}, unknown → error, duplicates → error;
- error text NEVER contains `env` or `headers` **values** (assert with `strings.Contains` on a poisoned fixture);
- empty `mcpServers` map → valid, zero specs.

**Typed error:** `type MCPConfigError struct { Binding, Field, Cause string }` following `*ModelConfigError`'s bounded style.

**Commit:** `git commit -m "feat(app): strict mcp.json schema and validation"`

### Task 7: file hygiene + load-at-boundary

**Files:**
- Modify: `internal/app/mcpconfig.go`
- Test: extend `internal/app/mcpconfig_test.go`

Add `loadMCPConfig(cfg Config) ([]mcpServerSpec, error)`:
- path = `filepath.Join(looprigHome(cfg)-result, "mcp.json")`;
- **absent file → `(nil, nil)`** — feature off;
- hygiene identical to models.json: ≤ 1 MiB, regular file, no symlink, Unix owner-only `0600` — reuse `modelconfig.go`'s open/verify helpers (`modelconfig_open_unix.go` etc.). Extract shared helpers rather than copy if the extraction is clean; otherwise a parallel `mcpconfig_open_*.go` trio is acceptable.

**Tests:** absent → nil,nil; mode `0644` → error (unix-tagged); symlink → error; >1MiB → error; happy path returns specs; `HomeDir` override honored.

**Commit:** `git commit -m "feat(app): load mcp.json with models.json file hygiene"`

---

## Phase 4 — MCP assembly

Import alias throughout: `mcpharness "github.com/looprig/mcp/pkg/harness"`, `mcpclient "github.com/looprig/mcp/pkg/client"`.

### Task 8: transports from specs

**Files:**
- Create: `internal/app/mcp.go`
- Test: `internal/app/mcp_test.go`

`func mcpDefinitions(specs []mcpServerSpec) ([]mcpharness.Binding, error)` — per spec:

- **stdio** (`transport/stdio.New(stdio.Config{...})`):
  - `Command: spec.command`, `Args: spec.args`;
  - `Env`: `stdio.EnvAllowlist{ PassThrough: []string{"PATH","HOME","TMPDIR","LANG","LC_ALL"}, Set: <spec.env as the struct requires> }` — READ `stdio.go`'s `EnvAllowlist` field names first (the design fixed the semantics: fixed pass-through baseline + explicit sets; nothing else inherited);
  - missing command → `New` fails (`exec.LookPath`) → propagate as construction failure (fail closed, design §1.5).
- **http** → `streamablehttp.New(streamablehttp.Config{ Endpoint: spec.url, Headers: <spec.headers converted to []auth.Header> })` — `HTTPClient` nil (transport builds its own from `Timeouts`; do NOT pass the session client, its non-zero `Timeout` is refused), `Timeouts` zero (defaults).
- **sse** → same shape with `sse.New`.
- Wrap in `mcpclient.Definition{ Name: mcpclient.Name(spec.name), Transport: f }` (zero Timeouts/Limits/Compat = defaults).
- Binding: `mcpharness.Binding{ Name: spec.name, Server: def, Scope: mcpharness.ScopeSession, Required: false, Visibility: mcpharness.Named(rolesOrAllThree...) }`; call `Validate()` and fail closed.

**Tests (no network, no real processes — construction only):**
- stdio spec with command `"/bin/sh"` (exists everywhere) → one valid binding, visibility permits by each configured role name;
- stdio spec with command `"definitely-not-a-command-xyzzy"` → error (fail closed);
- roles empty → `Named("planner","builder","reviewer")` — assert via `Binding.Validate()` + the selector's behavior if exported, else assert no error and correct binding count;
- http spec → valid binding; header values absent from any error on a broken variant.

**Commit:** `git commit -m "feat(app): build mcp bindings and transports from mcp.json specs"`

### Task 9: GateOpener + Reporter

**Files:**
- Modify: `internal/app/mcp.go`
- Test: extend `internal/app/mcp_test.go`

```go
// mcpGateOpener routes MCP elicitation to the session's host-owned gate. It
// is late-binding: the Manager exists before the session, so until Bind is
// called every open refuses with a typed error. Headless sessions never bind.
type mcpGateOpener struct {
	mu   sync.Mutex
	host session.GateHost // nil until Bind
}
func (o *mcpGateOpener) Bind(h session.GateHost) { ... }
func (o *mcpGateOpener) OpenGate(ctx context.Context, req mcpharness.GateRequest) (mcpharness.GateResponse, error) {
	// nil host → refuse (typed error; for headless this is the permanent answer)
}
```

READ `mcpharness.GateOpener`/`GateRequest`/`GateResponse` (`mcp/pkg/harness/deps.go:58-94`) and `session.GateHost` (`harness/pkg/session/session.go:30-80`) before writing the mapping; keep it minimal (v1 elicitation can refuse-with-reason if a faithful mapping to the host gate is large — but attempt the faithful mapping first; if you refuse-by-default instead, say so loudly in the code comment and the phase report).

Reporter: small adapter publishing `mcpharness.Notice` strings through the session event publisher used elsewhere in `internal/app` (find how compaction/permission-review surface notices; reuse that path). Nil is acceptable if no clean publisher exists at construction time — then wire it at bind time like the gate host.

**Tests:** unbound opener refuses; bound opener forwards (fake `GateHost`); headless posture = never bound.

**Commit:** `git commit -m "feat(app): late-binding MCP gate opener and notice reporter"`

### Task 10: session wiring — Manager lifecycle in openRuntimeAgent

**Files:**
- Modify: `internal/app/swarm.go` (`openRuntimeAgent`, ~line 368) and/or `internal/app/persistence.go` (`openWithClient` path) — put construction where `sessionAccess` is built and closed; follow its ownership discipline exactly.
- Modify: `internal/app/agents.go` / the `RuntimeAgent` struct — it gains the Manager/Adopter closers.
- Modify: `coderig/go.mod` — add `github.com/looprig/mcp` (workspace-resolved; `go mod tidy`).
- Test: `internal/app/mcp_integration_test.go` (see Task 12 for the live test; this task's tests are wiring-shaped with a fake).

**Order inside open (design §1.2.4, verified against the code):**

```go
specs, err := loadMCPConfig(cfg)            // nil ⇒ skip ALL of this, zero change
bindings, err := mcpDefinitions(specs)
opener := &mcpGateOpener{}                   // headless: never bound
mgr, err := mcpharness.NewManager(bindings, mcpharness.Deps{
	Gates: opener, Events: <publisher>, Reporter: <reporter>,
})
rev := mgr.ConfigDigest()                    // → Task 11: into the rig fingerprint
// ... rig.NewSession / restore as today, with ExternalCapabilityRev: rev ...
mgr.BindSession(sessionID)
opener.Bind(host)                            // interactive only; host from controller assertion
err = mgr.Start(ctx)                         // connects + discovers; optional bindings degrade here
adopter, err := mgr.StartAdoption(sessionCtrl, sessionCtrl) // EventSource + LoopControllers
_ = adopter.Install(ctx, primerLoopID, primerLoopName)      // best-effort initial install; other loops adopt at idle
```

- `Deps.SessionID` stays zero (fingerprint-first, `attach.go` contract).
- Failure handling: any error after partial construction closes what exists (`adopter.Close()` then `mgr.Close()`), mirroring `sessionAccess` partial-failure handling. `Manager.Start` returning an error for a **required** binding would fail open — we have none; verify what `Start` returns when only optional bindings fail (read `manager.go:299`'s contract) and treat optional-only failure as success.
- Shutdown: `RuntimeAgent`'s existing close path gains `adopter.Close()` → `mgr.Close()` BEFORE executor-set closes, `sync.Once`-guarded like `sessionAccess.Close`.
- No mcp.json → `mgr == nil` everywhere; every touch point nil-checks. Acceptance: with no file, the assembled rig is byte-identical to today (existing tests keep passing untouched).

**Tests:** absent-config path leaves RuntimeAgent field nil and all existing `internal/app` tests green; construction-failure cleanup (bad command in specs → open fails, nothing leaks — assert via the fake/observable closers pattern used in `access_assembly_test.go`).

**Commit:** `git commit -m "feat(app): assemble MCP manager, adoption, and lifecycle into session open"`

### Task 11: fingerprint via ExternalCapabilityRev

**Files:**
- Modify: wherever CodeRig fills `rig.ConfigFingerprintFields` / calls `rig.Define`-equivalents (`internal/app/persistence.go` — find `NativePermissionPolicyRev` usage ~line 119-123 and the fingerprint-fields construction near it).
- Test: extend `internal/app/persistence` fingerprint/restore tests (find the existing restore-drift tests, e.g. around `AllowConfigMismatch`).

Set `ExternalCapabilityRev: mgr.ConfigDigest()` when a Manager exists; when none, leave the field at its zero value (the seam's no-external-capabilities default — confirm zero is what a no-MCP fingerprint used until now, which it is, since CodeRig never set it).

**Tests:** digest present → restore with a changed mcp.json (server added / URL changed / roles changed) is rejected; `AllowConfigMismatch` escapes; header-value change does NOT change the digest (construct two Managers differing only in header values, compare `ConfigDigest()` — if the mcp module's digest ignores values this passes; if it does NOT, stop and reread `identity.go:278` before asserting); absent-file sessions restore across the change.

**Commit:** `git commit -m "feat(app): fold MCP config digest into rig fingerprint via ExternalCapabilityRev"`

---

## Phase 5 — integration proof + docs

### Task 12: stdio round-trip integration test

**Files:**
- Create: `internal/app/mcp_live_integration_test.go` (`//go:build integration`)
- Create (if no reusable one exists in the mcp module's testdata): a tiny fake MCP server binary under `internal/app/testdata/` — CHECK `mcp/pkg/...` test helpers first (`grep -rn "fake" mcp/pkg/client/*_test.go mcp/pkg/harness/*_test.go` and the module's integration tests) and reuse its fake-server if it is importable or trivially vendorable as a `TestMain`-built helper binary.

**Asserts (design §Testing):** session opens with one stdio binding; primer sees `mcp__<binding>__<tool>`; first invoke raises a gate ask with identity `mcp:<binding>:<tool>` (headless: typed approval-required denial); reviewer excluded when `roles: ["planner","builder"]`; a resolvable-but-dead server (e.g. `/bin/false`… actually a command that exits immediately) degrades: session opens, tools absent, integration event observed; child env: server echoes env — sees `PATH`, does not see an unlisted parent var; close-exactly-once (double-close of RuntimeAgent).

Run: `make test-integration` → PASS.

**Commit:** `git commit -m "test(app): live stdio MCP round-trip integration coverage"`

### Task 13: docs + final gate

**Files:**
- Modify: `CLAUDE.md` — add an "MCP servers" section: mcp.json location/format/hygiene, roles extension, fail-closed posture, fingerprint/restore behavior, and the security bullets (headers are secrets; the file is operator-managed like models.json). Add `HomeDir` to the relevant path documentation.
- Modify: `README` only if it documents configuration today (check; don't invent a section).

**Final gate:** `gofmt` check, `go test -race ./...`, `make secure`, `make test-integration`. All green, then:
`git commit -m "docs: mcp.json, HomeDir, and permission-review configuration"`

---

## Execution protocol

- One task = one commit minimum; never batch tasks into a commit.
- After Phases 2, 4, and 5: request a spec+quality review (superpowers:requesting-code-review) against the design doc before proceeding.
- Any API mismatch discovered against the design doc (a signature the doc got wrong, a missing seam): STOP, report, and get the design amended — do not improvise around it silently.
- Linux-only concerns: none expected (no sandbox/landlock surface here), but the stdio integration test must pass on darwin; if anything turns out Linux-gated, flag it for a run on the Ubuntu box.
