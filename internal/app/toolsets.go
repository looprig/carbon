package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools"
	"github.com/looprig/tools/bash"
	"github.com/looprig/tools/editfile"
	"github.com/looprig/tools/permission"
	"github.com/looprig/tools/readfile"
	"github.com/looprig/tools/skill"
	"github.com/looprig/tools/websearch"
	"github.com/looprig/tools/writefile"
)

// toolsets.go owns CodeRig's direct sandbox assembly: it builds the one
// session sandbox.ExecutorSet, combined access gate, and standard Generic tool
// definitions bound to that set. There is no confinement bridge — CodeRig wires
// sandbox profiles, harness gate evaluation, and the tools package directly.
//
// The four sandbox capability kinds (command.execute, filesystem.read/write,
// network) route to the session's effective *sandbox.Profile; the two product kinds
// (tool.invoke, context.load) route to CodeRig's product access source. The bound
// per-Loop executor is the structural gate.GrantIssuer AND the confined command
// runner, so a minted grant validates against the exact executor that runs the
// command.
//
// process_adapter.go (same package) is this file's asynchronous-process
// counterpart to grantedExecutor: newProcessRunnerResolver captures the session's
// *sandbox.ExecutorSet and returns a tools.AsyncProcessRunnerResolver, a
// closure that mechanically wraps that session's per-Loop *sandbox.Executor as a
// harness tool.AsyncProcessRunner, resolved fresh on each call from the
// LoopID Harness Bind supplies. This file only exposes that constructor; a
// later task threads it into bashDefinition's build closure alongside the
// existing synchronous grantedExecutor lookup.

// maxReadBytes is CodeRig's per-file in-process read cap applied by the direct
// ReadFile tool. Sandbox profile access still governs read
// authority through the gate; this bound only limits how much a single approved
// read returns. It is product policy.
const maxReadBytes int64 = 5 << 20

// familyPolicyRev is the durable revision of CodeRig's automatic Bash-family
// eligibility catalog (exactly git log/status/diff/show/push). It folds into the
// Generic per-Loop policy revision so a catalog change invalidates a restore.
const familyPolicyRev = "coderig-family:git-log-status-diff-show-push:v1"

// executorScratchLimit bounds the number of memoized executor identities in the
// session's set: every primer/delegate Loop plus every spawnable sub-loop the
// delegation quota allows, with headroom.
const executorScratchLimit = delegationSpawnQuota + 4

// errNoLoopProvenance reports that the access gate was consulted outside a live
// loop step (no provenance), so the per-Loop executor cannot be resolved. It
// fails closed.
var errNoLoopProvenance = errors.New("coderig: access gate consulted without loop provenance")

// coderigReadGuard is CodeRig's in-process read guard for the direct read tools.
// It denies no path lexically — sandbox profile access is the read-authority
// source of truth, enforced by the gate on filesystem.read requirements and by
// the OS for confined commands — and applies the fixed per-file byte cap. This
// is only true end-to-end because ReadFile is constructed with tools'
// WithHostReads() below: without it, a host (out-of-workspace) path
// never reaches the gate at all, since the tools package's own workspace
// containment check rejects it first. With it, an out-of-workspace path still
// reaches the SAME filesystem.read requirement/gate/profile as a spawned Bash
// command's host reads, so the two never disagree about what a given profile
// allows.
type coderigReadGuard struct{}

func (coderigReadGuard) DeniedRead(string) bool { return false }
func (coderigReadGuard) MaxReadBytes() int64    { return maxReadBytes }

var _ loop.ReadGuard = coderigReadGuard{}

// grantedExecutor adapts a *sandbox.Executor to the tools package's command
// runner seams. The executor already satisfies tool.CommandRunner structurally;
// this adapter additionally satisfies tool.GrantedRunner by sourcing the
// execution ID from the prepared call the runner installed on ctx — the same ID
// the gate minted the grant tokens against — so a token validates against the
// exact executor that runs the command.
type grantedExecutor struct{ exec *sandbox.Executor }

func (g grantedExecutor) RunCommand(ctx context.Context, dir, command string) ([]byte, int, error) {
	return g.exec.RunCommand(ctx, dir, command)
}

func (g grantedExecutor) RunCommandWithGrants(ctx context.Context, dir, command string, grants []string) ([]byte, int, error) {
	executionID := ""
	if call, ok := loop.PreparedCallFromContext(ctx); ok {
		executionID = call.ExecutionID.String()
	}
	return g.exec.RunCommandWithGrants(ctx, executionID, dir, command, grants)
}

var (
	_ tool.CommandRunner = grantedExecutor{}
	_ tool.GrantedRunner = grantedExecutor{}
)

// accessGate is CodeRig's combined access gate. It satisfies loop.AccessGate and,
// per authorized call, resolves the calling loop's own executor from the session
// executor set (keyed by the live step's Loop ID) and runs one gate
// evaluator with that executor as the structural grant issuer. Interactive
// construction supplies the workspace rule writer and the loop's approval
// capability; headless construction supplies neither and returns a typed
// approval-required denial for any unmet gated requirement.
type accessGate struct {
	set         *sandbox.ExecutorSet
	bindings    []gate.AccessBinding
	matcher     gate.RuleMatcher
	writer      gate.RuleWriter // nil for headless
	interactive bool
}

func (g *accessGate) Authorize(ctx context.Context, request tool.Request) (gate.Resolution, error) {
	provenance, ok := loop.ProvenanceFrom(ctx)
	if !ok || provenance.LoopID.IsZero() {
		return gate.Resolution{}, errNoLoopProvenance
	}
	executor, err := g.set.For(provenance.LoopID.String())
	if err != nil {
		return gate.Resolution{}, err
	}
	var evaluator *gate.Evaluator
	if g.interactive {
		evaluator, err = gate.NewInteractiveEvaluator(g.bindings, g.matcher, loop.GateApprover(), g.writer, executor)
	} else {
		evaluator, err = gate.NewHeadlessEvaluator(g.bindings, g.matcher, executor)
	}
	if err != nil {
		return gate.Resolution{}, err
	}
	return evaluator.Authorize(ctx, request)
}

var _ loop.AccessGate = (*accessGate)(nil)

// sandboxAccessBindings routes the four sandbox capability kinds to the session's
// effective profile (the SAME immutable pointer the session executor set enforces)
// and the two product-owned kinds to CodeRig's product access source.
func sandboxAccessBindings(profile *sandbox.Profile, product gate.AccessSource) []gate.AccessBinding {
	return []gate.AccessBinding{
		{Kind: permission.CapabilityCommandExecute, Source: profile},
		{Kind: permission.CapabilityFilesystemRead, Source: profile},
		{Kind: permission.CapabilityFilesystemWrite, Source: profile},
		{Kind: permission.CapabilityNetwork, Source: profile},
		{Kind: capabilityToolInvoke, Source: product},
		{Kind: skill.CapabilityContextLoad, Source: product},
	}
}

// agentPolicyRevision is Generic's per-Loop durable policy revision: the selected
// profile name and family catalog revision. It is deliberately
// WORKSPACE-INDEPENDENT — the workspace root and the full normalized profile
// (with roots, HOME, and isolation) live in the rig-level access
// digest (NativePermissionPolicyRev), which the rig owns alongside workspace
// placement. Folding the root into the per-loop revision would couple the loop
// fingerprint to a placement concern the loop never captures. A selected-profile
// or family-catalog change still changes this revision.
func agentPolicyRevision(profile AccessProfile) string {
	return "coderig-access:generic:" + string(profile) + ":" + familyPolicyRev
}

// bashDefinition builds the workspace-bound, session-supervised Bash
// definition backed by Generic's per-Loop confined executor for BOTH its
// paths. The build closure retains the SAME synchronous
// set.For(bindings.LoopID.String()) lookup this definition has always used
// (the SAME instance the access gate resolves as grant issuer for the
// identical Loop ID, so a grant minted during evaluation validates against
// the runner that executes the command) for the foreground/confined-runner
// path, and separately invokes resolver — captured from the caller, Task
// 26B's newProcessRunnerResolver constructed over this SAME executor set —
// with the identical bindings.LoopID, for the background/async path.
// resolver supplies the concrete tool.AsyncProcessRunner Task 15's
// supervised Bash factory (bash.NewSupervisedFactory) requires; a nil
// resolver, a resolver error, or a nil/typed-nil returned runner all abort
// Build before any tool is produced. Bash proposes only the product family
// catalog for automatic reuse.
func bashDefinition(set *sandbox.ExecutorSet, resolver tools.AsyncProcessRunnerResolver) tool.Definition {
	catalog := productFamilyEligibility()
	return tool.NewDefinition("Bash", tool.RequiresWorkspace|tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if bindings.Workspace == nil {
			return nil, &WorkspaceRootError{}
		}
		executor, err := set.For(bindings.LoopID.String())
		if err != nil {
			return nil, err
		}
		if resolver == nil {
			return nil, &tools.DefinitionBuildError{Definition: "Bash", Dependency: "resolver"}
		}
		runner, err := resolver(ctx, bindings.LoopID)
		if err != nil {
			return nil, &tools.DefinitionBuildError{Definition: "Bash", Dependency: "runner", Cause: err}
		}
		// WithWorkspaceCoordinator/WithObservations are deliberately omitted:
		// bash.NewSupervisedFactory's returned factory always overwrites
		// those two fields from bindings.Workspace itself (matching
		// WriteFileDefinition/EditFileDefinition's identical "do not pass
		// here" pattern in the tools package), so passing them again would
		// be redundant.
		factory, err := bash.NewSupervisedFactory(
			bash.WithRunner(grantedExecutor{executor}),
			bash.WithFamilyCatalog(catalog),
		)
		if err != nil {
			return nil, err
		}
		built, err := factory(bindings, runner)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{built}, nil
	})
}

// genericToolDefinitions builds Generic's complete coding roster: read,
// mutate, session-supervised Bash, background process companions, web, and
// interaction utilities, plus the optional Skill tool. Every capability routes
// through the same session executor set.
func genericToolDefinitions(set *sandbox.ExecutorSet, client *http.Client, skillTool tool.Definition) []tool.Definition {
	guard := coderigReadGuard{}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard, readfile.WithHostReads()),
		tools.WriteFileDefinition(writefile.WithHostWrites()),
		tools.EditFileDefinition(editfile.WithHostWrites()),
		bashDefinition(set, newProcessRunnerResolver(set)),
		tools.ProcessOutputDefinition(),
		tools.ProcessInputDefinition(),
		tools.ProcessStopDefinition(),
		tools.WebSearchDefinition(websearch.NewDuckDuckGoProvider(client)),
		tools.FetchDefinition(client),
		tools.TaskDefinitions(),
		tools.AskUserDefinition(),
	}
	if skillTool != nil {
		definitions = append(definitions, skillTool)
	}
	return definitions
}

// sessionAccess is one session's resolved, session-fixed access wiring: one
// executor set, one combined gate, one durable policy revision, and the
// presentation metadata (fixed profile name, workspace root, and
// permission-load diagnostics). It is built once per Open — interactive or
// headless — and never mutated.
type sessionAccess struct {
	profileName string
	workspace   string
	configRev   string
	diagnostics []string
	set         *sandbox.ExecutorSet
	gate        loop.AccessGate
	policyRev   string

	closeOnce sync.Once
	closeErr  error
}

// Close releases the session executor set exactly once (idempotent), removing
// its owned scratch HOME directory and revoking its grant key and proxy.
func (a *sessionAccess) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.set != nil {
			a.closeErr = a.set.Close()
		}
	})
	return a.closeErr
}

// buildHeadlessAccess resolves the session access wiring for a headless run: a
// read-only permission store (no HOME search) and headless gate evaluators that
// never prompt. The interactive counterpart is reached through openRuntimeAgent's
// interactive flag (buildSessionAccess with interactive=true).
func buildHeadlessAccess(cfg Config, root string, explicitPermissionPath ...string) (*sessionAccess, error) {
	if len(explicitPermissionPath) > 1 {
		return nil, errors.New("coderig: headless access accepts at most one permission file")
	}
	permissionPath := ""
	if len(explicitPermissionPath) == 1 {
		permissionPath = explicitPermissionPath[0]
	}
	return buildSessionAccessWithPermissionFile(cfg, root, false, permissionPath)
}

// buildSessionAccess constructs the session's fixed access wiring. It builds
// the selected Generic profile, resolves the parent egress route, opens the
// permission store, and constructs one executor set plus one combined gate.
// On any partial failure it closes what it already built so no scratch HOME
// leaks.
func buildSessionAccess(cfg Config, root string, interactive bool) (*sessionAccess, error) {
	return buildSessionAccessWithPermissionFile(cfg, root, interactive, "")
}

func buildSessionAccessWithPermissionFile(cfg Config, root string, interactive bool, explicitPermissionPath string) (*sessionAccess, error) {
	profileName := cfg.AccessProfile
	if profileName == "" {
		profileName = DefaultAccessProfile
	}

	selected, err := coderigProfile(profileName, root)
	if err != nil {
		return nil, err
	}
	egress, err := resolveEgressRoute(os.Getenv)
	if err != nil {
		return nil, err
	}

	store, diagnostics, err := buildPermissionStore(cfg, root, interactive, explicitPermissionPath)
	if err != nil {
		return nil, err
	}
	var writer gate.RuleWriter
	if interactive {
		writer = store
	}

	product := newProductAccessSource()
	scratch := os.TempDir()

	set, err := sandbox.NewExecutorSet(selected,
		sandbox.WithScratchRoot(scratch),
		sandbox.WithMaxExecutors(executorScratchLimit),
		sandbox.WithEgressRoute(egress.Route),
	)
	if err != nil {
		return nil, err
	}

	return &sessionAccess{
		profileName: string(profileName),
		workspace:   root,
		diagnostics: diagnosticMessages(diagnostics),
		configRev:   accessConfigDigest(profileName, selected, egress.Route),
		set:         set,
		gate: &accessGate{
			set:         set,
			bindings:    sandboxAccessBindings(selected, product),
			matcher:     store,
			writer:      writer,
			interactive: interactive,
		},
		policyRev: agentPolicyRevision(profileName),
	}, nil
}

// buildPermissionStore opens the session's workspace permission store: the
// interactive read/write store at the HOME-derived per-workspace path, or the
// headless read-only store (an empty rule set with no HOME search). Both satisfy
// the gate's RuleMatcher; only the interactive store is a RuleWriter. This is
// the only place CodeRig's ACCESS wiring resolves HOME (via looprigHome), and it
// does so only on the interactive branch — the headless branch never touches it.
// openRuntimeAgent separately resolves HOME to load <home>/mcp.json
// (newMCPSessionAssembly, assembly.go), unconditionally on both branches — MCP
// server discovery is not a permission-store concern, and headless composition
// is allowed to use MCP tools even though it can never answer their elicitations.
func buildPermissionStore(cfg Config, root string, interactive bool, explicitPermissionPath string) (*permission.Store, []permission.Diagnostic, error) {
	if interactive {
		if explicitPermissionPath != "" {
			return nil, nil, errors.New("coderig: interactive access cannot use an explicit read-only permission file")
		}
		home, err := looprigHome(cfg)
		if err != nil {
			return nil, nil, err
		}
		config, err := interactivePermissionConfig(home, root)
		if err != nil {
			return nil, nil, err
		}
		return permission.NewWorkspaceStore(config)
	}
	config, err := headlessPermissionConfig(explicitPermissionPath)
	if err != nil {
		return nil, nil, err
	}
	return permission.NewReadOnlyStore(config)
}

// diagnosticMessages projects the permission-store load diagnostics into the
// display-ready, non-secret notice lines the TUI surfaces before the first gate.
func diagnosticMessages(diagnostics []permission.Diagnostic) []string {
	if len(diagnostics) == 0 {
		return nil
	}
	messages := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		messages = append(messages, diagnostic.Message)
	}
	return messages
}
