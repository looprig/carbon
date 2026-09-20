package main

import (
	"context"
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
	"github.com/looprig/sessionstore"
	"github.com/looprig/wui"
)

var ErrServeFactoryVerifierRequired = errors.New("carbon: browser serve requires an injected credential verifier")

// browserComposition is supplied by the browser lifecycle owner after it has
// opened storage and started the local Host. The legacy command path remains
// available during the tested cutover; it does not manufacture credentials.
type browserComposition struct {
	stores *carbon.ServeStorage
	host   *carbon.ServePooledHost
	cfg    ServeFactoryConfig
}

func runComposedFactory(ctx context.Context, addr string, composition browserComposition, out, errOut io.Writer) int {
	server, err := composeServeFactory(composition.stores, composition.host, composition.cfg)
	if err != nil {
		fmt.Fprintln(errOut, "serve:", err)
		return exitFailed
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(errOut, "serve: listen:", err)
		_ = server.Stop(context.Background())
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Stop(shutdownCtx); err != nil {
		fmt.Fprintln(errOut, "serve: shutdown Factory:", err)
		exit = exitFailed
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
	if cfg.UIRoutes != nil {
		opts = append(opts, factory.WithUIRoutes(cfg.UIRoutes, cfg.AuthorizeUI))
	}
	if cfg.ReconcileLimits != (factory.ReconcileLimits{}) {
		opts = append(opts, factory.WithReconcileLimits(cfg.ReconcileLimits))
	}
	return factory.New(opts...)
}
