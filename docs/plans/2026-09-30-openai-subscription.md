# OpenAI subscription provider implementation plan

**Goal:** Let Carbon's native agent loop use a user's ChatGPT subscription through a distinct `openai-subscription` provider.

**Approved design:** Implement the provider in `llm`, reuse the provider-neutral credential storage/refresh lifecycle, and compose it in Carbon. Authenticate through the official Sign in with ChatGPT public-client flow with PKCE, state, nonce and verified ID tokens. Keep account registration and a persistent host ID in Carbon-owned state. Use account-specific model discovery and streaming Responses requests with `store=false`. Keep the metered `openai` provider separate.

**Architecture:** `llm/providers/openaisubscription` owns provider policy, OAuth/OIDC, refresh adaptation and the streaming transport. Carbon owns local persistence, browser launch and CLI composition. Host-scoped refresh locks serialize rotating refresh tokens across Carbon processes. No local replaces or unpublished dependency version pins are added.

**Tech stack:** Go 1.26.8, existing Responses codec, `credentials/refresh`, `secrets/local`, standard HTTP and cryptography.

### Task 1: Provider and transport
- Add failing tests for exact OAuth subscription policy, canonical endpoint enforcement, streaming invocation, terminal failures and tool-call decoding.
- Add `ProviderOpenAISubscription`, Responses-only validation, automatic dispatch and token-counting policy.
- Implement call-scoped streaming transport and collected Invoke using existing stream aggregation.
- Run focused `llm` tests.

### Task 2: OAuth and credential source
- Add failing tests for dynamic-client authorization, callback state/identity validation, issued-client exchange, scopes, ID-token signature/claims, model discovery and refresh rotation.
- Implement stable-path loopback callback, bounded nonredirecting HTTP, official endpoint constants, OIDC signature validation and refresh adapter.
- Persist registration continuity in opaque refresh state. Reuse the existing coordinated refresh source.
- Run provider tests and race checks.

### Task 3: Carbon composition
- Add failing tests for subscription source construction and login persistence; retain old provider gates.
- Add browser OAuth login for `openai-subscription`, persistent host ID, account-specific credential records and discoverable model configuration.
- Register the OAuth source factory with host-scoped coordination and existing logout lifecycle.
- Document login, credential reference and model configuration.
- Run Carbon CLI/configuration/credential tests.

### Task 4: Verification
- Run repository-native checks and standalone tests with `GOTOOLCHAIN=go1.26.8 GOWORK=off`.
- Run Carbon against local modified llm through the workspace; distinguish this from its currently published dependency pin.
- Review diffs for secret exposure, durable-state races, incorrect terminal handling and repository boundaries.
- Report live-login validation and release/pin limitations accurately.

Sources checked 2026-09-30:
- https://developers.openai.com/siwc/token-sharing-open-source/sign-in
- https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference
- https://developers.openai.com/siwc/token-sharing-open-source/token-reference

The documented direct flow applies to open-source and locally hosted apps. Paid or remotely hosted offerings use the partner program.
