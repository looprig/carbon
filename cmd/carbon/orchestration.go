package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	carbon "github.com/looprig/carbon/internal/app"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/sessionstore"
	"github.com/looprig/wui"
)

var ErrServeFactoryVerifierRequired = errors.New("carbon: browser serve requires an injected credential verifier")

// browserStartConfig holds the choices an embedding application must make.
// The stock binary supplies no verifier and is refused before opening storage.
type browserStartConfig struct {
	Storage        carbon.ServeStorageConfig
	Host           carbon.ServePooledHostConfig
	Factory        ServeFactoryConfig
	Address        string
	RuntimeOptions []carbon.ServeHostOption
}

// runBrowserLifecycle owns the successful composition stages in start order.
// Failed Host construction/start cleanup waits for Host v0.6's unstarted-close
// contract; until then those failures return without closing the borrowed
// storage provider, which the process must discard on exit.
func runBrowserLifecycle(ctx context.Context, appCfg carbon.Config, cfg browserStartConfig, out, errOut io.Writer) int {
	if cfg.Factory.Verifier == nil {
		fmt.Fprintln(errOut, "serve:", ErrServeFactoryVerifierRequired)
		return exitFailed
	}
	stores, err := carbon.OpenServeStorage(ctx, appCfg, cfg.Storage, cfg.RuntimeOptions...)
	if err != nil {
		fmt.Fprintln(errOut, "serve: storage:", err)
		return exitFailed
	}
	localHost, err := carbon.OpenServePooledHost(ctx, stores, cfg.Host)
	if err != nil {
		fmt.Fprintln(errOut, "serve: Host compose:", err)
		return exitFailed
	}
	if err := localHost.Start(ctx); err != nil {
		fmt.Fprintln(errOut, "serve: Host start:", err)
		return exitFailed
	}
	disposal := browserDisposal{stopHost: localHost.Stop, closeStorage: stores.Close}
	server, err := composeServeFactory(stores, localHost, cfg.Factory)
	if err != nil {
		fmt.Fprintln(errOut, "serve: Factory compose:", err)
		if stopErr := disposal.Close(context.Background()); stopErr != nil {
			fmt.Fprintln(errOut, "serve: cleanup:", stopErr)
		}
		return exitFailed
	}
	disposal.stopFactory = server.Stop
	exit := serveFactoryUntil(ctx, cfg.Address, server, out, errOut)
	shutdown := startBrowserShutdown(&disposal)
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown.Wait(waitCtx); err != nil {
		fmt.Fprintln(errOut, "serve: cleanup:", err)
		return exitFailed
	}
	return exit
}

// browserDisposal keeps storage alive until Factory's sweeps and Host's own
// SessionStore have fully stopped. A failed Host Stop remains retryable.
type browserDisposal struct {
	stopFactory  func(context.Context) error
	stopHost     func(context.Context) (host.DrainReport, error)
	closeStorage func(context.Context) error
	factoryDone  bool
	hostDone     bool
	storageDone  bool
}

func (d *browserDisposal) Close(ctx context.Context) error {
	if !d.factoryDone && d.stopFactory != nil {
		if err := d.stopFactory(ctx); err != nil {
			return err
		}
		d.factoryDone = true
	}
	if !d.hostDone && d.stopHost != nil {
		if _, err := d.stopHost(ctx); err != nil {
			return err
		}
		d.hostDone = true
	}
	if !d.storageDone && d.closeStorage != nil {
		if err := d.closeStorage(ctx); err != nil {
			return err
		}
		d.storageDone = true
	}
	return nil
}

// The worker owns teardown beyond a caller's deadline. In particular, it
// never cancels Factory.Stop, whose first invocation is not safely retryable.
type browserShutdown struct {
	done chan struct{}
	err  error
}

func startBrowserShutdown(disposal *browserDisposal) *browserShutdown {
	shutdown := &browserShutdown{done: make(chan struct{})}
	go func() {
		shutdown.err = disposal.Close(context.Background())
		close(shutdown.done)
	}()
	return shutdown
}

func (s *browserShutdown) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.err
	default:
	}
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func serveFactoryUntil(ctx context.Context, addr string, server *factory.Server, out, errOut io.Writer) int {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(errOut, "serve: listen:", err)
		return exitFailed
	}
	fmt.Fprintf(out, "carbon serve listening on http://%s\n", listener.Addr())
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	exit := exitOK
	select {
	case <-ctx.Done():
	case err := <-done:
		if err != nil {
			fmt.Fprintln(errOut, "serve:", err)
			exit = exitFailed
		}
	}
	return exit
}

// ServeFactoryConfig supplies the product-owned authentication and deployment
// identity. The HostLink token must match the local Host's verifier.
type ServeFactoryConfig struct {
	DefaultTenant    sessionwire.TenantID
	StorageBindingID string
	BindingVersion   string
	HostLinkToken    string
	ReplicaID        string
	CookieName       string
	CSRF             identity.CSRFConfig
	Verifier         identity.Verifier
	Authorizer       factory.Authorizer
	UIRoutes         http.Handler
	AuthorizeUI      factory.UIRouteAuthorizer
	ReconcileLimits  factory.ReconcileLimits
}

type staticServeHostLinkCredential string

func (c staticServeHostLinkCredential) ServiceToken(context.Context) (string, error) {
	return string(c), nil
}

// OpenServeFactory composes the browser edge over the same control store the
// local Host uses. The caller starts Host before Factory and owns both lifetimes.
func composeServeFactory(stores *carbon.ServeStorage, localHost *carbon.ServePooledHost, cfg ServeFactoryConfig) (*factory.Server, error) {
	if cfg.Verifier == nil {
		return nil, ErrServeFactoryVerifierRequired
	}
	if cfg.Authorizer == nil {
		return nil, errors.New("carbon: browser serve requires an injected authorizer")
	}
	if stores == nil || stores.ControlStore() == nil || stores.Launcher() == nil || localHost == nil {
		return nil, errors.New("carbon: browser Factory requires open storage and a pooled Host")
	}
	if err := cfg.DefaultTenant.Validate(); err != nil {
		return nil, err
	}
	if cfg.DefaultTenant != stores.DefaultTenant() || cfg.StorageBindingID == "" || cfg.BindingVersion == "" || cfg.HostLinkToken == "" || cfg.ReplicaID == "" {
		return nil, errors.New("carbon: browser Factory tenant, binding, token or replica configuration is incomplete")
	}
	if !localHost.UsesServeStorage(stores) || !localHost.MatchesFactoryLinkConfig(cfg.StorageBindingID, cfg.HostLinkToken) {
		return nil, errors.New("carbon: browser Factory storage, binding or HostLink token differs from the local Host")
	}
	if (cfg.UIRoutes == nil) != (cfg.AuthorizeUI == nil) {
		return nil, errors.New("carbon: browser UI routes require a handler and authorizer together")
	}
	if cfg.UIRoutes == nil {
		cfg.UIRoutes = unavailableLegacyUIRoutes()
		cfg.AuthorizeUI = func(ctx context.Context, principal identity.Principal, _, _ string) error {
			if principal.Tenant() != cfg.DefaultTenant || principal.IsService() {
				return identity.ErrUnauthorized
			}
			return cfg.Authorizer.AuthorizeSessionList(ctx, principal)
		}
	}
	reader, err := carbon.NewServeSessionReader(stores.ControlStore(), stores.Launcher(), cfg.StorageBindingID, cfg.BindingVersion)
	if err != nil {
		return nil, err
	}
	directory, err := factory.NewStoreDirectory(stores.ControlStore(), factory.DefaultDirectoryLimits())
	if err != nil {
		return nil, err
	}
	serviceIdentity, err := identity.NewPrincipal(cfg.DefaultTenant, cfg.ReplicaID, identity.KindService)
	if err != nil {
		return nil, err
	}
	template := factory.LaunchTemplate{Key: sessionstore.HostTargetKey{
		AgentID: carbon.CarbonAgentID, RuntimeCompatibilityID: string(localHost.CompatibilityID()),
		Placement: sessionwire.HostPlacementPooled,
	}}
	opts := []factory.Option{
		factory.WithCredentialVerifier(cfg.Verifier), factory.WithAuthorizer(cfg.Authorizer),
		factory.WithSessionCookieName(cfg.CookieName), factory.WithDefaultTenant(cfg.DefaultTenant),
		factory.WithCSRF(cfg.CSRF), factory.WithSessionReader(reader), factory.WithCommands(stores.ControlStore()),
		factory.WithCatalog(stores.ControlStore()), factory.WithGates(stores.ControlStore()), factory.WithHostTargets(stores.ControlStore()),
		factory.WithPublicCreates(stores.ControlStore()), factory.WithPendingCommands(stores.ControlStore()),
		factory.WithDirectory(directory), factory.WithDepartment(template),
		factory.WithHostLinkCredential(staticServeHostLinkCredential(cfg.HostLinkToken)),
		factory.WithServiceIdentity(serviceIdentity), factory.WithReplicaID(cfg.ReplicaID),
		factory.WithSessionBinding(cfg.StorageBindingID, cfg.BindingVersion),
		factory.WithObjectStoreResolver(func(context.Context, sessionstore.SessionBinding) (factory.ObjectReader, error) {
			return nil, carbon.ErrServeObjectUnavailable
		}),
		factory.WithUIHandler(wui.Assets()),
	}
	opts = append(opts, factory.WithUIRoutes(cfg.UIRoutes, cfg.AuthorizeUI))
	if cfg.ReconcileLimits != (factory.ReconcileLimits{}) {
		opts = append(opts, factory.WithReconcileLimits(cfg.ReconcileLimits))
	}
	return factory.New(opts...)
}

// The old routes describe one process-global live workspace. A pooled Host
// can serve several sessions, so these names cannot truthfully answer until
// Carbon defines per-session replacements. Keep them protected and explicit.
func unavailableLegacyUIRoutes() http.Handler {
	mux := http.NewServeMux()
	unavailable := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "ui_route_unavailable", "message": "this route requires per-session browser semantics", "retryable": false,
		}})
	})
	mux.Handle("GET /ui/live", unavailable)
	mux.Handle("GET /ui/session-presentation", unavailable)
	mux.Handle("POST /ui/handoff", unavailable)
	return mux
}
