# Primer cross-provider consumer wiring — design

## Problem

Harness (`github.com/looprig/harness`, now on `main`, merged from
`feat/cross-provider-model-switching`) supports a loop definition declaring
multiple admitted `ContextTransport`s (provider/API-format/base-URL → trust
capability) via `pkg/loop.WithContextTransports(...ContextTransport)`, and a
live `ChangeLoopInference`/`SetMode` can move between any declared transport,
not just the loop's original one.

CodeRig's own primer-model-picker plan (Tasks 1-4 done) already built
`productionModels.PrimerCandidates`/`Config.PrimerCandidates` (every
`~/.looprig/models.json` entry tagged `uses:["primer"]`) and
`RuntimeAgent.SetModel`, but the loop is still constructed with exactly one
declared transport (today's harness default). Task 4's `SetModel` currently
*rejects* any cross-provider switch — translating harness's rejection into a
friendly message naming which same-transport candidates ARE reachable. That
restriction no longer needs to exist: harness now supports exactly what it's
working around.

## Design

### Reuse `inferenceCapabilityForModel`, don't duplicate it

`internal/app/inference_policy.go`'s `inferenceCapabilityForModel(model.Model)
(contextcount.InferenceCapability, error)` already derives the correct
per-provider trust capability (chutes/phala → end-to-end-encrypted remote with
a provider-specific `SecurityIdentity` revision; lmstudio → local, no
retention; anything else → `UnsupportedInferenceProviderError`). This is
exactly the function `ContextTransport.Capability` needs, called once per
distinct transport in the primer roster instead of once for the current
default.

### New function: `primerContextTransports`

In `inference_policy.go`, alongside `inferenceCapabilityForModel`:

```go
func primerContextTransports(candidates []PrimerCandidate) ([]loop.ContextTransport, error) {
	type transportKey struct {
		Provider  model.ProviderName
		APIFormat model.APIFormat
		BaseURL   string
	}
	seen := make(map[transportKey]struct{}, len(candidates))
	transports := make([]loop.ContextTransport, 0, len(candidates))
	for _, c := range candidates {
		key := transportKey{c.Model.Provider, c.Model.APIFormat, c.Model.BaseURL}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		capability, err := inferenceCapabilityForModel(c.Model)
		if err != nil {
			return nil, err
		}
		transports = append(transports, loop.ContextTransport{
			Provider: c.Model.Provider, APIFormat: c.Model.APIFormat, BaseURL: c.Model.BaseURL,
			Capability: capability,
		})
	}
	return transports, nil
}
```

Dedupes by `(Provider, APIFormat, BaseURL)` — `chutes-kimi-k3` and
`chutes-glm-5.2` already share one transport today, and harness's
`WithContextTransports` rejects duplicate-keyed members outright.
`UnsupportedInferenceProviderError` propagates exactly as it does today for
the single-model case — fail loud at construction if any `uses:["primer"]`
model has an unsupported provider, now checked against the whole roster
rather than just the configured default.

### `conversationContextPolicy` carries the transport set

`internal/app/compaction.go`:

```go
type conversationContextPolicy struct {
	counter         contextcount.ContextCounter
	capability      contextcount.InferenceCapability
	transports      []loop.ContextTransport
	compaction      loop.CompactionPolicy
	summaryFragment string
	summaryRevision string
}

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
		counter: inferencePolicy.ContextCounter(), capability: inferencePolicy.InferenceCapability(),
		transports: transports, compaction: compaction,
		summaryFragment: conversationSummaryConsumptionFragment, summaryRevision: conversationSummaryConsumptionRevision,
	}, nil
}

func (p conversationContextPolicy) options() []loop.Option {
	return []loop.Option{
		loop.WithContextCounter(p.counter),
		loop.WithInferenceCapability(p.capability),
		loop.WithContextTransports(p.transports...),
		loop.WithCompaction(p.compaction),
	}
}
```

`swarmDefinitions` already has `cfg.PrimerCandidates` in scope (threaded since
coderig's own Task 2) — its call becomes
`newConversationContextPolicy(model, cfg.PrimerCandidates)`, no new plumbing.

All three loops (planner/builder/reviewer) get the same declared transport
set, matching how they already share one client/model/capability today.
Only the active primer loop is ever the target of a live `SetModel`, so the
other two roles simply never exercise the extra admitted transports —
harmless, not a new authority surface.

### Restore is free

`swarmDefinitions` is coderig's one shared construction path for both New and
Restore sessions (per `coderig/CLAUDE.md`'s "New, restore, headless, and
interactive construction share one Open path"). This change lives entirely
inside that shared path, so restore picks it up automatically. Harness's own
`NewRestoredWithRuntime`/`RestoreTransportMismatchError` own the actual
restore-time fold and validation — nothing coderig-side needs to change for
restore specifically.

### `runtime_controls.go` gets simpler

Once harness accepts any `PrimerCandidates`-listed transport, `SetModel`'s
cross-transport rejection path can no longer legitimately fire for a
candidate reachable through the picker — every alias `SetModel` accepts comes
from `PrimerCandidates`, and every `PrimerCandidates` transport is now
declared. The Task-4-built defensive machinery
(`primerTransportSwitchError`, `sameTransport`, `liveSwitchAlternativesMessage`)
becomes dead code and is removed. `SetModel`'s error handling for an
unexpected harness `Change` failure (should one somehow still occur — e.g. a
future bug) falls back to a plain wrap, matching the function's other error
paths, rather than the elaborate same-transport-alternatives message that no
longer describes a real, expected outcome.

### CLAUDE.md

Folded into coderig's existing (paused) Task 7 — "Update coderig/CLAUDE.md
architecture rule" — not new scope. Add one clause noting the primer's
admitted transport set derives from `uses:["primer"]`-tagged models in
`models.json`, alongside the existing primer-picker carve-out language.

## Consequence worth naming

A live switch between `lmstudio-deepseek-v4-flash-0731` and either
`chutes-kimi-k3`/`chutes-glm-5.2` — which Task 4 currently rejects with a
friendly "switching transports mid-session is not supported" message — will
start **working**, since all three become declared transports on the same
loop.

## Out of scope

- Any TUI-level consent/confirmation UX for a transport-crossing switch
  (settled in the harness design: none required).
- Changing which models are `uses:["primer"]`-tagged in `models.json`
  (operator-configured, unchanged).
