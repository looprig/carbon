package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	carbon "github.com/looprig/carbon/internal/app"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/sessionstore"
	"github.com/looprig/wui"
)

// ServeFactoryConfig supplies the product-owned authentication and deployment
// identity. The HostLink token must match the local Host's verifier.
type FactoryConfig struct {
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
	ClientLinkLimits factory.ClientLinkLimits
}

type staticServeHostLinkCredential string

func (c staticServeHostLinkCredential) ServiceToken(context.Context) (string, error) {
	return string(c), nil
}

// OpenServeFactory composes the browser edge over the same control store the
// local Host uses. The caller starts Host before Factory and owns both lifetimes.
func composeFactory(stores *carbon.ServeStorage, localHost *carbon.ServePooledHost, cfg FactoryConfig) (*factory.Server, error) {
	if err := validateFactoryConfig(cfg); err != nil {
		return nil, err
	}
	if stores == nil || stores.ControlStore() == nil || stores.Launcher() == nil || localHost == nil {
		return nil, errors.New("carbon: browser Factory requires open storage and a pooled Host")
	}
	if cfg.DefaultTenant != stores.DefaultTenant() {
		return nil, errors.New("carbon: browser Factory tenant differs from the local storage tenant")
	}
	if !localHost.UsesServeStorage(stores) || !localHost.MatchesFactoryLinkConfig(cfg.StorageBindingID, cfg.HostLinkToken) {
		return nil, errors.New("carbon: browser Factory storage, binding or HostLink token differs from the local Host")
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
	if cfg.ClientLinkLimits != (factory.ClientLinkLimits{}) {
		opts = append(opts, factory.WithClientLinkLimits(cfg.ClientLinkLimits))
	}
	return factory.New(opts...)
}

func validateFactoryConfig(cfg FactoryConfig) error {
	if cfg.Verifier == nil {
		return ErrVerifierRequired
	}
	if cfg.Authorizer == nil {
		return errors.New("carbon: browser serve requires an injected authorizer")
	}
	if err := cfg.DefaultTenant.Validate(); err != nil {
		return err
	}
	if cfg.StorageBindingID == "" || cfg.BindingVersion == "" || cfg.HostLinkToken == "" || cfg.ReplicaID == "" {
		return errors.New("carbon: browser Factory tenant, binding, token or replica configuration is incomplete")
	}
	if (cfg.UIRoutes == nil) != (cfg.AuthorizeUI == nil) {
		return errors.New("carbon: browser UI routes require a handler and authorizer together")
	}
	return nil
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
