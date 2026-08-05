# Primer cross-provider consumer wiring Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make CodeRig's primer loop declare every `PrimerCandidates` transport to harness (via the new `loop.WithContextTransports`), so `SetModel` can live-switch between providers (e.g. lmstudio ↔ chutes) instead of rejecting the switch.

**Architecture:** A new pure function `primerContextTransports` derives one `loop.ContextTransport` per distinct (Provider, APIFormat, BaseURL) in the roster, reusing the existing `inferenceCapabilityForModel`. `conversationContextPolicy` carries that set and installs it via `loop.WithContextTransports` alongside its existing options. `swarmDefinitions` (CodeRig's one shared construction path for New/Restore/headless/interactive) passes `cfg.PrimerCandidates` through. `SetModel`'s now-obsolete cross-transport-rejection machinery is deleted.

**Tech Stack:** Go, harness `pkg/loop` (local, unreleased `main` — see prerequisite below), table-driven tests per `coderig/CLAUDE.md`.

---

## Prerequisite: local harness resolution (read before Task 0)

`coderig`'s `go.mod` pins `github.com/looprig/harness v0.19.0` (module-cache resolved), which predates the cross-provider-switching work — it still has `loop.ContextTransportBindingError`, not the new `loop.ContextTransportNotDeclaredError`/`loop.WithContextTransports`/`loop.ContextTransport`. Harness's `main` has all 12 tasks merged locally (commit `70932c57`) but is **not tagged or pushed** (`repositories.mk` still pins `harness|...|v0.19.0`).

This worktree (`coderig/.worktrees/primer-model-picker`) already has a gitignored, **uncommitted** `go.work` file at its root:

```
go 1.26.4

use .

replace github.com/looprig/harness => ../../../harness
```

This resolves `github.com/looprig/harness` to the local `~/code/looprig/harness` checkout (currently on `main`) for every command run from inside this directory, without touching the shared root `~/code/looprig/go.work` or `go.mod`, and without a premature release tag — consistent with `coderig/CLAUDE.md`'s "Use the root go.work workspace instead" guidance, scoped to this nested worktree. **Do not `git add` this file** — it's already gitignored (`go.work` / `go.work.sum` are in `.gitignore`). Every `go build`/`go test`/`go vet` command in this plan runs with **no `GOWORK=off`** (the opposite of earlier tasks in this plan) so this local workspace file is picked up. Confirm before Task 0:

```bash
cd ~/code/looprig/coderig/.worktrees/primer-model-picker
go env GOWORK   # must print this worktree's go.work path, not empty and not the root one
```

**This is temporary developer-local scaffolding**, not a substitute for the real release step. The actual harness version bump in `coderig/go.mod` (removing the need for this file) is explicit release/adoption work — out of scope for this plan, tracked as follow-up once harness is tagged and pushed.

---

## Task 0: Fix the pre-existing compile break from harness's `ContextTransportBindingError` → `ContextTransportNotDeclaredError` rename

This is a prerequisite compile fix, not new behavior — harness's H1.2 task renamed this type as a deliberate breaking change (see harness's `pkg/loop/context_transport.go`). Two files reference the old name outside of what Task 3 will delete outright.

**Files:**
- Modify: `internal/app/runtime_controls.go:158`
- Modify: `internal/app/inference_policy_test.go:180-190`

**Step 1: Confirm the break**

```bash
cd ~/code/looprig/coderig/.worktrees/primer-model-picker
go vet ./... 2>&1
```

Expected: `internal/app/runtime_controls.go:158:26: undefined: loop.ContextTransportBindingError`

**Step 2: Fix `runtime_controls.go`**

Change line 158 from:

```go
		var transportErr *loop.ContextTransportBindingError
```

to:

```go
		var transportErr *loop.ContextTransportNotDeclaredError
```

(This whole branch is deleted in Task 3 — this is the minimal fix to get back to a compiling baseline first.)

**Step 3: Fix `inference_policy_test.go`'s `TestInferencePolicyTransportBinding`**

The old `ContextTransportBindingError` carried a `Field string` naming which single field changed. The new `ContextTransportNotDeclaredError` instead carries the full candidate transport tuple (`Provider`, `APIFormat`, `BaseURL`) unconditionally — `TestInferencePolicyTransportBinding`'s intent (a definition with no declared `ContextTransport` set rejects any model whose transport differs from its base model) is unaffected by this plan's changes and still holds; only the assertion shape needs updating.

Read the current test first:

```bash
sed -n '160,190p' internal/app/inference_policy_test.go
```

Replace the `wantField string` table field and its two switch arms with a `wantRejected bool` field, and replace the final assertion block. The table becomes:

```go
	tests := []struct {
		name         string
		candidate    model.Model
		wantRejected bool
	}{
		{name: "model-local change is allowed", candidate: allowed},
		{name: "provider change is rejected", candidate: func() model.Model {
			value := allowed
			value.Provider = model.ProviderName(llm.ProviderPhala)
			return value
		}(), wantRejected: true},
		{name: "api format change is rejected", candidate: func() model.Model { value := allowed; value.APIFormat = model.APIFormatAnthropic; return value }(), wantRejected: true},
		{name: "base url change is rejected", candidate: func() model.Model { value := allowed; value.BaseURL = "https://other.example.test"; return value }(), wantRejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := definition.ValidateContextModel(tt.candidate)
			if !tt.wantRejected {
				if err != nil {
					t.Fatalf("ValidateContextModel() error = %v", err)
				}
				return
			}
			var target *loop.ContextTransportNotDeclaredError
			if !errors.As(err, &target) {
				t.Fatalf("ValidateContextModel() error = %T %v, want *loop.ContextTransportNotDeclaredError", err, err)
			}
			if target.Provider != tt.candidate.Provider || target.APIFormat != tt.candidate.APIFormat || target.BaseURL != tt.candidate.BaseURL {
				t.Errorf("error transport = {%q %q %q}, want candidate's {%q %q %q}", target.Provider, target.APIFormat, target.BaseURL, tt.candidate.Provider, tt.candidate.APIFormat, tt.candidate.BaseURL)
			}
		})
	}
```

**Step 4: Run tests to confirm the fix**

```bash
go build ./... && go vet ./... && go test ./internal/app/... -run 'TestInferencePolicyTransportBinding|TestSetModel' -v
```

Expected: builds clean, `TestInferencePolicyTransportBinding` passes. `TestSetModelCrossProviderCandidateFails` and its two siblings still pass too at this point (harness still rejects, since nothing declares extra transports yet) — that's expected; Task 3 changes their behavior.

**Step 5: Commit**

```bash
git add internal/app/runtime_controls.go internal/app/inference_policy_test.go
git commit -m "fix: adopt harness's ContextTransportNotDeclaredError rename"
```

---

## Task 1: `primerContextTransports` — derive the declared transport set from the roster

**Files:**
- Modify: `internal/app/inference_policy.go`
- Test: `internal/app/inference_policy_test.go`

**Step 1: Write the failing test**

Append to `internal/app/inference_policy_test.go` (same file, same package, so `chutesKimiK26()`/`phalaGLM52()`/`lmStudioLocal()` fixtures are already in scope):

```go
func TestPrimerContextTransports(t *testing.T) {
	t.Parallel()

	t.Run("dedups by provider/api-format/base-url", func(t *testing.T) {
		t.Parallel()
		a := chutesKimiK26()
		b := phalaGLM52()
		// c shares a's transport (same provider/format/base_url) but a different Name.
		c := model.CustomModel(a.Provider, a.APIFormat, a.BaseURL, "another-chutes-model", model.WithTools())
		candidates := []PrimerCandidate{
			{Alias: "a", Model: a},
			{Alias: "b", Model: b},
			{Alias: "c", Model: c},
		}

		transports, err := primerContextTransports(candidates)
		if err != nil {
			t.Fatalf("primerContextTransports() error = %v", err)
		}
		if len(transports) != 2 {
			t.Fatalf("transports = %#v, want 2 distinct transports (a/c share one)", transports)
		}
		for _, transport := range transports {
			if err := transport.Capability.Validate(); err != nil {
				t.Errorf("transport %+v Capability.Validate() error = %v", transport, err)
			}
		}
	})

	t.Run("propagates unsupported provider", func(t *testing.T) {
		t.Parallel()
		bad := chutesKimiK26()
		bad.Provider = model.ProviderName("unknown")
		candidates := []PrimerCandidate{{Alias: "bad", Model: bad}}

		_, err := primerContextTransports(candidates)
		var target *UnsupportedInferenceProviderError
		if !errors.As(err, &target) {
			t.Fatalf("primerContextTransports() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
		}
	})

	t.Run("empty roster returns empty set", func(t *testing.T) {
		t.Parallel()
		transports, err := primerContextTransports(nil)
		if err != nil {
			t.Fatalf("primerContextTransports(nil) error = %v", err)
		}
		if len(transports) != 0 {
			t.Fatalf("transports = %#v, want empty", transports)
		}
	})

	t.Run("capability matches inferenceCapabilityForModel for the same model", func(t *testing.T) {
		t.Parallel()
		m := chutesKimiK26()
		candidates := []PrimerCandidate{{Alias: "a", Model: m}}

		transports, err := primerContextTransports(candidates)
		if err != nil {
			t.Fatalf("primerContextTransports() error = %v", err)
		}
		want, err := inferenceCapabilityForModel(m)
		if err != nil {
			t.Fatalf("inferenceCapabilityForModel() error = %v", err)
		}
		if len(transports) != 1 || transports[0].Capability != want {
			t.Fatalf("transports = %#v, want one entry with capability %+v", transports, want)
		}
		if transports[0].Provider != m.Provider || transports[0].APIFormat != m.APIFormat || transports[0].BaseURL != m.BaseURL {
			t.Fatalf("transport identity = %+v, want it to match model %+v", transports[0], m)
		}
	})
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app/... -run TestPrimerContextTransports -v
```

Expected: FAIL with `undefined: primerContextTransports`.

**Step 3: Write minimal implementation**

In `internal/app/inference_policy.go`, add the import `"github.com/looprig/harness/pkg/loop"` and append:

```go
// primerContextTransports derives the loop-declarable transport set from the
// configured primer roster, deduplicating by (Provider, APIFormat, BaseURL) —
// harness's loop.WithContextTransports rejects duplicate-keyed members
// outright, and multiple candidates commonly share one endpoint (e.g.
// chutes-kimi-k3 and chutes-glm-5.2). Each distinct transport's capability is
// derived by the same inferenceCapabilityForModel used for the single-model
// case, so a live switch and a fresh Open resolve identical capability for
// the same transport. UnsupportedInferenceProviderError propagates exactly
// as it does for the single-model path, now checked against the whole roster.
func primerContextTransports(candidates []PrimerCandidate) ([]loop.ContextTransport, error) {
	type transportKey struct {
		Provider  model.ProviderName
		APIFormat model.APIFormat
		BaseURL   string
	}
	seen := make(map[transportKey]struct{}, len(candidates))
	transports := make([]loop.ContextTransport, 0, len(candidates))
	for _, c := range candidates {
		key := transportKey{Provider: c.Model.Provider, APIFormat: c.Model.APIFormat, BaseURL: c.Model.BaseURL}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		capability, err := inferenceCapabilityForModel(c.Model)
		if err != nil {
			return nil, err
		}
		transports = append(transports, loop.ContextTransport{
			Provider:   c.Model.Provider,
			APIFormat:  c.Model.APIFormat,
			BaseURL:    c.Model.BaseURL,
			Capability: capability,
		})
	}
	return transports, nil
}
```

**Step 4: Run test to verify it passes**

```bash
go build ./... && go test ./internal/app/... -run TestPrimerContextTransports -v
```

Expected: PASS (all 4 subtests).

**Step 5: Commit**

```bash
git add internal/app/inference_policy.go internal/app/inference_policy_test.go
git commit -m "feat: derive declared context transports from the primer roster"
```

---

## Task 2: Wire `WithContextTransports` into `conversationContextPolicy` and both `swarmDefinitions` call sites

**Files:**
- Modify: `internal/app/compaction.go`
- Modify: `internal/app/swarm.go:173,186`
- Modify: `internal/app/fingerprint_test.go:100`
- Modify: `internal/app/persistence_test.go:301`
- Test: `internal/app/compaction_test.go` (new assertions) — check first whether this file exists; if not, add to `internal/app/inference_policy_test.go` or wherever `conversationContextPolicy` is otherwise tested (grep first).

**Step 1: Find existing `conversationContextPolicy` test coverage**

```bash
grep -rln "conversationContextPolicy\b" internal/app/*_test.go
```

Add the new test into whichever file already covers `newConversationContextPolicy`'s behavior (likely `fingerprint_test.go` or a dedicated compaction test file — use what grep finds; do not create a new file if an existing one already tests this function).

**Step 2: Write the failing test**

```go
func TestConversationContextPolicyDeclaresPrimerTransports(t *testing.T) {
	t.Parallel()

	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	candidates := []PrimerCandidate{
		{Alias: "candidate-a", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Model: b, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
	}

	policy, err := newConversationContextPolicy(a, candidates)
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}

	definition, err := loop.Define(append(
		[]loop.Option{
			loop.WithName(identity.AgentName("policy-test")),
			loop.WithInference(&fakeLLM{}, a),
			loop.WithContextObservation(loop.ContextObservationPolicy{ReservedOutput: 1, SafetyMargin: 1, CountTimeout: time.Second}),
		},
		policy.options()...,
	)...)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	// b's transport must now be declared: ValidateContextModel must accept it,
	// where before this change (no WithContextTransports) it would reject any
	// transport other than a's own.
	if err := definition.ValidateContextModel(b); err != nil {
		t.Fatalf("ValidateContextModel(b) error = %v, want b's transport accepted", err)
	}
}

func TestConversationContextPolicyWithNoPrimerCandidatesStaysSingleTransport(t *testing.T) {
	t.Parallel()

	a := testModel()
	policy, err := newConversationContextPolicy(a, nil)
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}

	definition, err := loop.Define(append(
		[]loop.Option{
			loop.WithName(identity.AgentName("policy-test")),
			loop.WithInference(&fakeLLM{}, a),
			loop.WithContextObservation(loop.ContextObservationPolicy{ReservedOutput: 1, SafetyMargin: 1, CountTimeout: time.Second}),
		},
		policy.options()...,
	)...)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}

	other := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "other-model", model.WithTools())
	var target *loop.ContextTransportNotDeclaredError
	if err := definition.ValidateContextModel(other); !errors.As(err, &target) {
		t.Fatalf("ValidateContextModel(other) error = %v, want *loop.ContextTransportNotDeclaredError (no primer candidates -> single-transport default)", err)
	}
}
```

Add whatever imports are missing (`"github.com/looprig/harness/pkg/identity"`, `"github.com/looprig/harness/pkg/loop"`, `"time"`, `"errors"`) — check the target file's existing imports first; several are likely already present.

**Step 2: Run tests to verify they fail**

```bash
go build ./... 2>&1 | head -30
```

Expected: FAIL to build — `newConversationContextPolicy(a, candidates)` doesn't match the current one-argument signature. This is expected; the whole package won't build until Step 3 lands, including the two other test call sites — fix those in the same step (see below) rather than leaving the package uncompilable between steps.

**Step 3: Implement**

In `internal/app/compaction.go`:

```go
type conversationContextPolicy struct {
	counter         contextcount.ContextCounter
	capability      contextcount.InferenceCapability
	transports      []loop.ContextTransport
	compaction      loop.CompactionPolicy
	summaryFragment string
	summaryRevision string
}

// newConversationContextPolicy resolves and validates the model-specific,
// secret-free context contract before any session is opened. primerCandidates
// is the full configured roster (may be nil/empty outside the primer-picker
// path, or when no candidates are configured) — its distinct transports
// become the loop's declared ContextTransport set, so a live SetModel can
// move between any of them. model must itself resolve to one of those
// transports when primerCandidates is non-empty (swarmDefinitions' callers
// already guarantee this: model is always drawn from PrimerCandidates or is
// the sole configured model).
func newConversationContextPolicy(model model.Model, primerCandidates []PrimerCandidate) (conversationContextPolicy, error) {
	inferencePolicy, err := newModelInferencePolicy(model)
	if err != nil {
		return conversationContextPolicy{}, err
	}
	transports, err := primerContextTransports(primerCandidates)
	if err != nil {
		return conversationContextPolicy{}, err
	}
	compaction := conversationCompactionPolicy()
	if err := compaction.Validate(inferencePolicy.ContextCounter().CounterCapability()); err != nil {
		return conversationContextPolicy{}, err
	}
	return conversationContextPolicy{
		counter:         inferencePolicy.ContextCounter(),
		capability:      inferencePolicy.InferenceCapability(),
		transports:      transports,
		compaction:      compaction,
		summaryFragment: conversationSummaryConsumptionFragment,
		summaryRevision: conversationSummaryConsumptionRevision,
	}, nil
}

// options returns fresh loop options so each definition installs the complete
// shared context contract without sharing mutable option slices.
func (p conversationContextPolicy) options() []loop.Option {
	return []loop.Option{
		loop.WithContextCounter(p.counter),
		loop.WithInferenceCapability(p.capability),
		loop.WithContextTransports(p.transports...),
		loop.WithCompaction(p.compaction),
	}
}
```

In `internal/app/swarm.go`, change both call sites:

```go
// line ~173, inside swarmDefinitions:
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates)
```

```go
// line ~186, inside swarmDefinitionsWithAdditionalTools:
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates)
```

Fix the two pre-existing test call sites to compile (they don't exercise the primer roster, so pass `nil`):

`internal/app/fingerprint_test.go:100`:

```go
	basePolicy, err := newConversationContextPolicy(testModel(), nil)
```

`internal/app/persistence_test.go:301`:

```go
				policy, err := newConversationContextPolicy(testModel(), nil)
```

**Step 4: Run tests to verify they pass**

```bash
go build ./... && go vet ./... && go test ./internal/app/... -run 'TestConversationContextPolicy|TestCompactionCompositionFingerprint|TestSessionRestore' -v
go test ./internal/app/... 2>&1 | tail -40
```

Expected: builds clean; new tests pass; full package test run has no new failures (the three `TestSetModelCrossProvider*` tests in `runtime_controls_test.go` are expected to now *pass differently or fail* — leave them for Task 3, don't fix them here).

**Step 5: Commit**

```bash
git add internal/app/compaction.go internal/app/swarm.go internal/app/fingerprint_test.go internal/app/persistence_test.go
git commit -m "feat: declare primer roster's transports on every native loop definition"
```

---

## Task 2.4: Generalize `inferenceCapabilityForModel` to any models.json-configured provider

**Revised after a second-opinion review** found the first draft of Task 2.5 (below) would make the motivating regression test fail even harder: applying the existing `inferenceCapabilityForModel` (chutes/phala/lmstudio only, hard `UnsupportedInferenceProviderError` otherwise) to delegate models means an `"openai"` delegate — a real, already-supported `models.json` configuration — would now fail at `Open()` itself. Asked directly, the product decision is: **any provider and model listed in `models.json` should be usable**, not just the three specially-reviewed ones. See the design doc's addendum "Revision 1" for the full reasoning.

`internal/app/modelconfig_normalize.go` already validates every configured model's provider via `llm.Provider(target.Provider).RequiredAuth()` at load time — a provider that function doesn't recognize can never reach production code as a `model.Model`. Reusing that exact function as the validity gate here means: any provider real enough to pass models.json normalization gets a conservative, generically-correct capability; a provider that function itself doesn't recognize (typo, bogus test value) still hard-fails, preserving CLAUDE.md's fail-closed posture for genuinely unknown input.

**Files:**
- Modify: `internal/app/inference_policy.go`
- Test: `internal/app/inference_policy_test.go`

**Step 1: Write the failing test**

Append to `internal/app/inference_policy_test.go`:

```go
func TestInferenceCapabilityForModelSupportsAnyKnownProvider(t *testing.T) {
	t.Parallel()

	t.Run("a provider outside the reviewed three gets a conservative default", func(t *testing.T) {
		t.Parallel()
		m := model.CustomModel(model.ProviderName(llm.ProviderOpenAI), model.APIFormatOpenAIResponses, "", "gpt-test", model.WithTools())

		capability, err := inferenceCapabilityForModel(m)
		if err != nil {
			t.Fatalf("inferenceCapabilityForModel() error = %v", err)
		}
		if err := capability.Validate(); err != nil {
			t.Fatalf("capability.Validate() error = %v", err)
		}
		if capability.Transport != contextcount.InferenceTransportTLS {
			t.Errorf("Transport = %v, want InferenceTransportTLS", capability.Transport)
		}
		if capability.Retention != contextcount.RetentionUnknown {
			t.Errorf("Retention = %v, want RetentionUnknown", capability.Retention)
		}
		if capability.SecurityIdentity == (contextcount.SecurityIdentity{}) {
			t.Errorf("SecurityIdentity is zero, want a derived non-zero identity (contextcount.InferenceCapability.Validate() requires non-zero SecurityIdentity for any transport at or above TLS)")
		}
	})

	t.Run("a provider llm itself doesn't recognize still fails closed", func(t *testing.T) {
		t.Parallel()
		m := model.CustomModel(model.ProviderName("not-a-real-provider"), model.APIFormatOpenAI, "https://bad.example.test", "bad-model", model.WithTools())

		_, err := inferenceCapabilityForModel(m)
		var target *UnsupportedInferenceProviderError
		if !errors.As(err, &target) {
			t.Fatalf("inferenceCapabilityForModel() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
		}
	})
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app/... -run TestInferenceCapabilityForModelSupportsAnyKnownProvider -v
```

Expected: FAIL — the first subtest gets `*UnsupportedInferenceProviderError` instead of a valid capability (current default branch rejects `openai` unconditionally).

**Step 3: Implement**

In `internal/app/inference_policy.go`, add a new revision constant next to the existing two:

```go
const (
	chutesInferenceIdentityRevision  = "chutes-e2ee-tee-v1"
	phalaInferenceIdentityRevision   = "phala-aci-e2ee-v1"
	genericInferenceIdentityRevision = "generic-tls-v1"
)
```

Then change `inferenceCapabilityForModel`'s default branch:

```go
func inferenceCapabilityForModel(model model.Model) (contextcount.InferenceCapability, error) {
	provider := llm.Provider(model.Provider)
	switch provider {
	case llm.ProviderChutes:
		return protectedInferenceCapability(model, chutesInferenceIdentityRevision), nil
	case llm.ProviderPhala:
		return protectedInferenceCapability(model, phalaInferenceIdentityRevision), nil
	case llm.ProviderLMStudio:
		return contextcount.InferenceCapability{
			Provider:  contextcount.ProviderID(model.Provider),
			Transport: contextcount.InferenceTransportLocal,
			Retention: contextcount.RetentionNone,
		}, nil
	default:
		// Any provider modelconfig_normalize.go's llm.Provider(...).RequiredAuth()
		// gate would also accept gets the same conservative, unreviewed-tier
		// capability: plain TLS to a remote endpoint, retention unknown. A
		// provider RequiredAuth itself doesn't recognize still fails closed —
		// this keeps the fail-closed posture for genuinely unknown input
		// while extending trust to exactly what models.json's own
		// normalization already trusts, no further. SecurityIdentity is
		// still derived (contextcount.InferenceCapability.Validate() requires
		// non-zero SecurityIdentity for any transport at or above TLS) using
		// the same transportSecurityIdentity helper chutes/phala use, with a
		// revision string marking this tier as generic/unreviewed rather than
		// naming a specific reviewed provider policy — SecurityIdentity's
		// role is a stable comparison fingerprint of transport identity plus
		// revision, not a claim of review; the "no TEE-attestation review"
		// distinction is fully carried by Transport/Retention, not by this
		// field's zero-ness.
		if _, err := provider.RequiredAuth(); err != nil {
			return contextcount.InferenceCapability{}, &UnsupportedInferenceProviderError{Provider: model.Provider}
		}
		return contextcount.InferenceCapability{
			Provider:         contextcount.ProviderID(model.Provider),
			Transport:        contextcount.InferenceTransportTLS,
			SecurityIdentity: transportSecurityIdentity(model, genericInferenceIdentityRevision),
			Retention:        contextcount.RetentionUnknown,
		}, nil
	}
}
```

**Step 4: Run tests to verify they pass**

```bash
go build ./... && go test ./internal/app/... -run 'TestInferenceCapabilityForModelSupportsAnyKnownProvider|TestNewModelInferencePolicy|TestPrimerContextTransports' -v
```

Expected: PASS on all — including the pre-existing `TestNewModelInferencePolicy`'s "unknown provider fails closed" case (it uses `model.ProviderName("unknown")`, which `llm.Provider.RequiredAuth()` doesn't recognize either, so it still correctly fails closed) and `TestPrimerContextTransports` (unaffected — it never exercised the default branch with a real-but-unreviewed provider).

**Step 5: Commit**

```bash
git add internal/app/inference_policy.go internal/app/inference_policy_test.go
git commit -m "feat: support any models.json-configured provider's inference capability"
```

---

## Task 2.5: Also declare gateway-backed delegate models' transports (restore regression fix)

**Discovered while running Task 2's verification**, not part of the original design: `TestPersistedOpenRoutesNativeAgentThroughRuntimeClientAcrossRestore` fails on restore with harness's `RestoreTransportMismatchError`. This is a real regression from adopting harness's new `main` (not caused by Tasks 0-2's own changes — confirmed present before Task 2's wiring too), but this plan's scope now covers closing it. See the design doc's "Addendum (2026-08-06)" section (`docs/plans/2026-08-05-primer-cross-provider-consumer-design.md`) for the full diagnosis, including "Revision 2" — a second, independent bug a second-opinion review found in the first draft of this task, described below.

CodeRig's native (in-process, `RuntimeClient`-routed) delegate loops — spawned via `StartAgent` against a `configuredDelegateDefault`/`ACPGatewaySource` entry (models.json's gateway-backed delegate catalog, distinct from `uses:["primer"]`) — are ordinary harness `loop.Definition` instances, subject to the same declared-transport restore check as the three primer roles. Their models were never part of `PrimerCandidates`, so their transports were never declared. `NativeACP` delegates are unaffected (separate harness's own login state, never bind to a CodeRig-owned `loop.Definition` — see `coderig/CLAUDE.md`) and stay out of scope.

**Base-transport membership.** Harness's `validateContextDefinition` (`pkg/loop/definition.go`) requires a non-empty declared `ContextTransport` set to contain a member matching the loop's own base `WithInference` model, with `Capability` exactly equal to `WithInferenceCapability`, or `Define()` itself rejects with `DefinitionInvalidContextTransport` — this is a *build-time* check, not restore-only. The failing test's shape (empty `PrimerCandidates`, one delegate) has no guaranteed base-model membership if the merged set is built from candidates+delegates alone. Fix: the merge function takes the base model as an explicit first parameter and always seeds it first.

**`primerContextTransports` (Task 1) is retired, not kept alongside the new function.** Once every real call site needs the base-model-seeded, delegate-merged set, a primer-only entry point with no base-membership guarantee has no legitimate caller left. Its dedup core survives as `contextTransportsForModels`; the function itself and its dedicated test (`TestPrimerContextTransports`) are deleted in this task. This is a normal mid-plan revision, not a redo of Task 1's review — Task 1's function was correct for the scope it was reviewed against; new information changed what shape the real call site needs.

**Files:**
- Modify: `internal/app/inference_policy.go` (retire `primerContextTransports`; add `declaredContextTransports`)
- Modify: `internal/app/compaction.go` (`newConversationContextPolicy` gains a `delegateModels` parameter)
- Modify: `internal/app/config.go` (new `Config.DelegateModels []model.Model` field)
- Modify: `internal/app/swarm.go` (both call sites; `newWithProductionModelsLoader` populates `cfg.DelegateModels`)
- Modify: `internal/app/persistence.go` (`SessionStoreFactory.Open` populates `cfg.DelegateModels`)
- Modify: `internal/app/fingerprint_test.go`, `internal/app/persistence_test.go` (existing `newConversationContextPolicy` call sites gain a third `nil` arg)
- Test: `internal/app/inference_policy_test.go` (remove `TestPrimerContextTransports`, add the new test below)

**Step 1: Write the failing test**

In `internal/app/inference_policy_test.go`, **delete `TestPrimerContextTransports` entirely** (it tests a function this task retires) and append:

```go
func TestDeclaredContextTransportsMergesBasePrimerAndDelegateModels(t *testing.T) {
	t.Parallel()

	primer := testModel() // lmstudio
	primerCandidates := []PrimerCandidate{{Alias: "primer", Model: primer}}

	t.Run("base model is always included even with no candidates or delegates", func(t *testing.T) {
		t.Parallel()
		transports, err := declaredContextTransports(primer, nil, nil)
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 1 || transports[0].Provider != primer.Provider || transports[0].APIFormat != primer.APIFormat || transports[0].BaseURL != primer.BaseURL {
			t.Fatalf("transports = %#v, want exactly the base model's transport", transports)
		}
	})

	t.Run("base model is included even when absent from PrimerCandidates", func(t *testing.T) {
		t.Parallel()
		// PrimerCandidates deliberately does NOT include primer here — this is
		// the exact shape that broke the first draft of this fix (empty
		// PrimerCandidates, base model only implied by the definition's own
		// WithInference call).
		transports, err := declaredContextTransports(primer, nil, nil)
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		found := false
		for _, tr := range transports {
			if tr.Provider == primer.Provider && tr.APIFormat == primer.APIFormat && tr.BaseURL == primer.BaseURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("transports = %#v, want base model's own transport included", transports)
		}
	})

	t.Run("delegate on a foreign transport is included", func(t *testing.T) {
		t.Parallel()
		delegate := model.CustomModel(model.ProviderName(llm.ProviderOpenAI), model.APIFormatOpenAIResponses, "", "delegate-model", model.WithTools(), model.WithThinking())

		transports, err := declaredContextTransports(primer, primerCandidates, []model.Model{delegate})
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 2 {
			t.Fatalf("transports = %#v, want 2 (primer + delegate)", transports)
		}
		found := false
		for _, tr := range transports {
			if tr.Provider == delegate.Provider && tr.APIFormat == delegate.APIFormat && tr.BaseURL == delegate.BaseURL {
				found = true
			}
		}
		if !found {
			t.Fatalf("transports = %#v, want delegate's transport included", transports)
		}
	})

	t.Run("delegate sharing the primer's transport does not duplicate", func(t *testing.T) {
		t.Parallel()
		delegate := model.CustomModel(primer.Provider, primer.APIFormat, primer.BaseURL, "delegate-same-transport", model.WithTools())

		transports, err := declaredContextTransports(primer, primerCandidates, []model.Model{delegate})
		if err != nil {
			t.Fatalf("declaredContextTransports() error = %v", err)
		}
		if len(transports) != 1 {
			t.Fatalf("transports = %#v, want 1 (shared transport deduped)", transports)
		}
	})

	t.Run("propagates unsupported delegate provider", func(t *testing.T) {
		t.Parallel()
		bad := model.CustomModel("not-a-real-provider", model.APIFormatOpenAI, "https://bad.example.test", "bad-delegate", model.WithTools())

		_, err := declaredContextTransports(primer, primerCandidates, []model.Model{bad})
		var target *UnsupportedInferenceProviderError
		if !errors.As(err, &target) {
			t.Fatalf("declaredContextTransports() error = %T %v, want *UnsupportedInferenceProviderError", err, err)
		}
	})
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/app/... -run TestDeclaredContextTransportsMergesBasePrimerAndDelegateModels -v
```

Expected: FAIL with `undefined: declaredContextTransports`. (`TestPrimerContextTransports` is gone, so no separate failure there — its deletion is part of this same commit's diff, not a separate step.)

**Step 3: Implement**

In `internal/app/inference_policy.go`, remove `primerContextTransports` entirely and add:

```go
// contextTransportsForModels derives the deduplicated loop-declarable
// transport set (by Provider, APIFormat, BaseURL) across models, in order,
// keeping the first occurrence of each distinct transport. The sole
// primitive behind declaredContextTransports.
func contextTransportsForModels(models []model.Model) ([]loop.ContextTransport, error) {
	type transportKey struct {
		Provider  model.ProviderName
		APIFormat model.APIFormat
		BaseURL   string
	}
	seen := make(map[transportKey]struct{}, len(models))
	transports := make([]loop.ContextTransport, 0, len(models))
	for _, m := range models {
		key := transportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		capability, err := inferenceCapabilityForModel(m)
		if err != nil {
			return nil, err
		}
		transports = append(transports, loop.ContextTransport{
			Provider:   m.Provider,
			APIFormat:  m.APIFormat,
			BaseURL:    m.BaseURL,
			Capability: capability,
		})
	}
	return transports, nil
}

// declaredContextTransports derives the full loop-declarable transport set:
// the loop's own base model, every configured primer candidate's transport,
// and every configured gateway-backed delegate model's transport (native,
// in-process, RuntimeClient-routed StartAgent delegates — NOT NativeACP,
// which runs via a separate harness's own login state and never binds to a
// CodeRig-owned loop.Definition). base is always seeded first so harness's
// build-time base-transport-membership requirement
// (pkg/loop/definition.go's validateContextDefinition: a non-empty declared
// set must contain a member matching the loop's own WithInference model,
// with Capability exactly equal to WithInferenceCapability) holds
// regardless of whether base happens to also appear in primerCandidates —
// equality is automatic since both are derived by calling
// inferenceCapabilityForModel on the same model value. Native delegate
// loops are ordinary harness Loop instances subject to the same
// declared-transport restore check as the primer roles, so omitting their
// transport here would make restoring a session with an active/prior
// delegate on a foreign transport fail harness's RestoreTransportMismatchError.
func declaredContextTransports(base model.Model, primerCandidates []PrimerCandidate, delegateModels []model.Model) ([]loop.ContextTransport, error) {
	models := make([]model.Model, 0, 1+len(primerCandidates)+len(delegateModels))
	models = append(models, base)
	for _, c := range primerCandidates {
		models = append(models, c.Model)
	}
	models = append(models, delegateModels...)
	return contextTransportsForModels(models)
}
```

**Step 4: Run test to verify it passes**

```bash
go build ./... && go test ./internal/app/... -run TestDeclaredContextTransportsMergesBasePrimerAndDelegateModels -v
```

Expected: PASS (all 5 subtests).

**Step 5: Wire `Config.DelegateModels` and thread it through**

In `internal/app/config.go`, add a field next to `PrimerCandidates`:

```go
	// DelegateModels is every configured gateway-backed delegate model
	// (models.json's ACPGatewaySource catalog) — the models StartAgent can
	// bind a native, in-process delegate loop to. Every native loop declares
	// these transports alongside PrimerCandidates' so a session with an
	// active or prior delegate on a different transport than the primer can
	// still restore. NativeACP delegates are not included: they never bind
	// to a CodeRig-owned loop.Definition.
	DelegateModels []model.Model
```

In `internal/app/compaction.go`, change `newConversationContextPolicy`'s signature and body:

```go
func newConversationContextPolicy(model model.Model, primerCandidates []PrimerCandidate, delegateModels []model.Model) (conversationContextPolicy, error) {
	inferencePolicy, err := newModelInferencePolicy(model)
	if err != nil {
		return conversationContextPolicy{}, err
	}
	transports, err := declaredContextTransports(model, primerCandidates, delegateModels)
	if err != nil {
		return conversationContextPolicy{}, err
	}
	compaction := conversationCompactionPolicy()
	if err := compaction.Validate(inferencePolicy.ContextCounter().CounterCapability()); err != nil {
		return conversationContextPolicy{}, err
	}
	return conversationContextPolicy{
		counter:         inferencePolicy.ContextCounter(),
		capability:      inferencePolicy.InferenceCapability(),
		transports:      transports,
		compaction:      compaction,
		summaryFragment: conversationSummaryConsumptionFragment,
		summaryRevision: conversationSummaryConsumptionRevision,
	}, nil
}
```

(`options()` is unchanged — it already installs `p.transports` via `loop.WithContextTransports`, regardless of which function derived them.)

In `internal/app/swarm.go`, update both call sites to pass the new argument:

```go
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates, cfg.DelegateModels)
```

(both in `swarmDefinitions` and `swarmDefinitionsWithAdditionalTools`).

In the same file, `newWithProductionModelsLoader` (where `cfg.PrimerCandidates` is set from `configured.PrimerCandidates`) gains one more line:

```go
	cfg.PrimerCandidates = append([]PrimerCandidate(nil), configured.PrimerCandidates...)
	cfg.DelegateModels = delegateModelsFrom(configured.ACP)
```

Add the small mapper near `PrimerCandidate`/`ACPGatewaySource`'s definitions (`internal/app/inference_policy.go` is a reasonable home, next to `declaredContextTransports`, or `productionmodels.go` next to `ACPGatewaySource` — pick whichever the codebase's existing convention favors after a quick look; either is fine):

```go
func delegateModelsFrom(sources []ACPGatewaySource) []model.Model {
	models := make([]model.Model, len(sources))
	for i, s := range sources {
		models[i] = s.Model
	}
	return models
}
```

In `internal/app/persistence.go`'s `SessionStoreFactory.Open` (the second, independent place `cfg.PrimerCandidates` is set — see line ~388), add the matching line:

```go
	cfg.PrimerCandidates = append([]PrimerCandidate(nil), configured.PrimerCandidates...)
	cfg.DelegateModels = delegateModelsFrom(configured.ACP)
```

Fix the two pre-existing test call sites (`fingerprint_test.go`, `persistence_test.go`) to pass a third `nil` argument:

```go
	basePolicy, err := newConversationContextPolicy(testModel(), nil, nil)
```

```go
				policy, err := newConversationContextPolicy(testModel(), nil, nil)
```

**Step 6: Run tests to verify the regression is fixed**

```bash
go build ./... && go vet ./...
go test ./internal/app/... -run TestPersistedOpenRoutesNativeAgentThroughRuntimeClientAcrossRestore -v
go test ./internal/app/... 2>&1 | tail -60
```

Expected: `TestPersistedOpenRoutesNativeAgentThroughRuntimeClientAcrossRestore` now PASSES. Full package run has no new failures beyond the three `TestSetModelCrossProvider*` tests (still expected to differ/fail until Task 3 lands — do not touch them here).

**Step 7: Commit**

```bash
git add internal/app/inference_policy.go internal/app/inference_policy_test.go internal/app/compaction.go internal/app/config.go internal/app/swarm.go internal/app/persistence.go internal/app/fingerprint_test.go internal/app/persistence_test.go
git commit -m "fix: declare gateway-backed delegate models' transports alongside the primer roster"
```

---

## Task 3: Simplify `SetModel`, delete the now-dead cross-transport-rejection machinery, prove cross-provider switching works

**Files:**
- Modify: `internal/app/runtime_controls.go`
- Modify: `internal/app/runtime_controls_test.go`

**Step 1: Read current state and confirm what Task 2 changed**

By this point, `TestSetModelCrossProviderCandidateFails` should already be **passing differently than intended** — SetModel still returns an error (harness's `Change` may now fail for an unrelated reason, or may actually succeed) because `SetModel`'s own logic hasn't changed yet. Run it to see current behavior before touching anything:

```bash
go test ./internal/app/... -run TestSetModelCrossProvider -v
```

Note the actual output (don't assume) — this confirms exactly what Task 3's rewrite needs to change.

**Step 2: Delete the dead code in `runtime_controls.go`**

Remove `primerTransportSwitchError` (the whole type, lines ~171-193), `sameTransport` (lines ~237-244), and `liveSwitchAlternativesMessage` (lines ~246-269) entirely.

Simplify `SetModel`'s error handling — replace:

```go
	if err := controller.Change(ctx, changes...); err != nil {
		var transportErr *loop.ContextTransportNotDeclaredError
		if errors.As(err, &transportErr) {
			return &primerTransportSwitchError{
				id:           id,
				alternatives: liveSwitchAlternativesMessage(a.primerCandidates, currentModel, candidate.Alias),
				cause:        err,
			}
		}
		return err
	}
	return nil
```

with:

```go
	if err := controller.Change(ctx, changes...); err != nil {
		return fmt.Errorf("coderig: switch to model %q: %w", id, err)
	}
	return nil
```

`currentModel` is still used earlier in the function (for the effort-admission check) — leave that line alone; only the error-handling branch changes. Remove the now-unused `errors` and `strings` imports if nothing else in the file uses them (check with `goimports`/`go build` after editing).

**Step 3: Rewrite `runtime_controls_test.go`'s three cross-provider tests**

Delete `TestSetModelCrossProviderErrorNamesLiveAlternatives` and `TestSetModelCrossProviderErrorReportsNoAlternatives` entirely (lines ~267-306) — they test message-formatting behavior for machinery that no longer exists. There's nothing to repurpose: once all declared candidates are live-switchable, there's no "alternatives" framing left to test.

Replace `TestSetModelCrossProviderCandidateFails` (and its doc comment) with a test proving the switch now **succeeds**:

```go
// TestSetModelSwitchesAcrossProviders proves the limitation documented at
// this test's previous incarnation (TestSetModelCrossProviderCandidateFails)
// no longer holds: harness's loop.WithContextTransports (see
// docs/plans/2026-08-05-primer-cross-provider-consumer-design.md) lets a
// loop definition declare more than one admitted transport, and
// conversationContextPolicy now declares every configured PrimerCandidates
// transport. A live SetModel between two candidates naming genuinely
// different providers (lmstudio candidate-a -> chutes candidate-b) now
// succeeds instead of being rejected.
func TestSetModelSwitchesAcrossProviders(t *testing.T) {
	a := testModel()
	b := model.CustomModel("chutes", model.APIFormatOpenAI, "https://api.chutes.ai", "candidate-b", model.WithTools(), model.WithThinking())
	candidates := []PrimerCandidate{
		{Alias: "candidate-a", Description: "Candidate A", Model: a, Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone},
		{Alias: "candidate-b", Description: "Candidate B", Model: b, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortLow},
	}
	agent, _ := openAcceptanceAgentSelectingPrimerCandidate(t, candidates, a)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-b")); err != nil {
		t.Fatalf("SetModel(candidate-b) error = %v, want cross-provider switch to succeed", err)
	}

	options, err := agent.LoopRuntimeOptions(ctx, loopID)
	if err != nil {
		t.Fatal(err)
	}
	// candidate-a's effort (none) is admitted by candidate-b's effort set too,
	// so the switch must not force a reset.
	found := false
	for _, e := range options.Efforts {
		if e.ID == tui.EffortID(model.EffortNone) {
			found = true
		}
	}
	if !found || len(options.Efforts) != 4 {
		t.Fatalf("efforts after cross-provider switch = %#v, want candidate-b's 4 options including none", options.Efforts)
	}

	// Switch back, proving the transport declaration works both directions.
	if err := agent.SetModel(ctx, loopID, tui.ModelID("candidate-a")); err != nil {
		t.Fatalf("SetModel(candidate-a) error = %v, want switching back to succeed", err)
	}
}
```

Keep `crossTransportPrimerCandidates()` (the fixture) — it's still useful roster shape (candidate-a/candidate-c share lmstudio, candidate-b is chutes-alone) even though its original doc comment described rejection behavior. Update its doc comment to drop the "these can never be switched between at runtime" claim:

```go
// crossTransportPrimerCandidates builds a three-candidate roster spanning two
// transports: candidate-a and candidate-c share one transport (both
// lmstudio, distinct model Name), candidate-b sits alone on a different one
// (chutes). All three are live-switchable from one another now that
// conversationContextPolicy declares every configured transport.
func crossTransportPrimerCandidates() []PrimerCandidate {
```

Add one test using it to prove the 3-candidate roster (not just the 2-candidate one above) is fully declared:

```go
func TestSetModelSwitchesAcrossAllConfiguredTransports(t *testing.T) {
	candidates := crossTransportPrimerCandidates()
	agent, _ := openAcceptanceAgentSelectingPrimerCandidate(t, candidates, candidates[0].Model)
	ctx := context.Background()
	loopID := agent.ActiveLoopID()

	for _, alias := range []string{"candidate-b", "candidate-c", "candidate-a"} {
		if err := agent.SetModel(ctx, loopID, tui.ModelID(alias)); err != nil {
			t.Fatalf("SetModel(%s) error = %v, want every declared candidate reachable", alias, err)
		}
	}
}
```

**Step 4: Run tests to verify they pass**

```bash
go build ./... && go vet ./...
go test ./internal/app/... -run 'TestSetModel' -v
go test ./internal/app/... 2>&1 | tail -60
```

Expected: all `TestSetModel*` tests pass, including the two new ones; no other package test regresses. Confirm `errors`/`strings`/`loop` imports in `runtime_controls.go` are still needed post-deletion (adjust import list if `go vet`/`gofmt` flags anything unused).

**Step 5: Commit**

```bash
git add internal/app/runtime_controls.go internal/app/runtime_controls_test.go
git commit -m "feat: allow live SetModel across every declared primer transport"
```

---

## Task 4: Full regression pass + verification gate

**Files:** none (verification only)

**Step 1: Run the full test suite with race detection**

```bash
cd ~/code/looprig/coderig/.worktrees/primer-model-picker
gofmt -l .   # must print nothing
go vet ./...
go test -race ./... 2>&1 | tail -80
```

Expected: zero `gofmt` output, clean vet, all tests pass (including `-race`).

**Step 2: Run the project's own verification gate**

```bash
make secure 2>&1 | tail -100
```

This runs `gofmt` check, `go vet`, staticcheck, gosec, `go mod verify`, govulncheck — per `coderig/CLAUDE.md`. Note: `go mod verify` may complain about the worktree's local `go.work` harness replace (an expected, temporary artifact of the prerequisite section above, not a real vendoring problem) — if so, verify manually that the *only* discrepancy is the harness replace, and record that clearly rather than silently ignoring a `make secure` failure.

**Step 3: Manual review of the diff**

```bash
git log --oneline feature/primer-model-picker..HEAD
git diff main...HEAD --stat
```

Confirm no stray edits to `go.work`/`go.work.sum` (gitignored, but double-check `git status` shows them untracked, not staged) and no unintended changes outside `internal/app/`.

**Step 4: Report status**

Summarize: which tests are new, which were deleted/rewritten and why, confirmation that `TestSetModelSwitchesAcrossProviders` and `TestSetModelSwitchesAcrossAllConfiguredTransports` pass, and the explicit note that `coderig/go.mod`'s harness version bump (removing the need for the worktree-local `go.work`) is deferred release/adoption work, not part of this task.

**Step 5: No commit** (verification-only task; if Steps 1-2 required fixes, those are separate commits made during this task, not folded into this step).
