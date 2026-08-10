# CodeRig Assembly Specification

## Purpose

CodeRig is the reference coding Rig built with looprig. It should contain coding behavior and product choices, while reusable runtime machinery lives in the appropriate modules.

## Name

Rename the repository, module, command, binary, and live product references from SWE or CodeRig to CodeRig:

```text
folder:  swe -> coderig
module:  github.com/looprig/coderig -> github.com/looprig/coderig
command: cmd/coderig -> cmd/coderig
binary:  bin/coderig -> bin/coderig
remote:  git@github.com:looprig/swe.git -> git@github.com:looprig/coderig.git
```

No source or session compatibility layer is required.

## Product-owned behavior

CodeRig owns:

- operator and reviewer prompts
- which tools each Loop receives
- which Loops may delegate to which Loops
- Loop modes and reasoning effort
- model and provider defaults
- the three named access profiles exposed by the command
- startup banner and product identity
- storage location and command flags

## Reusable behavior moved out

CodeRig does not own:

- the `SessionController` to `tui.Agent` adapter
- generic tool definition wrappers
- permission checker construction mechanics
- sandbox executor implementation and permission persistence
- a generic agent Registry
- model economy, standard, or premium tiers
- separate headless and persisted assembly graphs

## Loops and Rig assembly

CodeRig defines its Loops explicitly, then assembles one Rig from them.

```go
operator, err := operatorLoop(deps)
reviewer, err := reviewerLoop(deps)

assembly, err := rig.Define(
    rig.WithLoops(operator, reviewer),
    rig.WithPrimers(operator.Name()),
    rig.WithActivePrimer(operator.Name()),
    // storage, workspace, access profile, snapshots, and limits
)
```

There is no Registry. Duplicate Loop names are already rejected by Rig definition validation.

## Modes and effort

Delete `ModelCatalog`, its resolver, and the economy, standard, and premium vocabulary.

Each Loop declares purposeful modes such as `build`, `review`, or `quick`, using `loop.WithModes`. A mode may change the model, reasoning effort, instructions, tools, or tool limits. Reasoning strength uses `model.Effort`.

The base model remains an explicit CodeRig configuration dependency. There is no unused premium tier and no economy tier that is validated but not selected.

## One construction path

CodeRig has one session-opening function with injected storage and an explicit selector:

```go
type OpenConfig struct {
    Stores        Stores
    Workspace     string
    SessionID     uuid.UUID
    AccessProfile AccessProfile
    EgressRoute   sandbox.EgressRoute
    Resume        bool
}

func Open(ctx context.Context, app Config, open OpenConfig) (tui.Agent, error)
```

Production injects fsstore-backed stores. Tests inject memstore-backed stores. New and restore share model resolution, Loop definitions, Rig definition, workspace policy, and adapter selection.

Production resolves egress routing in the parent before opening the session. A
direct route still goes through sandbox's local target-enforcement proxy. When
standard `HTTP_PROXY` or `HTTPS_PROXY` configuration selects an organization
proxy, CodeRig constructs a chained route: the sandboxed command connects only
to loopback, and sandbox forwards approved targets through that upstream. The
parent may deliberately translate validated `NO_PROXY` entries into explicit
direct routes, but the child never inherits them as a bypass. Upstream
credentials remain inside the parent-side route and are redacted from errors,
fingerprints, logs, and audit summaries.

The access profile is selected before `Open` constructs the Rig and remains
fixed for that session. The TUI may display the selected name but does not expose
an in-session access switch. Opening with a different profile creates a new
session; restoring requires the stored profile and non-secret egress-route
fingerprints to match.

If a public headless helper remains, it is a small wrapper that supplies memstore. It must not define another assembly graph.

## Access profiles

CodeRig directly constructs its `ReadOnly`, `Trusted`, and `Unconfined` access
profiles from the standalone `sandbox.Profile` API. The profile names and their
combinations are product behavior; reusable modules provide no named presets.

There is no policy translation layer and CodeRig does not depend on
`github.com/looprig/confinement`. The dependency direction is:

```text
sandbox                         harness/pkg/gate
  Profile + OS enforcement       structural evaluation + prompt routing
       ^                                      ^
       | effective profile source + issuer   |
       +--------------- CodeRig --------------+
                              |  product access source
                              +-- prepared tools / MCP
```

The gate interface contains only built-in Go types:

```go
type AccessSource interface {
    AccessVersion() uint16
    AccessFor(kind, scope string) (uint8, error)
}
```

`*sandbox.Profile` satisfies that interface without importing harness. CodeRig
includes a compile-time assertion at its composition boundary:

```go
var _ gate.AccessSource = (*sandbox.Profile)(nil)
```

The target profile construction is direct and product-owned:

```go
type AccessProfile string

const (
    AccessReadOnly   AccessProfile = "readonly"
    AccessTrusted    AccessProfile = "trusted"
    AccessUnconfined AccessProfile = "unconfined"
)

// The CLI defaults to AccessReadOnly. AccessUnconfined requires a separate
// explicit acknowledgement before Open is called.

func coderigProfile(name AccessProfile, workspace string) (*sandbox.Profile, error) {
    profile := sandbox.ProfileConfig{
        WorkspaceRoot: workspace,
        Home:          sandbox.IsolatedHome,
        Isolation:     sandbox.Sandboxed,
    }

    switch name {
    case AccessReadOnly:
        profile.WorkspaceRead = sandbox.Allow
        profile.WorkspaceWrite = sandbox.Deny
        profile.HostRead = sandbox.Deny
        profile.HostWrite = sandbox.Deny
        profile.Network = sandbox.Deny
        profile.Command = sandbox.Gated
    case AccessTrusted:
        profile.WorkspaceRead = sandbox.Allow
        profile.WorkspaceWrite = sandbox.Allow
        profile.HostRead = sandbox.Allow
        profile.HostWrite = sandbox.Gated
        profile.Network = sandbox.Allow
        profile.Command = sandbox.Allow
    case AccessUnconfined:
        profile.WorkspaceRead = sandbox.Allow
        profile.WorkspaceWrite = sandbox.Allow
        profile.HostRead = sandbox.Allow
        profile.HostWrite = sandbox.Allow
        profile.Network = sandbox.Allow
        profile.Command = sandbox.Allow
        profile.Home = sandbox.RealHome
        profile.Isolation = sandbox.Unconfined
        profile.AckUnconfined = true
    default:
        return nil, fmt.Errorf("coderig: unknown access profile %q", name)
    }

    return sandbox.NewProfile(profile)
}
```

CodeRig's initial automatic Bash-family catalog is exactly `git log`,
`git status`, `git diff`, `git show`, and `git push`. Any catalog change updates
the product policy revision and durable configuration fingerprint. External MCP
tools emit `tool.invoke`; skill loads emit `context.load`; both are `Gated` by
the CodeRig-owned product access source and remain separate from sandbox command
authority.

The reviewer is restricted independently of the selected product profile:

```go
func reviewerProfile(selected *sandbox.Profile, workspace string) (*sandbox.Profile, error) {
    ceiling, err := sandbox.NewProfile(sandbox.ProfileConfig{
        WorkspaceRoot:  workspace,
        WorkspaceRead:  sandbox.Allow,
        WorkspaceWrite: sandbox.Deny,
        HostRead:       sandbox.Deny,
        HostWrite:      sandbox.Deny,
        Network:        sandbox.Deny,
        Command:        sandbox.Gated,
        Home:           sandbox.IsolatedHome,
        Isolation:      sandbox.Sandboxed,
    })
    if err != nil {
        return nil, err
    }
    return sandbox.Restrict(selected, ceiling)
}
```

The operator uses `selected` directly. The reviewer gate and reviewer executors
both receive the returned restricted pointer. No reusable module publishes a
named reviewer profile.

Assembly passes the same immutable pointer to the two independent systems. The
names below describe the target contracts; the final implementation may adjust
constructor spelling without changing the ownership or dependency direction:

```go
func buildRole(
    profile *sandbox.Profile,
    productAccess gate.AccessSource,
    sessionScratch string,
    maxExecutors int,
    egressRoute sandbox.EgressRoute,
    interaction gate.Interaction,
    rules gate.RuleMatcher,
    writer gate.RuleWriter, // optional: nil for headless
    approver gate.Approver, // optional: nil for headless
) (_ loopTools, err error) {
    // ExecutorSet is sandbox-owned and keyed by an opaque string. It does not
    // know about harness bindings; CodeRig supplies the session scratch root
    // here and the Loop ID at binding time.
    executors, err := sandbox.NewExecutorSet(
        profile,
        sandbox.WithScratchRoot(sessionScratch),
        sandbox.WithMaxExecutors(maxExecutors),
        sandbox.WithEgressRoute(egressRoute),
    )
    if err != nil {
        return loopTools{}, err
    }
    // ExecutorSet owns scratch HOME directories, grant keys, proxies, and
    // processes. From this point, every downstream construction error closes
    // it. Successful return transfers that responsibility to loopTools.
    releaseExecutors := true
    defer func() {
        if !releaseExecutors {
            return
        }
        closeErr := executors.Close()
        if err == nil {
            err = closeErr
        }
    }()

    executorFor := func(b tool.Bindings) (*sandbox.Executor, error) {
        return executors.For(b.LoopID.String())
    }

    permissionFactory, err := gate.NewPermissionFactory(gate.Config{
        Interaction: interaction,
        Access: []gate.AccessBinding{
            {Kind: "command.execute", Source: profile},
            {Kind: "filesystem.read", Source: profile},
            {Kind: "filesystem.write", Source: profile},
            {Kind: "network", Source: profile},
            {Kind: "tool.invoke", Source: productAccess},
            {Kind: "context.load", Source: productAccess},
        },
        Rules:       rules,
        Writer:      writer,
        Approver:    approver,
        // The bound *sandbox.Executor structurally satisfies gate.GrantIssuer;
        // tokens are minted only after evaluation succeeds.
        GrantIssuerFor: executorFor,
    })
    if err != nil {
        return loopTools{}, err
    }

    result := loopTools{
        definitions: []tool.Definition{
            tools.ReadFileDefinition(),
            tools.WriteFileDefinition(),
            tools.GrepDefinition(executorFor),
            tools.BashDefinition(executorFor),
            tools.FetchDefinition(),
        },
        permission:     permissionFactory,
        policyRevision: profile.Fingerprint(),
        closer:         executors,
    }
    releaseExecutors = false
    return result, nil
}
```

The caller supplies `productAccess`, interaction mode, rule matcher, and the
optional writer and approver explicitly. The same constructor therefore serves
interactive assembly with `gate.Interactive` plus a durable writer and approver,
or headless assembly with `gate.Headless`, a read-only matcher, and nil
writer/approver. Gate validates those combinations: interactive construction
requires a durable writer and approver, while headless construction rejects
either and returns a typed approval-required decision for an unmatched `Gated`
requirement. This is
configuration of one assembly path, not a second graph.

Tool preparation emits all requirements before evaluation. The evaluator reads
the profile through `gate.AccessSource`, queries the tool-owned workspace rule
store, and returns either deny, proceed, or one combined approval request. After
approval it invokes the structural grant issuer for command-backed deltas. Direct
tools enforce their approved canonical resources themselves. The executor
remains OS-denied for every `Gated` command capability not represented by a
valid grant. Specifically, every prepared `command.execute` requirement carries
`command.start.v1` and its exact normalized command. A saved exact, wildcard, or
family rule may satisfy the gate, but the issued command token remains
exact-command and single-spawn. Command `Allow` needs no token, command `Deny`
never mints one, and command start shares the existing combined approval rather
than producing a second prompt.

`sandbox.NewExecutorSet` is part of the target standalone sandbox API. It owns
per-key executor memoization and derives a separate isolated HOME beneath the
provided session scratch root. Its key is an opaque string, so it has no harness
dependency. `Profile.Fingerprint`, `Restrict`, `NewExecutorSet`,
`WithScratchRoot`, `WithMaxExecutors`, `ExecutorSet.For`, and
`ExecutorSet.Close` are required API—not placeholders for CodeRig-local
wrappers. The session assembly owns each set and closes it with the session.
The returned `loopTools` owns the set through its `closer` field; its `Close`
path invokes that closer exactly once.

The operator-primary and operator leaf use separate executor instances keyed by
their Loop IDs but the same selected operator profile. The reviewer uses its
restricted effective profile and separate executor instances. Sharing a profile
does not share grants, processes, or scratch HOME directories.

The egress route is shared as immutable, non-secret session configuration, but
each executor receives independent proxy authentication and grant state. A
configured upstream's connection, DNS, or authentication failure fails closed;
the executor never retries the target directly. When the upstream performs DNS,
the achieved guarantees distinguish hostname/port enforcement from verified
resolved-address enforcement.

The complete capability table, gate semantics, confinement behavior, and
persistence requirements are defined in
[`access-profiles.md`](./access-profiles.md).

## Repository rules

Replace rigid ceremony with engineering guidance:

- prefer cohesive responsibilities, but do not split code based on sentence grammar
- introduce interfaces at consumer boundaries or when multiple implementations are useful
- use typed errors when callers need classification or recovery
- use table-driven tests when several cases share the same setup and assertion shape
- use focused tests when a single scenario is clearer
- split functions based on ownership and readability, not an arbitrary line count
- preserve fail-secure behavior, context cancellation, race testing, and explicit validation

Apply the same correction to CLI and harness guidance where the current absolute rules created the misplaced abstractions.

## Historical documents

Existing plans that describe SWE remain historical records. Update live code, READMEs, package comments, examples, and current website content. Do not rewrite old plan documents as if CodeRig existed when they were written.
