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

## Addendum (2026-08-06): gateway-backed delegate models must also be declared

Discovered while implementing Task 2 (consumer-wiring implementation plan):
`TestPersistedOpenRoutesNativeAgentThroughRuntimeClientAcrossRestore` fails on
restore with harness's `RestoreTransportMismatchError` — a regression from
adopting harness's new `main`, not caused by this plan's own Tasks 1-3, but
one this plan's scope needs to close before the branch's test suite is clean.

CodeRig's native (in-process, `RuntimeClient`-routed) delegate loops —
spawned via `StartAgent` against a `configuredDelegateDefault`/
`ACPGatewaySource` entry (models.json's gateway-backed delegate catalog,
distinct from `uses:["primer"]`) — are ordinary harness `loop.Definition`
instances, subject to the exact same declared-transport restore check as the
three primer roles. Their models were never part of `PrimerCandidates`, so
their transports were never declared, and restoring a session with a prior
delegate on a foreign transport now hard-fails.

`NativeACP` delegates are unaffected and stay out of scope: they run via a
separate harness's own login state (see `coderig/CLAUDE.md`, "Model
catalogue and credentials"), never bind to a CodeRig-owned
`loop.Definition`, and so never participate in this check.

Fix, revised after an independent second-opinion review found two further
defects in the first draft of this fix:

**Revision 1 — provider capability was too narrow, and the user overruled
the narrower option.** `inferenceCapabilityForModel` only classifies
chutes/phala/lmstudio; everything else is a hard `UnsupportedInferenceProviderError`.
Applied naively to delegate models, this makes the *fix* fail the motivating
test even harder (an `"openai"` delegate would now fail at `Open()`, not
just at restore), and real `models.json` configs already allow ~50
providers for `uses:["delegate"]` rows (any provider `llm.Provider`
recognizes — `modelconfig_normalize.go` validates every configured model
against `llm.Provider(...).RequiredAuth()` at load time). Asked directly,
the user's decision: **people should be able to switch to any provider and
model mentioned in models.json** — not just the three reviewed ones. Fix:
`inferenceCapabilityForModel`'s default branch no longer hard-rejects.
Instead it calls the same `llm.Provider(...).RequiredAuth()` models.json
normalization already uses as its validity gate — a provider that passes it
gets a conservative, unreviewed-tier capability
(`contextcount.InferenceTransportTLS` + `contextcount.RetentionUnknown`).
**Correction found during implementation:** `contextcount.InferenceCapability.Validate()`
requires a non-zero `SecurityIdentity` for any transport at or above `TLS` —
a zero `SecurityIdentity` only validates for `InferenceTransportLocal`. The
generic tier therefore derives one exactly like chutes/phala already do, via
the existing `transportSecurityIdentity(model, policyRevision)` helper, with
a new `genericInferenceIdentityRevision` revision constant distinguishing it
from the two *reviewed* provider-specific revisions — `SecurityIdentity`'s
role is a stable comparison fingerprint of transport identity + revision,
not literally "proof of review"; the "no TEE-attestation review exists for
it" distinction remains fully carried by `Transport`/`Retention` (TLS +
RetentionUnknown vs chutes/phala's EndToEndEncrypted), not by
`SecurityIdentity` zero-ness. A provider `RequiredAuth` itself doesn't
recognize (a typo, a bogus test value) still hard-fails with
`UnsupportedInferenceProviderError` — this keeps CLAUDE.md's fail-closed
posture for genuinely unknown input while extending trust to exactly what
models.json's own normalization already trusts, no further. This change
benefits every caller of `inferenceCapabilityForModel`, not just delegates —
the primer roster gets the same broadened support.

**Revision 2 — base-transport membership.** Harness's `validateContextDefinition`
(`pkg/loop/definition.go`) requires a non-empty declared `ContextTransport`
set to contain a member matching the loop's own base `WithInference` model,
with `Capability` **exactly equal** to `WithInferenceCapability`, or
`Define()` itself rejects (`DefinitionInvalidContextTransport`) — this is
not a restore-only check. The first draft's `declaredContextTransports(primerCandidates, delegateModels)`
had no guaranteed base-model membership (the failing test's shape — empty
`PrimerCandidates`, non-empty delegates — hits exactly this gap). Fix:
`declaredContextTransports` takes the base `model.Model` as an explicit
first parameter and always seeds it first in the model list passed to the
shared dedup core, replacing the fragile "callers already guarantee this"
comment from the first draft. Capability equality with `WithInferenceCapability`
holds automatically: both are derived by calling the same
`inferenceCapabilityForModel` on the same model value.

**Consequently, `primerContextTransports` (Task 1) is retired**, not kept
alongside the new function — once every real call site needs the
base-model-seeded, delegate-merged set, a separate primer-only entry point
with no base-membership guarantee has no legitimate caller left and would
just be dead, confusing surface area. Its dedup core survives as
`contextTransportsForModels(models []model.Model) ([]loop.ContextTransport, error)`,
now the single shared primitive under `declaredContextTransports`.

`Config` gains `DelegateModels []model.Model`, populated at both
`Config`-assembly call sites (`newWithProductionModelsLoader` in `swarm.go`,
`SessionStoreFactory.Open` in `persistence.go`) by mapping `configured.ACP`
(`[]ACPGatewaySource`) to their `.Model` fields — the same two-call-site
shape `PrimerCandidates` already uses. `newConversationContextPolicy` calls
`declaredContextTransports(model, primerCandidates, delegateModels)`. Both
`swarmDefinitions` call sites pass `cfg.DelegateModels` through alongside
`cfg.PrimerCandidates`.

## Out of scope

- Any TUI-level consent/confirmation UX for a transport-crossing switch
  (settled in the harness design: none required).
- Changing which models are `uses:["primer"]`-tagged in `models.json`
  (operator-configured, unchanged).
- `NativeACP` delegate models (see addendum above — architecturally never
  subject to this check).
- `Config.PermissionReviewModel` — not demonstrated to hit this restore path;
  not preemptively touched (YAGNI). Revisit only if a real failure surfaces.
  Independently re-confirmed during the second-opinion review: it's bound
  via `commandsafety.New`/harness `pkg/hustle`, which has its own
  `hustle.Definition` and no loop-restore path — genuinely unaffected.
