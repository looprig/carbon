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
	"github.com/looprig/inference"
)

// PooledLauncher constructs a separate Carbon rig, access policy, gate, process
// supervisor, credential admission, and MCP manager for every launched session.
// Each tenant owns a durable backend because Harness's journal layout marker
// binds one backend to one tenant. A restore resolves the same backend and root.
type PooledLauncher struct {
	mu      sync.Mutex
	dataDir string
	cfg     Config
	options serveHostConfig
	tenants map[sessionwire.TenantID]*pooledTenantStores
	live    map[uuid.UUID]*pooledSession
	closed  bool
}

type pooledTenantStores struct {
	fs     *fsstore.Store
	stores *sessionStores
}

const tenantJournalDigestDomain = "looprig/carbon/tenant-journal-root/v1"

type pooledSession struct {
	controller        session.SessionController
	access            *sessionAccess
	mcp               mcpSessionAssembly
	credentialRuntime *credentialRuntime
	credentialLease   *credentialRegistryLease
	once              sync.Once
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
	return &PooledLauncher{dataDir: dataDir, cfg: cfg, options: options, tenants: make(map[sessionwire.TenantID]*pooledTenantStores), live: make(map[uuid.UUID]*pooledSession)}, nil
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
	defer l.mu.Unlock()
	if l.closed {
		return nil, &StoreClosedError{}
	}
	bundle, err := l.tenantStoresLocked(tenant)
	if err != nil {
		return nil, err
	}
	return bundle.stores.session, nil
}

func (l *PooledLauncher) tenantStoresLocked(tenant sessionwire.TenantID) (*pooledTenantStores, error) {
	if err := tenant.Validate(); err != nil {
		return nil, err
	}
	if bundle := l.tenants[tenant]; bundle != nil {
		return bundle, nil
	}
	sum := sha256.Sum256([]byte(tenantJournalDigestDomain + "\x00" + string(tenant)))
	root := filepath.Join(l.dataDir, "tenant-journals", hex.EncodeToString(sum[:]))
	fs, err := fsstore.Open(fsstore.Options{Root: root})
	if err != nil {
		return nil, &StoreInitError{Stage: "tenant-fsstore", Cause: err}
	}
	stores, err := openTenantStores(fs.Backend(), tenant)
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	stores.resourceStorage = newPersistedResourceStorageProvider(root)
	bundle := &pooledTenantStores{fs: fs, stores: stores}
	l.tenants[tenant] = bundle
	return bundle, nil
}

// Launch always builds session-scoped bindings. It returns the harness controller
// itself so Host retains its segregated capabilities and liveness observation.
func (l *PooledLauncher) Launch(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
	if l == nil {
		return nil, errors.New("carbon: nil pooled launcher")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, &StoreClosedError{}
	}
	root, err := materializeSessionWorkspaceRoot(l.dataDir, scope.TenantID, scope.SessionID)
	if err != nil {
		return nil, err
	}
	if scope.WorkspaceRoot != "" && scope.WorkspaceRoot != root {
		return nil, &ForeignWorkspaceRootError{SessionID: scope.SessionID, Want: scope.WorkspaceRoot, Served: root}
	}
	if scope.RigSessionID.IsZero() {
		return nil, fmt.Errorf("carbon: pooled launch needs a runtime session id")
	}
	if _, exists := l.live[scope.RigSessionID]; exists {
		return nil, fmt.Errorf("carbon: runtime session %s is already live", scope.RigSessionID)
	}
	bundle, err := l.tenantStoresLocked(scope.TenantID)
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
		resolved, resolveErr := resolveServeModelsAtRoot(ctx, cfg, load, loadProductionModels, root)
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
	cfg.AccessConfigRev = access.configRev
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
	ctx = detachSessionLifetime(ctx)
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
	entry := &pooledSession{controller: controller, access: access, mcp: mcp, credentialRuntime: credentials, credentialLease: lease}
	l.live[scope.RigSessionID] = entry
	if signal, ok := controller.(session.Liveness); ok {
		go func() {
			<-signal.Done()
			_ = controller.Shutdown(ctx)
			l.release(scope.RigSessionID, entry)
		}()
	}
	return controller, nil
}

func (l *PooledLauncher) release(id uuid.UUID, entry *pooledSession) {
	entry.once.Do(func() {
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
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	entries := make(map[uuid.UUID]*pooledSession, len(l.live))
	for id, entry := range l.live {
		entries[id] = entry
	}
	l.mu.Unlock()
	var first error
	for id, entry := range entries {
		if err := entry.controller.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
		l.release(id, entry)
	}
	for _, bundle := range l.tenants {
		if err := bundle.fs.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
