# ACP Launcher Configuration and Availability Diagnostics Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement the approved design in `docs/plans/2026-08-05-acp-launcher-config-and-diagnostics-design.md`: an optional `acp_launchers` block in `models.json`, env-var-then-config-then-PATH executable resolution, and bounded secret-free startup diagnostics for dropped/reduced ACP harnesses, surfaced through the existing `PermissionDiagnostics` presentation channel.

**Architecture:** Add `acp_launchers` to the schema/normalize/digest-exclusion layers exactly like the existing `native_acp` block (schema → normalize → `productionModels` pass-through), add a pure resolution helper in `acpproduction.go` with a three-way precedence, and extend `NewACPComposition` to emit a `Diagnostics []string` alongside the existing preflight-decision bookkeeping it already computes. Diagnostics ride the existing `Config`/`sessionAccess.diagnostics`/`SessionPresentation` pipe — no new presentation API.

**Tech Stack:** Go 1.26, existing `internal/app` package, table-driven tests per `CLAUDE.md`.

---

## Before Task 1

Record the baseline:

```bash
cd /Users/ipotter/code/looprig/carbon
git status --short
go test ./internal/app/... -race -count=1 2>&1 | tail -20
```

Expected: clean tree, tests pass. Confirm `CLAUDE_CODE_ACP_EXECUTABLE` and `CODEX_ACP_EXECUTABLE` are set in this shell (`echo $CLAUDE_CODE_ACP_EXECUTABLE`) — several new tests use a fake preflight so they don't need this, but the manual end-to-end check at the end does.

---

### Task 1: Add `acp_launchers` to the wire schema

**Files:**
- Modify: `internal/app/modelconfig.go`
- Test: `internal/app/modelconfig_decode_test.go`

**Step 1: Write the failing tests**

Add to `modelconfig_decode_test.go` (follow the existing table-driven style in that file — read it first to match field names used in other cases):

```go
func TestDecodeModelConfigACPLaunchers(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "valid launchers block",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"delegate_defaults":{"planner":{"harness":"looprig"},"builder":{"harness":"looprig"},"reviewer":{"harness":"looprig"}},
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}],
				"acp_launchers":{"claude-code":{"executable":"/usr/local/bin/claude-code-acp"},"codex":{"executable":"/usr/local/bin/codex-acp"}}}`,
		},
		{
			name: "omitted launchers block is valid",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"delegate_defaults":{"planner":{"harness":"looprig"},"builder":{"harness":"looprig"},"reviewer":{"harness":"looprig"}},
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}]}`,
		},
		{
			name: "unknown field inside a launcher entry is rejected",
			json: `{"version":2,"primer_default":"p","claude_code_small_model":"p",
				"delegate_defaults":{"planner":{"harness":"looprig"},"builder":{"harness":"looprig"},"reviewer":{"harness":"looprig"}},
				"models":[{"alias":"p","provider":"anthropic","api_format":"anthropic","model":"m","uses":["primer"],"efforts":["high"],"default_effort":"high"}],
				"acp_launchers":{"claude-code":{"executable":"/usr/local/bin/claude-code-acp","args":["x"]}}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeModelConfig([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeModelConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestDecodeModelConfigACPLaunchers -v
```

Expected: FAIL — `decoder.Decode` rejects the unrecognized `acp_launchers` key on the "valid" cases (since `DisallowUnknownFields` is already active and the field doesn't exist yet).

**Step 3: Implement**

In `internal/app/modelconfig.go`, add the field to `modelConfigFile` and two new types:

```go
type modelConfigFile struct {
	Version              int                               `json:"version"`
	PrimerDefault        string                            `json:"primer_default"`
	ClaudeCodeSmallModel string                            `json:"claude_code_small_model"`
	DelegateDefaults     map[string]delegateDefaultConfig  `json:"delegate_defaults"`
	Models               []modelTargetConfig               `json:"models"`
	NativeACP            map[string]nativeACPProfileConfig `json:"native_acp"`
	ACPLaunchers         map[string]acpLauncherConfig      `json:"acp_launchers"`
}
```

```go
// acpLauncherConfig is one harness's configured ACP adapter executable
// location. It is machine-local launcher configuration, not a model
// credential: it never enters the model-configuration digest.
type acpLauncherConfig struct {
	Executable string `json:"executable"`
}
```

Use the plain struct (not a custom `UnmarshalJSON`) since there is exactly one field today; `DisallowUnknownFields` on the outer decoder already rejects an unknown nested key like `args` because `json.Decoder.DisallowUnknownFields` applies recursively to nested structs decoded through the same decoder.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestDecodeModelConfigACPLaunchers -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/modelconfig.go internal/app/modelconfig_decode_test.go
git commit -m "feat: decode optional acp_launchers block in models.json"
```

---

### Task 2: Validate and normalize `acp_launchers`

**Files:**
- Modify: `internal/app/modelconfig_normalize.go`
- Test: `internal/app/modelconfig_validate_test.go`

**Step 1: Write the failing tests**

Read `modelconfig_validate_test.go` first to match its harness helper (it likely builds a valid base `modelConfigFile` and mutates one field per case — reuse that helper). Add:

```go
func TestNormalizeModelConfigACPLaunchers(t *testing.T) {
	tests := []struct {
		name      string
		launchers map[string]acpLauncherConfig
		wantErr   bool
	}{
		{name: "nil is valid"},
		{name: "empty map is valid", launchers: map[string]acpLauncherConfig{}},
		{name: "valid claude-code and codex", launchers: map[string]acpLauncherConfig{
			"claude-code": {Executable: "/usr/local/bin/claude-code-acp"},
			"codex":       {Executable: "/usr/local/bin/codex-acp"},
		}},
		{name: "unknown harness key", launchers: map[string]acpLauncherConfig{
			"gpt-cli": {Executable: "/usr/local/bin/gpt-cli"},
		}, wantErr: true},
		{name: "empty executable", launchers: map[string]acpLauncherConfig{
			"codex": {Executable: ""},
		}, wantErr: true},
		{name: "relative executable path", launchers: map[string]acpLauncherConfig{
			"codex": {Executable: "codex-acp"},
		}, wantErr: true},
		{name: "unclean executable path", launchers: map[string]acpLauncherConfig{
			"codex": {Executable: "/usr/local/bin/../bin/codex-acp"},
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := validModelConfigFileFixture() // use the existing base-fixture helper in this test file
			config.ACPLaunchers = tt.launchers
			_, err := normalizeModelConfig(config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeModelConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

If `modelconfig_validate_test.go` has no shared fixture helper named `validModelConfigFileFixture`, find whatever the existing tests in that file actually call (grep the file first) and use that instead — do not invent a second fixture builder.

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestNormalizeModelConfigACPLaunchers -v
```

Expected: FAIL — `normalizedModelConfig` has no `ACPLaunchers` field yet, so this won't compile. That compile failure IS the expected "fails" state.

**Step 3: Implement**

In `internal/app/modelconfig_normalize.go`:

```go
type normalizedModelConfig struct {
	PrimerDefault        string
	ClaudeCodeSmallModel string
	DelegateDefaults     []normalizedDelegateDefault
	Models               []normalizedModelTarget
	NativeACP            map[string]normalizedNativeACPProfile
	ACPLaunchers         map[string]normalizedACPLauncher
}

type normalizedACPLauncher struct {
	Harness    string
	Executable string
}
```

In `normalizeModelConfig`, after the `nativeACP` block:

```go
	acpLaunchers, err := normalizeACPLaunchers(config.ACPLaunchers)
	if err != nil {
		return normalizedModelConfig{}, err
	}
	normalized.ACPLaunchers = acpLaunchers
```

New function, placed near `normalizeNativeACPProfiles`:

```go
func normalizeACPLaunchers(config map[string]acpLauncherConfig) (map[string]normalizedACPLauncher, error) {
	if config == nil {
		return nil, nil
	}
	known := map[string]struct{}{"claude-code": {}, "codex": {}}
	launchers := make(map[string]normalizedACPLauncher, len(config))
	for harness, entry := range config {
		if _, ok := known[harness]; !ok {
			return nil, modelConfigValidationError("acp_launchers contains an unknown harness")
		}
		if !isExactNonEmptyModelConfigString(entry.Executable) {
			return nil, modelConfigValidationError("acp_launchers executable must be non-empty and unpadded")
		}
		if !filepath.IsAbs(entry.Executable) || filepath.Clean(entry.Executable) != entry.Executable {
			return nil, modelConfigValidationError("acp_launchers executable must be a clean absolute path")
		}
		launchers[harness] = normalizedACPLauncher{Harness: harness, Executable: entry.Executable}
	}
	return launchers, nil
}
```

Add `"path/filepath"` to the file's import block if not already present (check first — `modelconfig.go` imports it, `modelconfig_normalize.go` may not).

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestNormalizeModelConfigACPLaunchers -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/modelconfig_normalize.go internal/app/modelconfig_validate_test.go
git commit -m "feat: validate and normalize acp_launchers"
```

---

### Task 3: Exclude `acp_launchers` from the config digest

**Files:**
- Test: `internal/app/modelconfig_digest_test.go`

**Step 1: Write the failing test**

```go
func TestModelConfigDigestExcludesACPLaunchers(t *testing.T) {
	base := validNormalizedModelConfigFixture(t) // use whatever existing helper this file's other tests call
	withLaunchers := base
	withLaunchers.ACPLaunchers = map[string]normalizedACPLauncher{
		"codex": {Harness: "codex", Executable: "/usr/local/bin/codex-acp"},
	}
	baseDigest, err := modelConfigDigest(base)
	if err != nil {
		t.Fatalf("digest(base): %v", err)
	}
	launcherDigest, err := modelConfigDigest(withLaunchers)
	if err != nil {
		t.Fatalf("digest(withLaunchers): %v", err)
	}
	if baseDigest != launcherDigest {
		t.Fatalf("acp_launchers must not change the model config digest: base=%q withLaunchers=%q", baseDigest, launcherDigest)
	}
}
```

Grep `modelconfig_digest_test.go` first for the actual fixture helper name/shape it already uses and adapt the two variable names above to match it exactly.

**Step 2: Run test to verify it fails**

It should actually PASS immediately once Task 2 compiles, because `secretFreeModelConfigJSON` never reads `config.ACPLaunchers` — nothing was added to project it. Run it anyway to confirm:

```bash
go test ./internal/app -run TestModelConfigDigestExcludesACPLaunchers -v
```

Expected: PASS on first run. This is a **characterization test**, not a red/green cycle — it locks in the "excluded from digest" invariant so a future accidental addition of the field to `secretFreeModelConfig` breaks a test instead of silently rejecting restores. No production code changes in this task.

**Step 3: Commit**

```bash
git add internal/app/modelconfig_digest_test.go
git commit -m "test: lock acp_launchers out of the model config digest"
```

---

### Task 4: Thread `ACPLaunchers` into `productionModels`

**Files:**
- Modify: `internal/app/productionmodels.go`
- Test: `internal/app/productionmodels_test.go`

**Step 1: Write the failing test**

```go
func TestCompileProductionModelsCarriesACPLaunchers(t *testing.T) {
	config := validNormalizedModelConfigFixture(t) // match existing helper name in this file
	config.ACPLaunchers = map[string]normalizedACPLauncher{
		"claude-code": {Harness: "claude-code", Executable: "/usr/local/bin/claude-code-acp"},
	}
	produced, err := compileProductionModels(config, fakeConfiguredClientFactory) // match existing fake factory name in this file
	if err != nil {
		t.Fatalf("compileProductionModels: %v", err)
	}
	if got := produced.ACPLaunchers["claude-code"]; got != "/usr/local/bin/claude-code-acp" {
		t.Fatalf("ACPLaunchers[claude-code] = %q, want the configured path", got)
	}
}
```

Adapt fixture/factory names to whatever `productionmodels_test.go` already defines — read the file first.

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestCompileProductionModelsCarriesACPLaunchers -v
```

Expected: FAIL to compile — `productionModels` has no `ACPLaunchers` field.

**Step 3: Implement**

In `internal/app/productionmodels.go`, add to `productionModels`:

```go
type productionModels struct {
	PrimerClient     inference.Client
	RuntimeClient    inference.Client
	PrimerModel      model.Model
	PrimerAlias      string
	PrimerEfforts    []model.Effort
	PrimerCandidates []PrimerCandidate
	ACP              []ACPGatewaySource
	NativeACP        map[string]ACPNativeProfile
	ACPLaunchers     map[string]string // harness -> configured executable path
	Defaults         map[identity.AgentName]configuredDelegateDefault
	ClaudeSmall      loop.ModelAlias
	ConfigRev        string
}
```

In `compileProductionModels`, after the `nativeACP` block and before the final `return`:

```go
	var acpLaunchers map[string]string
	if config.ACPLaunchers != nil {
		acpLaunchers = make(map[string]string, len(config.ACPLaunchers))
		for harness, launcher := range config.ACPLaunchers {
			acpLaunchers[harness] = launcher.Executable
		}
	}
```

Add `ACPLaunchers: acpLaunchers,` to the returned `productionModels{...}` literal.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestCompileProductionModelsCarriesACPLaunchers -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/app/productionmodels.go internal/app/productionmodels_test.go
git commit -m "feat: carry acp_launchers through to productionModels"
```

---

### Task 5: Executable resolution precedence (env → config → PATH)

**Files:**
- Modify: `internal/app/acpproduction.go`
- Test: `internal/app/acpproduction_test.go`

**Step 1: Write the failing tests**

Read `acpproduction_test.go` first for its existing style (it likely tests `preflightProductionACPExecutable` and related helpers with `t.TempDir()`-built fake executables). Add:

```go
func TestResolveACPExecutable(t *testing.T) {
	dir := t.TempDir()
	configuredPath := filepath.Join(dir, "configured-claude-code-acp")
	writeExecutableFixture(t, configuredPath) // reuse this file's existing helper that writes+chmods a fake binary; if none exists, write one inline with os.WriteFile + os.Chmod(0o755)

	pathDir := t.TempDir()
	pathExecutable := filepath.Join(pathDir, "claude-code-acp")
	writeExecutableFixture(t, pathExecutable)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name          string
		env           string
		configured    string
		wellKnownName string
		want          string
	}{
		{name: "env var wins", env: "/env/claude-code-acp", configured: configuredPath, wellKnownName: "claude-code-acp", want: "/env/claude-code-acp"},
		{name: "config wins over PATH", env: "", configured: configuredPath, wellKnownName: "claude-code-acp", want: configuredPath},
		{name: "falls back to PATH", env: "", configured: "", wellKnownName: "claude-code-acp", want: pathExecutable},
		{name: "nothing resolves", env: "", configured: "", wellKnownName: "no-such-acp-adapter-binary", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveACPExecutable(tt.env, tt.configured, tt.wellKnownName)
			if got != tt.want {
				t.Fatalf("resolveACPExecutable() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

`writeExecutableFixture` — if `acpproduction_test.go` or a sibling `_test.go` in the package doesn't already have an equivalent helper, add this small one in the same test file:

```go
func writeExecutableFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fixture executable: %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestResolveACPExecutable -v
```

Expected: FAIL to compile — `resolveACPExecutable` doesn't exist yet.

**Step 3: Implement**

In `internal/app/acpproduction.go`, add:

```go
// resolveACPExecutable picks one harness's ACP adapter executable by explicit
// precedence: the environment-variable override, then the configured
// acp_launchers path, then PATH discovery of the fixed well-known adapter
// name. It performs no existence or executability check; preflight still
// owns that. A relative PATH match is resolved to an absolute path so the
// clean-absolute-path invariant used by the rest of the ACP pipeline holds.
func resolveACPExecutable(envValue, configuredPath, wellKnownName string) string {
	if envValue != "" {
		return envValue
	}
	if configuredPath != "" {
		return configuredPath
	}
	found, err := exec.LookPath(wellKnownName)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(found) {
		if abs, absErr := filepath.Abs(found); absErr == nil {
			found = abs
		}
	}
	return found
}
```

Add `"os/exec"` to the import block.

Now wire it into `newProductionACPCompositionWithPreflight`, replacing the direct env reads:

```go
	return NewACPComposition(ACPChildrenConfig{
		Catalog: catalog,
		Executables: map[loop.AgentHarnessName]string{
			"claude-code": resolveACPExecutable(os.Getenv(acpClaudeExecutableEnv), configured.ACPLaunchers["claude-code"], "claude-code-acp"),
			"codex":       resolveACPExecutable(os.Getenv(acpCodexExecutableEnv), configured.ACPLaunchers["codex"], "codex-acp"),
		},
		...
```

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestResolveACPExecutable -v
```

Expected: PASS.

**Step 5: Run the full ACP production test file to make sure the wiring change didn't break existing behavior**

```bash
go test ./internal/app -run TestNewACPComposition -v
go test ./internal/app/... -race -count=1
```

Expected: PASS (existing tests set explicit `Executables` in `ACPChildrenConfig` directly and don't go through `newProductionACPCompositionWithPreflight`, so they're unaffected; confirm this by reading which tests call the production seam vs `NewACPComposition` directly).

**Step 6: Commit**

```bash
git add internal/app/acpproduction.go internal/app/acpproduction_test.go
git commit -m "feat: resolve ACP executables by env, then acp_launchers, then PATH"
```

---

### Task 6: Diagnostics — no-executable and preflight-failed categories

**Files:**
- Modify: `internal/app/acpchildren.go`
- Test: `internal/app/acpchildren_test.go`

**Step 1: Write the failing tests**

Read `TestNewACPCompositionPreflightsProfilesAndFiltersEnv` and `TestNewACPCompositionRejectsUnavailableConfiguredDefaultHarness` first — they already build an `ACPChildrenConfig` with a fake `executablePreflight` and a compiled `ACPCompiledCatalog`. Reuse their catalog-construction helper. Add:

```go
func TestNewACPCompositionDiagnosticsNoExecutable(t *testing.T) {
	config := <catalog fixture with a gateway-eligible or native-eligible claude-code entry, matching what TestNewACPCompositionPreflightsProfilesAndFiltersEnv builds>
	config.Executables = map[loop.AgentHarnessName]string{} // no executable resolved for either harness
	comp, err := NewACPComposition(config)
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range comp.Diagnostics {
		if strings.Contains(line, "claude-code unavailable: no executable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a no-executable diagnostic for claude-code, got %v", comp.Diagnostics)
	}
}

func TestNewACPCompositionDiagnosticsPreflightFailed(t *testing.T) {
	config := <same style of fixture, but Executables set to a real preflighting path (t.TempDir fixture file) and executablePreflight always returning ACPPreflightResult{Ready: false}>
	comp, err := NewACPComposition(config)
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range comp.Diagnostics {
		if strings.Contains(line, "codex unavailable: preflight failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a preflight-failed diagnostic for codex, got %v", comp.Diagnostics)
	}
}

func TestNewACPCompositionNoDiagnosticWhenHarnessNotConfigured(t *testing.T) {
	config := <catalog fixture with NEITHER claude-code nor codex configured at all — e.g. only the looprig native profile row>
	comp, err := NewACPComposition(config)
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	if len(comp.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics for an unconfigured harness, got %v", comp.Diagnostics)
	}
}
```

Build the exact fixtures by copying the catalog-construction pattern already used in the three existing tests named above — do not invent a new catalog builder.

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestNewACPCompositionDiagnostics -v
go test ./internal/app -run TestNewACPCompositionNoDiagnosticWhenHarnessNotConfigured -v
```

Expected: FAIL to compile — `ACPComposition.Diagnostics` doesn't exist.

**Step 3: Implement**

In `internal/app/acpchildren.go`:

1. Add `Diagnostics []string` to `ACPComposition`:

```go
type ACPComposition struct {
	Catalog     ACPCompiledCatalog
	Registry    *foreign.BuilderRegistry
	Live        foreign.Builder
	Restored    foreign.RestoredBuilder
	Diagnostics []string
}
```

2. In `NewACPComposition`, track why each configured harness was dropped. Replace the preflight loop body:

```go
	decisions := make(map[loop.AgentHarnessName]acpPreflightDecision)
	var diagnostics []string
	preflightContext := config.preflightContext
	if preflightContext == nil {
		preflightContext = context.Background()
	}
	for _, profile := range []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"} {
		if preflightContext.Err() != nil {
			break
		}
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
		if !preflightACPExecutable(config.Executables[harness]) {
			diagnostics = append(diagnostics, acpDiagnosticNoExecutable(harness))
			continue
		}
		decision := preflightACPProfile(preflightContext, config, harness, preflight)
		if decision.gatewayReady || decision.nativeReady {
			decisions[harness] = decision
			continue
		}
		diagnostics = append(diagnostics, acpDiagnosticPreflightFailed(harness, decision))
	}
```

3. Add the two category-formatting helpers near the bottom of the file:

```go
// acpDiagnosticNoExecutable and acpDiagnosticPreflightFailed produce fixed,
// secret-free category strings. They never include stderr content, provider
// messages, URLs, tokens, or full filesystem paths.
func acpDiagnosticNoExecutable(harness loop.AgentHarnessName) string {
	return fmt.Sprintf("acp: %s unavailable: no executable (set acp_launchers in models.json or its executable environment variable)", harness)
}

func acpDiagnosticPreflightFailed(harness loop.AgentHarnessName, decision acpPreflightDecision) string {
	mode := "gateway"
	if len(decision.gatewayAliases) == 0 && len(decision.nativeAliases) == 0 && !decision.nativeManagedReady {
		mode = "gateway or native"
	} else if len(decision.gatewayAliases) == 0 {
		mode = "gateway"
	} else {
		mode = "native"
	}
	return fmt.Sprintf("acp: %s unavailable: preflight failed (%s)", harness, mode)
}
```

Reconsider `acpDiagnosticPreflightFailed`'s mode logic against what actually reaches this branch: `preflightACPProfile` always attempts whichever credential modes have configured models, so a fully-failed decision means every attempted mode failed. Simplify to report exactly which modes were attempted and failed by having `preflightACPProfile` also return which modes it attempted (or, simpler: since this branch only runs when `!decision.gatewayReady && !decision.nativeReady`, just check whether the catalog had gateway-credentialed models, native-credentialed models, or both for this harness, mirroring the split already computed inside `preflightACPProfile`). Prefer the simplest correct implementation; adjust the helper signature if that reads cleaner than passing `decision`.

4. Set `Diagnostics: diagnostics` on the returned `*ACPComposition` in both the early-return branches (there are none that skip diagnostics currently since preflight always runs first) and the final return.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestNewACPCompositionDiagnostics -v
go test ./internal/app -run TestNewACPCompositionNoDiagnosticWhenHarnessNotConfigured -v
go test ./internal/app/... -race -count=1
```

Expected: PASS, no regressions.

**Step 5: Commit**

```bash
git add internal/app/acpchildren.go internal/app/acpchildren_test.go
git commit -m "feat: emit no-executable and preflight-failed ACP diagnostics"
```

---

### Task 7: Diagnostics — reduced-models category

**Files:**
- Modify: `internal/app/acpchildren.go`
- Test: `internal/app/acpchildren_test.go`

**Step 1: Write the failing test**

```go
func TestNewACPCompositionDiagnosticsReducedModels(t *testing.T) {
	config := <catalog fixture where claude-code has 2 gateway-credentialed model rows, and executablePreflight is a fake that reports Ready:true with AdvertisedModels containing only ONE of the two configured aliases>
	comp, err := NewACPComposition(config)
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}
	found := false
	for _, line := range comp.Diagnostics {
		if strings.Contains(line, "claude-code:") && strings.Contains(line, "not advertised") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a reduced-models diagnostic for claude-code, got %v", comp.Diagnostics)
	}
}
```

Model this fixture on `TestNewACPCompositionPreflightsProfilesAndFiltersEnv`, which already exercises `AdvertisedModels` filtering — adapt its catalog to configure two aliases and its fake preflight to advertise only one.

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestNewACPCompositionDiagnosticsReducedModels -v
```

Expected: FAIL — no such diagnostic emitted yet.

**Step 3: Implement**

After `filtered, err := filterACPPreflightCatalog(config.Catalog, decisions)` in `NewACPComposition`, compute the reduced-models diagnostics by comparing distinct model-alias counts per harness before and after filtering:

```go
	diagnostics = append(diagnostics, acpDiagnosticsReducedModels(config.Catalog, filtered)...)
```

Add the helper:

```go
// acpDiagnosticsReducedModels reports, per harness, how many distinct
// configured model aliases were dropped by preflight filtering. A harness
// with zero surviving models already produced a preflight-failed diagnostic
// above and is skipped here to avoid a duplicate, contradictory line.
func acpDiagnosticsReducedModels(before, after ACPCompiledCatalog) []string {
	beforeCounts := acpDistinctModelCountsByHarness(before)
	afterCounts := acpDistinctModelCountsByHarness(after)
	harnesses := make([]string, 0, len(beforeCounts))
	for harness := range beforeCounts {
		harnesses = append(harnesses, string(harness))
	}
	sort.Strings(harnesses)
	var diagnostics []string
	for _, harnessName := range harnesses {
		harness := loop.AgentHarnessName(harnessName)
		beforeCount := beforeCounts[harness]
		afterCount := afterCounts[harness]
		if afterCount == 0 || afterCount >= beforeCount {
			continue
		}
		diagnostics = append(diagnostics, fmt.Sprintf(
			"acp: %s: %d configured model(s) not advertised by the adapter", harness, beforeCount-afterCount,
		))
	}
	return diagnostics
}

func acpDistinctModelCountsByHarness(catalog ACPCompiledCatalog) map[loop.AgentHarnessName]int {
	seen := make(map[loop.AgentHarnessName]map[loop.ModelAlias]struct{})
	for _, entry := range catalog.entries {
		if entry.AgentHarness == looprigRuntimeHarness {
			continue
		}
		set, ok := seen[entry.AgentHarness]
		if !ok {
			set = make(map[loop.ModelAlias]struct{})
			seen[entry.AgentHarness] = set
		}
		for _, option := range entry.Models {
			set[option.Alias] = struct{}{}
		}
	}
	counts := make(map[loop.AgentHarnessName]int, len(seen))
	for harness, set := range seen {
		counts[harness] = len(set)
	}
	return counts
}
```

Check `sort` is already imported in `acpchildren.go` (it is, per the file's existing `sort.Strings`/`sort.Slice` calls) — no new import needed there; `fmt` is also already imported.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestNewACPCompositionDiagnosticsReducedModels -v
go test ./internal/app/... -race -count=1
```

Expected: PASS, no regressions across the full package.

**Step 5: Commit**

```bash
git add internal/app/acpchildren.go internal/app/acpchildren_test.go
git commit -m "feat: emit reduced-models ACP diagnostics"
```

---

### Task 8: Thread diagnostics through Config into session presentation

**Files:**
- Modify: `internal/app/config.go`
- Modify: `internal/app/acpproduction.go`
- Modify: `internal/app/swarm.go`
- Test: `internal/app/persistence_test.go` or a new focused test file `internal/app/acp_diagnostics_presentation_test.go`

**Step 1: Write the failing test**

Add a test proving the diagnostics reach `RuntimeAgent.SessionPresentation()`. The exact seam to test is `newWithClientUsingStores`, which already has package tests constructing a fake `productionModelsLoader`/`swarmStoresProvider` — read `swarm_test.go` or `persistence_test.go` for the existing pattern of driving that function with a fake ACP composition. If no existing test drives ACP composition through this path, test one level lower and more directly:

```go
func TestSessionPresentationSurfacesACPDiagnostics(t *testing.T) {
	access := &sessionAccess{diagnostics: []string{"existing permission diagnostic"}}
	agent := newRuntimeAgentWithPrimerCandidates(nil, nil, "/workspace", access, "", nil, nil)
	access.diagnostics = append(access.diagnostics, "acp: codex unavailable: no executable (set acp_launchers in models.json or its executable environment variable)")
	presentation := agent.SessionPresentation()
	found := false
	for _, line := range presentation.PermissionDiagnostics {
		if strings.Contains(line, "acp: codex unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ACP diagnostic in session presentation, got %v", presentation.PermissionDiagnostics)
	}
}
```

This test only proves the existing plumbing (`access.diagnostics` → `SessionPresentation`) already carries whatever is appended to `access.diagnostics` — it will PASS immediately with zero production changes, because that channel is generic. Its purpose is to pin the append point down; it should be paired with Step 3's actual wiring so a regression in the wiring, not just the channel, would be caught. If it passes trivially, extend it to call the real `newWithClientUsingStores` (or `newProductionACPCompositionWithPreflight` plus `withProductionACPChildren`) with a fake ACP composition that returns non-empty `Diagnostics`, then assert `cfg.ACPDiagnostics` is non-empty *before* touching `access` at all — that is the part that doesn't exist yet and will fail to compile.

Prefer this second form:

```go
func TestConfigCarriesACPDiagnostics(t *testing.T) {
	cfg := Config{}
	configured := productionModels{} // whatever zero-value fixture productionmodels_test.go already uses
	// Directly construct the ACPComposition your Task 6/7 changes now return with Diagnostics set,
	// bypassing the real ACP process launch by using the executablePreflight test seam already
	// present in acpchildren_test.go.
	comp := &ACPComposition{Diagnostics: []string{"acp: codex unavailable: no executable (...)"}}
	cfg.ACPChildren = comp
	cfg.ACPDiagnostics = comp.Diagnostics
	if len(cfg.ACPDiagnostics) != 1 {
		t.Fatalf("expected 1 ACP diagnostic on Config, got %d", len(cfg.ACPDiagnostics))
	}
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app -run TestConfigCarriesACPDiagnostics -v
```

Expected: FAIL to compile — `Config.ACPDiagnostics` doesn't exist.

**Step 3: Implement**

1. In `internal/app/config.go`, add near the existing `ACPChildren *ACPComposition` field:

```go
	// ACPDiagnostics are the bounded, secret-free ACP availability notices
	// produced at composition time (dropped or reduced harnesses). They ride
	// the same presentation channel as the permission-store diagnostics.
	ACPDiagnostics []string
```

2. In `internal/app/acpproduction.go`, `withProductionACPChildren`:

```go
func withProductionACPChildren(ctx context.Context, cfg Config, configured productionModels) (Config, error) {
	composition, err := newProductionACPCompositionWithPreflight(ctx, configured, nil)
	if err != nil {
		return Config{}, err
	}
	cfg.ACPChildren = composition
	cfg.RuntimeCatalog = composition.Catalog.RuntimeCatalog
	cfg.ACPDiagnostics = composition.Diagnostics
	return cfg, nil
}
```

3. In `internal/app/swarm.go`, in `newWithClientUsingStores`, immediately after `access, err := buildHeadlessAccess(cfg, root)` succeeds:

```go
	access.diagnostics = append(access.diagnostics, cfg.ACPDiagnostics...)
```

Do the same in `openRuntimeAgent` after `access, err := buildSessionAccess(cfg, root, interactive)` succeeds. Both sites already have `access` in scope and already check the error before proceeding, so this is a one-line addition right after the existing `if err != nil { return nil, err }` block in each function.

**Step 4: Run test to verify it passes**

```bash
go test ./internal/app -run TestConfigCarriesACPDiagnostics -v
go test ./internal/app/... -race -count=1
```

Expected: PASS, no regressions.

**Step 5: Commit**

```bash
git add internal/app/config.go internal/app/acpproduction.go internal/app/swarm.go internal/app/persistence_test.go
git commit -m "feat: surface ACP diagnostics through session presentation"
```

(Adjust the `git add` file list to whatever test file you actually added the test to.)

---

### Task 9: End-to-end verification against the real installed adapters

This task has no new test to write — it re-runs the existing suite plus one manual check against the two ACP adapters already installed on this machine (`claude-code-acp`, `codex-acp`, both on `PATH`) and already exported as `CLAUDE_CODE_ACP_EXECUTABLE`/`CODEX_ACP_EXECUTABLE` in `~/.zshrc`.

**Step 1: Full package test with -race**

```bash
cd /Users/ipotter/code/looprig/carbon
go test ./... -race -count=1
```

Expected: PASS.

**Step 2: `make secure`**

```bash
make secure
```

Expected: clean (gofmt, go vet, staticcheck, gosec, go mod verify, govulncheck).

**Step 3: Manual acp_launchers end-to-end check**

Temporarily unset the env vars and add an `acp_launchers` block to `~/.looprig/models.json` pointing at the same two binaries, to prove the config path (not just the env-var path) resolves and preflights successfully:

```bash
which claude-code-acp codex-acp
```

Add to `~/.looprig/models.json` (back up the file first):

```bash
cp ~/.looprig/models.json ~/.looprig/models.json.bak
```

Use `jq` to add the block non-destructively:

```bash
jq --arg claude "$(command -v claude-code-acp)" --arg codex "$(command -v codex-acp)" \
  '. + {acp_launchers: {"claude-code": {executable: $claude}, "codex": {executable: $codex}}}' \
  ~/.looprig/models.json.bak > ~/.looprig/models.json
```

Write a throwaway verification test exactly like the one used earlier in this session (`internal/app/zzz_manual_acp_verify_test.go`, deleted after use — see the conversation's manual check pattern): load production models, call `newProductionACPCompositionWithPreflight` with `env.Unsetenv` for both executable env vars in effect, and assert `comp.Catalog.HasProfile("acp/claude-code")` and `comp.Catalog.HasProfile("acp/codex")` are both true, and `comp.Diagnostics` is empty. Delete the throwaway file when done; do not commit it.

**Step 4: Restore the original models.json**

```bash
mv ~/.looprig/models.json.bak ~/.looprig/models.json
```

**Step 5: Rebuild and note completion**

```bash
make build
```

No commit for this task — it is verification only.

---

## Required Verification Checklist

- [ ] `acp_launchers` decodes, rejects unknown harness keys and unknown nested fields
- [ ] `acp_launchers` validates non-empty, clean, absolute executable paths
- [ ] `acp_launchers` is excluded from `modelConfigDigest`
- [ ] `productionModels.ACPLaunchers` carries the configured paths
- [ ] `resolveACPExecutable` precedence: env > config > PATH > none
- [ ] `ACPComposition.Diagnostics` reports no-executable, preflight-failed, and reduced-models categories
- [ ] An unconfigured harness produces zero diagnostics
- [ ] Diagnostics contain no stderr content, provider messages, URLs, tokens, or full filesystem paths beyond the fixed category text
- [ ] `Config.ACPDiagnostics` reaches `RuntimeAgent.SessionPresentation().PermissionDiagnostics`
- [ ] `go test ./... -race -count=1` and `make secure` both pass
- [ ] Manual check: `acp_launchers` alone (no env vars) resolves and preflights both adapters successfully on this machine
