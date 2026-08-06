package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// persistence.go is the CodeRig's composition-root wiring for durable session state. The
// rig owns the whole session lifecycle — the single-writer lease, workspace snapshots at
// quiescence, and offload-blob GC — so this layer only opens the store facades, builds one
// immutable rig per resolved Open (a fresh rig per config; /clear rebuilds with the same
// process config), and hands NewSession/RestoreSession to the sessionAdapter adapter. The
// headless New path (swarm.go) shares the SAME rig builder over a process-shared in-memory
// store, so headless and persisted sessions are identical but for the backend.

// offloadGCInterval is how often the rig runs one offload-blob GC pass; offloadGCTimeout
// bounds each pass. A few minutes is plenty for a local single-user CLI.
const (
	offloadGCInterval = 5 * time.Minute
	offloadGCTimeout  = 60 * time.Second
	// snapshotTimeout bounds one best-effort workspace snapshot at quiescence.
	snapshotTimeout = 60 * time.Second
	// maxRuntimeManifestIdentifierBytes matches Harness's current-schema runtime
	// manifest identifier cap. Production model and catalog revisions are each
	// 64-byte hex digests, so their short form needs the digest fallback below.
	maxRuntimeManifestIdentifierBytes = 128
)

const runtimeCatalogRevisionDigestDomain = "looprig/coderig/runtime-catalog-revision/v1"

// DefaultDataDir is the default root for the on-disk session store: ~/.looprig/store. It
// fails loud with a typed *StoreInitError if the home directory cannot be resolved. It
// preserves this exact behavior/signature for compatibility; DefaultDataDirIn is the
// home-relative form callers holding a resolved Config should prefer.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreInitError{Stage: "data-dir", Cause: err}
	}
	return DefaultDataDirIn(filepath.Join(home, ".looprig"))
}

// DefaultDataDirIn is the on-disk session store root under an already-resolved
// looprig base directory: <home>/store. home is looprigHome's result (e.g.
// ~/.looprig or Config.HomeDir) — this function does not resolve HOME itself.
func DefaultDataDirIn(home string) (string, error) {
	return filepath.Join(home, "store"), nil
}

// swarmStores bundles the session + workspace facades and the root leaser the rig needs,
// all over ONE storage.Composite backend. It is read-only after construction.
type swarmStores struct {
	session   *sessionstore.Store
	workspace *workspacestore.Store
	leaser    storage.Leaser
	catalog   *sessionstore.Catalog
	// resourceStorage is this backend's rig.SessionResourceStorageProvider: the persisted
	// on-disk provider for ensureStoresLocked's fsstore backend, the process-owned headless
	// provider for headlessStores' process-shared in-memory backend, and nil for any
	// swarmStores a narrow package test builds directly via openStores (most package tests
	// are unconcerned with process-service tools). buildRigWithRegistrationAndACP only
	// installs rig.WithSessionResourceStorage when this is non-nil, so its absence changes
	// nothing for a topology that does not declare tool.RequiresProcessServices (none does
	// today).
	resourceStorage rig.SessionResourceStorageProvider
}

// openStores wires the session + workspace facades and the listing catalog over one backend
// composite (fsstore for the persisted path, memstore for headless). The catalog is wired
// with a replayer so a missing listing entry can be repaired by folding the ledger.
func openStores(backend *storage.Composite) (*swarmStores, error) {
	sessionStore, err := sessionstore.Open(backend)
	if err != nil {
		return nil, &StoreInitError{Stage: "sessionstore", Cause: err}
	}
	workspaceStore, err := workspacestore.Open(backend.Blobs)
	if err != nil {
		return nil, &StoreInitError{Stage: "workspacestore", Cause: err}
	}
	catalog := sessionStore.OpenCatalog(sessionstore.WithCatalogReplayer(sessionStore))
	return &swarmStores{
		session:   sessionStore,
		workspace: workspaceStore,
		leaser:    backend.Leaser,
		catalog:   catalog,
	}, nil
}

// sessionResourceStorageIdentity is the stable identity CodeRig's session resource-storage
// provider reports through rig.SessionResourceStorage.Identity. It names the on-disk scheme
// this provider implements -- <data-dir>/resources/<session-id> for persisted sessions, a
// process-owned temporary base for headless ones -- and must change ONLY when that scheme's
// shape changes (a different path template, a different anchor convention, etc.), never
// between two calls for the same session, and never merely because a session's config, model,
// or access profile changes (that drift is the rig's own config fingerprint's job, an
// entirely separate mechanism from this one). Harness's own identity anchor is what actually
// detects and fails a restore closed on a mismatch (harness internal/sessionruntime's
// ensureSessionResourceAnchor/validateSessionResourceAnchor); this constant is CodeRig's one
// input to that check. Bump the version suffix, never reuse it, if CodeRig's resource-storage
// scheme ever changes shape.
const sessionResourceStorageIdentity = "coderig:session-resource-storage/v1"

// persistedResourceStorageProvider resolves each persisted session's process-resource storage
// root to <data-dir>/resources/<session-id>, under the SAME data-dir root SessionStoreFactory
// opens its session/workspace stores under (ensureStoresLocked). It is a pure function of
// dataDir + session id -- no mutable state -- so it is trivially safe for concurrent use
// (SessionResourceStorageProvider's doc requirement) and trivially resolves the SAME
// path/identity for the same session id across repeated calls, including across a real
// process restart: a fresh SessionStoreFactory reconstructed over the same dataDir after
// CodeRig restarts constructs an equal provider by construction, not by any cached state. The
// resolved path is always a subdirectory of dataDir, which is CodeRig's own state directory
// (DefaultDataDirIn) and structurally independent of any session's workspace root (the
// checked-out code directory the rig places separately via WithExclusiveWorkspace), so it can
// never overlap a workspace.
type persistedResourceStorageProvider struct {
	dataDir string
}

func newPersistedResourceStorageProvider(dataDir string) *persistedResourceStorageProvider {
	return &persistedResourceStorageProvider{dataDir: dataDir}
}

func (p *persistedResourceStorageProvider) StorageForSession(_ context.Context, id uuid.UUID) (rig.SessionResourceStorage, error) {
	return rig.SessionResourceStorage{
		Path:     filepath.Join(p.dataDir, "resources", id.String()),
		Identity: sessionResourceStorageIdentity,
	}, nil
}

var _ rig.SessionResourceStorageProvider = (*persistedResourceStorageProvider)(nil)

// headlessResourceStorageProvider resolves process-resource storage roots for headless
// CodeRig sessions under ONE process-owned temporary base directory, minted once via
// os.MkdirTemp at construction. It never collides with another concurrently running headless
// CodeRig process: each process constructs its own provider, and os.MkdirTemp mints a fresh,
// uniquely named base directory every time. Unlike the persisted provider, it deliberately
// does NOT promise stability across a real process restart -- a fresh process gets a fresh
// base and so a fresh (and, if a prior run's directory happens to still be on disk, distinct)
// resource root for the same session id. The narrower stability it DOES uphold, matching this
// task's explicit contract, is same-process stability: bySession remembers each session id's
// subdirectory for the lifetime of this provider, so reconstructing a session within the same
// running process (e.g. a resume during this run) resolves the identical root. The base
// directory is intentionally never removed by this type -- like the process-shared in-memory
// store it sits alongside (headlessStores), it is scoped to, and discarded only with, the
// CodeRig process itself.
type headlessResourceStorageProvider struct {
	base string

	mu        sync.Mutex
	bySession map[uuid.UUID]string
}

func newHeadlessResourceStorageProvider() (*headlessResourceStorageProvider, error) {
	base, err := os.MkdirTemp("", "coderig-headless-resources-*")
	if err != nil {
		return nil, &StoreInitError{Stage: "headless-resource-storage", Cause: err}
	}
	return &headlessResourceStorageProvider{base: base, bySession: make(map[uuid.UUID]string)}, nil
}

func (p *headlessResourceStorageProvider) StorageForSession(_ context.Context, id uuid.UUID) (rig.SessionResourceStorage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	path, ok := p.bySession[id]
	if !ok {
		path = filepath.Join(p.base, id.String())
		p.bySession[id] = path
	}
	return rig.SessionResourceStorage{Path: path, Identity: sessionResourceStorageIdentity}, nil
}

var _ rig.SessionResourceStorageProvider = (*headlessResourceStorageProvider)(nil)

// headlessShared holds the process-shared in-memory store the headless New path uses, opened
// once. Two headless sessions therefore share ONE backend and contend on the SAME exclusive
// root lease for the current checkout — exactly like two persisted sessions. They also share
// ONE headlessResourceStorageProvider, matching the "one process-owned temporary base" contract:
// two headless sessions in this process get distinct subdirectories of the SAME base, never two
// different bases.
var (
	headlessOnce   sync.Once
	headlessResult *swarmStores
	headlessError  error
)

// headlessStores returns the process-shared in-memory store facades, opening them once.
func headlessStores() (*swarmStores, error) {
	headlessOnce.Do(func() {
		stores, err := openStores(memstore.New())
		if err != nil {
			headlessError = err
			return
		}
		provider, err := newHeadlessResourceStorageProvider()
		if err != nil {
			headlessError = err
			return
		}
		stores.resourceStorage = provider
		headlessResult = stores
	})
	return headlessResult, headlessError
}

// agentFingerprintFields assembles the rig-level config-fingerprint inputs that are not
// part of a loop.Definition: the swarm+active-primer AgentKind, the human-set RuntimeSkills mode,
// and the durable access configuration identity. NativePermissionPolicyRev carries the
// secret-free access digest (access ABI version, selected profile, normalized builder, planner,
// and reviewer profiles, and the non-secret egress route identity/guarantees) so a product-profile,
// reviewer-restriction, or egress-boundary change invalidates a restore rather than silently
// resuming with different authority. AppFields additionally surfaces the human-visible profile
// name for the richer manifest (the 5.2 SessionPresenter consumes it). The workspace-root field
// is owned by the rig's exclusive-workspace placement, so it is not set here. A zero Config
// (AccessProfile/AccessConfigRev empty) leaves both access inputs empty, keeping the fields
// additive for callers that do not select a profile.
func agentFingerprintFields(cfg Config) rig.ConfigFingerprintFields {
	fields := rig.ConfigFingerprintFields{
		AgentKind:                 agentKind,
		RuntimeSkills:             cfg.RuntimeSkills,
		NativePermissionPolicyRev: cfg.AccessConfigRev,
		ExternalCapabilityRev:     cfg.MCPConfigRev,
		AppFields:                 accessAppFields(cfg.AccessProfile),
	}
	fields.RuntimeCatalogRev = cfg.ModelConfigRev
	catalog := effectiveRuntimeCatalog(cfg)
	if catalog.HasEntries() {
		catalogRev := catalog.Digest()
		fields.RuntimeCatalogRev = combineRuntimeCatalogRevisions(fields.RuntimeCatalogRev, catalogRev)
	}
	return fields
}

// effectiveRuntimeCatalog keeps focused pre-general-catalogue fixtures
// source-compatible while production always supplies RuntimeCatalog directly.
// The ACP fallback is intentionally not used by production composition.
func effectiveRuntimeCatalog(cfg Config) loop.RuntimeCatalog {
	if cfg.RuntimeCatalog.HasEntries() {
		return cfg.RuntimeCatalog
	}
	if cfg.ACPChildren != nil {
		return cfg.ACPChildren.Catalog.RuntimeCatalog
	}
	return cfg.RuntimeCatalog
}

// combineRuntimeCatalogRevisions derives the one durable runtime revision from
// the model configuration and compiled runtime catalogue revisions. Keep the
// historical short synthetic form where it fits; production-length revisions
// use a domain-separated hex digest so the manifest cap can never be exceeded.
func combineRuntimeCatalogRevisions(modelConfigRev, runtimeCatalogRev string) string {
	if modelConfigRev == "" {
		return runtimeCatalogRev
	}
	if runtimeCatalogRev == "" {
		return modelConfigRev
	}
	combined := modelConfigRev + "/" + runtimeCatalogRev
	if len(combined) <= maxRuntimeManifestIdentifierBytes {
		return combined
	}
	digest := sha256.Sum256([]byte(runtimeCatalogRevisionDigestDomain + "\x00" + modelConfigRev + "\x00" + runtimeCatalogRev))
	return hex.EncodeToString(digest[:])
}

// operatorFingerprintFields is a legacy package-local fixture seam. New
// production sessions use agentFingerprintFields and the builder-active kind.
//
//lint:ignore U1000 retained for package-local legacy fixtures.
func operatorFingerprintFields(cfg Config) rig.ConfigFingerprintFields {
	fields := agentFingerprintFields(cfg)
	fields.AgentKind = operatorAgentKind
	return fields
}

// accessAppFields returns the secret-free, human-visible access manifest fields, or nil when no
// profile is selected (keeping the manifest additive).
func accessAppFields(profile AccessProfile) map[string]string {
	if profile == "" {
		return nil
	}
	return map[string]string{"access_profile": string(profile)}
}

// buildRig assembles ONE immutable rig from the three loop definitions over the given store
// facades, placing root as the session's EXCLUSIVE workspace (edit-the-open-checkout). The
// rig owns snapshots-on-idle, delegation limits, the config fingerprint, the per-session
// security limit, and offload-blob GC. allowMismatch opts a resume into proceeding despite a config
// fingerprint change (never set for a new session). It always assembles with permission review
// DISABLED (the zero permissionReviewRegistration): the two session-opening paths that can
// actually enable it (openSessionWithDefinitions, called from swarm.go, where the inference
// Client the classifier needs is available) call buildRigForDelegationCaps directly with an
// explicit registration instead. buildRig stays the plain default-composition path most
// existing callers (production's delegation defaults, and every test unconcerned with
// permission review) use unchanged.
func buildRig(definitions []loop.Definition, stores *swarmStores, root string, cfg Config, allowMismatch bool) (*rig.Rig, error) {
	return buildRigForDelegationCaps(definitions, stores, root, cfg, allowMismatch, rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReviewRegistration{})
}

// buildRigForDelegationCaps is the common assembly path with explicit delegation caps and an
// explicit permission-review registration. Production callers use buildRig's CodeRig delegation
// defaults paired with the real registration built from the live inference Client
// (openSessionWithDefinitions); focused topology tests vary only the delegation limits while
// retaining the exact production definitions, stores, workspace, and policy wiring.
func buildRigForDelegationCaps(definitions []loop.Definition, stores *swarmStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, permissionReview permissionReviewRegistration) (*rig.Rig, error) {
	registration, err := newConversationHustleRegistration()
	if err != nil {
		return nil, err
	}
	return buildRigWithRegistrationAndACP(definitions, stores, root, cfg, allowMismatch, limits, registration, permissionReview, cfg.ACPChildren)
}

// buildRigWithRegistration is the common immutable rig assembly. Production
// supplies exactly one reviewed hustle registration and one permission-review
// registration; focused tests can vary either's public descriptor/limits to
// prove fingerprint sensitivity. permissionReview's disabled zero value adds
// no options, so passing it changes nothing about the assembled rig.
func buildRigWithRegistration(definitions []loop.Definition, stores *swarmStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, registration conversationHustleRegistration, permissionReview permissionReviewRegistration) (*rig.Rig, error) {
	return buildRigWithRegistrationAndACP(definitions, stores, root, cfg, allowMismatch, limits, registration, permissionReview, cfg.ACPChildren)
}

func buildRigWithRegistrationAndACP(definitions []loop.Definition, stores *swarmStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, registration conversationHustleRegistration, permissionReview permissionReviewRegistration, acpChildren *ACPComposition) (*rig.Rig, error) {
	primers, activePrimer := primerConfiguration(definitions)
	options := []rig.Option{
		rig.WithLoops(definitions...),
		rig.WithPrimers(primers...),
		rig.WithActivePrimer(activePrimer),
		rig.WithSessionStore(stores.session),
		rig.WithExclusiveWorkspace(stores.workspace, root, stores.leaser),
		rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: rig.SnapshotBestEffort, Timeout: snapshotTimeout}),
		rig.WithDelegationLimits(limits),
		rig.WithFingerprintFields(agentFingerprintFields(cfg)),
		rig.WithOffloadGC(rig.OffloadGCPolicy{Interval: offloadGCInterval, Timeout: offloadGCTimeout}),
	}
	if stores.resourceStorage != nil {
		options = append(options, rig.WithSessionResourceStorage(stores.resourceStorage))
	}
	if catalog := effectiveRuntimeCatalog(cfg); catalog.HasEntries() {
		options = append(options, rig.WithRuntimeCatalog(catalog))
	}
	if acpChildren != nil {
		if acpChildren.Live != nil && acpChildren.Restored != nil {
			options = append(options, rig.WithForeignBuilders(acpChildren.Live, acpChildren.Restored))
		}
	}
	options = append(options, registration.options()...)
	options = append(options, permissionReview.options()...)
	if allowMismatch {
		options = append(options, rig.WithAllowConfigMismatch())
	}
	return rig.Define(options...)
}

// primerConfiguration keeps the production roster explicit while allowing
// narrow package tests to assemble a smaller custom definition set. The real
// CodeRig definitions always take the three-primer branch; a custom set uses
// its managed-delegating root as its only primer.
func primerConfiguration(definitions []loop.Definition) ([]string, string) {
	byName := make(map[identity.AgentName]loop.Definition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name()] = definition
	}
	if _, plannerOK := byName[planner.Name]; plannerOK {
		if _, builderOK := byName[builder.Name]; builderOK {
			if _, reviewerOK := byName[reviewer.Name]; reviewerOK {
				return []string{string(planner.Name), string(builder.Name), string(reviewer.Name)}, string(activePrimerName)
			}
		}
	}
	for _, definition := range definitions {
		if len(definition.Delegates()) != 0 || definition.Delegation().Style == loop.DelegationManaged {
			return []string{string(definition.Name())}, string(definition.Name())
		}
	}
	if len(definitions) == 0 {
		return nil, ""
	}
	return []string{string(definitions[0].Name())}, string(definitions[0].Name())
}

// SessionSelector chooses which session a persisted Open opens. The zero value (Resume zero)
// opens a NEW session; a non-zero Resume restores that existing session. AllowConfigMismatch
// is the resume-only opt-in to proceed despite a config fingerprint change.
type SessionSelector struct {
	Resume              uuid.UUID
	AllowConfigMismatch bool
}

// SessionStoreFactory is the process-level composition root that owns the on-disk store and,
// on each Open, builds one immutable rig and opens (new) or restores (resume) a session over
// it. It holds the fsstore backend (closed once at teardown) and the store facades + listing
// catalog over it. Read-only after construction.
type SessionStoreFactory struct {
	dataDir    string
	fs         *fsstore.Store
	stores     *swarmStores
	loadModels productionModelsLoader
	// buildClient is retained as an explicit-client compatibility seam for
	// package tests. When set, Open bypasses models.json and production ACP
	// composition exactly like openWithClient.
	buildClient    func() (inference.Client, ModelFactory, error)
	storeMu        sync.Mutex
	closed         bool
	mu             sync.Mutex
	currentSession uuid.UUID
}

// NewSessionStoreFactory returns a lazy production factory for the on-disk store rooted at
// dataDir. The backend is opened by List, or by Open after production model configuration has
// been loaded and validated, so configuration failures cannot open or mutate persistence.
func NewSessionStoreFactory(dataDir string) (*SessionStoreFactory, error) {
	return &SessionStoreFactory{dataDir: dataDir, loadModels: loadProductionModels}, nil
}

// Close releases the shared on-disk backend, once, at process teardown (after every session
// opened from this factory has been Closed). It is idempotent.
func (f *SessionStoreFactory) Close() error {
	f.storeMu.Lock()
	defer f.storeMu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	fs := f.fs
	if fs == nil {
		return nil
	}
	return fs.Close()
}

func (f *SessionStoreFactory) ensureStoresLocked() (*swarmStores, error) {
	if f.closed {
		return nil, &StoreClosedError{}
	}
	if f.stores != nil {
		return f.stores, nil
	}
	fs, err := fsstore.Open(fsstore.Options{Root: f.dataDir})
	if err != nil {
		return nil, &StoreInitError{Stage: "fsstore", Cause: err}
	}
	stores, err := openStores(fs.Backend())
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	stores.resourceStorage = newPersistedResourceStorageProvider(f.dataDir)
	f.fs = fs
	f.stores = stores
	return stores, nil
}

// List returns the session catalog (most-recently-active-first), the source the CLI --list
// path prints. It reads the listing index only — no lease, no replay — so it stays cheap.
func (f *SessionStoreFactory) List(ctx context.Context) ([]sessionstore.SessionMeta, error) {
	f.storeMu.Lock()
	defer f.storeMu.Unlock()
	stores, err := f.ensureStoresLocked()
	if err != nil {
		return nil, err
	}
	return stores.catalog.ListSessions(ctx)
}

// Open builds a fully-persisted CodeRig session from one models.json load.
func (f *SessionStoreFactory) Open(ctx context.Context, sel SessionSelector, cfg Config) (*RuntimeAgent, error) {
	var client inference.Client
	var factory ModelFactory
	if f.buildClient != nil {
		var err error
		client, factory, err = f.buildClient()
		if err != nil {
			return nil, err
		}
	} else {
		home, err := looprigHome(cfg)
		if err != nil {
			return nil, err
		}
		configured, err := f.loadModels(home)
		if err != nil {
			return nil, err
		}
		if configured.PrimerClient == nil || configured.PrimerModel.Name == "" {
			return nil, &ModelConfigCapabilityError{}
		}
		client = configured.RuntimeClient
		if client == nil {
			client = configured.PrimerClient
		}
		factory = newModelFactoryFor(configured.PrimerModel)
		cfg.ModelConfigRev = configured.ConfigRev
		cfg.PrimerAlias = configured.PrimerAlias
		cfg.PrimerEfforts = append([]model.Effort(nil), configured.PrimerEfforts...)
		cfg.PrimerCandidates = append([]PrimerCandidate(nil), configured.PrimerCandidates...)
		cfg.DelegateModels = delegateModelsFrom(configured.ACP)
		cfg, err = withProductionACPChildren(ctx, cfg, configured)
		if err != nil {
			return nil, err
		}
		// Programmatic enable wins: a models.json permission_review section can
		// only ever ENABLE permission review, never override an already-enabled
		// programmatic selection (see Config.PermissionReviewEnabled's doc
		// comment). newPermissionReviewRegistration's own trusted-profile gate
		// (permission_review.go) still applies regardless of which source set it.
		if !cfg.PermissionReviewEnabled && configured.PermissionReviewEnabled {
			cfg.PermissionReviewEnabled = true
			cfg.PermissionReviewModel = configured.PermissionReviewModel
			cfg.PermissionReviewStrictPolicy = configured.PermissionReviewStrict
		}
	}
	agent, err := f.openWithClient(ctx, client, factory, sel, cfg)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.currentSession = agent.SessionID()
	f.mu.Unlock()
	return agent, nil
}

// openWithClient resolves the workspace root, builds the three loop definitions and one rig
// over the shared store, and opens (Resume zero) or restores the session. It is the seam the
// integration tests drive with an injected fake client. A resume threads
// sel.AllowConfigMismatch into the rig so a deliberate config change can proceed.
func (f *SessionStoreFactory) openWithClient(ctx context.Context, client inference.Client, factory ModelFactory, sel SessionSelector, cfg Config) (*RuntimeAgent, error) {
	f.storeMu.Lock()
	defer f.storeMu.Unlock()
	stores, err := f.ensureStoresLocked()
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	// Persisted CodeRig is INTERACTIVE: a human at the TUI resolves gates, so the
	// access wiring uses the HOME-derived workspace permission store and interactive
	// gate evaluators. The selected access profile is fixed for the session; the TUI
	// only displays it (SessionPresenter). New and restore share this one path.
	return openRuntimeAgent(ctx, client, factory, cfg, stores, root, sel, true)
}
