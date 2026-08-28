package app

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
)

// servehost.go is Carbon's composition seam for `carbon serve`: the ONE
// process-lifetime rig an HTTP surface serves many sessions over.
//
// It differs from SessionStoreFactory (persistence.go) in exactly one structural
// way, and that difference is the whole point. SessionStoreFactory builds a FRESH
// rig on every Open -- a fresh sessionAccess, a fresh loop.Definition, a fresh
// rig.Define -- because the TUI hosts one session at a time and a /clear reopen may
// legitimately re-resolve configuration. An HTTP surface cannot work that way: it
// holds a single rig for the life of the server and calls NewSession/RestoreSession
// on it. So everything that is a pure function of (Config, workspace root) moves UP
// here and is built exactly once:
//
//   - sessionAccess (toolsets.go) -- a function of (cfg, root, interactive); the
//     executor set and the workspace permission store are per-WORKSPACE, never per
//     session, and the loop.Definition closes over both (assembly.go's
//     carbonDefinition captures access.set and access.gate), so under a shared rig
//     access CANNOT be per-session;
//   - the permission-review registration (permission_review.go) -- a pure function
//     of (cfg, client) producing rig.Options, already rig-scoped;
//   - the loop.Definition and the rig itself.
//
// What stays per-session is the MCP composition: mcpharness.Manager.BindSession is a
// compare-and-swap that permanently binds one Manager to one session id, so a second
// session needs a second Manager. With no <home>/mcp.json the assembly is the zero
// value and every method is a no-op, so the common case costs nothing.
//
// ONE LIVE SESSION PER WORKSPACE ROOT. The rig places the workspace with
// rig.WithExclusiveWorkspace (persistence.go), which acquires a root lease per
// session under a lease name derived from the canonical root. fsstore's leaser
// conflicts even within one process, so a second concurrently-live session fails
// with an opaque workspace-busy error. ServeHost therefore admits one live session
// at a time -- but it REFUSES rather than serializes: opening B while A is live
// returns *LiveSessionHandoffError naming A, and the caller must confirm the handoff
// with CloseLive. A may be mid-turn, and a click in a browser must never silently
// kill it. Switching to rig.WithSharedWorkspace would allow concurrency but would
// change the placement fingerprint from "exclusive:<root>" to "shared:<root>",
// making every session the TUI created unrestorable here, so it is not an option.
//
// This file deliberately names no type from harness/pkg/serve: the HTTP layer is
// composed exactly once, at the process root in package main
// (cmd/carbon/main_test.go's TestRigPackagesHaveNoServeAdapter enforces it). The
// error types below are Carbon's own; mapping them onto HTTP status codes is the
// process root's job.

// errServeSessionNotImplemented is the placeholder the session-lifecycle methods
// return until their own task lands. It is never returned once those are built.
var errServeSessionNotImplemented = errors.New("carbon: serve session lifecycle is not implemented")

// ServeHostOption configures OpenServeHost.
type ServeHostOption func(*serveHostConfig)

type serveHostConfig struct {
	buildClient func() (inference.Client, ModelFactory, error)
	loadModels  productionModelsContextLoader
}

// WithServeInferenceClient bypasses models.json and production ACP composition,
// supplying the inference client and model factory directly. It mirrors
// SessionStoreFactory's buildClient seam: production never sets it, tests do, and it
// lets a test drive the real composition without a model configuration or a network.
func WithServeInferenceClient(build func() (inference.Client, ModelFactory, error)) ServeHostOption {
	return func(c *serveHostConfig) { c.buildClient = build }
}

// withServeModelsLoader substitutes the models.json loader while keeping the REST of
// the production resolution path (the capability check, the credential admission,
// the config folding, the ACP children). It is the seam for testing what
// WithServeInferenceClient skips over. Production never sets it.
func withServeModelsLoader(load productionModelsContextLoader) ServeHostOption {
	return func(c *serveHostConfig) { c.loadModels = load }
}

// ServeHost owns, for the whole process: the durable fsstore backend, the store
// facades and listing catalog, the session access wiring, the one immutable rig, and
// the credential admission. Per live session it owns one mcpSessionAssembly.
type ServeHost struct {
	fs          *fsstore.Store
	stores      *sessionStores
	access      *sessionAccess
	rig         *rig.Rig
	serveConfig Config

	credentialRuntime *credentialRuntime
	credentialLease   *credentialRegistryLease

	// mu guards live, the single-live-session invariant.
	mu   sync.Mutex
	live *liveServeSession

	closeOnce sync.Once
	closeErr  error
}

// liveServeSession is one open session plus the MCP composition bound to it.
type liveServeSession struct {
	id      uuid.UUID
	session session.SessionController
	mcp     mcpSessionAssembly
}

// OpenServeHost resolves the production model configuration (or the injected
// client), builds the process-lifetime access wiring, loop definition,
// permission-review registration and rig over the DURABLE fsstore backend rooted at
// dataDir, and returns the host. The headless memstore path (persistence.go's
// headlessStores) is deliberately unreachable from here: carbon serve serves the
// user's real, persisted sessions.
//
// Every failure after a resource is acquired releases that resource before
// returning, so a failed start leaves no scratch HOME, no credential admission and
// no open backend behind.
func OpenServeHost(ctx context.Context, cfg Config, dataDir string, opts ...ServeHostOption) (*ServeHost, error) {
	options := &serveHostConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(options)
		}
	}

	var (
		client            inference.Client
		factory           ModelFactory
		credentialRuntime *credentialRuntime
		credentialLease   *credentialRegistryLease
	)
	if options.buildClient != nil {
		var err error
		client, factory, err = options.buildClient()
		if err != nil {
			return nil, err
		}
	} else {
		load := options.loadModels
		if load == nil {
			load = loadProductionModelsWithContext
		}
		resolved, err := resolveServeModels(ctx, cfg, load, loadProductionModels)
		if err != nil {
			return nil, err
		}
		cfg = resolved.cfg
		client = resolved.client
		factory = resolved.factory
		credentialRuntime = resolved.credentialRuntime
		credentialLease = resolved.credentialLease
	}

	// releaseCredentials undoes the admission resolveServeModels took. It is armed
	// for every failure path below and disarmed once the host owns it.
	releaseCredentials := func() {
		if credentialRuntime == nil {
			return
		}
		credentialRuntime.endSession()
		releaseCredentialComposition(credentialRuntime, credentialLease)
	}

	root, err := os.Getwd()
	if err != nil {
		releaseCredentials()
		return nil, &WorkspaceRootError{Cause: err}
	}

	// Interactive: a human at a browser resolves gates over the HTTP surface, so the
	// access wiring uses the HOME-derived read/write workspace permission store and
	// interactive gate evaluators -- the same wiring the TUI gets, not the headless
	// one.
	access, err := buildSessionAccess(cfg, root, true)
	if err != nil {
		releaseCredentials()
		return nil, err
	}
	access.diagnostics = append(access.diagnostics, cfg.ACPDiagnostics...)
	cfg.AccessConfigRev = access.configRev

	// fail is every remaining failure path's cleanup.
	fail := func(err error) (*ServeHost, error) {
		_ = access.Close()
		releaseCredentials()
		return nil, err
	}

	// The MCP configuration digest is part of the rig's config fingerprint, and the
	// rig is defined ONCE here, so the digest has to be resolved once here too --
	// not per session as openRuntimeAgent does it. A host that left MCPConfigRev
	// empty would define a rig whose fingerprint disagrees with every session the
	// TUI created from the same mcp.json, and every restore would be rejected as
	// configuration drift. The probe Manager is built only for its digest and is
	// closed immediately; each session builds its own (see adoptLocked).
	baseline, err := newMCPSessionAssembly(cfg)
	if err != nil {
		return fail(err)
	}
	cfg.MCPConfigRev = baseline.configRev()
	baseline.close(ctx)

	definition, err := carbonDefinition(client, factory(), cfg, access, nil)
	if err != nil {
		return fail(err)
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, client)
	if err != nil {
		return fail(err)
	}

	fs, err := fsstore.Open(fsstore.Options{Root: dataDir})
	if err != nil {
		return fail(&StoreInitError{Stage: "fsstore", Cause: err})
	}
	stores, err := openStores(fs.Backend())
	if err != nil {
		_ = fs.Close()
		return fail(err)
	}
	stores.resourceStorage = newPersistedResourceStorageProvider(dataDir)

	assembled, err := buildRigForDelegationCaps(definition, stores, root, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReview)
	if err != nil {
		_ = fs.Close()
		return fail(err)
	}

	return &ServeHost{
		fs:                fs,
		stores:            stores,
		access:            access,
		rig:               assembled,
		serveConfig:       cfg,
		credentialRuntime: credentialRuntime,
		credentialLease:   credentialLease,
	}, nil
}

// Catalog is the listing index the read plane reads the sessions list from.
func (h *ServeHost) Catalog() *sessionstore.Catalog { return h.stores.catalog }

// SessionStore is the journal store the read plane replays status and journal pages
// from.
func (h *ServeHost) SessionStore() *sessionstore.Store { return h.stores.session }

// LiveSessionHandoffError reports that the requested operation cannot proceed because
// a DIFFERENT session is already live over this workspace root.
//
// It is not an internal failure and it is not a race to be retried: it is the signal
// that a human decision is owed. Carbon composes its rig with
// rig.WithExclusiveWorkspace, so at most one session may hold the workspace root at a
// time; the incumbent may be mid-turn, with a browser watching its event stream. The
// alternative -- closing the incumbent so the new open can proceed -- would make an
// ordinary click silently kill running work, so ServeHost refuses instead and names
// the session that must be handed off. A caller that has obtained the human's consent
// calls CloseLive with this id and retries.
type LiveSessionHandoffError struct {
	// LiveID is the session currently holding the workspace root.
	LiveID uuid.UUID
}

func (e *LiveSessionHandoffError) Error() string {
	if e == nil {
		return "carbon: another session is live"
	}
	return "carbon: session " + e.LiveID.String() + " is live and must be handed off first"
}

// MCPConfigDriftError reports that <home>/mcp.json changed after the rig was defined.
// The rig's config fingerprint records the digest resolved at host construction, so a
// session opened against a different MCP configuration would durably record a
// fingerprint that does not describe the tools it actually had. The open fails closed;
// restarting the server re-resolves the configuration.
type MCPConfigDriftError struct {
	Want string
	Got  string
}

func (e *MCPConfigDriftError) Error() string {
	return "carbon: mcp configuration changed since the server started"
}

// LiveSession reports the id of the session currently holding the workspace root, if
// any. It is the read half of the handoff seam: a caller asks what is live BEFORE it
// asks a human to confirm closing it.
func (h *ServeHost) LiveSession() (uuid.UUID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live == nil {
		return uuid.UUID{}, false
	}
	return h.live.id, true
}

// CloseLive performs a CONFIRMED handoff: it shuts the live session down only if that
// session is the one the caller named. A mismatch returns *LiveSessionHandoffError
// carrying the id that is ACTUALLY live, so a caller whose confirmation raced another
// tab's handoff re-asks about the right session instead of killing the wrong one. An
// idle host is a no-op rather than an error, because two tabs confirming the same
// handoff is the expected case, not a failure.
func (h *ServeHost) CloseLive(ctx context.Context, expected uuid.UUID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live == nil {
		return nil
	}
	if h.live.id != expected {
		return &LiveSessionHandoffError{LiveID: h.live.id}
	}
	return h.closeLiveLocked(ctx)
}

// NewSession brings up a fresh session on the shared rig and attaches its OWN MCP
// composition. It REFUSES while another session is live rather than closing it; see
// LiveSessionHandoffError.
//
// The returned controller is the rig's own, unwrapped. That is deliberate: harness's
// live registry evicts a dead session by watching an optional Done() channel on the
// value it was handed, and a wrapper that did not forward Done would silently opt
// every session out of eviction, pinning a subscription and a goroutine per dead
// session.
func (h *ServeHost) NewSession(ctx context.Context) (session.SessionController, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live != nil {
		return nil, &LiveSessionHandoffError{LiveID: h.live.id}
	}
	sess, err := h.rig.NewSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.adoptLocked(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// adoptLocked builds this session's own MCP composition and records it as the live
// session. mcpharness.Manager.BindSession is a compare-and-swap that permanently
// binds one Manager to one session id, so the Manager cannot be hoisted to the host
// alongside the rig. With no <home>/mcp.json the assembly is the zero value and every
// method is a no-op, so the common case costs nothing.
//
// A failure tears the freshly-opened session back down rather than leaving it live
// with a half-attached manager, matching openRuntimeAgent's own fail closure. The
// shutdown is also what releases the exclusive workspace root lease, so a failed
// adopt does not strand the workspace.
func (h *ServeHost) adoptLocked(ctx context.Context, sess session.SessionController) error {
	abort := func(assembly *mcpSessionAssembly, err error) error {
		if assembly != nil {
			assembly.close(ctx)
		}
		_ = sess.Shutdown(ctx)
		return err
	}
	assembly, err := newMCPSessionAssembly(h.serveConfig)
	if err != nil {
		return abort(nil, err)
	}
	if rev := assembly.configRev(); rev != h.serveConfig.MCPConfigRev {
		return abort(&assembly, &MCPConfigDriftError{Want: h.serveConfig.MCPConfigRev, Got: rev})
	}
	if err := assembly.attach(ctx, sess, true); err != nil {
		return abort(&assembly, err)
	}
	h.live = &liveServeSession{id: sess.SessionID(), session: sess, mcp: assembly}
	return nil
}

// RestoreSession rebuilds a persisted session on the shared rig.
func (h *ServeHost) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	return nil, errServeSessionNotImplemented
}

// HasSession reports whether a session id has ever been persisted in this workspace.
func (h *ServeHost) HasSession(ctx context.Context, id uuid.UUID) (bool, error) {
	return false, errServeSessionNotImplemented
}

// Close tears the host down once, in the reverse of construction order: the live
// session (which releases the exclusive root lease), the credential admission, the
// access wiring (which removes its scratch HOME), then the durable backend. Repeated
// calls return the first call's result.
func (h *ServeHost) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		var first error
		if err := h.closeLive(ctx); err != nil {
			first = err
		}
		if h.credentialRuntime != nil {
			h.credentialRuntime.endSession()
			if h.credentialLease != nil {
				if err := h.credentialLease.Release(); err != nil && first == nil {
					first = err
				}
			} else if err := h.credentialRuntime.Close(); err != nil && first == nil {
				first = err
			}
		}
		if err := h.access.Close(); err != nil && first == nil {
			first = err
		}
		if err := h.fs.Close(); err != nil && first == nil {
			first = err
		}
		h.closeErr = first
	})
	return h.closeErr
}

// closeLive closes the live session, if any, taking the lock.
func (h *ServeHost) closeLive(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closeLiveLocked(ctx)
}

// closeLiveLocked stops the MCP consumers first, then shuts the session down -- the
// same "stop consumers before the resource they consume" order RuntimeAgent.Close
// documents. The shutdown is what releases the exclusive root lease, so the NEXT open
// can acquire it.
func (h *ServeHost) closeLiveLocked(ctx context.Context) error {
	live := h.live
	if live == nil {
		return nil
	}
	h.live = nil
	live.mcp.close(ctx)
	return live.session.Shutdown(ctx)
}
