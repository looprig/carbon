package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/inference"
)

// PooledLauncher constructs a separate Carbon rig, access policy, gate, process
// supervisor, credential admission, and MCP manager for every launched session.
// The durable backend is shared, while each workspace is derived from the tenant
// and session identity under dataDir. A restore resolves the same workspace.
type PooledLauncher struct {
	mu      sync.Mutex
	dataDir string
	cfg     Config
	options serveHostConfig
	fs      *fsstore.Store
	stores  *sessionStores
	live    map[uuid.UUID]*pooledSession
	closed  bool
}

type pooledSession struct {
	controller        session.SessionController
	access            *sessionAccess
	mcp               mcpSessionAssembly
	credentialRuntime *credentialRuntime
	credentialLease   *credentialRegistryLease
	once              sync.Once
}

// OpenPooledLauncher opens the durable backend used by all per-session rigs.
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
	fs, err := fsstore.Open(fsstore.Options{Root: dataDir})
	if err != nil {
		return nil, &StoreInitError{Stage: "fsstore", Cause: err}
	}
	stores, err := openStores(fs.Backend())
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	stores.resourceStorage = newPersistedResourceStorageProvider(dataDir)
	return &PooledLauncher{dataDir: dataDir, cfg: cfg, options: options, fs: fs, stores: stores, live: make(map[uuid.UUID]*pooledSession)}, nil
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

func (l *PooledLauncher) SupportsPooled() bool { return l != nil && l.stores != nil }

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
		resolved, resolveErr := resolveServeModels(ctx, cfg, load, loadProductionModels)
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
	assembled, err := buildRigForDelegationCaps(definition, l.stores, root, cfg, false,
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
	if err := l.fs.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
