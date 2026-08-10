# Primer model picker — design

## Problem

`~/.looprig/models.json` can list multiple `uses: ["primer", ...]`-capable
models (e.g. the local LM Studio deepseek model alongside Chutes-hosted Kimi
K3 and GLM 5.2), but CodeRig's `/model` TUI command only ever shows one row:
the loop's currently active model. There is no way to switch the primer loop
to a different configured model at runtime — `primer_default` picks exactly
one model at config-load time and that choice is frozen for the session.

## Current mechanics (why this is tractable)

- `compileProductionModels` (`internal/app/productionmodels.go`) already
  builds `productionModels.RuntimeClient` via `newModelRoutingClient`, a
  routing client constructed from **every** configured model binding
  (provider + name → client), not just the primer. Inference calls resolve
  by model identity against this full roster already.
- `harness/pkg/loop.ChangeModel` + `sessionruntime.loopHandle.Change` are
  already generic: they accept any secret-free `model.Model` descriptor and
  apply it at the next turn boundary via `command.ChangeLoopInference`. There
  is no primer-specific restriction at this layer.
- The only place the picker is narrowed to one row is CodeRig's presentation
  glue: `RuntimeAgent.LoopRuntimeOptions` (`internal/app/runtime_controls.go`)
  hardcodes `options.Models` to a single entry equal to the current model, and
  `RuntimeAgent.SetModel` rejects any requested ID that isn't already active.

So switching the primer to another configured model is mechanically already
supported end-to-end; the gap is entirely in what CodeRig chooses to expose.

## Design

### Roster source: reuse `uses: ["primer"]`

No `models.json` schema change. Any model entry already tagged
`"uses": ["primer", ...]` becomes a selectable candidate. Today that's
`lmstudio-deepseek-v4-flash-0731`, `chutes-kimi-k3`, and `chutes-glm-5.2`.

Alternatives considered:
- A new explicit `primer_candidates` list — duplicates what `uses` already
  expresses, adds schema/validation surface for no behavioral gain.
- List every configured model regardless of tag, and reject non-primer
  choices at switch time — worse UX (offers unselectable options) and pushes
  a validation that belongs at listing time into the error path instead.

### `productionModels.PrimerCandidates`

`compileProductionModels` (`internal/app/productionmodels.go`) gains a
`PrimerCandidates []PrimerCandidate` field, populated in the same loop that
already builds `ACP` delegate sources, gated on
`containsModelConfigUse(target.Uses, "primer")` instead of `"delegate"`:

```go
type PrimerCandidate struct {
    Alias       string
    Description string
    Model       model.Model
    Efforts     []model.Effort
}
```

Order follows `config.Models` order (deterministic, no re-sorting).

### `RuntimeAgent` (`internal/app/runtime_controls.go`)

- Replace the `primerAlias string` / `primerEfforts []model.Effort` fields
  with `primerCandidates []PrimerCandidate`, threaded from
  `productionModels.PrimerCandidates` through `Config` and
  `newRuntimeAgentWithPrimerAlias` (renamed to reflect the new shape) in
  `swarm.go`.
- `publicModelID` resolves a `model.Model` to a candidate alias by identity
  match (provider + name, same comparison `runtimeModelKeyFor` already uses
  elsewhere) against `primerCandidates`; falls back to the existing
  `modelID(value)` format if no candidate matches (defensive — e.g. a
  restored session whose model fell out of the current config).
- `LoopRuntimeOptions.Models` becomes one `tui.ModelOption{ID, Label,
  Description}` per candidate, instead of the hardcoded single row.
- `LoopRuntimeOptions.Efforts` is computed from the candidate matching the
  **currently selected** model, not a construction-time-fixed field. This is
  a correctness fix required by the feature itself: today
  `a.primerEfforts` freezes whichever efforts the *startup* primer allowed,
  so after switching from deepseek (`efforts: ["none"]`) to kimi-k3
  (`none/low/medium/high`) the effort picker would still wrongly offer only
  "none".
- `SetModel` looks the requested `tui.ModelID` up in `primerCandidates`
  (instead of requiring it equal the current model) and calls the existing
  `controller.Change(ctx, loop.ChangeModel(candidate.Model))`. Unknown/stale
  IDs keep returning the existing "stale or unknown" error.
- On a successful model switch, if the loop's current effort isn't in the
  new candidate's allowed effort set, `SetModel` also applies the new
  candidate's `DefaultEffort` via the same `Change` call (folds both a
  `ChangeModel` and `ChangeEffort` into one atomic command) — otherwise a
  switch could leave the loop in an effort state the new model doesn't
  support.
- `SetEffort`'s admission check (`containsPrimerEffort`) keys off the
  currently selected candidate's efforts, not a frozen startup list.

### `coderig/CLAUDE.md`

The existing line — "Do not add a generic agent registry or model tier
catalog. The roster is a small fixed set of Loop definitions. Runtime
choices belong in Loop modes and model effort." — predates this feature and
needs to explicitly carve it out rather than be contradicted by the code.
Proposed replacement:

> Do not add an open-ended agent registry. The primer loop may expose a
> bounded picker over `models.json` entries tagged primer-capable
> (`uses: ["primer", ...]`); delegate roles remain fixed via
> `delegate_defaults`. Do not reintroduce a confinement bridge, a
> security-limit ordinal, or any in-session authority-mutation surface.

### Scope boundary

Delegate roles (planner/builder/reviewer) are **not** switchable through
this feature — they stay bound to `delegate_defaults` as today. Only the
primer loop gets a real picker.

## Testing

- `productionmodels_test.go`: a config with multiple `uses:["primer"]`
  entries produces a `PrimerCandidates` roster in config order; a
  single-primer config (today's shape) produces a one-element roster
  (backward compatible).
- `runtime_controls_test.go`:
  - `TestRuntimeCatalogExposesModesAndModel`'s existing "exactly one model
    row" assertion is deliberately changed to assert one row per configured
    primer candidate.
  - New case: `LoopRuntimeOptions.Efforts` reflects the *current* candidate's
    effort set after a switch, not the startup candidate's.
  - New case: `SetModel` to a valid alternate primer-capable alias succeeds
    and the loop's model updates.
  - New case: `SetModel` to an unknown or non-primer-capable alias returns
    the existing "stale or unknown" error.
  - New case: switching to a model whose effort set doesn't include the
    current effort resets to that model's `default_effort`.
