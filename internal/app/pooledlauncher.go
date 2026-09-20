package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
)

// PooledLauncher constructs a separate Carbon rig, access policy, gate, process
// supervisor, credential admission, and MCP manager for every launched session.
// Each tenant owns a durable backend because Harness's journal layout marker
// binds one backend to one tenant. A restore resolves the same backend and root.
type PooledLauncher struct {
	mu            sync.Mutex
	dataDir       string
	cfg           Config
	options       serveHostConfig
	tenants       map[sessionwire.TenantID]*pooledTenantStores
	live          map[uuid.UUID]*pooledSession
	reserved      map[uuid.UUID]struct{}
	active        sync.WaitGroup
	closeDone     chan struct{}
	closeContext  context.Context
	closeCancel   context.CancelFunc
	closeErr      error
	closed        bool
	preparing     bool
	prepared      bool
	compatibility department.CompatibilityID
}

type pooledTenantStores struct {
	ready  chan struct{}
	fs     *fsstore.Store
	stores *sessionStores
	err    error
}

const tenantJournalDigestDomain = "looprig/carbon/tenant-journal-root/v1"

type pooledSession struct {
	checkpointMu      sync.Mutex
	tenant            sessionwire.TenantID
	sessionID         sessionwire.SessionID
	controller        session.SessionController
	access            *sessionAccess
	mcp               mcpSessionAssembly
	credentialRuntime *credentialRuntime
	credentialLease   *credentialRegistryLease
	once              sync.Once
}

// PooledCompatibilityDriftError refuses a session whose effective rig identity
// differs from the capability the Host advertised at startup.
type PooledCompatibilityDriftError struct {
	Prepared department.CompatibilityID
	Current  department.CompatibilityID
}

func (e *PooledCompatibilityDriftError) Error() string {
	return fmt.Sprintf("carbon: pooled runtime compatibility drift: prepared %q, current %q", e.Prepared, e.Current)
}

// PrepareCompatibility freezes the effective, secret-free runtime identity
// before Host publishes capacity. The probe owns and closes every temporary
// model, credential, access and MCP dependency; Launch builds fresh ones.
func (l *PooledLauncher) PrepareCompatibility(ctx context.Context) (department.CompatibilityID, error) {
	if l == nil {
		return "", errors.New("carbon: nil pooled launcher")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", &StoreClosedError{}
	}
	if l.prepared {
		id := l.compatibility
		l.mu.Unlock()
		return id, nil
	}
	if l.preparing || len(l.live) != 0 || len(l.reserved) != 0 {
		l.mu.Unlock()
		return "", errors.New("carbon: compatibility preparation requires an idle launcher")
	}
	l.preparing = true
	l.active.Add(1)
	l.mu.Unlock()
	prepareCtx, cancel := context.WithCancel(ctx)
	stopCloseCancel := context.AfterFunc(l.closeContext, cancel)
	defer func() { stopCloseCancel(); cancel() }()
	defer func() {
		l.mu.Lock()
		l.preparing = false
		l.mu.Unlock()
		l.active.Done()
	}()
	root, err := os.MkdirTemp(l.dataDir, "compatibility-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	cfg := l.cfg
	var credentials *credentialRuntime
	var lease *credentialRegistryLease
	if l.options.buildClient == nil {
		load := l.options.loadModels
		if load == nil {
			load = loadProductionModelsWithContext
		}
		resolved, err := resolveServeModelsAtRoot(prepareCtx, cfg, load, loadProductionModels, root)
		if err != nil {
			return "", err
		}
		cfg, credentials, lease = resolved.cfg, resolved.credentialRuntime, resolved.credentialLease
	} else {
		if _, _, err := l.options.buildClient(); err != nil {
			return "", err
		}
	}
	defer func() {
		if credentials != nil {
			credentials.endSession()
			releaseCredentialComposition(credentials, lease)
		}
	}()
	access, err := buildSessionAccess(cfg, root, true)
	if err != nil {
		return "", err
	}
	defer access.Close()
	cfg.AccessConfigRev = access.pooledConfigRev
	mcp, err := newMCPSessionAssembly(cfg)
	if err != nil {
		return "", err
	}
	defer mcp.close(context.Background())
	cfg.MCPConfigRev = mcp.configRev()
	id := CarbonCompatibilityID(cfg)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", &StoreClosedError{}
	}
	if err := prepareCtx.Err(); err != nil {
		l.mu.Unlock()
		return "", err
	}
	l.compatibility, l.prepared = id, true
	l.mu.Unlock()
	return id, nil
}

// OpenPooledLauncher fixes the durable data root. Tenant backends are opened
// lazily, once per tenant, before Host composition requests their readers.
func OpenPooledLauncher(_ context.Context, cfg Config, dataDir string, opts ...ServeHostOption) (*PooledLauncher, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, ErrNoDataRoot
	}
	var options serveHostConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, &StoreInitError{Stage: "data-root", Cause: err}
	}
	closeContext, closeCancel := context.WithCancel(context.Background())
	return &PooledLauncher{dataDir: dataDir, cfg: cfg, options: options, tenants: make(map[sessionwire.TenantID]*pooledTenantStores), live: make(map[uuid.UUID]*pooledSession), reserved: make(map[uuid.UUID]struct{}), closeDone: make(chan struct{}), closeContext: closeContext, closeCancel: closeCancel}, nil
}

var _ SessionLauncher = (*PooledLauncher)(nil)

// EnsureWorkspace is the Host workspace seam. It uses the same derivation as
// Launch, so the root Host passes through LaunchScope is the one the rig places.
func (l *PooledLauncher) EnsureWorkspace(_ context.Context, tenant sessionwire.TenantID, sessionID sessionwire.SessionID) (string, error) {
	if l == nil {
		return "", errors.New("carbon: nil pooled launcher")
	}
	return materializeSessionWorkspaceRoot(l.dataDir, tenant, sessionID)
}

// ReleaseWorkspace retains the tree: Host release is nonterminal residency
// release, and the next placement may need to restore the same files.
func (l *PooledLauncher) ReleaseWorkspace(_ context.Context, _ sessionwire.TenantID, _ sessionwire.SessionID) error {
	return nil
}

func (l *PooledLauncher) SupportsPooled() bool { return l != nil && l.tenants != nil }

// JournalStoreForTenant returns the exact Harness store Launch will write
// through for tenant. Register it in Host's immutable JournalStores table
// before Compose; discovering a new tenant later requires Host recomposition.
// The launcher retains ownership and closes the backend after Host stops.
func (l *PooledLauncher) JournalStoreForTenant(tenant sessionwire.TenantID) (*harnessstore.Store, error) {
	if l == nil {
		return nil, errors.New("carbon: nil pooled launcher")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, &StoreClosedError{}
	}
	l.active.Add(1)
	l.mu.Unlock()
	defer l.active.Done()
	bundle, err := l.tenantStores(tenant)
	if err != nil {
		return nil, err
	}
	return bundle.stores.session, nil
}

func (l *PooledLauncher) tenantStores(tenant sessionwire.TenantID) (*pooledTenantStores, error) {
	if err := tenant.Validate(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if bundle := l.tenants[tenant]; bundle != nil {
		l.mu.Unlock()
		<-bundle.ready
		return bundle, bundle.err
	}
	bundle := &pooledTenantStores{ready: make(chan struct{})}
	l.tenants[tenant] = bundle
	l.mu.Unlock()
	sum := sha256.Sum256([]byte(tenantJournalDigestDomain + "\x00" + string(tenant)))
	root := filepath.Join(l.dataDir, "tenant-journals", hex.EncodeToString(sum[:]))
	fs, err := fsstore.Open(fsstore.Options{Root: root})
	if err != nil {
		bundle.err = &StoreInitError{Stage: "tenant-fsstore", Cause: err}
	} else {
		stores, openErr := openTenantStores(fs.Backend(), tenant)
		if openErr != nil {
			_ = fs.Close()
			bundle.err = openErr
		} else {
			stores.resourceStorage = newPersistedResourceStorageProvider(root)
			bundle.fs, bundle.stores = fs, stores
		}
	}
	l.mu.Lock()
	if bundle.err != nil {
		delete(l.tenants, tenant)
	}
	close(bundle.ready)
	l.mu.Unlock()
	return bundle, bundle.err
}

// Launch always builds session-scoped bindings. It returns the harness controller
// itself so Host retains its segregated capabilities and liveness observation.
func (l *PooledLauncher) Launch(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
	if l == nil {
		return nil, errors.New("carbon: nil pooled launcher")
	}
	if scope.RigSessionID.IsZero() {
		return nil, fmt.Errorf("carbon: pooled launch needs a runtime session id")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, &StoreClosedError{}
	}
	if l.preparing {
		l.mu.Unlock()
		return nil, errors.New("carbon: pooled compatibility preparation is in progress")
	}
	_, live := l.live[scope.RigSessionID]
	_, reserved := l.reserved[scope.RigSessionID]
	if live || reserved {
		l.mu.Unlock()
		return nil, fmt.Errorf("carbon: runtime session %s is already live", scope.RigSessionID)
	}
	l.reserved[scope.RigSessionID] = struct{}{}
	l.active.Add(1)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.reserved, scope.RigSessionID)
		l.mu.Unlock()
		l.active.Done()
	}()
	launchCtx, cancel := context.WithCancel(ctx)
	stopCloseCancel := context.AfterFunc(l.closeContext, cancel)
	defer func() { stopCloseCancel(); cancel() }()
	root, err := materializeSessionWorkspaceRoot(l.dataDir, scope.TenantID, scope.SessionID)
	if err != nil {
		return nil, err
	}
	if scope.WorkspaceRoot != "" && scope.WorkspaceRoot != root {
		return nil, &ForeignWorkspaceRootError{SessionID: scope.SessionID, Want: scope.WorkspaceRoot, Served: root}
	}
	bundle, err := l.tenantStores(scope.TenantID)
	if err != nil {
		return nil, err
	}
	cfg := l.cfg
	var client inference.Client
	var factory ModelFactory
	var credentials *credentialRuntime
	var lease *credentialRegistryLease
	if l.options.buildClient != nil {
		client, factory, err = l.options.buildClient()
	} else {
		load := l.options.loadModels
		if load == nil {
			load = loadProductionModelsWithContext
		}
		resolved, resolveErr := resolveServeModelsAtRoot(launchCtx, cfg, load, loadProductionModels, root)
		err = resolveErr
		if err == nil {
			cfg, client, factory = resolved.cfg, resolved.client, resolved.factory
			credentials, lease = resolved.credentialRuntime, resolved.credentialLease
		}
	}
	if err != nil {
		return nil, err
	}
	cleanupCredentials := func() {
		if credentials != nil {
			credentials.endSession()
			releaseCredentialComposition(credentials, lease)
		}
	}
	access, err := buildSessionAccess(cfg, root, true)
	if err != nil {
		cleanupCredentials()
		return nil, err
	}
	access.diagnostics = append(access.diagnostics, cfg.ACPDiagnostics...)
	cfg.AccessConfigRev = access.pooledConfigRev
	mcp, err := newMCPSessionAssembly(cfg)
	if err != nil {
		_ = access.Close()
		cleanupCredentials()
		return nil, err
	}
	cfg.MCPConfigRev = mcp.configRev()
	fail := func(err error) (session.SessionController, error) {
		mcp.close(ctx)
		_ = access.Close()
		cleanupCredentials()
		return nil, err
	}
	l.mu.Lock()
	prepared, advertised := l.prepared, l.compatibility
	l.mu.Unlock()
	if prepared {
		current := CarbonCompatibilityID(cfg)
		if current != advertised {
			return fail(&PooledCompatibilityDriftError{Prepared: advertised, Current: current})
		}
	}
	definition, err := carbonDefinition(client, factory(), cfg, access, nil)
	if err != nil {
		return fail(err)
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, client)
	if err != nil {
		return fail(err)
	}
	assembled, err := buildRigForDelegationCaps(definition, bundle.stores, root, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReview)
	if err != nil {
		return fail(err)
	}
	ctx = detachSessionLifetime(launchCtx)
	var controller session.SessionController
	if scope.Restore {
		controller, err = assembled.RestoreSession(ctx, scope.RigSessionID)
	} else {
		controller, err = assembled.NewSession(ctx, carbonRigSessionOptions(scope)...)
	}
	if err != nil {
		return fail(err)
	}
	if err := mcp.attach(ctx, controller, true); err != nil {
		_ = controller.Shutdown(ctx)
		return fail(err)
	}
	entry := &pooledSession{tenant: scope.TenantID, sessionID: scope.SessionID, controller: controller, access: access, mcp: mcp, credentialRuntime: credentials, credentialLease: lease}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = controller.Shutdown(context.Background())
		return fail(&StoreClosedError{})
	}
	l.live[scope.RigSessionID] = entry
	l.mu.Unlock()
	if signal, ok := controller.(session.Liveness); ok {
		go func() {
			<-signal.Done()
			_ = controller.Shutdown(ctx)
			l.release(scope.RigSessionID, entry)
		}()
	}
	return controller, nil
}

// Checkpoint commits a live session's workspace before Host begins a
// nonterminal release. The conversation is already journaled. Host later asks
// the runtime to ReleaseResidency, which writes its own checked release anchor.
func (l *PooledLauncher) Checkpoint(ctx context.Context, tenant sessionwire.TenantID, sessionID sessionwire.SessionID) error {
	if l == nil {
		return errors.New("carbon: nil pooled launcher")
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return &StoreClosedError{}
	}
	var selected *pooledSession
	for _, entry := range l.live {
		if entry.tenant == tenant && entry.sessionID == sessionID {
			selected = entry
			break
		}
	}
	if selected == nil {
		l.mu.Unlock()
		return fmt.Errorf("carbon: no live session for tenant %q session %q", tenant, sessionID)
	}
	l.active.Add(1)
	l.mu.Unlock()
	defer l.active.Done()
	selected.checkpointMu.Lock()
	defer selected.checkpointMu.Unlock()
	controller := selected.controller
	checkpointCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	if idle, ok := controller.(session.IdleWaiter); ok {
		if err := idle.WaitIdle(checkpointCtx); err != nil {
			return err
		}
	} else {
		return errors.New("carbon: pooled session has no idle waiter")
	}
	ref, err := controller.CheckpointWorkspace(checkpointCtx)
	if err != nil {
		return err
	}
	if ref == "" {
		return errors.New("carbon: workspace checkpoint returned no reference")
	}
	probe, ok := controller.(interface{ FaultErr() error })
	if !ok {
		return errors.New("carbon: pooled session cannot report durable checkpoint faults")
	}
	return probe.FaultErr()
}

func (l *PooledLauncher) release(id uuid.UUID, entry *pooledSession) {
	entry.once.Do(func() {
		entry.checkpointMu.Lock()
		defer entry.checkpointMu.Unlock()
		entry.mcp.close(context.Background())
		_ = entry.access.Close()
		if entry.credentialRuntime != nil {
			entry.credentialRuntime.endSession()
			releaseCredentialComposition(entry.credentialRuntime, entry.credentialLease)
		}
		l.mu.Lock()
		if l.live[id] == entry {
			delete(l.live, id)
		}
		l.mu.Unlock()
	})
}

func (l *PooledLauncher) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		l.closeCancel()
		// #nosec G118 -- cleanup outlives this caller's deadline so admitted
		// launches cannot lose their journal backend while still using it.
		go l.closeOwned()
	}
	l.mu.Unlock()
	select {
	case <-l.closeDone:
		return l.closeErr
	default:
	}
	select {
	case <-l.closeDone:
		return l.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeOwned waits for admitted launches and journal opens before closing their
// stores. A dependency that ignores cancellation may retain resources; caller
// deadlines bound waiting for Close, not this single ownership cleanup.
func (l *PooledLauncher) closeOwned() {
	l.active.Wait()
	l.mu.Lock()
	entries := make(map[uuid.UUID]*pooledSession, len(l.live))
	for id, entry := range l.live {
		entries[id] = entry
	}
	l.mu.Unlock()
	var first error
	for id, entry := range entries {
		if err := entry.controller.Shutdown(context.Background()); err != nil && first == nil {
			first = err
		}
		l.release(id, entry)
	}
	for _, bundle := range l.tenants {
		if err := bundle.fs.Close(); err != nil && first == nil {
			first = err
		}
	}
	l.closeErr = first
	close(l.closeDone)
}
