package app

import (
	"context"
	"errors"
	"net/http"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/sessionstore"
	"github.com/looprig/wui"
)

var ErrServeFactoryVerifierRequired = errors.New("carbon: browser serve requires an injected credential verifier")

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
func OpenServeFactory(stores *ServeStorage, localHost *ServePooledHost, cfg ServeFactoryConfig) (*factory.Server, error) {
	if cfg.Verifier == nil {
		return nil, ErrServeFactoryVerifierRequired
	}
	if cfg.Authorizer == nil {
		return nil, errors.New("carbon: browser serve requires an injected authorizer")
	}
	if stores == nil || stores.control == nil || stores.launcher == nil || localHost == nil {
		return nil, errors.New("carbon: browser Factory requires open storage and a pooled Host")
	}
	if err := cfg.DefaultTenant.Validate(); err != nil {
		return nil, err
	}
	if cfg.DefaultTenant != stores.defaultTenant || cfg.StorageBindingID == "" || cfg.BindingVersion == "" || cfg.HostLinkToken == "" || cfg.ReplicaID == "" {
		return nil, errors.New("carbon: browser Factory tenant, binding, token or replica configuration is incomplete")
	}
	if cfg.StorageBindingID != localHost.bindingID || cfg.HostLinkToken != localHost.authToken {
		return nil, errors.New("carbon: browser Factory binding or HostLink token differs from the local Host")
	}
	if (cfg.UIRoutes == nil) != (cfg.AuthorizeUI == nil) {
		return nil, errors.New("carbon: browser UI routes require a handler and authorizer together")
	}
	reader, err := NewServeSessionReader(stores.control, stores.launcher, cfg.StorageBindingID, cfg.BindingVersion)
	if err != nil {
		return nil, err
	}
	directory, err := factory.NewStoreDirectory(stores.control, factory.DefaultDirectoryLimits())
	if err != nil {
		return nil, err
	}
	serviceIdentity, err := identity.NewPrincipal(cfg.DefaultTenant, cfg.ReplicaID, identity.KindService)
	if err != nil {
		return nil, err
	}
	template := factory.LaunchTemplate{Key: sessionstore.HostTargetKey{
		AgentID: CarbonAgentID, RuntimeCompatibilityID: string(localHost.CompatibilityID()),
		Placement: sessionwire.HostPlacementPooled,
	}}
	opts := []factory.Option{
		factory.WithCredentialVerifier(cfg.Verifier), factory.WithAuthorizer(cfg.Authorizer),
		factory.WithSessionCookieName(cfg.CookieName), factory.WithDefaultTenant(cfg.DefaultTenant),
		factory.WithCSRF(cfg.CSRF), factory.WithSessionReader(reader), factory.WithCommands(stores.control),
		factory.WithCatalog(stores.control), factory.WithGates(stores.control), factory.WithHostTargets(stores.control),
		factory.WithPublicCreates(stores.control), factory.WithPendingCommands(stores.control),
		factory.WithDirectory(directory), factory.WithDepartment(template),
		factory.WithHostLinkCredential(staticServeHostLinkCredential(cfg.HostLinkToken)),
		factory.WithServiceIdentity(serviceIdentity), factory.WithReplicaID(cfg.ReplicaID),
		factory.WithSessionBinding(cfg.StorageBindingID, cfg.BindingVersion),
		factory.WithObjectStoreResolver(func(context.Context, sessionstore.SessionBinding) (factory.ObjectReader, error) {
			return nil, ErrServeObjectUnavailable
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
