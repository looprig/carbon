// Package localbrowser is an embedding example for one Carbon browser process.
// The application supplies credentials, policy, model wiring, and secrets.
package localbrowser

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/looprig/carbon/browser"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

// Settings are non-secret deployment choices. HostGeneration must advance on
// every restart of the same HostID; the process supervisor owns that counter.
type Settings struct {
	HomeDir, DataDir string
	Tenant           string
	PublicAddress    string
	TrustedOrigin    string
	HostID           string
	HostGeneration   uint64
	ReplicaID        string
	StorageBindingID string
	BindingVersion   string
}

// Dependencies come from the embedding application's credential, policy and
// model providers. Never persist their secret values in an example manifest.
type Dependencies struct {
	Verifier      identity.Verifier
	Authorizer    factory.Authorizer
	HostLinkToken string
	CSRFKey       []byte
	ClientBuilder func() (inference.Client, func() model.Model, error)
}

// NewConfig describes a private, loopback-only deployment. The browser's HTTP
// origin must equal the public listener's IP and port. Carbon's stock CLI does
// not supply browser credentials.
func NewConfig(s Settings, d Dependencies) (browser.Config, error) {
	if !filepath.IsAbs(s.HomeDir) || !filepath.IsAbs(s.DataDir) ||
		filepath.Clean(s.HomeDir) != s.HomeDir || filepath.Clean(s.DataDir) != s.DataDir {
		return browser.Config{}, fmt.Errorf("local browser: home and data directories must be clean absolute paths")
	}
	tenant := sessionwire.TenantID(s.Tenant)
	if err := tenant.Validate(); err != nil {
		return browser.Config{}, err
	}
	publicHost, publicPortText, err := net.SplitHostPort(s.PublicAddress)
	if err != nil || net.ParseIP(publicHost) == nil || !net.ParseIP(publicHost).IsLoopback() {
		return browser.Config{}, fmt.Errorf("local browser: public address must bind a loopback IP")
	}
	publicPort, err := strconv.Atoi(publicPortText)
	if err != nil || publicPort < 1 || publicPort > 65535 {
		return browser.Config{}, fmt.Errorf("local browser: public address needs a fixed numeric port")
	}
	origin, err := url.Parse(s.TrustedOrigin)
	if err != nil || origin.Scheme != "http" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return browser.Config{}, fmt.Errorf("local browser: trusted origin must be a bare http origin")
	}
	originIP := net.ParseIP(origin.Hostname())
	originPortText := origin.Port()
	if originPortText == "" {
		originPortText = "80"
	}
	originPort, err := strconv.Atoi(originPortText)
	if originIP == nil || !originIP.Equal(net.ParseIP(publicHost)) || err != nil || originPort != publicPort {
		return browser.Config{}, fmt.Errorf("local browser: trusted origin must match the public listener IP and port")
	}
	if s.HostID == "" || s.HostGeneration == 0 || s.ReplicaID == "" || s.StorageBindingID == "" || s.BindingVersion == "" {
		return browser.Config{}, fmt.Errorf("local browser: Host, replica and binding identity are required")
	}
	if d.Verifier == nil || d.Authorizer == nil || d.ClientBuilder == nil || d.HostLinkToken == "" || len(d.CSRFKey) < identity.MinCSRFSharedKeyBytes {
		return browser.Config{}, fmt.Errorf("local browser: verifier, authorizer, model builder and injected secrets are required")
	}
	clientLimits := factory.DefaultClientLinkLimits()
	clientLimits.MaxConnections = 256
	clientLimits.PerConnectionQueueBytes = 64 << 10
	return browser.Config{
		Runtime:       browser.RuntimeConfig{HomeDir: s.HomeDir, AccessProfile: browser.AccessReadOnly},
		ClientBuilder: d.ClientBuilder,
		Storage:       browser.StorageConfig{DataDir: s.DataDir, DefaultTenant: tenant},
		Host: browser.HostConfig{
			ListenAddress: "127.0.0.1:0", AuthToken: d.HostLinkToken, StorageBindingID: s.StorageBindingID,
			Options: host.Options{
				HostID: sessionwire.HostID(s.HostID), IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
				Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute,
				RegistryHeartbeat: 10 * time.Second, RegistryExpiry: time.Minute,
				ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
				CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32,
			},
			Generation:           s.HostGeneration,
			Link:                 host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 1},
			Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
			CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
		},
		Factory: browser.FactoryConfig{
			DefaultTenant: tenant, StorageBindingID: s.StorageBindingID, BindingVersion: s.BindingVersion,
			HostLinkToken: d.HostLinkToken, ReplicaID: s.ReplicaID, CookieName: "carbon_browser_session",
			Verifier: d.Verifier, Authorizer: d.Authorizer,
			CSRF: identity.CSRFConfig{SharedKey: append([]byte(nil), d.CSRFKey...), TokenTTL: time.Hour,
				TrustedOrigins: []string{s.TrustedOrigin}},
			ReconcileLimits: factory.DefaultReconcileLimits(), ClientLinkLimits: clientLimits,
		},
		Address: s.PublicAddress,
	}, nil
}
