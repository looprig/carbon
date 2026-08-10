# Consumer-Defined Sandbox Access Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace reusable sandbox modes and permission tiers with consumer-defined profiles while keeping sandbox independently usable and presenting at most one approval gate.

**Architecture:** `sandbox` owns the standalone `Profile`, enforces it, and provides a loopback egress proxy for hostname/port grants with optional fail-closed chaining through an organization proxy. Harness owns generic versioned access evaluation through built-in-only structural interfaces, so neither module imports the other; tools normalize requests and implement durable rule storage. CodeRig constructs one immutable effective profile per role, binds it directly for sandbox capability kinds, adds its own product-only access source, and supplies the same profile to sandbox. For commands, gate resolves `Gated` requirements before sandbox mints narrow per-spawn grants.

**Tech Stack:** Go, macOS Seatbelt, Linux Landlock/namespaces/network controls, looprig harness/tools/TUI modules.

---

## Goal

Replace the sandbox security-level ladder and reusable presets with explicit,
consumer-owned access choices. The reusable modules enforce the policy; CodeRig
and other consumers decide which access is appropriate for each Loop.

The design must keep command approval separate from process confinement while
presenting at most one approval gate to the user.

## Public access model

Every gateable capability has exactly three states:

| State | Meaning |
|---|---|
| `Deny` | Block the capability. Do not ask the user, and do not accept a saved approval. |
| `Gated` | Check for a matching saved approval. Ask the user when none exists. |
| `Allow` | Permit the capability without asking. |

`Gated` is the public name. There is no separate `ApprovalRequired`, baseline,
ceiling, trust level, or ordinal mode exposed to consumers.

The state applies independently to:

- workspace reads;
- workspace writes;
- host reads;
- host writes;
- network access;
- command execution; and
- read or write access for each explicit additional root.

Command HOME selection is a separate `Isolated` or `Real` choice. `Isolated`
points `HOME` at consumer/session-owned scratch storage. `Real` points it at the
user's actual home, but filesystem access rules still determine what can be read
or written. Minimal operating-system runtime paths and writable process plumbing
such as `/dev/null` are implementation necessities, not implicit host-data access.

The `ProfileConfig` zero values are fail-closed: all gateable access is `Deny`,
HOME is isolated, network is blocked, and no unconfined execution is
acknowledged. Construction still rejects a missing canonical workspace root. An
unconstructed zero `Profile` has an unsupported ABI version and cannot create an
executor.

## No reusable presets

The sandbox module does not publish `Untrusted`, `ReadOnly`, `Write`, `Trusted`,
`Unconfined`, or replacement presets. It does not assign product meaning to a
combination of capabilities.

A consumer constructs the complete policy it wants. It may define local names,
configuration, or UI for its own combinations, but those choices stay in the
consumer. CodeRig may therefore choose different policies for its operator and
reviewer without adding a registry or mode catalog to the reusable modules.

The reusable construction API belongs in `sandbox`, because the profile
describes the authority its OS boundary enforces. It exposes a validated
`Profile` whose access fields use `Deny`, `Gated`, or `Allow`, whose HOME field
selects isolated or real HOME, and whose isolation field selects sandboxed or
explicitly acknowledged unconfined execution. It exposes no named combinations
and has no dependency on harness, tools, TUI, or CodeRig.

Unconfined behavior remains possible only as an explicitly constructed policy
with a separate acknowledgement. It is an escape hatch, not a preset or a rung
in a security ladder. Because an unconfined process has the invoking user's
authority, profile validation requires workspace read/write, host read/write,
and network to be `Allow`; command invocation itself may still be denied or
gated. Isolated HOME under unconfined execution redirects configuration but does
not create a secrecy boundary.

## Ownership

| Component | Responsibility |
|---|---|
| `sandbox` | Own and validate `Profile`; compile and enforce filesystem, network, environment, process, and resource restrictions; run the loopback hostname/port enforcement proxy and chain it through an optional organization proxy; mint and verify narrow per-command capability grants. It imports no harness package. |
| `harness/pkg/gate` | Own generic three-state evaluation; consume access, rule, approval, and grant services through structural interfaces; combine unmet requirements into one prompt; and route/audit the response. It imports no sandbox package. |
| `tools/permission` | Normalize and match stored capability approvals and denies and harden and atomically update the workspace permission file. Prepared command requirements declare the exact structural grant class and target; tools do not mint tokens or choose an access state. |
| Consumer | Choose every access state, HOME behavior, additional roots, defaults, product profile names, product-only access kinds, and UI. |

The reusable modules must not silently fill omitted consumer policy with a more
permissive preset.

## Dependency-free access contract

`sandbox.Access` is a named internal/public profile field type with explicitly
fixed values. The gate-facing method deliberately returns `uint8`, so an
independent package can consume it without sharing a Go named type:

```go
// github.com/looprig/sandbox
type Access uint8

const (
    Deny  Access = 0
    Gated Access = 1
    Allow Access = 2
)

type ProfileConfig struct {
    WorkspaceRoot string
    // Complete access, HOME, isolation, and additional-root fields.
}

type Profile struct { /* normalized private state */ }

func NewProfile(config ProfileConfig) (*Profile, error)
func (p *Profile) AccessVersion() uint16
func (p *Profile) AccessFor(kind, scope string) (uint8, error)
func (p *Profile) Fingerprint() string
func Restrict(base, ceiling *Profile) (*Profile, error)
```

```go
// github.com/looprig/harness/pkg/gate — no sandbox import.
type AccessSource interface {
    AccessVersion() uint16
    AccessFor(kind, scope string) (uint8, error)
}

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

Access ABI version `1` defines the initial normalized kinds
`command.execute`, `filesystem.read`,
`filesystem.write`, and `network`; adding or changing one is a contract change.
Exact filesystem scope is a canonical path; broad Bash filesystem access uses
`tree:<canonical-root>` for one configured tree or the versioned `host:*` scope;
command and network use an empty scope. The typed
prepared requirement separately carries the normalized resource used for rule
matching. Unknown kinds or malformed scopes return an error. Unsupported ABI
versions, source errors, and unknown numeric values are configuration errors and
fail closed.

The version and fixed numeric values are a tiny structural ABI, like the existing
`GuaranteeBits() uint64` seam. Contract tests in both independent modules and an
integration test in CodeRig pin the values and normalized kind identifiers.
CodeRig passes the same `*sandbox.Profile` instance to the gate bindings for the
four sandbox kinds and to executor construction; it does not copy or translate
the profile. Separate CodeRig-owned bindings provide gated `tool.invoke` and
`context.load` kinds for MCP tools and skills. Gate requires exactly one source
per requested kind, so product-only kinds do not expand the standalone sandbox
vocabulary or get misclassified as command execution.

`Gated` has complementary meanings at each boundary. The gate may satisfy it
through a consumer-provided rule matcher or user approval. The sandbox compiles it as denied and
opens only the exact approved capability carried by a valid single-spawn grant. The
sandbox never reads permission files or prompts, and the gate never implements
OS enforcement.

The v1 sandbox grant classes are `filesystem.path.read.v1`,
`filesystem.tree.read.v1`, `filesystem.host.read.v1`,
`filesystem.path.write.v1`, `filesystem.tree.write.v1`,
`filesystem.host.write.v1`, `network.proxy-target.v1`,
`network.broad.v1`, and `command.start.v1`. Every prepared
`command.execute` requirement declares `command.start.v1` with its exact
normalized command as the target. A compatible saved exact, wildcard, or family
permission may satisfy a gated command decision, but the minted token is still
exact-command and single-spawn. Command `Allow` needs no token, command `Deny`
never mints one, and command start participates in the same combined approval as
all requested deltas rather than creating another prompt.

Gate remains useful without sandbox. A standalone consumer supplies its own
access source, rule services, and approver. Without an enforcing runner, gate
controls whether a command starts but cannot constrain its filesystem or network
behavior; such a consumer must describe command execution as unconfined and must
not claim sandbox guarantees.

## CodeRig profiles

CodeRig directly constructs three product-owned profiles:

| Capability | ReadOnly | Trusted | Unconfined |
|---|---|---|---|
| Workspace read | `Allow` | `Allow` | `Allow` |
| Workspace write | `Deny` | `Allow` | `Allow` |
| Host read | `Deny` | `Allow` | `Allow` |
| Host write | `Deny` | `Gated` | `Allow` |
| Network | `Deny` | `Allow` | `Allow` |
| Command execution | `Gated` | `Allow` | `Allow` |
| Additional roots | None | None | Unrestricted |
| Command HOME | Isolated | Isolated | Real |
| Confinement | Sandboxed | Sandboxed | Unconfined |
| Explicit acknowledgement | No | No | Yes |

These combinations do not become reusable presets. CodeRig validates the three
names directly.

CodeRig fixes the selected profile at session open. The TUI displays it but does
not change it in flight; a different profile requires a new session. The
operator uses the selected profile. The reviewer uses the component-wise
intersection of the selected profile and a CodeRig-owned sandboxed read-only
ceiling. Each role passes its same effective immutable profile to gate and
sandbox.

The durable configuration fingerprint includes the access ABI version, selected
name and normalized profile, every role's effective profile, and the non-secret
egress route identity and guarantee contract. Changing the reviewer ceiling or
egress boundary therefore invalidates restore just like changing a selected
product profile.

## Gate flow

There is one user-facing permission gate, even when a call needs more than one
approval.

Before a permission decision, the tool prepares the call. Tool preparation
decodes and validates raw arguments, normalizes commands and resource identities,
resolves canonical paths, and emits a typed request containing the stable match
and required capabilities. Invalid input fails there and never reaches the gate.
The gate contains no raw JSON parsing or tool-specific field
extraction.

Bash accepts an optional structured access declaration containing normalized
network targets, canonical read paths, and canonical write paths. The
declaration asks for authority; it does not grant it. Preparation adds declared
deltas to the same
request as `command.execute`. `Deny` rejects them, `Gated` includes them in the
combined evaluation, and `Allow` needs no grant. If an exact target cannot be
enforced, Bash may request a truthfully labeled broad network, host-read, or
host-write delta bound to the exact command. An omitted gated delta remains
blocked, and the caller may retry with a new explicitly declared request.
Shell-text heuristics may suggest requirements but are never an authorization
boundary.

For a prepared Bash call, the gate evaluates:

1. command execution access;
2. each sandbox capability requested for that command;
3. matching workspace deny records; and
4. matching workspace allow records for gated capabilities.

The result is:

- any required `Deny` capability rejects the request without prompting;
- every required `Allow` capability proceeds without prompting;
- every required `Gated` capability is satisfied by a compatible normalized
  saved approval or included in one combined approval prompt.

After approval, the sandbox mints capability tokens bound to the exact command,
working directory, current policy revision, executor instance, and expiry. The
executor verifies those tokens and loosens only the approved capabilities for
that spawn. Access that was not requested and approved remains OS-denied; the
sandbox never opens an interactive gate during a filesystem or network syscall.

An undeclared or unrecognized access attempt simply encounters the enforced
sandbox denial. Gate-only consumers without an enforcing runner control whether
a process starts but cannot claim filesystem or network confinement afterward.

Canonical resource resolution belongs to preparation, permission belongs to the
three-state evaluator, and enforcement belongs to the direct tool or sandbox.
The old tiered sequence of security ordinals, mode postures, hard approvals,
tool-effect overrides, and trivial-command classification is removed.

## Stored permissions

Permission files store independent capability rules rather than separate policy
systems per tool. Tool permission and sandbox capability permission remain
distinct records that one approval may append atomically:

- `Bash(<exact command>)` approves that normalized command;
- `Bash(git log:*)` approves the parsed `git log` shell segment and its trailing
  argument tokens;
- `Bash(*)` approves command invocation for all Bash calls;
- a network rule approves only the normalized target or exact command-bound
  egress displayed to the user; and
- capability deltas approve only the exact additional access displayed to the
  user.

The `:*` form is display and permission-file syntax for a structured,
versioned, token-prefix match rather than a raw string prefix. Every segment of
a compound shell command is parsed and matched independently; no wildcard can
cross `&&`, `||`, `;`, a pipeline, backgrounding, newline, or subshell boundary.
Automatic persistence candidates require a non-empty literal token prefix and
the conservatively supported simple-command grammar. Unsupported quoting,
redirection, substitution, expansion, or other complex syntax falls back to an
exact rule. A manually authored ambiguous pattern fails loading. The gate shows
the exact candidate that `Approve always for this workspace` will write.

Every family, including `Bash(git push:*)`, is command-access-only. It cannot
carry, satisfy, persist, or imply capability deltas. Reusable push command
access therefore composes with a separate target rule such as
`Network(github.com:443)`; the network grant is independently minted per spawn.

The parser alone does not decide which families are proposed automatically.
CodeRig owns a small positive catalog of eligible literal command/subcommand
prefixes. Unknown prefixes fall back to exact rules. Shells, interpreters,
`find`, `xargs`, `env`, package/task runners, and other prefixes that evaluate
code or select another executable are ineligible. A syntactically valid manual
allow-family remains authoritative, but any allow family outside the eligibility
catalog returns a non-fatal security diagnostic that interactive and headless
consumers must surface. This covers exec-capable and unknown prefixes without
pretending a denylist is exhaustive. Deny families require no diagnostic.

`Bash(*)` never grants network, host filesystem, HOME, or additional-root access.
Neither a bare wildcard nor family Bash record may carry capability deltas. A
saved approval cannot override a capability configured as `Deny`.

When a `Gated` command requirement and its capability deltas match saved workspace
approval, the gate may auto-approve the call and ask the sandbox grant issuer to
mint fresh single-spawn tokens. Tokens themselves are never persisted.
Workspace rules bind to the normalization schema, exact requested delta, and
enforcement class, but not to the selected CodeRig profile or live executor
revision. A different command, delta, schema, or enforcement class does not
match. A profile `Deny` still overrides a matching rule. Fresh grants bind to the
exact profile fingerprint, command, working directory, executor, guarantees,
and expiry.

For command access specifically, the reusable permission satisfies only the
gate. The fresh sandbox token uses `command.start.v1` and the exact normalized
command, even when the reusable permission was `Bash(*)` or a token-aware
family such as `Bash(git log:*)`.

An interactive gate exposes exactly `Approve`, `Approve always for this
workspace`, and `Deny`. `Approve` is ephemeral and writes nothing. Workspace
approval atomically appends the displayed validated allow rules for every unmet
capability to the single hardened out-of-repository permission file. There is no session or
user-global approval scope and no persistent-deny UI action.

CodeRig uses
`~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json` as its
interactive workspace file. A headless consumer may explicitly supply one
read-only file at startup. With no file, rules are empty; an invalid configured
file fails startup; files are not watched; and an unmatched gated requirement
returns a typed denial without opening a gate.

When a call has multiple gated requirements, the evaluator collects them before
asking. `git push` with gated command execution and gated network therefore
produces one prompt, not two. A saved command rule with no saved network rule
produces one network-only prompt; a matching deny for either requirement rejects
the call.

Fetch and WebSearch do not have separate permission postures. Their preparation
steps emit the same typed network requirement used by Bash. A target-scoped rule
may be shared across tools only when their normalized network targets and the
available enforcement guarantees match. A broader Bash egress grant that is
bound to an exact command cannot satisfy Fetch.

## Target-scoped Bash networking

Target-scoped Bash rules require an enforcement proxy. Seatbelt cannot filter
outbound traffic by domain, and a port-only allowance would make a hostname rule
dishonest. `sandbox` therefore runs a local proxy outside the child boundary.
The OS policy denies direct remote connections and permits the sandboxed child
to connect only to the authenticated loopback proxy. The first version accepts
HTTP forwarding and HTTPS `CONNECT`, matches the granted transport, normalized
hostname, and port, and does not terminate TLS. Proxy-unaware programs fail
closed instead of receiving broad egress. SOCKS and transparent interception
are deferred.

HTTPS method and path remain invisible to this proxy. Those fields stay in the
Fetch enforcement class. Method/path enforcement for Bash would require a
separate opt-in MITM design and certificate lifecycle; it is not part of this
plan.

An undeclared redirect or secondary hostname is blocked and reported as a typed
target denial. The caller may prepare a new request and retry after the ordinary
combined gate. The proxy never calls harness or pauses a syscall for approval.

Raw-TCP programs that ignore HTTP proxy variables fail closed in v1. This
includes ordinary Git-over-SSH remotes. A target-scoped `github.com:22` grant
does not make such a client proxy-aware; the available fallback is a truthfully
labeled broad egress grant bound to the exact normalized `git push` command and
the backend's actual port/address enforcement. It cannot attach to or become
reusable through `Bash(git push:*)`, so distinct pushes may ask again.

### Organization proxy chaining

When a consumer already uses an organization proxy, the network path is:

```text
sandboxed command -> local enforcement proxy -> organization proxy -> target
```

The supervisor captures upstream routing before it scrubs the child
environment. The child receives only loopback proxy variables. For HTTPS, the
local proxy sends `CONNECT host:port` to the upstream, allowing the organization
proxy to perform destination DNS resolution and apply corporate policy.
Upstream credentials stay in the supervisor and are excluded from prompts,
permission files, fingerprints, logs, and audit events.

The first version supports direct routing plus static HTTP/HTTPS upstream
proxies. A consumer-provided route resolver can select a configured upstream for
PAC or organization-specific routing without making `sandbox` depend on a
product. Once an upstream is selected, connection, resolution, and
authentication failure must not fall back to direct egress. `NO_PROXY` is not an
implicit bypass; a direct route must be an explicit consumer decision and still
passes through the local target matcher.

The local proxy enforces the hostname and port supplied by the child. When the
upstream resolves DNS, the local proxy cannot independently prove the resolved
address class. Hostname-target and address-class guarantees are reported
separately; private/metadata-address guarantees require a direct enforcing
resolver/dialer or a trusted guarantee declaration from the upstream. The
non-secret route identity and guarantee contract participate in the executor
fingerprint and grant binding.

## Sandbox v2 network TODO

The sandbox v2 roadmap is separate from the permission-file schema version. It
contains these explicitly deferred items:

- [ ] Add SOCKS5 child-listener and upstream chaining support.
- [ ] Add a complete SSH `ProxyCommand`-style adapter with explicit agent/key,
  known-hosts, isolated-HOME, host-rewrite, and organization-proxy behavior.
- [ ] Add transparent TCP interception only where the OS can preserve direct
  egress denial.
- [ ] Add a Linux rung-1 namespace/socket bridge that exposes only the
  parent-owned enforcement proxy to the child.
- [ ] Add an address-aware Linux rung-2 alternative before claiming
  target-scoped proxy enforcement from port-only Landlock rules.
- [ ] Add opt-in TLS termination with a managed CA lifecycle before supporting
  HTTPS method/path rules.
- [ ] Add stronger TLS destination binding for policies that do not trust a
  client-supplied CONNECT hostname.

Every item requires its own enforcement class, guarantee reporting, fingerprint
change, and fail-closed tests.

These checkboxes are not part of the v1 execution plan. Start a separate design
and implementation plan before selecting any of them for v2.

## Filesystem confinement

Workspace-only reading must be real OS confinement, not merely a working-directory
convention. A command starting in the workspace must not gain host visibility via
absolute paths, `..`, symlinks, `$HOME`, or a shell expansion.

On macOS, the Seatbelt policy must start from deny-by-default and add only the
configured workspace, additional roots, isolated storage, and curated runtime
paths. It must not use a broad `(allow file-read*)` base rule for a policy whose
host-read state is not `Allow`.

Linux backends must compile the same effective policy into their mount, Landlock,
network, and process boundaries. Rung 1 cannot reach a parent-loopback proxy
without the deferred namespace bridge, and rung 2 cannot restrict the proxy port
to loopback rather than a remote host. Neither reports target-network in v1.
Target grants fail closed; a visibly broad exact-command grant is available only
where the backend can enforce its stated class. Every unavailable required
guarantee fails closed.

## Policy changes and persistence

The old ordinal minimum operation is replaced by capability-by-capability role
restriction. `sandbox.Restrict` may only retain or reduce access; it must never
widen the selected consumer profile. Profiles do not change during a session.

Durable sessions store a stable policy fingerprint derived from the access ABI
version and every field that affects enforcement or automatic gate decisions.
Restoring under a changed policy fails with a configuration mismatch and all
ephemeral grants are invalid. Workspace permission rules remain reusable when
their normalization version, exact request, and enforcement class match; they
remain subordinate to profile `Deny`. Unknown states, omitted required
configuration, invalid path rules, or unavailable required guarantees fail
closed.

## Testing

Tests must cover:

- all three states for every capability;
- access ABI version mismatch, source errors, unknown kinds, and unknown values
  failing closed;
- `Deny` never prompting and never accepting persisted approval;
- `Gated` asking without a match and auto-approving only a compatible normalized
  match;
- `Allow` proceeding without a permission record;
- `Bash(*)` affecting command invocation but not sandbox capabilities;
- rejection of bare-wildcard or family records carrying capability deltas;
- automatic `Bash(git log:*)` candidates, manual use of the same syntax,
  segment-aware matching, and exact fallback for unsupported shell syntax;
- family and bare-wildcard rules remaining command-only, a positive eligibility
  catalog for automatic families, exact fallback for unknown or exec-capable
  prefixes, and surfaced diagnostics for manual allow families outside the
  eligibility catalog;
- explicit Bash network/read/write declarations and omitted gated deltas
  remaining blocked;
- loopback proxy authentication, target matching, direct-egress denial,
  undeclared redirects failing closed, proxy-unaware clients failing closed,
  and end-to-end TLS without method/path claims;
- direct and chained HTTP/HTTPS routing, upstream DNS, credential redaction,
  upstream failure without direct fallback, explicit `NO_PROXY` behavior, and
  separate hostname/address guarantee reporting;
- Git-over-HTTPS target reuse and Git-over-SSH target failure followed only by a
  visibly broad exact-command-bound grant;
- command-, directory-, policy-, executor-, and expiry-bound grant tokens;
- gate-only command execution claiming no confinement guarantees;
- rejection of inconsistent unconfined profiles;
- isolated HOME and denial of real-home reads and writes;
- executor-set per-key isolation, limits, and close;
- workspace containment through absolute paths, traversal, and symlinks;
- macOS and Linux host-read denial with required runtime paths still usable;
- component-wise role restrictions, session-fixed selection, and policy
  fingerprint drift; and
- explicit acknowledgement for truly unconfined execution.

CodeRig integration tests must construct explicit operator and reviewer policies,
exercise new and restored sessions through the same assembly path, and prove that
the reviewer remains sandboxed and read-only under every selected product
profile.

## Greenfield replacement

This project uses a coordinated hard cut, not expand-migrate-contract. Each
owning repository removes its obsolete public API, wire values, tests, and
examples in the same phase that lands the replacement. There are no deprecated
aliases, adapters, dual codecs, migration readers, renamed presets, or temporary
compatibility shims.

The active repositories are `sandbox`, `harness`, `tools`, `mcp`, `tui`, and
`coderig`; the `confinement` repository is retired after CodeRig moves to direct
assembly. `mcp` is included because its Harness adapter currently implements
the removed permission-prompter and approval-scope contracts. `core`,
`inference`, `storage`, and `fsstore` have no planned behavior changes.

The hard cut removes the user-global and session approval layers, replaces the
old approval scopes with once and workspace actions, replaces hidden headless
HOME lookup with an explicit read-only file, deletes the mode/posture/security
ordinal APIs, and makes `permissions.json` the sole durable workspace rule
format. Historical plans may retain old vocabulary as history; live code,
public documentation, fixtures, and examples may not.

Implementation uses the same feature-branch name in isolated worktrees for
every affected repository. Fresh implementation subagents work one scoped task
at a time with test-first commits. Formal spec-compliance and code-quality
reviews occur only at phase boundaries, across the complete phase delta; no
phase advances while either review has open findings.

The detailed task order, exact files, commands, deletion lists, worktree layout,
commit protocol, and phase-boundary review gates are defined in
[`2026-07-19-sandbox-access-profiles-execution.md`](./2026-07-19-sandbox-access-profiles-execution.md).
