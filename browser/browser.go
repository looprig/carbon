// Package browser composes Carbon's public Factory surface over its local Host.
// Credentials and browser authentication remain the embedding application's job.
package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	carbon "github.com/looprig/carbon/internal/app"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

var ErrVerifierRequired = errors.New("carbon: browser serve requires an injected credential verifier")

type StorageConfig = carbon.ServeStorageConfig
type HostConfig = carbon.ServePooledHostConfig
type ACPComposition = carbon.ACPComposition
type PrimerCandidate = carbon.PrimerCandidate
type AccessProfile = carbon.AccessProfile

const (
	AccessReadOnly   AccessProfile = carbon.AccessReadOnly
	AccessTrusted    AccessProfile = carbon.AccessTrusted
	AccessUnconfined AccessProfile = carbon.AccessUnconfined
)

func ParseAccessProfile(name string) (AccessProfile, bool) { return carbon.ParseAccessProfile(name) }

// RuntimeConfig preserves Carbon's complete runtime configuration. The alias
// allows callers to select public fields without importing internal/app.
type RuntimeConfig = carbon.Config

type Config struct {
	Runtime       RuntimeConfig
	Storage       StorageConfig
	Host          HostConfig
	Factory       FactoryConfig
	Shutdown      ShutdownPolicy
	Address       string
	ClientBuilder func() (inference.Client, func() model.Model, error)
	startFactory  func(context.Context, *Server) error
}

// Server retains every owned stage until an orderly shutdown completes.
// A failed Stop attempt leaves the handle retryable.
type Server struct {
	mu                sync.Mutex
	storage           *carbon.ServeStorage
	host              *carbon.ServePooledHost
	factory           *factory.Server
	listener          net.Listener
	serveDone         chan error
	done              chan struct{}
	failureDone       chan struct{}
	failureErr        error
	stopping          bool
	attempt           *stopAttempt
	terminalErr       error
	factoryStopped    bool
	hostStopped       bool
	storageClosed     bool
	shutdownPolicy    ShutdownPolicy
	quiesceFactory    func(context.Context) error
	pendingReader     outstandingReader
	pendingTenant     sessionwire.TenantID
	waitPending       func(context.Context) error
	stopFactory       func(context.Context) error
	stopHost          func(context.Context) (host.DrainReport, error)
	closeStorage      func(context.Context) error
	hostListenerDone  <-chan struct{}
	hostListenerError func() error
}

// DrainIncompleteError means Host reported failures after Stop returned.
// Ownership remains with Server; the backing provider is not closed.
type DrainIncompleteError struct{ Report host.DrainReport }

func (e *DrainIncompleteError) Error() string {
	return fmt.Sprintf("carbon: Host drain incomplete (%d failures)", len(e.Report.Failures))
}

type stopAttempt struct {
	done chan struct{}
	err  error
}

func (s *Server) Addr() net.Addr {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}
func (s *Server) Done() <-chan struct{} { return s.done }

// Start owns the listener only after the Host is ready. A nonnil Server on
// error retains resources whose cleanup may need a later Stop call.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := validateFactoryConfig(cfg.Factory); err != nil {
		return nil, err
	}
	policy, err := cfg.EffectiveShutdownPolicy()
	if err != nil {
		return nil, err
	}
	profile, ok := carbon.ParseAccessProfile(string(cfg.Runtime.AccessProfile))
	if cfg.Runtime.AccessProfile == "" {
		profile, ok = carbon.DefaultAccessProfile, true
	}
	if !ok {
		return nil, fmt.Errorf("carbon: invalid browser access profile %q", cfg.Runtime.AccessProfile)
	}
	appCfg := cfg.Runtime
	appCfg.AccessProfile = profile
	var opts []carbon.ServeHostOption
	if cfg.ClientBuilder != nil {
		build := cfg.ClientBuilder
		opts = append(opts, carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
			client, modelFactory, err := build()
			return client, carbon.ModelFactory(modelFactory), err
		}))
	}
	storage, err := carbon.OpenServeStorage(ctx, appCfg, cfg.Storage, opts...)
	if err != nil {
		return nil, err
	}
	s := &Server{storage: storage, shutdownPolicy: policy, done: make(chan struct{}), failureDone: make(chan struct{})}
	h, err := carbon.OpenServePooledHost(ctx, storage, cfg.Host)
	if err != nil {
		if closeErr := storage.Close(context.Background()); closeErr != nil {
			return s, errors.Join(err, closeErr)
		}
		close(s.done)
		return nil, err
	}
	s.host = h
	if err := h.Start(ctx); err != nil {
		return failStart(s, err)
	}
	f, err := composeFactory(storage, h, cfg.Factory)
	if err != nil {
		return failStart(s, err)
	}
	s.factory = f
	startFactory := cfg.startFactory
	if startFactory == nil {
		startFactory = func(ctx context.Context, owner *Server) error { return owner.factory.Start(ctx) }
	}
	if err := startFactory(ctx, s); err != nil {
		return failStart(s, err)
	}
	if err := ctx.Err(); err != nil {
		return failStart(s, err)
	}
	ln, err := listenReady(ctx, cfg.Address, net.Listen)
	if err != nil {
		return failStart(s, err)
	}
	s.listener = ln
	s.serveDone = make(chan error, 1)
	s.hostListenerDone, s.hostListenerError = h.ListenerDone(), h.ListenerError
	// #nosec G118 -- the observer belongs to Server until HostLink terminates.
	go s.watchHost()
	// #nosec G118 -- Serve is owned by Server; Start's context only bounds startup.
	go s.runServe(f.Serve)
	return s, nil
}

// listenReady closes a successful bind if startup was cancelled while Listen
// ran. Without the post-bind check Start could advertise a cancelled process.
func listenReady(ctx context.Context, addr string, listen func(string, string) (net.Listener, error)) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ln, err := listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *Server) runServe(serve func(net.Listener) error) {
	err := serve(s.listener)
	s.serveDone <- err
	// Any return from the public listener begins owner cleanup, even when no
	// caller is waiting on Stop. The result is retained for Wait.
	s.mu.Lock()
	if !s.stopping {
		if err == nil {
			err = errors.New("carbon: public browser listener stopped unexpectedly")
		}
		s.recordFailureLocked(fmt.Errorf("carbon: public browser listener: %w", err))
	}
	s.mu.Unlock()
	_ = s.Stop(context.Background())
}

func (s *Server) watchHost() {
	<-s.hostListenerDone
	err := s.hostListenerError()
	s.mu.Lock()
	if !s.stopping {
		if err == nil {
			err = errors.New("HostLink listener stopped unexpectedly")
		}
		s.recordFailureLocked(fmt.Errorf("carbon: HostLink listener: %w", err))
		s.mu.Unlock()
		_ = s.Stop(context.Background())
		return
	}
	s.mu.Unlock()
}

func (s *Server) recordFailureLocked(err error) {
	s.terminalErr = errors.Join(s.terminalErr, err)
	if s.failureDone != nil && s.failureErr == nil {
		s.failureErr = err
		close(s.failureDone)
	}
}

func failStart(s *Server, cause error) (*Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cleanupErr := s.Stop(ctx)
	select {
	case <-s.Done():
		return nil, errors.Join(cause, cleanupErr)
	default:
		return s, errors.Join(cause, cleanupErr)
	}
}

// Stop starts or joins one lifecycle-owned cleanup attempt. Caller cancellation
// ends only this wait; a later Stop can join or retry an incomplete attempt.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	select {
	case <-s.done:
		err := s.terminalErr
		s.mu.Unlock()
		return err
	default:
	}
	if s.attempt == nil {
		s.stopping = true
		s.attempt = &stopAttempt{done: make(chan struct{})}
		// #nosec G118 -- caller cancellation must not interrupt owned cleanup.
		go s.cleanup(s.attempt)
	}
	attempt := s.attempt
	s.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) Wait(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.terminalErr
		s.mu.Unlock()
		return err
	default:
	}
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.terminalErr
		s.mu.Unlock()
		return err
	case <-s.failureDone:
		s.mu.Lock()
		err := s.failureErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) cleanup(attempt *stopAttempt) {
	var diagnostic, err error
	if !s.factoryStopped && (s.factory != nil || s.stopFactory != nil) {
		if s.quiesceFactory != nil {
			quiesceCtx, cancel := context.WithTimeout(context.Background(), s.shutdownPolicy.QuiesceTimeout)
			quiesceErr := s.quiesceFactory(quiesceCtx)
			cancel()
			if quiesceErr != nil {
				diagnostic = errors.Join(diagnostic, fmt.Errorf("carbon: Factory quiesce: %w", quiesceErr))
			} else if s.pendingReader != nil {
				settleCtx, cancel := context.WithTimeout(context.Background(), s.shutdownPolicy.SettlementTimeout)
				wait := s.waitPending
				if wait == nil {
					wait = waitPendingPoll
				}
				diagnostic = errors.Join(diagnostic, waitOutstanding(settleCtx, s.pendingReader, s.pendingTenant, 4096, wait))
				cancel()
			}
		}
		// Factory's first Stop is idempotently terminal even when it reports an
		// HTTP error. The uncancelled call has completed its sweep join.
		stop := s.stopFactory
		if stop == nil {
			stop = s.factory.Stop
		}
		diagnostic = errors.Join(diagnostic, stop(context.Background()))
		s.factoryStopped = true
	}
	// Factory.Stop closes its active HTTP listener. If Stop overtook Serve's
	// state claim, the unclaimed listener still belongs to this Server.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.serveDone != nil {
		serveErr := <-s.serveDone
		if serveErr != nil && !errors.Is(serveErr, factory.ErrServerStopped) && !errors.Is(serveErr, net.ErrClosed) {
			diagnostic = errors.Join(diagnostic, serveErr)
		}
		s.serveDone = nil
	}
	if !s.hostStopped && (s.host != nil || s.stopHost != nil) {
		var report host.DrainReport
		stop := s.stopHost
		if stop == nil {
			stop = s.host.Stop
		}
		report, err = stop(context.Background())
		if err == nil && len(report.Failures) != 0 {
			err = &DrainIncompleteError{Report: report}
		}
		if err == nil {
			s.hostStopped = true
		}
	}
	if err == nil && !s.storageClosed && (s.storage != nil || s.closeStorage != nil) {
		closeStore := s.closeStorage
		if closeStore == nil {
			closeStore = s.storage.Close
		}
		err = closeStore(context.Background())
		if err == nil {
			s.storageClosed = true
		}
	}
	s.mu.Lock()
	if diagnostic != nil {
		s.recordFailureLocked(diagnostic)
	}
	if err != nil {
		s.recordFailureLocked(err)
	}
	attempt.err = errors.Join(err, diagnostic)
	if err == nil {
		close(s.done)
	}
	s.attempt = nil
	close(attempt.done)
	s.mu.Unlock()
}
