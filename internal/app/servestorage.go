package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sessionstore"
	"github.com/looprig/storage"
)

// ServeStoreLayout is the explicit layout of Carbon's control SessionStore.
// An empty value selects tenant-v1. The historical layout is recognized but
// browser composition refuses it until a stopped-store migration exists.
type ServeStoreLayout string

const (
	ServeStoreLayoutTenantV1           ServeStoreLayout = "tenant-v1"
	ServeStoreLayoutLegacySingleTenant ServeStoreLayout = "legacy-single-tenant-v1"
)

type ServeStorageConfig struct {
	DataDir       string
	DefaultTenant sessionwire.TenantID
	Layout        ServeStoreLayout
}

type ServeStorageConfigError struct {
	Field string
	Cause error
}

func (e *ServeStorageConfigError) Error() string {
	return fmt.Sprintf("carbon: invalid serve storage %s: %v", e.Field, e.Cause)
}
func (e *ServeStorageConfigError) Unwrap() error { return e.Cause }

// ServeStoreLayoutMismatchError preserves SessionStore's typed marker cause.
type ServeStoreLayoutMismatchError struct {
	Layout ServeStoreLayout
	Tenant sessionwire.TenantID
	Cause  error
}

func (e *ServeStoreLayoutMismatchError) Error() string {
	return fmt.Sprintf("carbon: control store layout %q for tenant %q disagrees with persisted marker: %v", e.Layout, e.Tenant, e.Cause)
}
func (e *ServeStoreLayoutMismatchError) Unwrap() error { return e.Cause }

// ServeLegacyCompatibilityError refuses an affirmative legacy-single-tenant-v1
// request, before any store is opened or marked. Browser serve composes Factory,
// whose control records and per-tenant journals require the tenant-v1 layout;
// there is no stopped-store migrator. The TUI and headless paths do not use
// Factory and still read legacy sessions from their own store root.
type ServeLegacyCompatibilityError struct{}

func (*ServeLegacyCompatibilityError) Error() string {
	return "carbon: browser serve refuses store layout \"legacy-single-tenant-v1\": Factory composition requires the \"tenant-v1\" layout " +
		"and no stopped-store migrator exists; legacy sessions remain readable from the TUI and headless paths"
}

// ServeStorage owns separate control and harness journal stores under one
// configured data root. The control store uses the root's own fsstore layout;
// tenant journals use PooledLauncher's hashed child roots. Host must stop before
// Close, so its journal readers never outlive these backends.
type ServeStorage struct {
	closeOnce      sync.Once
	closeDone      chan struct{}
	closeErr       error
	closeProvider  func() error
	controlFS      *fsstore.Store
	controlBackend *storage.Composite
	control        *sessionstore.Store
	launcher       *PooledLauncher
	defaultJournal *harnessstore.Store
	defaultTenant  sessionwire.TenantID
}

func (s *ServeStorage) ControlStore() *sessionstore.Store        { return s.control }
func (s *ServeStorage) Launcher() *PooledLauncher                { return s.launcher }
func (s *ServeStorage) DefaultJournalStore() *harnessstore.Store { return s.defaultJournal }
func (s *ServeStorage) DefaultTenant() sessionwire.TenantID      { return s.defaultTenant }

// ControlBackend is borrowed by Host.Compose; ServeStorage remains its owner.
func (s *ServeStorage) ControlBackend() *storage.Composite { return s.controlBackend }

// OpenServeStorage validates layout before opening anything. Legacy browser
// composition is explicitly refused; tenant-v1 against an old marker aborts
// before creating a tenant journal or any runtime dependency.
func OpenServeStorage(ctx context.Context, cfg Config, selected ServeStorageConfig, opts ...ServeHostOption) (*ServeStorage, error) {
	if !filepath.IsAbs(selected.DataDir) || filepath.Clean(selected.DataDir) != selected.DataDir {
		return nil, &ServeStorageConfigError{Field: "data_dir", Cause: ErrNoDataRoot}
	}
	if err := selected.DefaultTenant.Validate(); err != nil {
		return nil, &ServeStorageConfigError{Field: "default_tenant", Cause: err}
	}
	layout := selected.Layout
	if layout == "" {
		layout = ServeStoreLayoutTenantV1
	}
	if layout != ServeStoreLayoutTenantV1 && layout != ServeStoreLayoutLegacySingleTenant {
		return nil, &ServeStorageConfigError{Field: "layout", Cause: fmt.Errorf("unsupported layout %q", layout)}
	}
	if layout == ServeStoreLayoutLegacySingleTenant {
		return nil, &ServeLegacyCompatibilityError{}
	}
	fs, err := fsstore.Open(fsstore.Options{Root: selected.DataDir})
	if err != nil {
		return nil, &StoreInitError{Stage: "control-fsstore", Cause: err}
	}
	backend := *fs.Backend()
	backend.Blobs = newBoundedBlobs(backend.Blobs)
	// No legacy option: SessionStore's unmarked default is tenant-v1, and its
	// persisted marker comparison refuses a historical legacy root.
	if err := ctx.Err(); err != nil {
		_ = fs.Close()
		return nil, err
	}
	// SessionStore retains Open's context for its entire lifetime. The caller's
	// startup context also governs Serve's run loop and is cancelled to begin
	// shutdown, when this control store must still answer pending-command reads.
	// ServeStorage owns the store and closes it after Host and Factory stop.
	control, err := sessionstore.Open(context.WithoutCancel(ctx), &backend)
	if err != nil {
		_ = fs.Close()
		var marker *sessionstore.KeyspaceError
		if errors.As(err, &marker) && marker.Code == sessionstore.KeyspaceLayoutMismatch {
			return nil, &ServeStoreLayoutMismatchError{Layout: layout, Tenant: selected.DefaultTenant, Cause: err}
		}
		return nil, &StoreInitError{Stage: "control-sessionstore", Cause: err}
	}
	if err := ctx.Err(); err != nil {
		_ = closeServeStorageResources(nil, control, fs.Close)
		return nil, err
	}
	launcher, err := OpenPooledLauncher(ctx, cfg, selected.DataDir, opts...)
	if err != nil {
		_ = closeServeStorageResources(nil, control, fs.Close)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = closeServeStorageResources(launcher, control, fs.Close)
		return nil, err
	}
	journal, err := launcher.JournalStoreForTenant(selected.DefaultTenant)
	if err != nil {
		_ = closeServeStorageResources(launcher, control, fs.Close)
		return nil, &StoreInitError{Stage: "default-tenant-journal", Cause: err}
	}
	if err := ctx.Err(); err != nil {
		_ = closeServeStorageResources(launcher, control, fs.Close)
		return nil, err
	}
	return &ServeStorage{controlFS: fs, controlBackend: &backend, control: control, launcher: launcher, defaultJournal: journal, defaultTenant: selected.DefaultTenant, closeProvider: fs.Close, closeDone: make(chan struct{})}, nil
}

// The provider must outlive SessionStore's background shutdown. In particular,
// the caller's cancelled context cannot govern failure unwind.
func closeServeStorageResources(launcher *PooledLauncher, control *sessionstore.Store, closeProvider func() error) error {
	var errs []error
	if launcher != nil {
		errs = append(errs, launcher.Close(context.Background()))
	}
	if control != nil {
		errs = append(errs, control.Close(context.Background()))
	}
	if closeProvider != nil {
		errs = append(errs, closeProvider())
	}
	return errors.Join(errs...)
}

func (s *ServeStorage) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		go func() {
			s.closeErr = closeServeStorageResources(s.launcher, s.control, s.closeProvider)
			close(s.closeDone)
		}()
	})
	select {
	case <-s.closeDone:
		return s.closeErr
	default:
	}
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
