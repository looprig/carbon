# ACP Native Model Selection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Allow Claude Code and Codex ACP children to use their own logged-in/default models when no native model list is configured, while preserving explicit gateway-backed model routing and supporting constrained native model lists.

**Architecture:** Add a source-aware runtime selection (`gateway` or `native`) and an explicit harness-managed selection kind. Native ACP profiles are optional in `~/.looprig/models.json`; an enabled profile without `models` passes no model or effort override to the harness. Explicit native rows are preflighted and exposed like ordinary choices. Gateway children remain loopback-proxy-only; native children receive only their native login environment.

**Tech Stack:** Go 1.26.x, the local `harness`, `acp`, `foreignloops`, and CodeRig modules, ACP Claude/Codex launch adapters, strict JSON configuration, and existing persistence/fingerprint tests.

---

## Semantics

- `native_acp` absent: preserve gateway-only behavior.
- A native profile absent or `enabled: false`: native ACP is unavailable.
- `enabled: true` with omitted `models`: harness-managed model, small-model, effort, and picker; CodeRig applies no model/effort override.
- `enabled: true` with non-empty `models`: CodeRig exposes only configured, preflight-valid native choices.
- `models: []` is invalid; omission is the harness-managed mode.
- Existing delegate defaults without `source` remain gateway selections. Native defaults require `source: "native"`.
- Native explicit effort overrides remain deferred unless the adapter can enforce them; omitted effort remains harness-managed.

## Phase 0: Confirm adapter contracts

Inspect and test the current Claude/Codex launch APIs, native environment allowlists, and ACP restore behavior. Record any adapter constraints before changing shared types.

## Phase 1: Source-aware Harness runtime selection

Update the Harness runtime catalogue, delegation request validation, event validation, and restore identity to carry `source` and `selection_kind`. Permit model-less entries only for native harness-managed selections and reject model/effort overrides in that mode. Add focused tests first, then run Sol spec and code reviews.

## Phase 2: Optional ACP launch overrides

Allow native Codex to omit `-c model=...` and native Claude to omit both main and small model overrides. Keep gateway requirements unchanged and reject partial Claude model configuration. Add launch/driver tests, then run Sol spec and code reviews.

## Phase 3: Native configuration schema and digest

Add strict optional `native_acp` wire types, normalize enabled profiles and explicit rows, validate defaults/source combinations, and include native configuration identity in the secret-free digest without including login state, tokens, or executable paths. Add decode/validation/digest tests, then run Sol spec and code reviews.

## Phase 4: Catalogue, preflight, and production wiring

Compile native gateway-backed and native-auth ACP sources, key preflight by harness and source, preserve configured defaults without fallback substitution, and remove only unavailable sources. Add gateway-only, native-only, mixed, and failure-isolation tests, then run Sol spec and code reviews.

## Phase 5: Child construction and security

Resolve source-aware ACP children, omit model/effort environment and arguments for harness-managed native selections, preserve native login allowlists, and prove gateway/native environment isolation and role posture. Add focused and end-to-end tests, then run Sol spec and code reviews.

## Phase 6: Persistence, restore, documentation, and verification

Persist source, selection kind, profile, catalogue revision, and ACP session identity without inventing a native model. Fail closed on source/catalogue drift and tombstone failed native resume rather than silently selecting a new default. Update the design and implementation plans and project guidance. Run all module tests, CodeRig tests, lint, security checks, integration tests, and `git diff --check` with the worktree-local `go.work` and temporary Go caches as needed.

Native login setup/discovery, live config reload, interactive ACP permissions, native effort enforcement where unsupported, executable discovery, and a config-writing CLI remain deferred.
