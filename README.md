# Carbon

Carbon is Looprig's coding agent product. The CLI supports an interactive TUI and
headless use. Its browser composition uses Factory's public API and ClientLink,
one local pooled Host, and a shared filesystem SessionStore. The browser path is
on `main`; check the published Carbon tag before relying on it as a release.

## Browser composition

`browser.Start` starts the authenticated internal HostLink listener before it
starts Factory's public listener. The embedding application supplies a credential
verifier, authorizer, tenant, HostLink service token, CSRF policy, and listener
addresses. The stock `carbon serve` binary has no browser login or verifier and
refuses startup before opening storage. It is not a ready-to-expose web service.
The CLI's `--addr` default is `127.0.0.1:8722`; an embedding application sets
`browser.Config.Address` explicitly for Factory's public bind. Keep the
HostLink listener private. Terminate TLS at a trusted ingress or supply an
appropriately protected listener before exposing the public API. Configure
trusted origins and hosts, and require CSRF tokens for cookie-authenticated
writes. Do not expose HostLink to browsers or reuse a browser credential as its
service token.

The local Host serves only the configured default tenant. Carbon wraps the
injected authorizer so that a principal of any other tenant is refused with
`browser.TenantNotServedError` (unwrapping to `identity.ErrUnauthorized`, so
`403 not_authorized`) before Factory writes anything; otherwise its create would
be admitted and never placed. The pin covers every list, read, command, and
subscribe. It does not cover routes Factory authenticates without consulting the
authorizer: a foreign-tenant principal still gets `200` from `/v1/bootstrap`,
`/v1/agents`, and `/v1/csrf-token`, and can open the `/v1/realtime` connection
(every subscription on it is refused). None of these writes anything durable.
Embedder-supplied `UIRoutes`/`AuthorizeUI` are not tenant-pinned by Carbon;
an embedder that needs "a foreign tenant is refused everywhere" must pin those
itself.

Factory ships no UI; Carbon supplies the official `wui.Assets()` bundle through
Factory's UI seam (`WithUIHandler`). One application-scoped
ClientLink can view several sessions. REST list, status, and public journal
reads work from the durable store while a session is cold; opening a session
does not launch its runtime. An explicit create or input can place it. A
disconnected browser can reconnect and recover missed public events from the
journal. Browser connection lifetime does not own Host runtime lifetime.
Factory and Host use the same durable store; correctness does not require sticky
sessions, a shared Centrifuge history/broker, a cache, NATS, or a notifier.

The local Host is **pooled**: it can hold several sessions up to configured
capacity, with a compatible Carbon Department launch target and isolated
per-session runtime resources. A dedicated Host is a separate service/controller
placement mode, not a Carbon CLI mode. Neither placement mode makes a session
durable; durable catalog and journal state comes from the selected providers.
Runtime compatibility IDs reject a restore when Carbon's restore-critical
configuration changes. Do not advertise cloud or dedicated deployment from this
local composition without the owning controller and verified provider wiring.

The stock `/ui/live`, `/ui/session-presentation`, and `/ui/handoff` routes answer
an authenticated, non-retryable `503 ui_route_unavailable`: a pooled Host has no
single process-global live session to hand off. An embedding application may
provide per-session UI routes with a matching authorizer.

## Local state and operations

The browser data root is the configured absolute `Storage.DataDir`. Its control
SessionStore uses the root filesystem backend; Harness journals live under
`tenant-journals/<tenant-hash>/`, and browser session workspaces under
`session-workspaces/<tenant-and-session-hash>/`. A browser session's runtime
context names that session workspace as its `cwd` (never the server process's
directory), and reports git state only for a repository at or inside it.

Carbon never runs git with the server's (or the TUI's) own authority in a
directory the model can write: repository-local configuration such as
`core.fsmonitor` or a `.gitattributes` clean filter would otherwise execute a
command the model planted. The runtime context's branch/status probe runs inside
the session's own sandbox, with exactly the authority the model's `Bash` already
has, for browser, TUI, and headless sessions alike. Under the `readonly` profile
commands are gated, so the runtime context carries no git lines there; the model
can still ask to run `git status` itself. The default layout is
`tenant-v1` for the configured default tenant. Browser serving refuses
`legacy-single-tenant-v1` with `browser.LegacyLayoutRefusedError` before opening
any store: Factory composition requires the tenant-v1 layout, and there is no
automatic or stopped-store migrator. A persisted layout marker mismatch fails
startup with `browser.StoreLayoutMismatchError`; pointing browser serve at a
TUI/headless store root is refused this way, creates nothing there, and leaves
that root readable by the TUI/headless path. The
TUI/headless legacy construction remains
independent of Factory and can use its own session-store root (default
`~/.looprig/carbon/store`, overridable with `--data-dir`). Do not point two
different layouts at the same writable root.

**Upgrading to Carbon v0.29.0 abandons existing data directories.** Its storage
backend (fsstore v0.6.0) changed its on-disk layout and does not migrate: a data
directory written by an earlier Carbon, whether the TUI/headless store or a
browser root, is refused at startup with a message naming the directory. Move or
delete it (pre-v0.6.0 data is not migrated); Carbon creates a fresh one on the
next start. The change is one-way: an older Carbon cannot read a new directory.

Back up a stopped, consistent browser data root, including its layout marker,
control records, tenant journals, and `session-workspaces/` tree. Also back up
operator-managed Carbon configuration and any separate TUI/headless workspace
roots needed by those paths. Restore the matching browser root and tenant
together; copying only the journal or only the control store does not
restore a browser session. Allow shutdown to quiesce Factory admission, settle
accepted work, drain Host residents, and then close storage. If drain is
incomplete, retain the store and investigate before retrying shutdown.

Browser serve retains large tool output. A tool result larger than the model's
50 KiB preview is captured in full, up to 8 MiB, as a tool-result object in the
session's own tenant journal store. The model reads it back with
`read_tool_result`, and the session's browser reads it through Factory's object
route (`GET /v1/sessions/{sid}/objects/{oid}` and `/metadata`, 1 MiB Range
pages). An object is served only when a committed step in that session's
journal names it; every other reference answers the same 404 as an absent
object. Evidence lookups are rate limited per principal and per session
(a throttled read answers 500, never a false 404). Known limit: a Factory
that has not cached a capture scans at most 65,536 journal records back from
the tip, so a capture with more records after it answers 404 although it
exists; the model's own `read_tool_result` is unaffected. The TUI and headless
paths do not retain tool output. Resident gate responses are supported;
answering and resuming a **cold AskUser** turn is not implemented. Capacity,
pending commands, resident wait, reconciliation, and drain need operational
monitoring at Factory and Host boundaries; this repository supplies no cloud
dashboard or deployment manifest for them.

The compatibility `harness/pkg/serve` and `wui.Handler` path remains for other
published consumers. Carbon's browser runtime uses Factory and `wui.Assets()`;
its TUI and headless paths do not require a Factory connection.
