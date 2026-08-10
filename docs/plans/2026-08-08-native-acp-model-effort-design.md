# Native ACP Model and Effort Allowlist Design

**Date:** 2026-08-08

**Status:** Approved

## Goal

Let CodeRig operators constrain each native ACP harness to an explicit set of
model and effort combinations in `~/.looprig/coderig/models.json`. When no
model list is configured, the ACP harness remains free to use any model and
effort available to its existing login. Configured choices are attempted only
when `StartAgent` selects them; CodeRig does not launch ACP sessions at startup
to validate model availability.

When an ACP launch or prompt fails, the parent agent receives the bounded ACP
wire error code and message so it can distinguish a temporary usage limit from
an invalid selection or authentication failure. Raw error data, stderr, local
paths, environment details, and wrapped internal causes remain excluded from
model context.

## Configuration

`native_acp.<harness>.models` continues to distinguish omission from an
explicit allowlist:

- Omitted `models` means harness-managed selection. CodeRig passes no model or
  effort selector.
- Present `models` is a non-empty strict allowlist. `StartAgent` may select only
  a configured model and one of that model's configured efforts.

The preferred entry is structured:

```json
{
  "native_acp": {
    "codex": {
      "enabled": true,
      "models": [
        {
          "model": "gpt-5.6-sol",
          "efforts": ["medium"],
          "default_effort": "medium"
        },
        {
          "model": "gpt-5.6-luna",
          "efforts": ["max"],
          "default_effort": "max"
        }
      ]
    },
    "claude-code": {
      "enabled": true,
      "models": [
        {
          "model": "sonnet",
          "efforts": ["high"],
          "default_effort": "high"
        },
        {
          "model": "opus",
          "efforts": ["high"],
          "default_effort": "high"
        },
        {
          "model": "fable",
          "efforts": ["medium"],
          "default_effort": "medium"
        }
      ]
    }
  }
}
```

The existing string entry remains accepted for compatibility and retains its
current model-only behavior. New configurations use structured entries. Each
structured entry requires a valid unpadded model identifier, a non-empty unique
effort list, and a `default_effort` contained in that list. Model identifiers
must be unique within one harness. Unknown fields, duplicate JSON keys, empty
lists, unsupported neutral effort values, and invalid defaults fail static
configuration validation.

This is an additive version-2 schema extension. It does not reinterpret an
existing string entry or require an automatic file migration.

## Runtime Catalogue and StartAgent

Normalization produces one native model option containing the adapter-facing
model identifier, admitted efforts, and default effort. The Harness runtime
catalogue already models these fields, so the normalized values flow directly
into `StartAgent`'s generated `model` and `effort` branches.

The configured catalogue is authoritative. CodeRig does not intersect it with
an adapter-advertised model list during startup and does not remove choices
because of transient login, quota, network, or provider state. Static parsing,
identifier validation, executable resolution, and access-policy construction
remain fail-closed; the removed behavior is process/session preflight for ACP
model availability.

Selecting an explicit native choice binds the model and effective effort into
the child runtime identity. Persistence and restore continue to reject model,
effort, source, profile, or catalogue drift rather than substitute another
choice.

## Adapter Application

The neutral catalogue tuple is translated at the adapter owner boundary:

- Codex receives its base model and reasoning effort as separate launch
  configuration values. A child process is created for the immutable selected
  tuple; no in-session model mutation is introduced.
- Claude Code starts a session, selects the configured model through the
  advertised model config option, and selects the configured effort through
  the advertised thought-level config option. An absent option or rejected
  value fails that `StartAgent` call.
- Harness-managed native selection applies neither model nor effort.

Adapter-specific syntax such as Codex ACP's combined display identifier never
leaks into `models.json` or the model-facing `StartAgent` contract.

## Lazy Failure and Error Boundary

No ACP session is opened at CodeRig startup solely to prove a configured model
or effort. The selected pair is validated by the real child launch. A rejected
pair, exhausted quota, authentication problem, or other ACP failure returns to
the parent agent instead of causing blind retries behind a generic error.

Only peer-authored ACP JSON-RPC `code` and `message` cross the model-facing
boundary. The projection is UTF-8-safe, single-line, and length-bounded. ACP
`data`, local causes, subprocess stderr, command paths, environment values,
proxy details, and credentials are never included. Non-protocol/internal
failures retain fixed categories. This applies both to child construction and
to later `session/prompt` failures.

Foreground and background delegation preserve the safe failure detail in the
child result so the parent can decide whether to try another configured pair,
wait until a reported reset time, or ask the user. The detail is not added to
audit summaries or ordinary logs.

## Ownership

- CodeRig owns the `models.json` extension, normalized native allowlist,
  catalogue assembly, removal of model-availability startup preflight, and
  product error policy.
- `acp/launch` owns applying Codex and Claude model/effort selectors.
- `foreignloops/driver/acp` owns projecting ACP prompt failures into bounded
  foreign-loop terminal details.
- Harness owns preserving an already-bounded child failure detail through the
  `StartAgent` result without exposing arbitrary internal errors.

## Testing

Tests are written first and cover:

- strict decoding and normalization of structured entries;
- compatibility of omitted model lists and legacy string entries;
- invalid, duplicate, empty, and mismatched effort configurations;
- secret-free digest changes for model/effort allowlist changes;
- runtime catalogue and generated `StartAgent` model/effort choices;
- absence of ACP session/model preflight during CodeRig startup;
- Codex model plus reasoning-effort launch configuration;
- Claude model then effort config-option selection;
- lazy rejection of unsupported selections;
- bounded ACP code/message propagation for construction and prompt failures;
- exclusion of error data, stderr, paths, environment values, and causes;
- foreground/background delegation failure results and restore identity.

Production verification includes focused module tests, full affected-module
test suites with race detection where supported, CodeRig build/lint checks,
configuration decode, and an actual CodeRig startup followed by selected native
ACP launches.
