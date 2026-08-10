# CodeRig Access Profile Specification

## Purpose

CodeRig selects command authority through consumer-defined access profiles.
The standalone `sandbox` module provides the profile vocabulary, validation,
and OS enforcement. The generic harness gate evaluates the three access states,
combines unmet requirements into one approval, and routes the response without
importing `sandbox`. Tools prepare normalized requirements and provide durable
rule matching and storage. Consumers join the modules through dependency-free
structural contracts. Reusable modules do not ship named profiles or product
defaults.

The model has two independent concerns:

- access choices decide whether an operation is denied, gated, or allowed;
- confinement decides whether commands run inside an OS sandbox or directly on
  the host.

## Standalone sandbox profile API

The reusable `sandbox` module owns the profile because the profile describes
the authority the OS boundary must enforce. `sandbox` must remain usable by a
consumer that does not use harness, its gate package, tools, TUI, or CodeRig.

```go
type Access uint8

const (
    Deny  Access = 0
    Gated Access = 1
    Allow Access = 2
)

type Home uint8

const (
    IsolatedHome Home = iota
    RealHome
)

type Isolation uint8

const (
    Sandboxed Isolation = iota
    Unconfined
)

type RootAccess struct {
    Path  string
    Read  Access
    Write Access
}

type ProfileConfig struct {
    WorkspaceRoot   string
    WorkspaceRead   Access
    WorkspaceWrite  Access
    HostRead        Access
    HostWrite       Access
    Network         Access
    Command         Access
    Home            Home
    Isolation       Isolation
    AdditionalRoots []RootAccess
    AckUnconfined   bool
}

// Profile is immutable after validated construction.
type Profile struct { /* normalized private state */ }

func NewProfile(config ProfileConfig) (*Profile, error)

// AccessFor is the dependency-free gate seam. Kind and scope are normalized by
// tool preparation. Its primitive return type is intentional.
func (p *Profile) AccessVersion() uint16
func (p *Profile) AccessFor(kind, scope string) (uint8, error)
func (p *Profile) Fingerprint() string

// Restrict returns the component-wise intersection without widening base.
func Restrict(base, ceiling *Profile) (*Profile, error)

// ExecutorSet memoizes independent executors and isolated HOME directories by
// an opaque consumer key. It has no knowledge of harness bindings.
func NewExecutorSet(profile *Profile, opts ...ExecutorSetOption) (*ExecutorSet, error)
func WithScratchRoot(path string) ExecutorSetOption
func WithMaxExecutors(limit int) ExecutorSetOption
func WithEgressRoute(route EgressRoute) ExecutorSetOption
func (s *ExecutorSet) For(key string) (*Executor, error)
func (s *ExecutorSet) Close() error

// IssueGrant mints one post-approval, single-spawn capability. The scalar
// signature is the dependency-free seam consumed structurally by gate.
func (e *Executor) GrantVersion() uint16
func (e *Executor) IssueGrant(
    ctx context.Context,
    executionID, command, workingDirectory string,
    kind, scope, enforcementClass, target string,
    expiresUnixMilli int64,
) (string, error)
```

`NewProfile` validates and normalizes a consumer-supplied `ProfileConfig` into
an immutable `Profile`. It must let consumers choose every field directly and
must not provide `ReadOnly`, `Trusted`, `Unconfined`, or other predefined
combinations.

The `ProfileConfig` zero values mean all access is `Deny`, HOME is isolated, and
execution is sandboxed, but `NewProfile` still rejects a missing canonical
workspace root. An unconstructed zero `Profile` is unusable: its ABI version is
unsupported and executor construction rejects it. Unknown enum values, relative
additional roots, contradictory root rules, and unacknowledged or internally
inconsistent unconfined profiles fail validation.

`Restrict` takes the less-authoritative access value for every capability using
`Deny < Gated < Allow`, intersects additional roots, requires matching canonical
workspace roots, prefers `Sandboxed` over `Unconfined`, and prefers
`IsolatedHome` over `RealHome`. It never mutates either input. Its result is
validated again, including the unconfined consistency rules.

`ExecutorSet` belongs in sandbox because executor isolation, grant-key
separation, and scratch-HOME lifecycle are enforcement concerns. It accepts only
an opaque key and sandbox options; CodeRig uses a Loop ID string, while a
standalone consumer may use any stable unique key. Every key gets an independent
executor, grant identity, and owner-only scratch HOME beneath the validated
scratch root. `WithScratchRoot` and `WithMaxExecutors` are required;
construction rejects a missing canonical scratch root or a non-positive limit.
The set creates one owner-only child beneath the caller-owned scratch root and
removes only that child on `Close`; it never removes the supplied root.
Concurrent `For` calls for the same key return the same executor, and `Close`
revokes the set and releases its owned resources.

`Fingerprint` covers the access ABI version, every normalized access field,
canonical workspace and additional roots, HOME choice, isolation choice,
unconfined acknowledgement, and required enforcement guarantees. It excludes
workspace permission rules and ephemeral grants.

`AccessVersion` returns access ABI version `1`. `AccessFor` returns the fixed
values `0=Deny`, `1=Gated`, and `2=Allow`. These values are explicit rather than
`iota`-dependent and may not be reordered. Both methods use only Go built-ins so
another package can consume the profile structurally without importing
`sandbox`. The initial normalized kinds are
`command.execute`, `filesystem.read`, `filesystem.write`, and `network`; adding
or changing a kind is a contract change. For exact filesystem requirements,
`scope` is the canonical target path and the profile selects workspace,
additional-root, or host access. A broad Bash filesystem request uses either
`tree:<canonical-root>` for one configured tree or the versioned `host:*` scope
for all other host paths. Command and network use an empty scope because their
profile state is global. The typed prepared requirement separately carries the
normalized command, path, or network target used for durable rule matching. An
unknown kind or malformed scope returns an error rather than becoming
indistinguishable from an intentional `Deny`.

The harness gate defines only the shape it needs:

```go
// Package gate does not import github.com/looprig/sandbox.
type AccessSource interface {
    AccessVersion() uint16
    AccessFor(kind, scope string) (uint8, error)
}

// AccessBinding routes one normalized requirement kind to one source.
// Multiple kinds may reference the same source instance.
type AccessBinding struct {
    Kind   string
    Source AccessSource
}

const (
    CurrentAccessVersion uint16 = 1
    AccessDeny           uint8  = 0
    AccessGated          uint8  = 1
    AccessAllow          uint8  = 2
)
```

This is the same structural pattern as `GuaranteeBits() uint64`: CodeRig can
pass a `*sandbox.Profile` directly as the source in bindings for the four
sandbox kinds. Gate
rejects an unsupported ABI version before evaluating a request. An access-source
error or unknown access value is reported as a configuration error and fails
closed. Contract tests in `sandbox`, harness, and CodeRig pin the version,
numeric meanings, and normalized capability identifiers so independent modules
cannot drift silently.

Gate accepts an explicit set of `AccessBinding` values rather than assuming one
source owns every possible requirement kind. Construction rejects duplicate or
missing kind bindings. This lets a standalone gate consumer add product-owned
kinds without extending `sandbox.Profile`. CodeRig binds `command.execute`,
`filesystem.read`, `filesystem.write`, and `network` to the effective profile;
it binds `tool.invoke` and `context.load` to a small CodeRig-owned immutable
source. `tool.invoke` is scoped to a stable external-tool identity and is
`Gated`; `context.load` is scoped to a canonical skill identity and is
`Gated`. Built-in pure tools may omit `tool.invoke`, while MCP and other
externally supplied tools must emit it. A skill outside the workspace also
emits the applicable filesystem requirement, so one combined prompt can cover
both the context load and host read. Neither kind is silently mapped to command
execution.

The two packages intentionally attach different behavior to the same state:

- `sandbox` compiles `Gated` as denied unless a valid per-spawn grant opens the
  exact requested delta;
- the gate resolves `Gated` through a consumer-provided rule matcher or one user
  prompt; and
- neither package calls into the other or owns the other's lifecycle.

`harness/pkg/gate` owns the generic evaluation order and combined prompt. It
accepts structural `AccessSource`, rule-matcher, rule-writer, and approver
interfaces. It does not parse tool input or implement a permission-file format.
`tools/permission` implements normalized workspace-rule matching, hardened file
loading, and atomic workspace-rule writes. A gate-only consumer may supply its
own implementations and does not need either `sandbox` or `tools`.

The gate also consumes this post-approval structural seam:

```go
type GrantIssuer interface {
    GrantVersion() uint16
    IssueGrant(
        context.Context,
        string, string, string, // execution ID, command, canonical cwd
        string, string, string, string, // kind, scope, class, target
        int64, // expiry as Unix milliseconds
    ) (string, error)
}
```

The signature intentionally uses standard-library and built-in types so a
`*sandbox.Executor` can satisfy it without importing harness. The gate invokes
it only after an approval or saved allow has satisfied the relevant `Gated`
requirement. Grant tokens are never proposed or minted before the decision and
are never persisted in permission files or audit records. The issuer accepts
only the v1 enforcement classes `filesystem.path.read.v1`,
`filesystem.tree.read.v1`, `filesystem.host.read.v1`,
`filesystem.path.write.v1`, `filesystem.tree.write.v1`,
`filesystem.host.write.v1`, `network.proxy-target.v1`, `network.broad.v1`, and
`command.start.v1`. It rejects malformed class/target combinations and any
delta the effective profile or backend cannot enforce. Every token binds the
executor, execution ID, exact normalized command, canonical working directory,
profile fingerprint, non-secret effective-route fingerprint, enforcement
guarantees, class, target, and expiry. The executor applies the structured
capability: `command.start.v1` authorizes the exact spawn, while filesystem and
network classes modify the per-spawn policy. Successful MAC verification
without actual enforcement is a security failure.

## Access semantics

| Access | Behavior |
|---|---|
| `Deny` | Reject without prompting. Saved permissions cannot override it. |
| `Gated` | Use a compatible normalized saved permission or ask the user. |
| `Allow` | Proceed without asking. |

## Tool preparation boundary

Permission evaluation does not parse raw tool arguments. Each tool owns a
preparation step that:

1. decodes its arguments;
2. validates required fields and types;
3. normalizes commands, URLs, and paths;
4. resolves canonical paths and other resource identities; and
5. produces a typed permission request containing its stable match and required
   capabilities.

Conceptually, the gate receives:

```text
Tool:  Bash
Match: git push
Capabilities:
  - command execution
  - network
```

It does not receive raw JSON or contain tool-specific field extraction. Invalid
input fails during preparation and never reaches the permission gate.

Each typed requirement carries its normalized `kind`, profile `scope`, durable
match, bounded display text, enforcement class, optional reusable candidate,
and optional `grant_class`/`grant_target`. Empty grant fields mean the prepared
direct tool enforces the approved resource itself. A command-backed requirement
sets both grant fields, and gate invokes the bound structural issuer only after
that `Gated` requirement is satisfied. Gate validates that grant fields are
either both empty or both present; tools classify the enforcement boundary but
never choose `Deny`, `Gated`, or `Allow`.

Every prepared `command.execute` requirement is command-backed: its grant class
is `command.start.v1` and its grant target is the exact normalized command. If
command access is `Gated`, a compatible saved exact, wildcard, or family rule
may satisfy the gate, but the issuer still mints only an exact-command,
single-spawn token. Command `Allow` needs no token, and command `Deny` rejects
without minting. This grant is produced from the same combined decision as any
filesystem or network grants; it never creates a second prompt.

Canonical path resolution also happens during preparation. Whether the resolved
path is denied, gated, or allowed remains a permission decision. Direct tools
must enforce the approved resolved path themselves; command tools pass the
approved capability grants to the OS sandbox for enforcement.

Bash exposes an optional structured access request alongside the command. It is
a request for authority, not a trusted description of what the shell will do:

```json
{
  "command": "git push",
  "access": {
    "network": [
      {"transport":"tcp","host":"github.com","port":443}
    ],
    "read": [],
    "write": []
  }
}
```

Filesystem entries select `path`, `tree`, or `host` scope. Preparation
canonicalizes path/tree values and adds their requirements to the same request
as `command.execute`; examples are `{"scope":"path","path":"../file"}` and
`{"scope":"host"}`, with `host` being explicitly broad. A declaration cannot
override `Deny`; it only makes a `Gated` delta eligible for approval. When an
exact endpoint or path cannot be enforced, Bash may request a truthfully labeled
broad, exact-command-bound network, host-read, or host-write delta. An omitted
gated delta remains blocked by the OS sandbox. After such a block, the caller
may issue a new Bash call that declares the needed capability. Shell-text
heuristics may suggest a declaration but never grant authority or widen the
enforced envelope.

There is one user-facing gate. A command requiring several gated capabilities
produces one approval request listing the command and every capability delta.

For example, when both command execution and network are `Gated`, `git push`
produces one request:

```text
Allow `git push`?

Capabilities:
- execute command
- network egress
```

The evaluator collects every unmet gated requirement before emitting the
request. If a saved rule already approves `Bash(git push)` but not its network
requirement, the prompt contains only network. If compatible saved approvals cover
both, no prompt is emitted. A deny matching either requirement rejects the whole
call without prompting.

An interactive gate offers exactly three actions:

| Action | Effect |
|---|---|
| `Approve` | Approve this call once. Write nothing. |
| `Approve always for this workspace` | Approve this call and atomically append the displayed reusable allow rules for every unmet capability to the workspace permission file. |
| `Deny` | Deny this call. Write nothing. |

There is no session approval scope, user-global approval scope, persistent-deny
button, or second capability prompt. Consumers may place explicit deny rules in
the workspace permission file they provide; deny continues to beat allow.

Gate construction explicitly selects interactive or headless interaction.
Interactive construction requires both an approver and durable rule writer so
all three actions are honest. Headless construction rejects an approver or
writer, never prompts, and exposes no interactive actions.

For a simple shell command, Bash preparation may offer a validated reusable
command candidate such as `Bash(git log:*)`. The gate shows the exact candidate
before `Approve always for this workspace` can persist it. The same syntax is
accepted in a manually authored permission file. `:*` means a token-prefix
family: `Bash(git log:*)` matches the `git log` command and its trailing argument
tokens, but it does not match `git status`, `git catalog`, or a second shell
segment. The stored canonical representation is structured rather than a raw
string prefix.

Every family rule, including `Bash(git push:*)`, belongs to the same
command-access-only class as `Bash(*)`. It cannot carry, satisfy, persist, or
imply a network or filesystem capability delta. For example, reusable
`git push origin <branch>` command access composes with an independent
target-scoped `Network(github.com:443)` rule; the sandbox mints the network
grant separately for each spawn.

A family or bare-wildcard command rule may satisfy only the gate decision for
`command.execute`. It never becomes a family-scoped sandbox token: after a
match, the issuer mints `command.start.v1` for the exact normalized command and
that spawn only.

Every shell segment separated by `&&`, `||`, `;`, `|`, `|&`, `&`, a newline, or
a subshell boundary is prepared and matched independently. A wildcard never
crosses one of those boundaries, so `Bash(git log:*)` cannot authorize
`git log; rm -rf output`. Parsing a simple command is necessary but not
sufficient for automatic proposal. CodeRig automatically proposes a family only
when its literal command/subcommand prefix appears in a small, explicit
product-owned eligibility catalog. Unknown commands fail closed to an exact
proposal. Shells, interpreters, `find`, `xargs`, `env`, package/task runners,
and other prefixes that can evaluate code or select another executable are not
eligible. This positive catalog is used instead of a denylist so an unknown
execution wrapper cannot become family-approved automatically.

CodeRig's v1 automatic-family catalog is exactly `git log`, `git status`,
`git diff`, `git show`, and `git push`. Catalog changes are product policy
changes and update the policy revision included in the durable configuration
fingerprint. Although `git push` may receive reusable command access, its
network permission remains an independent target rule or an exact-command-bound
broad grant as described below.

Bare `Bash(*)`, redirection, command or process substitution, dynamic expansion,
ambiguous quoting, and unsupported shell syntax are also never proposed
automatically; the UI falls back to the exact normalized segment. A manual rule
using unsupported or ambiguous syntax is rejected rather than interpreted as a
raw prefix. A manually authored, syntactically valid allow-family remains
authoritative even when it is outside the eligibility catalog, but loading it
emits a non-fatal security diagnostic that the consumer must surface. This
includes exec-capable and unknown prefixes. A deny-family never widens authority
and needs no such diagnostic. The existing
normalized-string prefix matcher is not a valid implementation of this
contract.

`Bash(*)` satisfies only the command-access decision. It never grants network,
host filesystem, real HOME, or additional-root access. Bare wildcard and family
command records must not carry capability deltas. A family command match
therefore does not make `git push` network-capable: the exact target still needs
its own compatible network rule or a gate. The sandbox mints fresh, short-lived,
single-spawn tokens for every execution.

The gate applies the three-state decision. The sandbox enforces the
resulting filesystem, network, environment, process, and resource policy. A
capability configured as `Gated` remains OS-blocked unless the spawn carries a
valid grant. An undeclared access attempt is blocked; the sandbox does not open
an interactive gate during a syscall.

Gate does not imply confinement. Without a sandbox or another enforcing command
runner, it controls whether a tool call may start but cannot constrain the
resulting process. A consumer using gate alone must describe its executor as
unconfined and must not claim that process filesystem or network restrictions
are enforced. Direct tools may still enforce their own approved resource
boundaries.

## Permission evaluation

The gate uses one ordered evaluator, not a security-level or posture ladder.
Its rule matcher and writer are supplied by the consumer or
`tools/permission`:

```text
Apply a matching saved deny
             ↓
Evaluate every required capability
  Deny   → reject
  Gated  → matching saved allow or ask
  Allow  → proceed
             ↓
Emit one combined prompt when anything remains gated
```

A saved deny may always tighten `Gated` or `Allow`. A saved allow may satisfy
`Gated`. No saved record may loosen `Deny`.

## Permission file model

Permission files store capability rules rather than a separate rule vocabulary
for every tool. The durable shape is versioned and conceptually contains:

```json
{
  "version": 2,
  "normalization_version": 1,
  "rules": [
    {
      "effect": "allow",
      "capability": "command.execute",
      "enforcement_class": "command.invoke.shell-segment-glob.v1",
      "match": {
        "tokens": ["git", "log"],
        "trailing_arguments": true
      }
    },
    {
      "effect": "allow",
      "capability": "network",
      "enforcement_class": "network.target.v1",
      "match": {
        "transport": "tcp",
        "host": "github.com",
        "port": 443
      }
    }
  ]
}
```

The concrete encoding must preserve these rules:

- each record controls one capability;
- deny beats allow;
- the file records the normalization schema used to produce every match;
- rules bind to an enforcement class such as target-scoped network access or
  exact-command-bound broad egress;
- command matches are exact after normalization or use the versioned,
  token-aware shell-segment family matcher;
- `Bash(git log:*)` is accepted as the display/file syntax for a structured
  token-prefix family, and `Bash(*)` is represented only as a wildcard
  `command.execute` rule;
- bare wildcard and family command rules cannot carry, satisfy, or imply another
  capability;
- automatic family candidates come only from the consumer's explicit eligible
  prefix catalog; manual allow families outside that catalog are accepted with
  a security diagnostic;
- one combined approval may atomically append several capability records;
- `Approve` writes nothing and `Approve always for this workspace` writes the
  exact displayed, validated allow candidates to the single workspace file;
- grants are never persisted—only their normalized capability descriptions are;
  and
- workspace rules are not bound to a selected CodeRig profile or a live sandbox
  executor revision.

`Approve always for this workspace` means that a compatible normalized rule may
be reused by later sessions and other selected profiles in that workspace. A
profile `Deny` still overrides it. A rule with an unsupported normalization
version or different enforcement class does not match. Short-lived sandbox
grants, unlike permission rules, bind to the exact profile fingerprint, command,
working directory, executor, guarantees, and expiry.

There is one optional permission file per run. CodeRig's interactive default is:

```text
~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json
```

The file lives outside the repository, is created with owner-only permissions,
and is the sole destination for `Approve always for this workspace`. The
implementation removes implicit reads from `~/.looprig/approvals.json` and does
not maintain an in-memory session-rule layer.

Interactive updates are safe across concurrent CodeRig processes. The store
locks per workspace, re-reads and merges under the lock, writes an owner-only
temporary file, fsyncs it, atomically renames it, and fsyncs the directory.
Loading and writing reject symlinks, unexpected owners, group/world permissions,
oversized files, unsupported versions, and unexpected link counts. A failed
write leaves the prior complete file intact; the call fails and does not execute
as an implicit once-only approval.

Loading returns non-fatal rule diagnostics separately from fatal file errors.
Interactive consumers display diagnostics for manually authored allow families
outside the automatic eligibility catalog before the first prompt; headless
consumers must emit them to their configured diagnostic sink. Diagnostics never
alter rule precedence or silently discard explicit consumer policy.

Headless consumers may supply one permission-file path explicitly at startup.
The file is loaded as a read-only, immutable rule set for that run. With no
configured file, the evaluator uses an empty rule set. A configured file that is
missing, malformed, too large, insecure, or unsupported fails startup. Files are
not watched or reloaded; the consumer restarts the run to apply a change.

In a headless run, a matching allow proceeds, a matching deny rejects, and a
`Gated` requirement with no match returns a typed approval-required denial. It
never opens a gate or waits for input. A consumer that wants to share rules
across workspaces does so explicitly by passing the same file; there is no hidden
user-global layer.

Network matches use the narrowest identity the preparing tool and enforcement
boundary can honestly enforce. A normalized network request may include
transport, scheme, host or IP, port, HTTP method, and path. Every field present
in a rule is a required constraint. Omitting a field deliberately broadens the
rule and must be explicit in the approval description.

For sandboxed Bash, target-scoped network enforcement is provided, on a backend
that reports the `TargetNetwork` guarantee, by a local egress proxy owned by
`sandbox`, not by Seatbelt hostname rules. The supporting OS backend blocks
direct external connections and permits the child to reach only the proxy's
loopback listener. The proxy enforces the granted transport,
normalized hostname, and port before connecting. It supports HTTP forwarding
and HTTPS `CONNECT` without terminating TLS. A client that ignores the injected
proxy variables cannot bypass the target policy; it fails to connect. SOCKS
client and upstream support, transparent interception, and TLS termination are
outside the first version.

Because HTTPS contents remain encrypted, Bash network rules do not claim HTTP
method or path enforcement. Method/path rules remain Fetch-specific and use a
different enforcement class. Adding HTTPS method/path enforcement later
requires an explicit MITM design, certificate lifecycle, and different
guarantees; it must not be smuggled into the hostname proxy.

When Bash cannot declare an enforceable endpoint, it may request a truthfully
labeled broad, exact-command-bound egress grant only on a backend that can
enforce that broader boundary. Such a command-bound rule cannot satisfy a Fetch
request. An undeclared hostname, including a redirect or secondary service,
is blocked by the proxy and returned as a typed denial so the caller can prepare
a new request, ask once, and retry. The proxy never opens a gate from inside a
network operation.

A proxy rejection is associated with the authenticated execution credential.
After the child exits, the executor returns `ErrNetworkTargetDenied` as the
primary typed error and retains any process exit status as diagnostic detail.
When no authenticated proxy rejection occurred, ordinary spawn, wait, and exit
errors retain their normal precedence.

Raw-TCP and proxy-unaware clients are a known v1 limitation. In particular, Git
over an SSH remote does not normally honor `HTTP_PROXY` or `HTTPS_PROXY`, so a
target-scoped `github.com:22` grant still fails closed. The honest v1 fallback
is a broad network grant bound to the exact normalized `git push` command and
the backend's actual enforcement class, commonly any destination on TCP/22.
That broad grant is not attached to or reusable through `Bash(git push:*)`;
different push commands may therefore ask again. The approval text must make the
broader destination scope explicit.

### Existing organization proxies

The local enforcement proxy can chain through an existing organization proxy:

```text
sandboxed command -> loopback enforcement proxy -> organization proxy -> target
```

The child sees only proxy variables pointing at the loopback listener. The
sandbox supervisor captures configured upstream HTTP/HTTPS proxy routing before
scrubbing the child environment, and the local proxy sends the normalized
hostname and port to the upstream proxy. For HTTPS, the organization proxy
receives `CONNECT host:port` and normally performs destination DNS resolution.
Upstream credentials remain in the supervisor: they are never placed in the
child environment, permission file, prompt, fingerprint, log, or audit record.

An upstream route is explicit executor configuration. Static HTTP and HTTPS
upstreams are required in the first version; a consumer-supplied resolver may
select among them for corporate routing or PAC integration. If an upstream is
configured, connection, resolution, or authentication failure fails closed and
must not fall back to a direct connection. Inherited `NO_PROXY` entries do not
silently create such a fallback; direct routes must be explicit consumer policy
and still pass through the local target matcher.

The local proxy can guarantee the client-requested hostname and port even when
the upstream proxy resolves DNS. It cannot independently guarantee the resolved
address class in that arrangement. Address/private-network/metadata guarantees
are reported only when the direct resolver/dialer enforces them or the configured
upstream supplies a trusted guarantee contract. Hostname enforcement and
address-class enforcement are separate guarantee bits. Route fingerprints
include the non-secret route identity and guarantee contract so restores and
grants cannot silently change egress boundaries.

The same honesty rule applies to filesystem access. If Bash cannot enumerate an
exact external path, an approved broad host-read or host-write delta is bound to
the exact command and its broad enforcement class. It cannot satisfy a direct
file-tool request for a canonical path.

## Shared network capability

`Fetch` has no separate permission posture, permission file, or approval stage.
Its preparation step validates the method and URL and emits a typed `network`
capability request. The common evaluator applies the selected profile's network
state:

- `Deny` rejects Fetch without prompting;
- `Gated` uses a matching network rule or asks once; and
- `Allow` runs Fetch without a permission prompt.

`WebSearch` follows the same path for the provider endpoints it uses. Bash uses
the same network capability when its prepared request declares network access.
A target-scoped network rule can therefore be reused across tools only when each
tool emits the same normalized target and the enforcement boundary can honor it.
More constrained Fetch rules, such as an exact HTTP method and path, do not
silently become broad Bash egress approvals.

## Sandbox v2 network TODO

This roadmap version is independent of the permission-file schema version.
Deferred network work is:

- [ ] Add a SOCKS5 child listener and SOCKS upstream chaining for target-scoped
  raw TCP.
- [ ] Add complete SSH integration, potentially using a sandbox-owned
  `ProxyCommand` helper, that also specifies SSH-agent/key access, known-hosts
  behavior, isolated-HOME interaction, host rewriting, and organization-proxy
  port policy.
- [ ] Add transparent TCP interception only on platforms where it can be
  implemented without weakening direct-egress denial.
- [ ] Add a Linux network-namespace bridge or equivalent socket plumbing that
  lets a rung-1 child reach only its parent-owned enforcement proxy without
  opening direct egress.
- [ ] Add a Linux address-aware alternative to rung-2 Landlock port rules before
  reporting target-scoped proxy enforcement there.
- [ ] Add opt-in TLS termination with a defined CA lifecycle before claiming
  HTTPS method or path enforcement.
- [ ] Add stronger TLS destination binding or inspection for threat models where
  a client-supplied CONNECT hostname is insufficient.

None of these deferred features may be inferred from `TargetNetwork` in v1.
They require new enforcement classes and guarantee/fingerprint changes.

The greenfield hard cut treats the existing permission mechanisms as follows:

| Existing mechanism | Description | Decision | Replacement or owner |
|---|---|---|---|
| Input validation | Parses and validates tool arguments | Move | Tool preparation |
| Capability classification | Derives normalized resources and required effects | Keep | Tool preparation emits a typed request |
| Workspace/path containment | Prevents access outside configured roots | Keep | Canonical resolution during preparation plus profile and OS enforcement |
| Grant authentication | Verifies MAC, command binding, policy revision, executor and expiry | Keep | Sandbox capability tokens |
| Persisted approvals | Stores normalized capability allows and denies in one workspace permission file | Keep | Gate evaluator with tools-owned rule matching and persistence |
| Session approvals | Stores temporary rules for the current session | Remove | `Approve` is once-only; durable rules are workspace-only |
| User-global approvals | Applies hidden rules across every workspace | Remove | Consumers explicitly provide one file when sharing is intended |
| Hard approvals | Auto-approves named tools before ordinary permission records | Remove | Profile `Allow`; saved denies may still tighten it |
| Separate hard-deny stage | Applies an additional path and command-prefix rule list | Remove | Profile `Deny` and non-configurable structural validation |
| Tool effect overrides | Allows a tool to force an effect inside the checker | Remove | Tools classify capabilities but do not decide permission |
| Security-level ordinal | Selects a numbered security tier | Remove | Consumer-selected complete profile |
| Sandbox mode presets | Expand reusable named modes into policies | Remove | Consumer-built profile |
| Permission postures | Map modes to different auto-approval behavior | Remove | Direct `Deny`/`Gated`/`Allow` evaluation |
| Trivial Bash classifier | Auto-approves selected command prefixes | Remove | Command access state and saved permissions |
| Mode-specific edit approval | Auto-approves edits at selected tiers | Remove | Workspace-write access state |
| Gate guarantee interlock | Makes mode-based auto-approval depend on achieved sandbox guarantees | Remove from permission evaluation | Validate required guarantees at sandbox construction and spawn |
| Backend guarantee reporting | Reports which OS restrictions were enforced | Keep | Sandbox validation and diagnostics |
| Gated unsandboxed fallback | Runs after an approval when sandbox construction failed | Remove | Reject unless the profile is explicitly acknowledged as unconfined |
| Unconfined acknowledgement | Prevents accidental direct host execution | Keep | Profile validation |
| Policy fingerprint | Detects restored sessions with different authority | Keep | Fingerprint the normalized profile |

## HOME and host access

`IsolatedHome` sets `HOME` to session-owned scratch storage so tools place caches
and configuration there. `RealHome` exposes the user's actual HOME value.

HOME selection does not replace filesystem policy. When host reads or writes are
allowed, a command may still reach the real home through an absolute path even
under `IsolatedHome`. When host access is denied, changing the working directory
or using `..`, symlinks, shell expansion, or absolute paths must not escape the
OS-enforced boundary.

Minimal runtime paths needed to launch binaries and writable process plumbing
such as `/dev/null` are implementation necessities, not host-data permission.
They are a fixed, tested backend allowlist rather than consumer-visible secret
or path presets. The old secret globs, `.git`/`.looprig` carveouts, mode-derived
environment policy, and implicit filesystem exceptions are removed. Explicit
workspace and root access values are authoritative. Environment scrubbing and
resource limits remain non-configurable executor safety mechanics and may not
silently widen filesystem or network access.

## Confinement

`Sandboxed` compiles the profile into the strongest available OS boundary.
Backends must report achieved guarantees and fail closed when a required
guarantee is unavailable.

`Unconfined` executes directly with the invoking user's authority. It is
an executor property and requires `AckUnconfined`. Because it cannot enforce
process filesystem or network restrictions, `NewProfile` rejects an unconfined
profile unless workspace read/write, host read/write, and network are all
`Allow`. Command execution itself may remain `Deny`, `Gated`, or `Allow` because
the executor can still require `command.start.v1` before the process starts.
`IsolatedHome` may still
redirect caches, but it is not a secrecy boundary for an unconfined process.
CodeRig must show an explicit warning before selecting unconfined execution. An
approval of an ordinary gated operation never implies unconfined execution.

## CodeRig profiles

CodeRig defines exactly three product profiles from the reusable API:

| Capability | ReadOnly | Trusted | Unconfined |
|---|---|---|---|
| Workspace read | `Allow` | `Allow` | `Allow` |
| Workspace write | `Deny` | `Allow` | `Allow` |
| Host read | `Deny` | `Allow` | `Allow` |
| Host write | `Deny` | `Gated` | `Allow` |
| Network | `Deny` | `Allow` | `Allow` |
| Command execution | `Gated` | `Allow` | `Allow` |
| Additional roots | None | None | Unrestricted |
| Command HOME | `IsolatedHome` | `IsolatedHome` | `RealHome` |
| Confinement | `Sandboxed` | `Sandboxed` | `Unconfined` |
| Explicit acknowledgement | No | No | Yes |

These names and combinations live in CodeRig, not in `sandbox`, harness, or
`tools`. CodeRig uses direct construction rather than a generic profile
registry. Other consumers may create entirely different combinations.

## Profile selection and persistence

CodeRig accepts only its three known profile names. It validates the selected
name before constructing the Rig. The command default is `ReadOnly`.
`Unconfined` additionally requires an explicit acknowledgement flag. New and
restored sessions use the same profile construction path.

The selected profile is fixed for the lifetime of a session. CodeRig chooses it
before constructing the Rig, the TUI displays it but does not change it, and a
different profile requires opening a new session. There is no live access-level
command, atomic profile swap, or in-flight authority migration.

CodeRig applies product-owned role restrictions before assembly. The operator
uses the selected profile. The reviewer uses `sandbox.Restrict(selected,
reviewerCeiling)`, where `reviewerCeiling` is a locally defined sandboxed,
read-only profile. CodeRig passes the same effective profile pointer to that
role's gate and executors. The reviewer therefore remains read-only even when
the selected product profile is Trusted or Unconfined.

The durable configuration fingerprint includes the access ABI version, selected
profile name, complete normalized selected profile, every role's effective
profile, and the non-secret egress route identity and declared guarantees. A
product profile, reviewer-ceiling, or egress-boundary change therefore causes a
configuration mismatch rather than silently restoring with different authority.
An interactive approval is either once-only or written to the workspace
permission file. Permission rules do not mutate the selected profile.

## Repository implementation impact

The implementation spans six active code repositories, retires one adapter
repository, and updates two publication repositories:

| Repository | Required work |
|---|---|
| `sandbox` | Own and validate `Profile`, expose the versioned primitive structural seams, remove reusable modes and presets in the hard cut, enforce explicit policies and isolated HOME, provide the loopback hostname/port enforcement proxy with optional fail-closed upstream chaining, retain narrow grants and backend guarantees, and update its README. It has no harness dependency. |
| `harness` | Own the generic three-state evaluator, define versioned structural access/rule/approval interfaces, carry typed prepared permission requests with multiple requirements, reduce approval scopes to once/workspace, remove ordinal security-limit commands and events, preserve generic routing/audit behavior, and add `pkg/gate/README.md`. It has no sandbox dependency. |
| `tools` | Move validation into preparation, emit typed capabilities including explicit Bash deltas, implement safe token-aware `Bash(...:*)` parsing/matching and catalog-aware persistence candidates/diagnostics, implement the single hardened permission file, and share network evaluation across Bash, Fetch and WebSearch. |
| `mcp` | Replace the old `PermissionPrompter`/approval-scope adapter with preparation that emits a stable `tool.invoke` requirement for each external MCP tool, then refresh its Harness dependency and vendor tree. |
| `tui` | Render one combined capability prompt with exactly the three actions, surface permission-file security diagnostics, and display the session-fixed consumer profile name without ordinal access controls. |
| `coderig` | Define the three product profiles, reviewer ceiling, and automatic-family eligibility catalog; pass the same immutable effective profile directly to each role's gate and sandbox executors; select the workspace permission file; accept an explicit read-only file for headless runs; update fingerprints/restore/CLI behavior; and test the complete assembly. |
| `confinement` | Remove from CodeRig and retire in the same feature. Its policy translation, mode/posture mapping, and gated unsandboxed fallback disappear rather than moving into another bridge. |
| `.github` profile docs and `www` | Replace public mode, security-limit, and confinement examples with the final profile/gate APIs, then update the website's profile-docs submodule pointer. These publication repositories use the same feature-branch name as the code repositories. |

`core`, `inference`, `storage`, and `fsstore` require no behavior changes unless
ordinary dependency or fixture updates are needed after harness wire changes.

## Platform enforcement

On macOS, a sandboxed profile starts from deny-by-default and adds only its
configured runtime paths, workspace, isolated storage, and allowed or granted
roots. A broad `(allow file-read*)` rule is forbidden unless host reads are
`Allow`.

For target-scoped Bash networking, Seatbelt permits only the local enforcement
proxy port and denies direct remote egress. Seatbelt's inability to filter by
domain therefore does not degrade a hostname grant into broad port access. The
proxy runs outside the child sandbox, authenticates an execution-bound token,
and enforces the grant before dialing directly or through the configured
organization proxy.

Linux backends compile the same normalized profile into mount, Landlock,
network, and process restrictions. The current rung-1 private network namespace
cannot reach a parent-loopback proxy without an explicit namespace bridge, and
rung 2 can restrict ports but not the destination address. Therefore neither
Linux rung reports `TargetNetwork` in v1. A target grant fails closed there;
where the backend can enforce it, the evaluator may instead request a visibly
broad, exact-command-bound egress grant. Platform-specific narrowing is
acceptable only when it grants less authority and is reported accurately.

The guarantee model includes a read-boundary bit distinct from legacy secret
deny lists. A `Sandboxed` executor must achieve every guarantee required by the
profile's non-`Allow` filesystem and network fields. It need not claim an
unrelated generic process-isolation bit when the selected backend does not
provide one. Unsupported sandboxed profiles fail construction or spawn; the
production null backend is not a fallback. Direct execution exists only through
an explicitly acknowledged `Unconfined` profile.

## Required module documentation

The implementation is incomplete until the owning modules document their final
public contracts:

- update `github.com/looprig/sandbox/README.md` to replace the security-mode and
  preset documentation with explicit policy enforcement, platform guarantees,
  HOME behavior, grant enforcement, unconfined acknowledgement, and examples of
  policies produced from consumer profiles; and
- create `github.com/looprig/harness/pkg/gate/README.md` describing the generic
  gate evaluator lifecycle, typed prepared requests, one combined prompt for
  multiple requirements, response routing, audit behavior, and the boundary
  between generic evaluation, sandbox enforcement, and tool-owned normalization
  and persistence.

The gate README must make clear that `pkg/gate` does not parse tool arguments,
define sandbox profiles, import sandbox, or implement a permission-file format.
It validates the access ABI, applies the generic evaluation order through
consumer-provided rule interfaces, and transports one typed request and
response. The sandbox README must make clear that sandbox enforcement never
opens an interactive gate itself and does not import harness.

README examples must compile. This greenfield change is a hard cut: the same
repository phase that lands a replacement API removes the obsolete API, tests,
and examples. There are no deprecated aliases, adapters, dual wire formats,
migration readers, or renamed preset shims.

## Verification

The reusable modules must test:

- every access field under `Deny`, `Gated`, and `Allow`;
- access ABI version mismatch, source errors, unknown kinds, and unknown values
  failing closed;
- invalid tool input failing before permission evaluation;
- typed normalized requests containing no raw argument parsing in the checker;
- fail-closed zero values and invalid enum values;
- rejection of unconfined profiles with any non-`Allow` process filesystem or
  network capability;
- gate-only command execution reporting no confinement guarantees;
- one combined prompt for multiple gated capabilities;
- exactly `Approve`, `Approve always for this workspace`, and `Deny` as the gate
  actions;
- partial saved approval producing one prompt containing only the unmet
  capability;
- Fetch, WebSearch, and Bash using the shared network evaluator;
- explicit Bash network/read/write declarations, omitted gated deltas remaining
  blocked, and broad deltas remaining exact-command-bound;
- automatic display and persistence of safe `Bash(git log:*)` candidates,
  manual use of the same syntax, exact fallback for unsupported shell syntax,
  and independent matching of every compound-command segment;
- family and bare-wildcard rules remaining command-only, exec-capable prefixes
  never being auto-proposed, unknown prefixes falling back to exact, and manual
  allow families outside the eligibility catalog producing a surfaced
  diagnostic;
- target-scoped network reuse only when normalized targets and enforcement
  guarantees match;
- saved exact and token-aware command approvals and rejection of wildcard
  capability grants;
- command- and policy-bound grant tokens;
- component-wise `Restrict` behavior and rejection of workspace-root mismatch;
- executor-set memoization, per-key grant/HOME isolation, limits, and close;
- isolated and real HOME behavior;
- workspace containment through absolute paths, traversal, and symlinks;
- macOS and Linux host-read and host-write enforcement;
- direct network bypass failure, loopback proxy authentication, hostname/port
  matching, redirects and undeclared targets failing closed, TLS remaining
  end-to-end, and proxy-unaware clients failing closed;
- direct and chained HTTP/HTTPS upstream routing, upstream DNS resolution,
  credential redaction, no direct fallback, explicit handling of `NO_PROXY`,
  and honest address-guarantee reporting;
- Git-over-HTTPS using reusable target rules while Git-over-SSH fails closed
  under target-only grants and requires a visibly broad exact-command grant; and
- explicit acknowledgement for unconfined execution.

Permission-file tests must cover once-only approval writing nothing, workspace
approval atomically appending all unmet allow rules, concurrent process merge,
failed-write rollback, symlink/owner/mode/link-count hardening, normalization and
enforcement-class mismatch, reuse across profiles with `Deny` still winning,
absence of user/session layers, headless explicit-file loading, headless no-match
denial, startup failure for an invalid configured file, and no live reload.

CodeRig must test the exact normalized value of all three profiles, CLI
selection, absence of in-session switching, new and restored session assembly,
fingerprint drift, and the rule that the reviewer remains sandboxed and
read-only under every selected profile.

Documentation verification must check both README paths exist, old mode/preset
examples are removed from the sandbox README, and any Go examples build or are
covered by documentation tests.
