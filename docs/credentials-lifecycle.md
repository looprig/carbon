# CodeRig credential lifecycle

Credential state is shared by CodeRig compositions within one process. Model
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
the credential-store tooling before removing anything manually. CodeRig never
reports remote revocation for an API-key logout; remote revocation is a
separate, provider-sanctioned operation.

The registry and its fence are process-scoped only. A second CodeRig process
can still hold a credential while the first process logs it out, so operators
must stop other processes when a cross-process drain is required.
