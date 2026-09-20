// Package browser composes Carbon's public Factory surface over its local Host.
// Credentials and browser authentication remain the embedding application's job.
package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/factory"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

var ErrVerifierRequired = errors.New("carbon: browser serve requires an injected credential verifier")

type StorageConfig = carbon.ServeStorageConfig
type HostConfig = carbon.ServePooledHostConfig

// RuntimeConfig selects the Carbon runtime and its model source. A nil
// ClientBuilder uses Carbon's configured production model resolution.
type RuntimeConfig struct {
	HomeDir       string
	AccessProfile string
	ClientBuilder func() (inference.Client, func() model.Model, error)
}

type Config struct {
	Runtime RuntimeConfig
	Storage StorageConfig
	Host    HostConfig
	Factory FactoryConfig
	Address string
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
	attempt        chan struct{}
	attemptErr     error
	terminalErr    error
	factoryStopped bool
	hostStopped    bool
	storageClosed  bool
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
	profile, ok := carbon.ParseAccessProfile(cfg.Runtime.AccessProfile)
	if cfg.Runtime.AccessProfile == "" {
		profile, ok = carbon.DefaultAccessProfile, true
	}
	if !ok {
		return nil, fmt.Errorf("carbon: invalid browser access profile %q", cfg.Runtime.AccessProfile)
	}
	appCfg := carbon.Config{HomeDir: cfg.Runtime.HomeDir, AccessProfile: profile}
	var opts []carbon.ServeHostOption
	if cfg.Runtime.ClientBuilder != nil {
		build := cfg.Runtime.ClientBuilder
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
		return s, err
	}
	f, err := composeFactory(storage, h, cfg.Factory)
	if err != nil {
		return s, err
	}
	s.factory = f
	ln, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return s, err
	}
	s.listener = ln
	s.serveDone = make(chan error, 1)
	// #nosec G118 -- Serve is owned by Server; Start's context only bounds startup.
	go func() {
		err := f.Serve(ln)
		s.serveDone <- err
		if err != nil {
			_ = s.Stop(context.Background())
		}
	}()
	return s, nil
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
		s.attempt = make(chan struct{})
		// #nosec G118 -- caller cancellation must not interrupt owned cleanup.
		go s.cleanup(s.attempt)
	}
	ch := s.attempt
	s.mu.Unlock()
	select {
	case <-ch:
		s.mu.Lock()
		err := s.attemptErr
		s.mu.Unlock()
		return err
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

func (s *Server) cleanup(ch chan struct{}) {
	// The listener is ours even if Factory.Stop overtakes Serve's state claim.
	if s.listener != nil {
		_ = s.listener.Close()
	}
	var diagnostic, err error
	if !s.factoryStopped && s.factory != nil {
		// Factory's first Stop is idempotently terminal even when it reports an
		// HTTP error. The uncancelled call has completed its sweep join.
		diagnostic = s.factory.Stop(context.Background())
		s.factoryStopped = true
	}
	if s.serveDone != nil {
		serveErr := <-s.serveDone
		if serveErr != nil && !errors.Is(serveErr, factory.ErrServerStopped) && !errors.Is(serveErr, net.ErrClosed) {
			diagnostic = errors.Join(diagnostic, serveErr)
		}
		s.serveDone = nil
	}
	if !s.hostStopped && s.host != nil {
		var report host.DrainReport
		report, err = s.host.Stop(context.Background())
		_ = report
		if err == nil {
			s.hostStopped = true
		}
	}
	if err == nil && !s.storageClosed && s.storage != nil {
		err = s.storage.Close(context.Background())
		if err == nil {
			s.storageClosed = true
		}
	}
	s.mu.Lock()
	s.terminalErr = errors.Join(s.terminalErr, diagnostic)
	s.attemptErr = errors.Join(err, diagnostic)
	if err == nil {
		close(s.done)
	}
	s.attempt = nil
	close(ch)
	s.mu.Unlock()
}
