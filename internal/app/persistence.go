package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
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
// headless New path (assembly.go) shares the SAME rig assembly over a process-shared in-memory
// store, so headless and persisted sessions are identical but for the backend.

// offloadGCInterval is how often the rig runs one offload-blob GC pass; offloadGCTimeout
// bounds each pass. A few minutes is plenty for a local single-user CLI.
const (
	offloadGCInterval = 5 * time.Minute
	offloadGCTimeout  = 60 * time.Second
	// snapshotTimeout bounds one manually-triggered workspace snapshot. CodeRig never
	// triggers one today (see WithSnapshots below), so this is inert in practice — it
	// exists only because rig.SnapshotPolicy requires a non-negative value.
	snapshotTimeout = 60 * time.Second
	// maxRuntimeManifestIdentifierBytes matches Harness's current-schema runtime
	// manifest identifier cap. Production model and catalog revisions are each
	// 64-byte hex digests, so their short form needs the digest fallback below.
	maxRuntimeManifestIdentifierBytes = 128
)

const runtimeCatalogRevisionDigestDomain = "looprig/coderig/runtime-catalog-revision/v1"

// DefaultDataDir is the default root for the on-disk session store:
// ~/.looprig/coderig/store. It fails loud with a typed *StoreInitError if the home
// directory cannot be resolved. It preserves this exact behavior/signature for
// compatibility; DefaultDataDirIn is the home-relative form callers holding a resolved
// Config should prefer.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreInitError{Stage: "data-dir", Cause: err}
	}
	return DefaultDataDirIn(filepath.Join(home, ".looprig", "coderig"))
}

// DefaultDataDirIn is the on-disk session store root under an already-resolved
// looprig base directory: <home>/store. home is looprigHome's result (e.g.
// ~/.looprig/coderig or Config.HomeDir) — this function does not resolve HOME itself.
func DefaultDataDirIn(home string) (string, error) {
	return filepath.Join(home, "store"), nil
}

// sessionStores bundles the session + workspace facades and the root leaser the rig needs,
// all over ONE storage.Composite backend. It is read-only after construction.
type sessionStores struct {
	session   *sessionstore.Store
	workspace *workspacestore.Store
	leaser    storage.Leaser
	catalog   *sessionstore.Catalog
	// resourceStorage is this backend's rig.SessionResourceStorageProvider: the persisted
	// on-disk provider for ensureStoresLocked's fsstore backend, the process-owned headless
	// provider for headlessStores' process-shared in-memory backend, and nil for any
	// sessionStores a narrow package test builds directly via openStores (a package test that
	// still needs one, because it assembles the real production session, uses openTestStores
	// instead). It is attached here, to sessionStores, rather than threaded as a new parameter
	// through buildRig/openRuntimeAgent, matching how session/workspace/leaser already reach
	// the rig — one more facade over the same backend, not a new call-chain parameter.
	// buildRigWithRegistrationAndACP only installs rig.WithSessionResourceStorage when this is
	// non-nil, so its absence changes nothing for a topology that does not declare
	// tool.RequiresProcessServices. Generic's Bash definition declares it in
	// production, so both real assembly paths above always populate this field;
	// only a topology with a scoped, process-free tool subset can still
	// legitimately leave it nil.
	resourceStorage rig.SessionResourceStorageProvider
}

// openStores wires the session + workspace facades and the listing catalog over one backend
// composite (fsstore for the persisted path, memstore for headless). The catalog is wired
// with a replayer so a missing listing entry can be repaired by folding the ledger.
func openStores(backend *storage.Composite) (*sessionStores, error) {
	sessionStore, err := sessionstore.Open(backend)
	if err != nil {
		return nil, &StoreInitError{Stage: "sessionstore", Cause: err}
	}
	workspaceStore, err := workspacestore.Open(backend.Blobs)
	if err != nil {
		return nil, &StoreInitError{Stage: "workspacestore", Cause: err}
	}
	catalog := sessionStore.OpenCatalog(sessionstore.WithCatalogReplayer(sessionStore))
	return &sessionStores{
		session:   sessionStore,
		workspace: workspaceStore,
		leaser:    backend.Leaser,
		catalog:   catalog,
	}, nil
}

// sessionResourceStorageIdentity is the stable identity CodeRig's session resource-storage
// provider reports through rig.SessionResourceStorage.Identity. It names the on-disk scheme
// this provider implements — <data-dir>/resources/<session-id> for persisted sessions, a
// process-owned temporary base for headless ones — and must change ONLY when that scheme's
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
// dataDir + session id — no mutable state — so it is trivially safe for concurrent use
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
// does NOT promise stability across a real process restart — a fresh process gets a fresh
// base and so a fresh (and, if a prior run's directory happens to still be on disk, distinct)
// resource root for the same session id. The narrower stability it DOES uphold, matching this
// task's explicit contract, is same-process stability: like persistedResourceStorageProvider,
// base is immutable after construction and uuid.UUID.String() is a pure hex encoding, so
// StorageForSession is a pure function of (base, id) with no mutable state — recomputing the
// join on every call already resolves the identical path for the same session id for the
// lifetime of this provider, so reconstructing a session within the same running process (e.g.
// a resume during this run) resolves the identical root, with no cache required. The base
// directory is intentionally never removed by this type — like the process-shared in-memory
// store it sits alongside (headlessStores), it is scoped to, and discarded only with, the
// CodeRig process itself.
type headlessResourceStorageProvider struct {
	base string
}

func newHeadlessResourceStorageProvider() (*headlessResourceStorageProvider, error) {
	base, err := os.MkdirTemp("", "coderig-headless-resources-*")
	if err != nil {
		return nil, &StoreInitError{Stage: "headless-resource-storage", Cause: err}
	}
	return &headlessResourceStorageProvider{base: base}, nil
}

func (p *headlessResourceStorageProvider) StorageForSession(_ context.Context, id uuid.UUID) (rig.SessionResourceStorage, error) {
	return rig.SessionResourceStorage{
		Path:     filepath.Join(p.base, id.String()),
		Identity: sessionResourceStorageIdentity,
	}, nil
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
	headlessResult *sessionStores
	headlessError  error
)

// headlessStores returns the process-shared in-memory store facades, opening them once.
func headlessStores() (*sessionStores, error) {
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
// part of a loop.Definition: the Generic AgentKind, the human-set RuntimeSkills mode,
// and the durable access configuration identity. NativePermissionPolicyRev carries the
// secret-free access digest (access ABI version, selected Generic profile, and
// the non-secret egress route identity/guarantees) so an access-profile or
// egress-boundary change invalidates a restore rather than silently
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

// accessAppFields returns the secret-free, human-visible access manifest fields, or nil when no
// profile is selected (keeping the manifest additive).
func accessAppFields(profile AccessProfile) map[string]string {
	if profile == "" {
		return nil
	}
	return map[string]string{"access_profile": string(profile)}
}

// buildRig assembles ONE immutable rig from the Generic definition over the given store
// facades, placing root as the session's EXCLUSIVE workspace (edit-the-open-checkout). The
// rig owns snapshots-on-idle, delegation limits, the config fingerprint, the per-session
// security limit, and offload-blob GC. allowMismatch opts a resume into proceeding despite a config
// fingerprint change (never set for a new session). It always assembles with permission review
// DISABLED (the zero permissionReviewRegistration): the two session-opening paths that can
// actually enable it (openSessionWithDefinitions, called from assembly.go, where the inference
// Client the classifier needs is available) call buildRigForDelegationCaps directly with an
// explicit registration instead. buildRig stays the plain default-composition path most
// existing callers (production's default delegation limits, and every test unconcerned with
// permission review) use unchanged.
func buildRig(definitions []loop.Definition, stores *sessionStores, root string, cfg Config, allowMismatch bool) (*rig.Rig, error) {
	return buildRigForDelegationCaps(definitions, stores, root, cfg, allowMismatch, rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReviewRegistration{})
}

// buildRigForDelegationCaps is the common assembly path with explicit delegation caps and an
// explicit permission-review registration. Production callers use buildRig's CodeRig delegation
// defaults paired with the real registration built from the live inference Client
// (openSessionWithDefinitions); focused topology tests vary only the delegation limits while
// retaining the exact production definitions, stores, workspace, and policy wiring.
func buildRigForDelegationCaps(definitions []loop.Definition, stores *sessionStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, permissionReview permissionReviewRegistration) (*rig.Rig, error) {
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
func buildRigWithRegistration(definitions []loop.Definition, stores *sessionStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, registration conversationHustleRegistration, permissionReview permissionReviewRegistration) (*rig.Rig, error) {
	return buildRigWithRegistrationAndACP(definitions, stores, root, cfg, allowMismatch, limits, registration, permissionReview, cfg.ACPChildren)
}

func buildRigWithRegistrationAndACP(definitions []loop.Definition, stores *sessionStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, registration conversationHustleRegistration, permissionReview permissionReviewRegistration, acpChildren *ACPComposition) (*rig.Rig, error) {
	primers, activePrimer := primerConfiguration(definitions)
	options := []rig.Option{
		rig.WithLoops(definitions...),
		rig.WithPrimers(primers...),
		rig.WithActivePrimer(activePrimer),
		rig.WithSessionStore(stores.session),
		rig.WithExclusiveWorkspace(stores.workspace, root, stores.leaser),
		// Manual, never OnIdle: CodeRig's exclusive workspace is the user's own persistent
		// local checkout (edit-the-open-checkout), not ephemeral compute that needs a
		// durable snapshot to survive being torn down — workspacestore's actual design
		// target (see its package doc). An OnIdle policy archived the ENTIRE tree (no
		// .git/vendor/build-artifact exclusion) on every idle turn, at ~100s of MB each,
		// unboundedly, purely so a later restore could verify-and-materialize against it —
		// and that verification then hard-failed the restore outright the moment the
		// checkout diverged even slightly from the checkpoint (any edit by the user or
		// another tool, or even the session's own best-effort snapshot lagging its own
		// last write). Manual disables automatic snapshotting entirely: no archive is ever
		// written, and a session with no recorded checkpoint pointer skips the
		// materialize-verification gate on restore altogether.
		rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotManual, Priority: rig.SnapshotBestEffort, Timeout: snapshotTimeout}),
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

// primerConfiguration chooses the sole Generic primer. Narrow package tests
// may still provide one custom managed definition as a local seam.
func primerConfiguration(definitions []loop.Definition) ([]string, string) {
	if len(definitions) == 1 && definitions[0].Name() == activePrimerName {
		return []string{string(activePrimerName)}, string(activePrimerName)
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
	stores     *sessionStores
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

func (f *SessionStoreFactory) ensureStoresLocked() (*sessionStores, error) {
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

// List returns the session catalog (most-recently-active-first), scoped to sessions
// opened from the CURRENT working directory's workspace — the source the CLI --list path
// prints. It reads the listing index only — no lease, no replay — so it stays cheap. The
// catalog itself (harness's Catalog.ListSessions) returns entries in ascending session-ID
// order, an arbitrary-but-deterministic order that carries no time meaning (session IDs are
// random UUIDs), so the recency ordering this method promises is applied here. The store is
// shared across every workspace CodeRig has ever been run from (it lives under the looprig
// home directory, not the workspace), so without this scoping every project's session
// history would bleed into every other project's picker — unlike Claude Code, which scopes
// its own session history to the current project.
func (f *SessionStoreFactory) List(ctx context.Context) ([]sessionstore.SessionMeta, error) {
	f.storeMu.Lock()
	defer f.storeMu.Unlock()
	stores, err := f.ensureStoresLocked()
	if err != nil {
		return nil, err
	}
	metas, err := stores.catalog.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	if root, err := currentWorkspaceFingerprint(); err == nil {
		metas = filterSameWorkspace(metas, root)
	}
	sortSessionsByRecency(metas)
	return metas, nil
}

// currentWorkspaceFingerprint canonicalizes the current working directory the same way
// harness's rig.WithExclusiveWorkspace placement does (pkg/rig/workspace.go's
// canonicalPath: Abs -> Clean -> best-effort EvalSymlinks), so it can be compared against
// SessionMeta.ConfigFingerprint.WorkspaceRoot (which harness folds as "<placement
// mode>:<canonical root>"). Returns an error only on an os.Getwd failure, in which case
// List leaves the catalog unfiltered rather than failing a session listing outright over
// an unrelated OS hiccup.
func currentWorkspaceFingerprint() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// filterSameWorkspace keeps only sessions whose recorded workspace root matches root, or
// that recorded no workspace at all (empty WorkspaceRoot never belongs to a DIFFERENT
// project, so it stays visible rather than being hidden as cross-workspace noise). It
// matches by suffix rather than reconstructing the exact "<mode>:<root>" fingerprint
// string, so it stays correct even if harness's placement-mode naming changes — CodeRig
// only ever opens sessions with fixed exclusive placement, so any fingerprint ending in
// this exact canonical root is this workspace's session. Filters in place (metas is not
// reused by the caller afterward).
func filterSameWorkspace(metas []sessionstore.SessionMeta, root string) []sessionstore.SessionMeta {
	kept := metas[:0]
	for _, meta := range metas {
		fp := meta.ConfigFingerprint.WorkspaceRoot
		if fp == "" || strings.HasSuffix(fp, ":"+root) {
			kept = append(kept, meta)
		}
	}
	return kept
}

// sortSessionsByRecency orders metas most-recently-active first, in place.
func sortSessionsByRecency(metas []sessionstore.SessionMeta) {
	sort.SliceStable(metas, func(i, j int) bool {
		return lastActivity(metas[i]).After(lastActivity(metas[j]))
	})
}

// lastActivity is a session's most recent activity instant for recency ordering.
// LastActiveAt stays the zero value until a turn actually runs (TurnStarted, StepDone, or
// RestoreDone stamps it — SessionStarted does not), so a never-active session falls back to
// CreatedAt rather than sorting behind every session that has ever run a turn.
func lastActivity(meta sessionstore.SessionMeta) time.Time {
	if meta.LastActiveAt.IsZero() {
		return meta.CreatedAt
	}
	return meta.LastActiveAt
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

// openWithClient resolves the workspace root, builds the one Generic loop definition and one rig
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
