# Carbon and Inference Package Refactor

## Goal

Move Inference domain types and Carbon application responsibilities out of their module roots without changing behavior.

## Work order

1. Move Inference model, stream, codec, usage, auth, route, transport, and context-counting declarations with their tests.
2. Update Inference implementations and verify the module.
3. Update Harness, LLM, TUI, and Carbon source imports and verify each consumer.
4. Move Carbon's cohesive application files into `internal/app` and its prompt catalog into `internal/catalog`.
5. Update the Carbon command and tests.
6. Run cross-module formatting, race tests, builds, dependency checks, lint, and security checks.

## Constraints

- No runtime logic changes.
- No compatibility facade at either module root.
- No new third-party dependencies.
- Preserve unrelated working-tree changes.
- Do not commit or push unless explicitly requested.
