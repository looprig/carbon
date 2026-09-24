# One local Carbon browser process

This package builds a `browser.Config` for one Factory and one **pooled** Host in
one process. `browser.Start` opens their shared on-disk control store at
`Settings.DataDir`, starts the private HostLink listener on loopback, and then
starts the public Factory listener. The control backend is `fsstore` with
Carbon's bounded blob-reader adapter; tenant journals and session workspaces
use separate hashed directories beneath the same data root. This sample does
not use `natsstore` or an in-memory SessionStore.

The embedding application must supply `Dependencies.Verifier`, `Authorizer`,
`ClientBuilder`, a unique HostLink service token, and a CSRF signing key of at
least `identity.MinCSRFSharedKeyBytes`. Load secrets from your local secret
provider at startup. This repository supplies no browser login. The stock
`carbon serve` command refuses browser startup because it has no verifier.

Inside an application that has already constructed those dependencies:

```go
cfg, err := localbrowser.NewConfig(localbrowser.Settings{
    HomeDir: "/absolute/private/carbon-home",
    DataDir: "/absolute/private/carbon-browser-data",
    Tenant: "local",
    PublicAddress: "127.0.0.1:8722",
    TrustedOrigin: "http://127.0.0.1:8722",
    HostID: "local-host",
    HostGeneration: nextPersistedGeneration,
    ReplicaID: "local-factory",
    StorageBindingID: "local-store-v1",
    BindingVersion: "v1",
}, localbrowser.Dependencies{
    Verifier: appVerifier,
    Authorizer: appAuthorizer,
    HostLinkToken: hostLinkSecret,
    CSRFKey: csrfSigningKey,
    ClientBuilder: appModelBuilder,
})
if err != nil { return err }
server, err := browser.Start(startupContext, cfg)
if err != nil {
    if server != nil { _ = server.Stop(cleanupContext) }
    return err
}
// Supervise server.Wait and call server.Stop with the shutdown policy on exit.
```

The code block is an embedding call site: `appVerifier`, `appAuthorizer`,
`appModelBuilder`, secret values, generation storage, and lifecycle contexts
are application owned. It is not a standalone `main` or a credential recipe.
The trusted origin must be HTTP on the exact loopback listener IP and port;
`PublicAddress` requires a fixed port. This helper is for a browser on the
same machine. Serving remote browsers through a TLS proxy requires a different
embedding configuration that validates the external HTTPS origin and its
proxy boundary; do not reuse this helper unchanged. Never expose HostLink.

`HostGeneration` must rise when the same Host ID restarts; persist that
counter outside this example. The Host holds at most two resident sessions,
its command queue has 16 entries, and Factory admits at most 256 ClientLinks
with a 64 KiB outbound queue each. That queue ceiling is 16 MiB in aggregate
before connection/runtime/provider overhead. These are local sample limits,
not the 1,000–5,000 link cloud profile. Review CPU, memory, open-file limits,
model concurrency, and observed queue pressure before changing them.

Use a private absolute data root with sufficient disk space. Back up the
stopped root as a unit; Carbon has no legacy-to-tenant layout migrator.
Graceful shutdown must quiesce Factory, let accepted work settle, drain the
Host, and close storage through `server.Stop`. Check `cfg.EffectiveShutdownPolicy()`
when assigning a process supervisor's termination budget. A browser reconnect
can recover missed public events from the durable journal. Because it goes
through `browser.Start`, it inherits browser serve's large-tool-result
retention and object route; it does not supply cold AskUser resume.

The repository's `go.mod` pins released external Looprig modules and contains
no local `replace`. Verify with:

```sh
GOWORK=off go test ./examples/local-browser
```

The smoke test binds two loopback listeners and uses injected test-only
credentials and a model stub. It proves composition starts and stops; it does
not prove a real identity provider, model endpoint, or browser deployment.
