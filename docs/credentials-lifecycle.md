# Carbon credential lifecycle

## ChatGPT subscription provider

Use `openai-subscription` to run Carbon's native loop with a ChatGPT plan through
the official [Sign in with ChatGPT public-client flow](https://developers.openai.com/siwc/token-sharing-open-source).
Sign in from a local machine with a browser:

```sh
carbon login openai-subscription
carbon credentials list
carbon credentials models credential://openai-subscription/account-<id>
```

Copy the account reference from `credentials list` and a model slug from
`credentials models`. Add a model entry to your owner-only
`~/.looprig/carbon/models.json` using schema version 3, for example:

```json
{
  "alias": "subscription",
  "description": "Coding with my ChatGPT subscription.",
  "provider": "openai-subscription",
  "api_format": "openai-responses",
  "model": "ACCOUNT_MODEL_SLUG",
  "credential_ref": "credential://openai-subscription/account-ACCOUNT_ID",
  "uses": ["primer", "delegate"],
  "capabilities": {"tools": true},
  "efforts": ["none"],
  "default_effort": "none"
}
```

Replace both placeholders with the discovered values. Set `primer_default` to
`subscription` to select it by default. Configure capabilities, reasoning efforts
and context limits for the chosen model. Omit `api_key` entirely; an explicitly
empty key alongside `credential_ref` is ambiguous and refused. The default
endpoint is `https://api.openai.com/v1`; custom endpoints are refused. Carbon
leaves the model catalogue unchanged during login and discovery. The model list
is account-specific; inference may still fail due to plan limits or model access.

Tokens live in Carbon's protected local credential store. Refreshes coordinate
across processes using the same Carbon home. Login uses PKCE, state, nonce and
signature-verified ID tokens; inference always streams Responses with
`store=false`.

Reauthenticate the same saved account, including after logout, with:

```sh
carbon login credential://openai-subscription/account-<id>
carbon logout credential://openai-subscription/account-<id>
```

Logout attempts remote refresh-token revocation and removes local credential
state even if revocation fails. The CLI reports those outcomes separately.
Carbon retains the issued client/account registration and stable host identity
for subsequent sign-in, with no retained token in that registration mapping.
Run login when sessions in that process are idle. The ordinary `openai` provider
continues to use API keys.

## Shared runtime lifecycle

Credential state is shared by Carbon compositions within one process. Model
loading, session opens, listing, and logout borrow the same local catalog/store
runtime for the canonical resolved home. Logout blocks the shared runtime from
new sessions or model compositions—not only the reference being logged out—
because a long-lived model catalog may still hold a client whose source is
being closed. It waits for admitted sessions, closes the source, and then
deletes the catalog record and referenced local state separately. The fence is
not cleared in place after a successful logout: all leases must release so the
runtime can close, and the next acquire composes a fresh runtime and model
catalog.

The CLI reports those local outcomes independently. `local_catalog=deleted`
with `local_state=not-deleted` means the catalog no longer references the
state, but an orphaned local record may remain. Treat that as incomplete
logout: preserve the outcome and reconcile the bounded local state through
the credential-store tooling before removing anything manually. Carbon never
reports remote revocation for an API-key logout; remote revocation is a
separate, provider-sanctioned operation.

The registry and its fence are process-scoped only. A second Carbon process
can still hold a credential while the first process logs it out, so operators
must stop other processes when a cross-process drain is required.
