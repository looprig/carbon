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
	"github.com/looprig/tools/glob"
	"github.com/looprig/tools/grep"
	"github.com/looprig/tools/permission"
	"github.com/looprig/tools/readfile"
	"github.com/looprig/tools/skill"
	"github.com/looprig/tools/websearch"
	"github.com/looprig/tools/writefile"
)

// toolsets.go owns CodeRig's direct sandbox assembly: it builds the per-role
// sandbox.ExecutorSet, the combined access gate, and the standard tool
// definitions bound to that set. There is no confinement bridge — CodeRig wires
// sandbox profiles, harness gate evaluation, and the tools package directly.
//
// The four sandbox capability kinds (command.execute, filesystem.read/write,
// network) route to the role's effective *sandbox.Profile; the two product kinds
// (tool.invoke, context.load) route to CodeRig's product access source. The bound
// per-Loop executor is the structural gate.GrantIssuer AND the confined command
// runner, so a minted grant validates against the exact executor that runs the
// command.

// maxReadBytes is CodeRig's per-file in-process read cap applied by the direct
// read tools (ReadFile/Grep). Sandbox profile access still governs read
// authority through the gate; this bound only limits how much a single approved
// read returns. It is product policy.
const maxReadBytes int64 = 5 << 20

// familyPolicyRev is the durable revision of CodeRig's automatic Bash-family
// eligibility catalog (exactly git log/status/diff/show/push). It folds into each
// role's per-Loop policy revision so a catalog change invalidates a restore.
const familyPolicyRev = "coderig-family:git-log-status-diff-show-push:v1"

// executorScratchLimit bounds the number of memoized executor identities in one
// role's set: every primer/delegate Loop plus every spawnable sub-loop the
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
// is only true end-to-end because ReadFile/Grep/Glob are constructed with
// tools' WithHostReads() below: without it, a host (out-of-workspace) path
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

// roleGate is CodeRig's per-role combined access gate. It satisfies loop.AccessGate
// and, per authorized call, resolves the calling loop's own executor from the
// role's executor set (keyed by the live step's Loop ID) and runs one gate
// evaluator with that executor as the structural grant issuer. Interactive
// construction supplies the workspace rule writer and the loop's approval
// capability; headless construction supplies neither and returns a typed
// approval-required denial for any unmet gated requirement.
type roleGate struct {
	set         *sandbox.ExecutorSet
	bindings    []gate.AccessBinding
	matcher     gate.RuleMatcher
	writer      gate.RuleWriter // nil for headless
	interactive bool
}

func (g *roleGate) Authorize(ctx context.Context, request tool.Request) (gate.Resolution, error) {
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

var _ loop.AccessGate = (*roleGate)(nil)

// sandboxAccessBindings routes the four sandbox capability kinds to the role's
// effective profile (the SAME immutable pointer the role's executor set enforces)
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

// rolePolicyRevision is the role's per-Loop durable policy revision: the selected
// profile NAME, the role, and the family catalog revision. It is deliberately
// WORKSPACE-INDEPENDENT — the workspace root and the full normalized profile
// (with roots, HOME, isolation, and reviewer ceiling) live in the rig-level access
// digest (NativePermissionPolicyRev), which the rig owns alongside workspace
// placement. Folding the root into the per-loop revision would couple the loop
// fingerprint to a placement concern the loop never captures. A selected-profile
// or family-catalog change still changes this revision.
func rolePolicyRevision(profile AccessProfile, role string) string {
	return "coderig-access:" + role + ":" + string(profile) + ":" + familyPolicyRev
}

// bashDefinition builds the workspace-bound Bash definition backed by the role's
// per-Loop confined executor. The build closure resolves the executor for the
// bound Loop ID (the SAME instance the access gate uses as grant issuer), so a
// grant minted during evaluation validates against the runner that executes the
// command. Bash proposes only the product family catalog for automatic reuse.
func bashDefinition(set *sandbox.ExecutorSet) tool.Definition {
	catalog := productFamilyEligibility()
	return tool.NewDefinition("Bash", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if bindings.Workspace == nil {
			return nil, &WorkspaceRootError{}
		}
		executor, err := set.For(bindings.LoopID.String())
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{bash.NewBash(bindings.Workspace.Root,
			bash.WithRunner(grantedExecutor{executor}),
			bash.WithWorkspaceCoordinator(bindings.Workspace.Coordinator),
			bash.WithObservations(bindings.Workspace.Observations),
			bash.WithFamilyCatalog(catalog),
		)}, nil
	})
}

// builderToolDefinitions builds the builder's tool roster: read, mutate,
// confined Bash, web, and the interaction utilities, plus the optional Skill
// tool. Bash routes through the builder role's confined executor set.
func builderToolDefinitions(set *sandbox.ExecutorSet, client *http.Client, skillTool tool.Definition) []tool.Definition {
	guard := coderigReadGuard{}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard, readfile.WithHostReads()),
		tools.WriteFileDefinition(writefile.WithHostWrites()),
		tools.EditFileDefinition(editfile.WithHostWrites()),
		tools.GlobDefinition(guard, glob.WithHostReads()),
		tools.GrepDefinition(guard, grep.WithHostReads()),
		bashDefinition(set),
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

// operatorToolDefinitions is a package-local compatibility seam for older
// tests; production uses builderToolDefinitions.
//
//lint:ignore U1000 retained for package-local legacy fixtures.
func operatorToolDefinitions(set *sandbox.ExecutorSet, client *http.Client, skillTool tool.Definition) []tool.Definition {
	return builderToolDefinitions(set, client, skillTool)
}

// plannerToolDefinitions builds the planner's read/research roster. It carries
// no file-mutation tools; its terminal is confined by the read-only planner
// profile. Bash is retained for read-only repository checks.
func plannerToolDefinitions(set *sandbox.ExecutorSet, client *http.Client, skillTool tool.Definition) []tool.Definition {
	guard := coderigReadGuard{}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard, readfile.WithHostReads()),
		tools.GlobDefinition(guard, glob.WithHostReads()),
		tools.GrepDefinition(guard, grep.WithHostReads()),
		bashDefinition(set),
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

// reviewerToolDefinitions builds the reviewer face's read-only critique roster:
// read, glob/grep, confined Bash, and interaction utilities, plus the optional
// Skill tool. It carries no file-mutation tools. Bash routes through the reviewer
// role's restricted executor set.
func reviewerToolDefinitions(set *sandbox.ExecutorSet, skillTool tool.Definition) []tool.Definition {
	guard := coderigReadGuard{}
	definitions := []tool.Definition{
		tools.ReadFileDefinition(guard, readfile.WithHostReads()),
		tools.GlobDefinition(guard, glob.WithHostReads()),
		tools.GrepDefinition(guard, grep.WithHostReads()),
		bashDefinition(set),
		tools.TaskDefinitions(),
		tools.AskUserDefinition(),
	}
	if skillTool != nil {
		definitions = append(definitions, skillTool)
	}
	return definitions
}

// sessionAccess is one session's resolved, session-fixed access wiring: one
// executor set per role (owned here, closed by the runtime agent), the three
// role gates, the durable access-config digest, and the presentation metadata (fixed
// profile name, workspace root, and permission-load diagnostics). It is built
// once per Open — interactive or headless — and never mutated.
type sessionAccess struct {
	profileName string
	workspace   string
	configRev   string
	diagnostics []string
	plannerSet  *sandbox.ExecutorSet
	builderSet  *sandbox.ExecutorSet
	reviewerSet *sandbox.ExecutorSet
	// Legacy aliases keep package-local tests that construct the former
	// operator face compiling; production always uses builderSet/builderGate.
	operatorSet       *sandbox.ExecutorSet
	plannerGate       loop.AccessGate
	builderGate       loop.AccessGate
	reviewerGate      loop.AccessGate
	operatorGate      loop.AccessGate
	plannerPolicyRev  string
	builderPolicyRev  string
	reviewerPolicyRev string
	operatorPolicyRev string

	closeOnce sync.Once
	closeErr  error
}

// Close releases all role executor sets exactly once (idempotent), removing
// their owned scratch HOME directories and revoking their grant keys and proxies.
func (a *sessionAccess) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		var errs []error
		if a.plannerSet != nil {
			errs = append(errs, a.plannerSet.Close())
		}
		if a.builderSet != nil {
			errs = append(errs, a.builderSet.Close())
		}
		if a.reviewerSet != nil {
			errs = append(errs, a.reviewerSet.Close())
		}
		a.closeErr = errors.Join(errs...)
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

// buildSessionAccess constructs the session's fixed access wiring. It builds the
// selected builder profile and the independent planner/reviewer restriction over the
// workspace root, resolves the parent egress route, opens the permission store,
// and constructs one executor set + one combined gate per role. On any partial
// failure it closes what it already built so no scratch HOME leaks. All role
// gates share the one workspace permission store (one workspace, one file).
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
	readOnly, err := restrictToReadOnly(selected, root)
	if err != nil {
		return nil, err
	}

	egress, err := resolveEgressRoute(os.Getenv)
	if err != nil {
		return nil, err
	}

	store, diagnostics, err := buildPermissionStore(root, interactive, explicitPermissionPath)
	if err != nil {
		return nil, err
	}
	var writer gate.RuleWriter
	if interactive {
		writer = store
	}

	product := newProductAccessSource()
	scratch := os.TempDir()

	plannerSet, err := sandbox.NewExecutorSet(readOnly,
		sandbox.WithScratchRoot(scratch),
		sandbox.WithMaxExecutors(executorScratchLimit),
		sandbox.WithEgressRoute(egress.Route),
	)
	if err != nil {
		return nil, err
	}
	builderSet, err := sandbox.NewExecutorSet(selected,
		sandbox.WithScratchRoot(scratch),
		sandbox.WithMaxExecutors(executorScratchLimit),
		sandbox.WithEgressRoute(egress.Route),
	)
	if err != nil {
		_ = plannerSet.Close()
		return nil, err
	}
	reviewerSet, err := sandbox.NewExecutorSet(readOnly,
		sandbox.WithScratchRoot(scratch),
		sandbox.WithMaxExecutors(executorScratchLimit),
		sandbox.WithEgressRoute(egress.Route),
	)
	if err != nil {
		_ = plannerSet.Close()
		_ = builderSet.Close()
		return nil, err
	}

	return &sessionAccess{
		profileName: string(profileName),
		workspace:   root,
		configRev:   accessConfigDigest(profileName, selected, readOnly, readOnly, egress.Route),
		diagnostics: diagnosticMessages(diagnostics),
		plannerSet:  plannerSet,
		builderSet:  builderSet,
		reviewerSet: reviewerSet,
		operatorSet: builderSet,
		plannerGate: &roleGate{
			set:         plannerSet,
			bindings:    sandboxAccessBindings(readOnly, product),
			matcher:     store,
			writer:      writer,
			interactive: interactive,
		},
		builderGate: &roleGate{
			set:         builderSet,
			bindings:    sandboxAccessBindings(selected, product),
			matcher:     store,
			writer:      writer,
			interactive: interactive,
		},
		operatorGate: &roleGate{
			set:         builderSet,
			bindings:    sandboxAccessBindings(selected, product),
			matcher:     store,
			writer:      writer,
			interactive: interactive,
		},
		reviewerGate: &roleGate{
			set:         reviewerSet,
			bindings:    sandboxAccessBindings(readOnly, product),
			matcher:     store,
			writer:      writer,
			interactive: interactive,
		},
		plannerPolicyRev:  rolePolicyRevision(profileName, "planner"),
		builderPolicyRev:  rolePolicyRevision(profileName, "builder"),
		reviewerPolicyRev: rolePolicyRevision(profileName, "reviewer"),
		operatorPolicyRev: rolePolicyRevision(profileName, "builder"),
	}, nil
}

// buildPermissionStore opens the session's workspace permission store: the
// interactive read/write store at the HOME-derived per-workspace path, or the
// headless read-only store (an empty rule set with no HOME search). Both satisfy
// the gate's RuleMatcher; only the interactive store is a RuleWriter.
func buildPermissionStore(root string, interactive bool, explicitPermissionPath string) (*permission.Store, []permission.Diagnostic, error) {
	if interactive {
		if explicitPermissionPath != "" {
			return nil, nil, errors.New("coderig: interactive access cannot use an explicit read-only permission file")
		}
		config, err := interactivePermissionConfig(root)
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
