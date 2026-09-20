package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/sessionstore"
)

// ServePooledHostConfig describes one internal Host run. Every capacity,
// timing and transport bound is explicit; only the endpoint is derived from
// the listener after the operating system binds it.
type ServePooledHostConfig struct {
	ListenAddress        string
	AuthToken            string
	StorageBindingID     string
	Options              host.Options
	Generation           uint64
	Link                 host.LinkOptions
	Drain                host.DrainOptions
	CompatibilityTimeout time.Duration
	WorkPoll             time.Duration
}

// ServePooledHost owns the internal listener and composed Host. Its storage
// backend and journal readers are borrowed from ServeStorage, which must outlive
// Stop.
type ServePooledHost struct {
	service       *host.Service
	listener      net.Listener
	server        *http.Server
	endpoint      sessionwire.InternalEndpoint
	compatibility department.CompatibilityID
	serveDone     chan error
	stateMu       sync.Mutex
	startCalled   bool
	stopCalled    bool
	stopReport    host.DrainReport
	stopErr       error
}

func (h *ServePooledHost) Endpoint() sessionwire.InternalEndpoint      { return h.endpoint }
func (h *ServePooledHost) Service() *host.Service                      { return h.service }
func (h *ServePooledHost) CompatibilityID() department.CompatibilityID { return h.compatibility }

type serveHostAuth struct {
	tenant sessionwire.TenantID
	token  string
}

func (a serveHostAuth) VerifyTenant(_ context.Context, tenant sessionwire.TenantID, token string) error {
	if tenant != a.tenant || subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) != 1 {
		return errors.New("carbon: HostLink credential refused")
	}
	return nil
}

func pooledNamespace(tenant sessionwire.TenantID, session sessionwire.SessionID) string {
	sum := sha256.Sum256([]byte("looprig/carbon/pooled-namespace/v1\x00" + string(tenant) + "\x00" + string(session)))
	return "carbon/pooled/v1/" + hex.EncodeToString(sum[:])
}

// OpenServePooledHost composes the one Carbon target over the same adapted
// control backend ServeStorage opened. Host.Compose opens its own evidence
// enabled SessionStore and closes that store at Stop; ServeStorage alone closes
// the underlying provider after the Host stops.
func OpenServePooledHost(ctx context.Context, stores *ServeStorage, cfg ServePooledHostConfig) (*ServePooledHost, error) {
	if stores == nil || stores.controlBackend == nil || stores.launcher == nil || stores.defaultJournal == nil {
		return nil, errors.New("carbon: pooled Host requires open serve storage")
	}
	if cfg.AuthToken == "" || cfg.StorageBindingID == "" {
		return nil, errors.New("carbon: pooled Host requires auth token and storage binding ID")
	}
	hostName, _, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil || net.ParseIP(hostName) == nil || !net.ParseIP(hostName).IsLoopback() {
		return nil, fmt.Errorf("carbon: pooled Host listen address %q must be an IP loopback address", cfg.ListenAddress)
	}
	if cfg.Options.InternalEndpoint != "" || cfg.Options.Department != nil || cfg.Options.SessionStore != nil || cfg.Options.Workspaces != nil || cfg.Options.Clock != nil || cfg.Options.Auth != nil {
		return nil, errors.New("carbon: pooled Host collaborators and endpoint are derived, not caller supplied")
	}
	if cfg.Options.Placement != sessionwire.HostPlacementPooled || cfg.Options.FixedSessionID != "" {
		return nil, errors.New("carbon: pooled Host requires pooled placement without a fixed session")
	}
	compatibility, err := stores.launcher.PrepareCompatibility(ctx)
	if err != nil {
		return nil, err
	}
	dept, err := NewCarbonDepartment(stores.launcher, compatibility)
	if err != nil {
		return nil, err
	}
	target, err := dept.Target(CarbonAgentID)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, err
	}
	endpoint := sessionwire.InternalEndpoint("ws://" + listener.Addr().String())
	opts := cfg.Options
	opts.InternalEndpoint = endpoint
	blueprint := host.Composition{
		Options: opts, Generation: cfg.Generation, Link: cfg.Link, Drain: cfg.Drain,
		CompatibilityTimeout: cfg.CompatibilityTimeout, WorkPoll: cfg.WorkPoll,
		Collaborators: host.Collaborators{
			Backend: stores.controlBackend,
			JournalStores: map[host.EvidenceKey]sessionstore.DispositionEvidenceReader{
				{TenantID: stores.defaultTenant, StorageBindingID: cfg.StorageBindingID}: stores.defaultJournal,
			},
			Registrar: host.RegistrarFunc(func(context.Context) ([]department.Registration, error) {
				return []department.Registration{{AgentID: CarbonAgentID, Target: target}}, nil
			}),
			Checkpointer:    stores.launcher,
			Auth:            serveHostAuth{tenant: stores.defaultTenant, token: cfg.AuthToken},
			Workspaces:      stores.launcher,
			NamespaceLayout: pooledNamespace,
		},
	}
	service, err := host.Compose(ctx, blueprint)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &ServePooledHost{service: service, listener: listener, server: &http.Server{Handler: service.Routes(), ReadHeaderTimeout: 5 * time.Second}, endpoint: endpoint, compatibility: compatibility, serveDone: make(chan error, 1)}, nil
}

// Start opens the internal listener before Host publishes capacity.
func (h *ServePooledHost) Start(ctx context.Context) error {
	if h == nil {
		return errors.New("carbon: nil pooled Host")
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.startCalled || h.stopCalled {
		return errors.New("carbon: pooled Host starts once")
	}
	h.startCalled = true
	go func() {
		err := h.server.Serve(h.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		h.serveDone <- err
	}()
	if err := h.service.Start(ctx); err != nil {
		h.stopReport, h.stopErr = h.service.Stop(context.Background())
		h.stopErr = errors.Join(err, h.stopErr, h.server.Close(), closeListener(h.listener))
		h.stopCalled = true
		return h.stopErr
	}
	select {
	case err := <-h.serveDone:
		if err != nil {
			h.stopReport, h.stopErr = h.service.Stop(context.Background())
			h.stopErr = errors.Join(fmt.Errorf("carbon: HostLink listener failed: %w", err), h.stopErr, h.server.Close(), closeListener(h.listener))
			h.stopCalled = true
			return h.stopErr
		}
	default:
	}
	return nil
}

// ServeDone reports the listener's terminal result to process orchestration.
func (h *ServePooledHost) ServeDone() <-chan error { return h.serveDone }

// Stop drains Host while HostLink remains reachable, then closes the listener.
// The caller must inspect DrainReport.Failures before reporting a clean drain.
func (h *ServePooledHost) Stop(ctx context.Context) (host.DrainReport, error) {
	if h == nil {
		return host.DrainReport{}, nil
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.stopCalled {
		return h.stopReport, h.stopErr
	}
	report, err := h.service.Stop(ctx)
	shutdownErr := h.server.Shutdown(ctx)
	if errors.Is(shutdownErr, http.ErrServerClosed) {
		shutdownErr = nil
	}
	var forceErr error
	if shutdownErr != nil {
		forceErr = h.server.Close()
	}
	h.stopReport, h.stopErr, h.stopCalled = report, errors.Join(err, shutdownErr, forceErr, closeListener(h.listener)), true
	return h.stopReport, h.stopErr
}

func closeListener(listener net.Listener) error {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

var _ host.Checkpointer = (*PooledLauncher)(nil)
