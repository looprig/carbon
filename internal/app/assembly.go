// Package app assembles Carbon's one Carbon loop, model/provider, system
// prompt, and session composition root.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	mcpharness "github.com/looprig/mcp/pkg/harness"
	"github.com/looprig/tools/skill"
	"github.com/looprig/tui"
	"github.com/looprig/tui/sessionadapter"
)

// skillToolName is the model-facing name of the Skill tool. It MUST equal the Carbon
// model-facing name used by the Skill definition. The definition and hard-approve
// rule must agree. A drift fails loudly at loop.Bind,
// which checks a built tool's Info().Name against its definition's declared name).
const skillToolName = "Skill"

// managedAgentToolsRevision is included in the Carbon policy revision so the
// immutable loop fingerprint changes whenever the injected collaboration-tool
// bundle changes.
const managedAgentToolsRevision = "agent-tools-v2"

// The rig-injected managed agent tools prepare empty access requests, so the
// Carbon access gate auto-allows them. Carbon is the sole legal delegate and
// active primer.

// activePrimerName is Carbon, the only loop and initial active primer.
const activePrimerName = carbon.Name

// agentKind is the durable Carbon identity stamped onto the session's
// configuration fingerprint.
const agentKind = "carbon:carbon"

// Agent-spawn safety caps applied to the rig's delegation limits. They are the two
// independent backstops against a runaway agent tree: delegationSpawnDepth bounds
// spawn-chain nesting, delegationSpawnQuota bounds the total sub-loops a session may ever spawn.
//
// Depth remains 2: a root primer can spawn one child level, while any child
// attempting to spawn again is refused by the rig. The same limit applies to
// Carbon's self-delegation is bounded by the same limits.
const (
	delegationSpawnDepth = 2
	delegationSpawnQuota = 64
)

// httpClientTimeout bounds every web request Carbon's Fetch/WebSearch tools make, so a hung
// endpoint can never block a tool call indefinitely (CLAUDE.md: no unbounded blocking).
const httpClientTimeout = 30 * time.Second

// newHTTPClient builds the single *http.Client shared by Carbon's web tools. It pins an
// explicit overall timeout and the TLS floor to 1.2 (never InsecureSkipVerify).
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpClientTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// LoopDefinitionError reports that the Carbon loop.Definition could not be
// assembled (a WithTools/WithPermissionFactory/WithPolicyRevision inconsistency, or a bad
// name). Agent names which loop failed. It is errors.As-recoverable and exists so the whole
// construction fails secure (no half-wired topology).
type LoopDefinitionError struct {
	Agent string
	Cause error
}

func (e *LoopDefinitionError) Error() string {
	if e.Cause == nil {
		return "carbon: cannot define loop " + e.Agent
	}
	return "carbon: cannot define loop " + e.Agent + ": " + e.Cause.Error()
}

func (e *LoopDefinitionError) Unwrap() error { return e.Cause }

// skillDefinitionFor builds Carbon's always-on, workspace-backed Skill
// definition. Workspace skill documents are untrusted and remain subject to
// the Skill tool's context.load and filesystem.read approval flow.
func skillDefinitionFor(loader skill.SkillLoader) tool.Definition {
	agent := carbon.Name
	return tool.NewDefinition(skillToolName, tool.RequiresWorkspace, func(_ context.Context, bind tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{skill.NewSkill(loader, agent, skill.WithWorkspaceRoot(bind.Workspace.Root))}, nil
	})
}

// carbonDefinition returns Carbon's sole immutable loop definition. extras is
// a narrow test probe seam; production passes nil.
func carbonDefinition(client inference.Client, model model.Model, cfg Config, access *sessionAccess, extras []tool.Definition) (loop.Definition, error) {
	contextPolicy, err := newConversationContextPolicy(model, cfg.PrimerCandidates, cfg.DelegateModels)
	if err != nil {
		return loop.Definition{}, err
	}
	return carbonDefinitionWithContextPolicy(client, model, cfg, contextPolicy, access, extras)
}

// carbonDefinitionWithContextPolicy keeps the context policy injectable for
// focused fingerprint and compaction tests without introducing a roster map.
func carbonDefinitionWithContextPolicy(client inference.Client, model model.Model, cfg Config, contextPolicy conversationContextPolicy, access *sessionAccess, extras []tool.Definition) (loop.Definition, error) {
	httpCl := newHTTPClient()
	runtimeCtx := newRuntimeContextProvider(runtimeSkillCatalogForAccess(access))

	loader := skill.NewEmbeddedSkillLoader(nil, nil)
	definitions := append([]tool.Definition(nil), carbonToolDefinitions(access.set, httpCl, skillDefinitionFor(loader))...)
	definitions = append(definitions, extras...)

	system := contextPolicy.system(carbon.SystemPrompt)
	options := []loop.Option{
		loop.WithName(carbon.Name),
		loop.WithDisplayName(carbon.DisplayName),
		loop.WithDescription(carbon.Description),
		loop.WithInference(client, model),
		loop.WithSystem(system),
		loop.WithTools(definitions...),
		loop.WithAccessGate(access.gate),
		loop.WithPolicyRevision(contextPolicy.policyRevision(access.policyRev + ":" + managedAgentToolsRevision)),
		loop.WithRuntimeContext(runtimeCtx),
		loop.WithDelegates(carbon.Name),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
	}
	options = append(options, contextPolicy.options()...)
	definition, err := loop.Define(options...)
	if err != nil {
		return loop.Definition{}, &LoopDefinitionError{Agent: string(carbon.Name), Cause: err}
	}
	return definition, nil
}

func runtimeSkillCatalogForAccess(access *sessionAccess) func() []skill.SkillMeta {
	if access == nil || access.workspace == "" {
		return nil
	}
	workspace := access.workspace
	return func() []skill.SkillMeta {
		return skill.DiscoverWorkspaceSkills(workspace)
	}
}

// New constructs the Carbon headless from one models.json load and returns it
// as a tui.Agent driven by the configured primer_default.
func New(ctx context.Context, cfg Config) (tui.Agent, error) {
	return newWithProductionModelsContextLoader(ctx, cfg, loadProductionModelsWithContext, headlessStores)
}

// productionModelsLoader is loadProductionModels's shape: it takes the
// already-resolved looprig home (looprigHome's result) so it never resolves
// HOME itself.
type productionModelsLoader func(home string) (productionModels, error)
type productionModelsContextLoader func(context.Context, string) (productionModels, error)

func newWithProductionModelsLoader(ctx context.Context, cfg Config, loader productionModelsLoader, storesProvider sessionStoresProvider) (*RuntimeAgent, error) {
	return newWithProductionModelsContextLoader(ctx, cfg, func(_ context.Context, home string) (productionModels, error) {
		return loader(home)
	}, storesProvider)
}

func newWithProductionModelsContextLoader(ctx context.Context, cfg Config, loader productionModelsContextLoader, storesProvider sessionStoresProvider) (*RuntimeAgent, error) {
	home, err := looprigHome(cfg)
	if err != nil {
		return nil, err
	}
	// Carry the resolved absolute home through every downstream composition
	// helper (permissions, MCP, credentials) so this process root is resolved
	// exactly once.
	cfg.HomeDir = home
	configured, err := loader(ctx, home)
	if err != nil {
		return nil, err
	}
	credentialRuntime := configured.credentialRuntime
	credentialLease := configured.credentialLease
	if credentialRuntime != nil {
		if err := credentialRuntime.beginSession(); err != nil {
			if credentialLease != nil {
				_ = credentialLease.Release()
			} else {
				_ = credentialRuntime.Close()
			}
			return nil, err
		}
		defer func() {
			if credentialRuntime != nil {
				credentialRuntime.endSession()
				if credentialLease != nil {
					_ = credentialLease.Release()
				} else {
					_ = credentialRuntime.Close()
				}
			}
		}()
	}
	if configured.PrimerClient == nil || configured.PrimerModel.Name == "" {
		return nil, &ModelConfigCapabilityError{}
	}
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
	runtimeClient := configured.RuntimeClient
	if runtimeClient == nil {
		runtimeClient = configured.PrimerClient
	}
	agent, err := newWithClientUsingStores(ctx, runtimeClient, newModelFactoryFor(configured.PrimerModel), cfg, storesProvider)
	if err != nil {
		return nil, err
	}
	if credentialRuntime != nil {
		agent.credentialRuntime = credentialRuntime
		agent.credentialLease = credentialLease
		credentialRuntime = nil
		credentialLease = nil
	}
	return agent, nil
}

type sessionStoresProvider func() (*sessionStores, error)

// newWithClientUsingStores validates the full model/context composition before
// resolving the process store. Tests inject a provider to prove invalid policy
// cannot open or mutate persistence. It resolves the HEADLESS access wiring (a
// read-only permission store and non-prompting gate evaluators), folds its digest
// into the fingerprint, and returns the runtime agent that owns the executor-set
// closers. Composition failure closes the partial access and never touches
// persistence.
func newWithClientUsingStores(ctx context.Context, client inference.Client, factory ModelFactory, cfg Config, storesProvider sessionStoresProvider) (*RuntimeAgent, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}
	access, err := buildHeadlessAccess(cfg, root)
	if err != nil {
		return nil, err
	}
	access.diagnostics = append(access.diagnostics, cfg.ACPDiagnostics...)
	cfg.AccessConfigRev = access.configRev
	definition, err := carbonDefinition(client, factory(), cfg, access, nil)
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, client)
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	stores, err := storesProvider()
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	adapter, err := openSessionWithDefinition(ctx, definition, cfg, stores, root, SessionSelector{}, permissionReview)
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	return newRuntimeAgentWithPrimerCandidates(adapter, adapter.Controller(), root, access, cfg.PrimerAlias, cfg.PrimerEfforts, cfg.PrimerCandidates), nil
}

// newSessionOverStores is the store-injecting construction seam shared by the headless New
// path (over the process-shared in-memory store + current checkout) and tests (over an
// isolated store + a temp root, so parallel session tests never contend on the current
// checkout's exclusive root lease). It resolves the headless access wiring, builds the Carbon
// definition, one rig placing root as the exclusive workspace, opens a NEW session, and wraps
// it as the runtime agent that owns the executor-set closers.
func newSessionOverStores(ctx context.Context, client inference.Client, factory ModelFactory, cfg Config, stores *sessionStores, root string) (*RuntimeAgent, error) {
	return openRuntimeAgent(ctx, client, factory, cfg, stores, root, SessionSelector{}, false)
}

// mcpSessionAssembly is openRuntimeAgent's local helper for the optional MCP
// composition step: loading mcp.json, constructing the Manager and its
// late-binding gate/event adapters, attaching them once a session exists, and
// closing them on any failure path. Its zero value is a legitimate value --
// every method is a safe no-op when manager is nil -- which is exactly the
// "no mcp.json" case: nothing about the assembled rig or session changes.
//
// It exists so openRuntimeAgent's several failure paths (before and after the
// live session exists) can share one cleanup call instead of each repeating
// its own nil-checked manager.Close()/adopter.Close(), which is exactly the
// kind of copy-pasted block a later edit forgets to update in one place.
type mcpSessionAssembly struct {
	manager  *mcpharness.Manager
	opener   *mcpGateOpener
	events   *mcpEventPublisher
	adopter  *mcpharness.Adopter
	recorder *mcpNoticeRecorder
}

// newMCPSessionAssembly loads <home>/mcp.json (via loadMCPConfig, honoring
// cfg.HomeDir) and, when it names at least one server, constructs the Manager
// over it. An absent file, or one with no servers, returns the zero
// mcpSessionAssembly and no error -- zero change to the assembled rig or
// session, matching every other touch point's nil-check.
func newMCPSessionAssembly(cfg Config) (mcpSessionAssembly, error) {
	specs, err := loadMCPConfig(cfg)
	if err != nil {
		return mcpSessionAssembly{}, err
	}
	if len(specs) == 0 {
		return mcpSessionAssembly{}, nil
	}
	bindings, err := mcpDefinitions(specs)
	if err != nil {
		return mcpSessionAssembly{}, err
	}
	opener := &mcpGateOpener{}
	events := &mcpEventPublisher{}
	// recorder is kept and threaded through the returned assembly (and, from
	// there, RuntimeAgent) so the notices it captures are actually
	// reachable via RuntimeAgent.MCPNotices() -- not just constructed and
	// then discarded with this function's local variables.
	recorder := newMCPNoticeRecorder()
	mgr, err := mcpharness.NewManager(bindings, mcpharness.Deps{
		Gates:  opener,
		Events: events,
		// A recorder rather than nil: it is bounded, always-safe, and gives
		// visibility into tool-name collisions and adoption
		// failures instead of silently dropping them, at no cost a nil
		// Reporter would have avoided (mcp.go's mcpNoticeRecorder doc).
		Reporter: recorder,
	})
	if err != nil {
		return mcpSessionAssembly{}, err
	}
	return mcpSessionAssembly{manager: mgr, opener: opener, events: events, recorder: recorder}, nil
}

// configRev is the digest openRuntimeAgent folds into cfg.MCPConfigRev, or ""
// when there is no MCP composition at all -- the same "no external
// capability" zero value Manager.ConfigDigest itself returns for a Manager
// with no bindings.
func (a mcpSessionAssembly) configRev() string {
	if a.manager == nil {
		return ""
	}
	return a.manager.ConfigDigest()
}

// attach binds the Manager to the live session, connects every binding, and
// starts the Adopter -- installing the active primer's own toolset
// immediately, since that Loop already exists and, having never run, will
// never reach the idle boundary that would otherwise trigger its first
// install (mcpharness.Adopter.Install's doc). It is a no-op when there is no
// manager.
//
// The gate opener is bound only when interactive: a headless composition has
// no human to route an elicitation to, matching mcpGateOpener's permanently-
// unbound posture for headless sessions (design: "Headless sessions install
// an always-refusing opener"). The event publisher is bound in both cases --
// publishing an integration status is not a human-input capability, so a
// headless session's own event stream still opens knowing what its servers
// are.
//
// The eager Install's error is intentionally discarded: a failure there
// (e.g. a slow build racing the timeout) leaves the active primer's first
// turn running with no MCP tools, exactly like any other Loop that has not
// yet adopted a generation, and it gets a real retry the moment that turn
// ends and the Loop reaches its own idle boundary -- matching Install's own
// "otherwise the same operation as a boundary" contract.
func (a *mcpSessionAssembly) attach(ctx context.Context, sess session.SessionController, interactive bool) error {
	if a.manager == nil {
		return nil
	}
	if err := a.manager.BindSession(sess.SessionID()); err != nil {
		return err
	}
	if interactive {
		if host, ok := sess.(session.GateHost); ok {
			a.opener.Bind(host)
		}
	}
	if pub, ok := sess.(mcpharness.EventPublisher); ok {
		a.events.Bind(pub)
	}
	if err := a.manager.Start(ctx); err != nil {
		return err
	}
	adopter, err := a.manager.StartAdoption(sess, sess)
	if err != nil {
		return err
	}
	a.adopter = adopter
	active := sess.ActiveLoop()
	// string(activePrimerName) names the active loop here rather than
	// reading it back from sess: this depends on Carbon never calling
	// SetActiveLoop before this point in session construction, so
	// active.ID() and activePrimerName always name the same Loop
	// (verified: SetActiveLoop is never called anywhere in internal/app
	// today). A future primer-picker/model-switch feature that reassigns
	// the active loop before session-open completes would need to revisit
	// this.
	_ = adopter.Install(ctx, active.ID(), string(activePrimerName))
	return nil
}

// close releases the adopter, then the manager, nil-safe and safe to call at
// any point in construction -- before a manager was ever built, after Start
// but before StartAdoption, or after a full attach. The order mirrors
// RuntimeAgent.Close's documented "stop consumers before the resource they
// consume": the adopter only reacts to the session's own idle events, so it
// is stopped before the manager that owns the actual connections.
func (a *mcpSessionAssembly) close(ctx context.Context) {
	if a.adopter != nil {
		_ = a.adopter.Close()
		a.adopter = nil
	}
	if a.manager != nil {
		_ = a.manager.Close(ctx)
	}
}

// openRuntimeAgent is Carbon's single session-assembly path. It resolves the
// session-fixed access wiring (interactive or headless), folds its secret-free
// digest into the config fingerprint, optionally discovers and constructs an MCP
// Manager from <home>/mcp.json (nil when the file is absent or empty -- every
// touch point below nil-checks it via mcpSessionAssembly), builds the one
// Carbon definition and one rig over the injected stores, opens (Resume zero) or
// restores the session, attaches the MCP composition to it, and returns the
// runtime agent that OWNS the executor-set and MCP closers. Any failure after the
// access is built closes the partial assembly (MCP composition, then access) so
// nothing leaks. New, restore, headless, and interactive construction differ only
// in the injected stores, selector, and the interactive flag (which selects the
// permission store + evaluator kind, and here, whether the MCP gate opener binds
// at all).
func openRuntimeAgent(ctx context.Context, client inference.Client, factory ModelFactory, cfg Config, stores *sessionStores, root string, selector SessionSelector, interactive bool) (*RuntimeAgent, error) {
	access, err := buildSessionAccess(cfg, root, interactive)
	if err != nil {
		return nil, err
	}
	access.diagnostics = append(access.diagnostics, cfg.ACPDiagnostics...)
	cfg.AccessConfigRev = access.configRev

	mcpAssembly, err := newMCPSessionAssembly(cfg)
	if err != nil {
		_ = access.Close()
		return nil, err
	}
	cfg.MCPConfigRev = mcpAssembly.configRev()

	// fail is every remaining failure path's cleanup: close whatever MCP
	// composition exists (a no-op when there is none), then the access
	// wiring, mirroring buildSessionAccess's own partial-failure discipline.
	fail := func(err error) (*RuntimeAgent, error) {
		mcpAssembly.close(ctx)
		_ = access.Close()
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
	adapter, err := openSessionWithDefinition(ctx, definition, cfg, stores, root, selector, permissionReview)
	if err != nil {
		return fail(err)
	}

	if err := mcpAssembly.attach(ctx, adapter.Controller(), interactive); err != nil {
		_ = adapter.Close(ctx)
		return fail(err)
	}

	return newRuntimeAgentWithMCP(adapter, adapter.Controller(), root, access, mcpAssembly.manager, mcpAssembly.adopter, mcpAssembly.recorder, cfg.PrimerAlias, cfg.PrimerEfforts, cfg.PrimerCandidates), nil
}

// openSessionWithDefinition is Carbon's single new-or-restore assembly path.
// Production and tests differ only in the injected stores, workspace, and selector.
// permissionReview is the caller-constructed classifier registration (built where
// the live inference Client is available); its disabled zero value adds no rig
// options, so every existing caller that never sets cfg.PermissionReviewEnabled
// sees no behavior change.
func openSessionWithDefinition(ctx context.Context, definition loop.Definition, cfg Config, stores *sessionStores, root string, selector SessionSelector, permissionReview permissionReviewRegistration) (*sessionadapter.Adapter, error) {
	assembly, err := buildRigForDelegationCaps(definition, stores, root, cfg, selector.AllowConfigMismatch, rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, permissionReview)
	if err != nil {
		return nil, err
	}
	if selector.Resume.IsZero() {
		controller, err := assembly.NewSession(ctx)
		if err != nil {
			return nil, err
		}
		adapter, err := sessionadapter.NewWithReplay(ctx, controller, stores.session)
		if err != nil {
			return nil, err
		}
		return adapter, nil
	}
	controller, err := assembly.RestoreSession(ctx, selector.Resume)
	if err != nil {
		return nil, classifyRestoreError(err, selector.Resume, definition)
	}
	adapter, err := sessionadapter.Restore(ctx, controller, stores.session)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// RestoredModeError reports a resume whose persisted mode this build of Carbon no
// longer declares.
//
// Carbon used to declare "quick" and "deep" primer modes and start every loop in
// "quick"; it now declares none, so the primer's effort is whatever models.json's
// default_effort resolves to. A loop's start mode is durable (harness records
// bound.InitialMode() on LoopStarted, which folds back into RestoredState.Mode), so
// every session persisted by the older build asks to resume in a mode that is gone.
//
// Without SessionSelector.AllowConfigMismatch such a resume never gets that far: the
// config-fingerprint gate refuses it first with a clear *session.ConfigMismatchError.
// This error covers the other half. With the flag set the gate is bypassed, restore
// reaches harness's exact-name mode resolution (loopruntime.NewRestoredWithRuntime ->
// configForMode), misses, and fails with a bare "loop: bind: invalid_definition:
// quick" that names neither the session nor the reason nor a way forward. Carbon is
// what removed the modes, so Carbon is what explains their absence.
type RestoredModeError struct {
	SessionID uuid.UUID
	Mode      loop.ModeName
	// Declared is the mode set this build does offer, empty when it declares none.
	Declared []loop.ModeName
	Cause    error
}

func (e *RestoredModeError) Error() string {
	message := "carbon: cannot resume session " + e.SessionID.String() + ": it was started in mode " +
		strconv.Quote(string(e.Mode)) + ", which this build of Carbon no longer defines"
	if len(e.Declared) == 0 {
		message += " (this build declares no modes; the primer's effort comes from default_effort in models.json)"
	} else {
		names := make([]string, 0, len(e.Declared))
		for _, name := range e.Declared {
			names = append(names, strconv.Quote(string(name)))
		}
		message += " (this build declares " + strings.Join(names, ", ") + ")"
	}
	return message + ". Start a new session instead of resuming this one."
}

func (e *RestoredModeError) Unwrap() error { return e.Cause }

// classifyRestoreError turns harness's unknown-mode bind failure into the actionable
// RestoredModeError and passes every other restore failure through untouched.
//
// harness reports an unresolvable mode name as *loop.BindError{Kind:
// BindInvalidDefinition, Name: <mode>}. The Name alone is not enough to claim the
// mode is missing -- that kind is also raised for bind failures carrying no mode --
// so the classification is confirmed against the definition's own declared set: only
// a name this definition genuinely does not offer becomes a RestoredModeError.
func classifyRestoreError(err error, sessionID uuid.UUID, definition loop.Definition) error {
	var bindErr *loop.BindError
	if err == nil || !errors.As(err, &bindErr) || bindErr.Kind != loop.BindInvalidDefinition || bindErr.Name == "" {
		return err
	}
	missing := loop.ModeName(bindErr.Name)
	modes := definition.Modes()
	declared := make([]loop.ModeName, 0, len(modes))
	for _, mode := range modes {
		if mode.Name == missing {
			return err
		}
		declared = append(declared, mode.Name)
	}
	return &RestoredModeError{SessionID: sessionID, Mode: missing, Declared: declared, Cause: err}
}
