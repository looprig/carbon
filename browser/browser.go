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
	Address       string
	ClientBuilder func() (inference.Client, func() model.Model, error)
}

// Server retains every owned stage until an orderly shutdown completes.
// A failed Stop attempt leaves the handle retryable.
type Server struct {
	mu             sync.Mutex
	storage        *carbon.ServeStorage
	host           *carbon.ServePooledHost
	factory        *factory.Server
	listener       net.Listener
	serveDone      chan error
	done           chan struct{}
	attempt        *stopAttempt
	terminalErr    error
	factoryStopped bool
	hostStopped    bool
	storageClosed  bool
	stopFactory    func(context.Context) error
	stopHost       func(context.Context) (host.DrainReport, error)
	closeStorage   func(context.Context) error
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
	if cfg.Factory.Verifier == nil {
		return nil, ErrVerifierRequired
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
	s := &Server{storage: storage, done: make(chan struct{})}
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
	if err := f.Start(ctx); err != nil {
		return failStart(s, err)
	}
	if err := ctx.Err(); err != nil {
		return failStart(s, err)
	}
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return failStart(s, err)
	}
	s.listener = ln
	s.serveDone = make(chan error, 1)
	// #nosec G118 -- Serve is owned by Server; Start's context only bounds startup.
	go s.runServe(f.Serve)
	return s, nil
}

func (s *Server) runServe(serve func(net.Listener) error) {
	s.serveDone <- serve(s.listener)
	// Any return from the public listener begins owner cleanup, even when no
	// caller is waiting on Stop. The result is retained for Wait.
	_ = s.Stop(context.Background())
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
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) cleanup(attempt *stopAttempt) {
	// The listener is ours even if Factory.Stop overtakes Serve's state claim.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	var diagnostic, err error
	if !s.factoryStopped && (s.factory != nil || s.stopFactory != nil) {
		// Factory's first Stop is idempotently terminal even when it reports an
		// HTTP error. The uncancelled call has completed its sweep join.
		stop := s.stopFactory
		if stop == nil {
			stop = s.factory.Stop
		}
		diagnostic = stop(context.Background())
		s.factoryStopped = true
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
	s.terminalErr = errors.Join(s.terminalErr, diagnostic)
	attempt.err = errors.Join(err, diagnostic)
	if err == nil {
		close(s.done)
	}
	s.attempt = nil
	close(attempt.done)
	s.mu.Unlock()
}
