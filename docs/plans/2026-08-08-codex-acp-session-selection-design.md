# Codex ACP Session Selection Design

## Problem

Carbon resolves native Codex model and effort choices correctly, but the launch
layer passes them as `-c model=...` and `-c model_reasoning_effort=...` arguments
to `codex-acp`. The installed `@agentclientprotocol/codex-acp` 1.1.9 server path
does not forward or consume those arguments. It starts `codex app-server`
without the overrides, so the session remains on the adapter default (observed
as `gpt-5.6-sol` with `medium`) even when StartAgent selected
`gpt-5.6-luna` with `max`.

## Decision

Apply Codex model and effort through the adapter's advertised ACP session
configuration after session creation or restoration.

The connector will resolve and set:

- category/config option `model` for the adapter model ID; then
- category `thought_level`, config option `reasoning_effort`, for the effort.

Selection is ordered because changing model can change the supported effort
set. Availability remains lazy: Carbon startup never contacts the adapter, and
an unavailable model or effort fails only when StartAgent opens the selected
Codex child.

## Data flow

1. Carbon resolves the advertised alias to the adapter-facing model ID and
   exact effort.
2. The foreign ACP driver launches and initializes `codex-acp` without relying
   on model or effort command-line overrides.
3. After `session/new` or `session/load`, the Codex connector reads the cached
   advertised session config options.
4. It applies model first and then effort.
5. Only after both selections succeed does construction return the child
   driver.

Managed selection with both fields empty remains a no-op. Legacy/model-only
selection applies only the model and leaves effort to the adapter. A partial
effort-without-model selection remains invalid.

## Errors and ownership

Missing or unadvertised model and effort values return typed bounded selection
errors without issuing an invalid wire request. Construction failure closes the
owned ACP session/process exactly once. Existing Carbon safe-error projection
continues to expose only approved bounded detail to the parent.

## Compatibility

The existing Codex connector constructors stay immutable. Native model and
effort no longer depend on `-c` launch arguments. Gateway and posture behavior
is outside this bug fix except where shared tests ensure it is not regressed.

Both fresh and restored Codex sessions reapply the configured selection so a
restored agent cannot silently return to adapter defaults.

## Testing

Test-first coverage will prove:

- `gpt-5.6-luna` then `max` produce ordered `session/set_config_option` calls;
- the same selection is applied after both new and loaded sessions;
- managed empty selection is a no-op;
- model-only selection sets only the model;
- unadvertised values fail before an invalid wire call and close owned
  resources exactly once;
- native Codex launch arguments no longer claim to carry model or effort; and
- Carbon still propagates the adapter-facing model ID and exact effort.
