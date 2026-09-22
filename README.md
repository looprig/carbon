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
be admitted and never placed.

Factory serves the official `wui.Assets()` bundle. One application-scoped
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
`session-workspaces/<tenant-and-session-hash>/`. The default layout is
`tenant-v1` for the configured default tenant. Browser serving refuses
`legacy-single-tenant-v1` with `browser.LegacyLayoutRefusedError` before opening
any store: Factory composition requires the tenant-v1 layout, and there is no
automatic or stopped-store migrator. A persisted layout marker mismatch fails startup. The
TUI/headless legacy construction remains
independent of Factory and can use its own session-store root (default
`~/.looprig/carbon/store`, overridable with `--data-dir`). Do not point two
different layouts at the same writable root.

Back up a stopped, consistent browser data root, including its layout marker,
control records, tenant journals, and `session-workspaces/` tree. Also back up
operator-managed Carbon configuration and any separate TUI/headless workspace
roots needed by those paths. Restore the matching browser root and tenant
together; copying only the journal or only the control store does not
restore a browser session. Allow shutdown to quiesce Factory admission, settle
accepted work, drain Host residents, and then close storage. If drain is
incomplete, retain the store and investigate before retrying shutdown.

Carbon currently does not wire a SessionObjectStore, object HTTP serving, or
large-tool-result capture/`read_tool_result`. Its object route explicitly
answers unavailable. SessionStore's legacy `PutObject` API is not evidence that
this product serves session objects. Resident gate responses are supported;
answering and resuming a **cold AskUser** turn is not implemented. Capacity,
pending commands, resident wait, reconciliation, and drain need operational
monitoring at Factory and Host boundaries; this repository supplies no cloud
dashboard or deployment manifest for them.

The compatibility `harness/pkg/serve` and `wui.Handler` path remains for other
published consumers. Carbon's browser runtime uses Factory and `wui.Assets()`;
its TUI and headless paths do not require a Factory connection.
