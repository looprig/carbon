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
	service        *host.Service
	storageOwner   *ServeStorage
	authToken      string
	bindingID      string
	listener       net.Listener
	server         *http.Server
	endpoint       sessionwire.InternalEndpoint
	compatibility  department.CompatibilityID
	listenerDone   chan struct{}
	listenerErr    error
	stateMu        sync.Mutex
	startCalled    bool
	startSucceeded bool
	stopCalled     bool
	stopReport     host.DrainReport
	stopErr        error
	stopAttempt    *pooledStopAttempt
	stopCancelHTTP context.CancelFunc
	stopService    func(context.Context) (host.DrainReport, error)
}

type pooledStopAttempt struct {
	done   chan struct{}
	report host.DrainReport
	err    error
}

func (h *ServePooledHost) Endpoint() sessionwire.InternalEndpoint      { return h.endpoint }
func (h *ServePooledHost) Service() *host.Service                      { return h.service }
func (h *ServePooledHost) CompatibilityID() department.CompatibilityID { return h.compatibility }

// MatchesFactoryLinkConfig checks the paired Factory's immutable binding and
// service credential without exposing the HostLink token.
func (h *ServePooledHost) MatchesFactoryLinkConfig(bindingID, token string) bool {
	return h != nil && h.bindingID == bindingID && subtle.ConstantTimeCompare([]byte(h.authToken), []byte(token)) == 1
}

// UsesServeStorage pins Factory composition to this Host's actual provider,
// rather than matching only deployment labels that another root may reuse.
func (h *ServePooledHost) UsesServeStorage(stores *ServeStorage) bool {
	return h != nil && stores != nil && h.storageOwner == stores
}

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
	return &ServePooledHost{service: service, storageOwner: stores, authToken: cfg.AuthToken, bindingID: cfg.StorageBindingID, listener: listener, server: &http.Server{Handler: service.Routes(), ReadHeaderTimeout: 5 * time.Second}, endpoint: endpoint, compatibility: compatibility, listenerDone: make(chan struct{})}, nil
}

// Open already bound the internal listener. Start publishes Host capacity,
// then launches the accept loop; a failed publication is disposed through
// CloseUnstarted without ever accepting HostLink traffic.
func (h *ServePooledHost) Start(ctx context.Context) error {
	if h == nil {
		return errors.New("carbon: nil pooled Host")
	}
	h.stateMu.Lock()
	if h.startCalled || h.stopCalled || h.stopAttempt != nil {
		h.stateMu.Unlock()
		return errors.New("carbon: pooled Host starts once")
	}
	h.startCalled = true
	if err := h.service.Start(ctx); err != nil {
		// CloseUnstarted owns this Service's store; the caller's Stop invokes it
		// before the borrowed storage backend is closed.
		h.stateMu.Unlock()
		return err
	}
	h.startSucceeded = true
	go func() {
		err := h.server.Serve(h.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		h.stateMu.Lock()
		h.listenerErr = err
		close(h.listenerDone)
		h.stateMu.Unlock()
	}()
	h.stateMu.Unlock()
	select {
	case <-h.listenerDone:
		if err := h.ListenerError(); err != nil {
			return fmt.Errorf("carbon: HostLink listener failed: %w", err)
		}
	default:
	}
	return nil
}

// ListenerDone broadcasts HostLink listener termination to every observer.
// ListenerError may be read after it closes; observation never steals Stop's join.
func (h *ServePooledHost) ListenerDone() <-chan struct{} { return h.listenerDone }
func (h *ServePooledHost) ListenerError() error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.listenerErr
}

// Stop drains Host while HostLink remains reachable, then closes the listener.
// A caller deadline ends only that caller's wait: shutdown continues, and the
// owner must call Stop again and await completion before ServeStorage.Close.
// The caller must inspect DrainReport.Failures before reporting a clean drain.
func (h *ServePooledHost) Stop(ctx context.Context) (host.DrainReport, error) {
	if h == nil {
		return host.DrainReport{}, nil
	}
	h.stateMu.Lock()
	if h.stopCalled {
		report, err := h.stopReport, h.stopErr
		h.stateMu.Unlock()
		return report, err
	}
	if h.stopAttempt == nil {
		h.stopAttempt = &pooledStopAttempt{done: make(chan struct{})}
		shutdownCtx, cancel := context.WithCancel(context.Background())
		h.stopCancelHTTP = cancel
		// #nosec G118 -- Host and SessionStore cleanup must outlive this caller's deadline.
		go h.stopOwned(shutdownCtx, cancel, h.stopAttempt)
	}
	attempt, cancelHTTP := h.stopAttempt, h.stopCancelHTTP
	h.stateMu.Unlock()
	select {
	case <-attempt.done:
		return attempt.report, attempt.err
	case <-ctx.Done():
		cancelHTTP()
		return host.DrainReport{}, ctx.Err()
	}
}

func (h *ServePooledHost) stopOwned(shutdownCtx context.Context, cancel context.CancelFunc, attempt *pooledStopAttempt) {
	defer cancel()
	var report host.DrainReport
	var err error
	if !h.startSucceeded {
		if h.service != nil {
			err = h.service.CloseUnstarted(context.Background())
		}
	} else {
		stop := h.stopService
		if stop == nil {
			stop = h.service.Stop
		}
		report, err = stop(context.Background())
	}
	// A publication error is an incomplete Host Stop; keep the listener and
	// borrowed backend alive for a later retry. A nonempty report may also
	// retain residency or an open store and is never clean drain evidence.
	if err != nil || len(report.Failures) != 0 {
		h.stateMu.Lock()
		attempt.report, attempt.err = report, err
		h.stopAttempt = nil
		close(attempt.done)
		h.stateMu.Unlock()
		return
	}
	shutdownErr := h.server.Shutdown(shutdownCtx)
	if errors.Is(shutdownErr, http.ErrServerClosed) {
		shutdownErr = nil
	}
	var forceErr error
	if shutdownErr != nil {
		forceErr = h.server.Close()
	}
	if errors.Is(shutdownErr, context.Canceled) && forceErr == nil {
		shutdownErr = nil
	}
	finalErr := errors.Join(shutdownErr, forceErr, closeListener(h.listener))
	if h.startSucceeded && h.listenerDone != nil {
		<-h.listenerDone
		serveErr := h.ListenerError()
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			finalErr = errors.Join(finalErr, serveErr)
		}
	}
	h.stateMu.Lock()
	h.stopReport, h.stopErr, h.stopCalled = report, finalErr, true
	attempt.report, attempt.err = report, finalErr
	close(attempt.done)
	h.stateMu.Unlock()
}

func closeListener(listener net.Listener) error {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

var _ host.Checkpointer = (*PooledLauncher)(nil)
