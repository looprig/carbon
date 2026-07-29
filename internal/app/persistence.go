package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
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
)

// DefaultDataDir is the default root for the on-disk session store: ~/.looprig/store. It
// fails loud with a typed *StoreInitError if the home directory cannot be resolved.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &StoreInitError{Stage: "data-dir", Cause: err}
	}
	return filepath.Join(home, ".looprig", "store"), nil
}

// swarmStores bundles the session + workspace facades and the root leaser the rig needs,
// all over ONE storage.Composite backend. It is read-only after construction.
type swarmStores struct {
	session   *sessionstore.Store
	workspace *workspacestore.Store
	leaser    storage.Leaser
	catalog   *sessionstore.Catalog
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

// headlessShared holds the process-shared in-memory store the headless New path uses, opened
// once. Two headless sessions therefore share ONE backend and contend on the SAME exclusive
// root lease for the current checkout — exactly like two persisted sessions.
var (
	headlessOnce   sync.Once
	headlessResult *swarmStores
	headlessError  error
)

// headlessStores returns the process-shared in-memory store facades, opening them once.
func headlessStores() (*swarmStores, error) {
	headlessOnce.Do(func() {
		headlessResult, headlessError = openStores(memstore.New())
	})
	return headlessResult, headlessError
}

// operatorFingerprintFields assembles the rig-level config-fingerprint inputs that are not
// part of a loop.Definition: the swarm+primary AgentKind, the human-set RuntimeSkills mode,
// and the durable access configuration identity. NativePermissionPolicyRev carries the
// secret-free access digest (access ABI version, selected profile, normalized operator and
// reviewer profiles, and the non-secret egress route identity/guarantees) so a product-profile,
// reviewer-restriction, or egress-boundary change invalidates a restore rather than silently
// resuming with different authority. AppFields additionally surfaces the human-visible profile
// name for the richer manifest (the 5.2 SessionPresenter consumes it). The workspace-root field
// is owned by the rig's exclusive-workspace placement, so it is not set here. A zero Config
// (AccessProfile/AccessConfigRev empty) leaves both access inputs empty, keeping the fields
// additive for callers that do not select a profile.
func operatorFingerprintFields(cfg Config) rig.ConfigFingerprintFields {
	return rig.ConfigFingerprintFields{
		AgentKind:                 operatorAgentKind,
		RuntimeSkills:             cfg.RuntimeSkills,
		NativePermissionPolicyRev: cfg.AccessConfigRev,
		AppFields:                 accessAppFields(cfg.AccessProfile),
	}
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
	return buildRigForDelegationCaps(definitions, stores, root, cfg, allowMismatch, rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, permissionReviewRegistration{})
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
	return buildRigWithRegistration(definitions, stores, root, cfg, allowMismatch, limits, registration, permissionReview)
}

// buildRigWithRegistration is the common immutable rig assembly. Production
// supplies exactly one reviewed hustle registration and one permission-review
// registration; focused tests can vary either's public descriptor/limits to
// prove fingerprint sensitivity. permissionReview's disabled zero value adds
// no options, so passing it changes nothing about the assembled rig.
func buildRigWithRegistration(definitions []loop.Definition, stores *swarmStores, root string, cfg Config, allowMismatch bool, limits rig.DelegationLimits, registration conversationHustleRegistration, permissionReview permissionReviewRegistration) (*rig.Rig, error) {
	options := []rig.Option{
		rig.WithLoops(definitions...),
		rig.WithPrimers(string(operatorPrimaryName)),
		rig.WithActivePrimer(string(operatorPrimaryName)),
		rig.WithSessionStore(stores.session),
		rig.WithExclusiveWorkspace(stores.workspace, root, stores.leaser),
		rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: rig.SnapshotBestEffort, Timeout: snapshotTimeout}),
		rig.WithDelegationLimits(limits),
		rig.WithFingerprintFields(operatorFingerprintFields(cfg)),
		rig.WithOffloadGC(rig.OffloadGCPolicy{Interval: offloadGCInterval, Timeout: offloadGCTimeout}),
	}
	options = append(options, registration.options()...)
	options = append(options, permissionReview.options()...)
	if allowMismatch {
		options = append(options, rig.WithAllowConfigMismatch())
	}
	return rig.Define(options...)
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
	fs             *fsstore.Store
	stores         *swarmStores
	buildClient    func() (inference.Client, ModelFactory, error)
	mu             sync.Mutex
	currentSession uuid.UUID
}

// NewSessionStoreFactory opens the on-disk store rooted at dataDir and returns the production
// factory. It fails closed with a typed *StoreInitError if any store layer cannot be opened,
// closing the backend if a later layer fails so no directory lock leaks.
func NewSessionStoreFactory(dataDir string) (*SessionStoreFactory, error) {
	fs, err := fsstore.Open(fsstore.Options{Root: dataDir})
	if err != nil {
		return nil, &StoreInitError{Stage: "fsstore", Cause: err}
	}
	stores, err := openStores(fs.Backend())
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	return &SessionStoreFactory{fs: fs, stores: stores, buildClient: buildClient}, nil
}

// Close releases the shared on-disk backend, once, at process teardown (after every session
// opened from this factory has been Closed). It is idempotent.
func (f *SessionStoreFactory) Close() error {
	return f.fs.Close()
}

// List returns the session catalog (most-recently-active-first), the source the CLI --list
// path prints. It reads the listing index only — no lease, no replay — so it stays cheap.
func (f *SessionStoreFactory) List(ctx context.Context) ([]sessionstore.SessionMeta, error) {
	return f.stores.catalog.ListSessions(ctx)
}

// Open builds a fully-persisted CodeRig session for sel (new or resumed) and returns it as
// a tui.Agent. It builds the provider client + ModelFactory exactly like New (reads
// LLM_API_KEY, fails loud on a missing key), then delegates to the construction seam.
func (f *SessionStoreFactory) Open(ctx context.Context, sel SessionSelector, cfg Config) (*RuntimeAgent, error) {
	client, factory, err := f.buildClient()
	if err != nil {
		return nil, err
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
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	// Persisted CodeRig is INTERACTIVE: a human at the TUI resolves gates, so the
	// access wiring uses the HOME-derived workspace permission store and interactive
	// gate evaluators. The selected access profile is fixed for the session; the TUI
	// only displays it (SessionPresenter). New and restore share this one path.
	return openRuntimeAgent(ctx, client, factory, cfg, f.stores, root, sel, true)
}
