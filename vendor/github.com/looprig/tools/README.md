# tools

`github.com/looprig/tools` provides optional standard tools for looprig Loops. The harness defines the contracts. This module provides implementations that consumers can select individually, plus the deliberate related-family `Tasks` bundle.

```go
loop.WithTools(
	tools.ReadFileDefinition(readGuard),
	tools.GlobDefinition(readGuard),
	tools.GrepDefinition(readGuard),
	tools.TaskDefinitions(),
)
```

`TaskDefinitions()` produces the four model-facing tools `TaskCreate`,
`TaskUpdate`, `TaskGet`, and `TaskList`. They are one deliberate bundle because
the four operations must share one bounded, Loop-local task graph. Each
definition build creates a fresh graph; parent and child Loops are isolated,
while modes within one Loop share the graph. The Harness owns and injects the
`Subagent` control tool for delegated Loops; consumers must not add it from this
module.

There is no bundled file-tool definition. A read-only Loop can receive ReadFile without also constructing WriteFile or EditFile. Consumers can mix these tools with their own definitions or use no standard tools at all.

The module root is intentionally a small definition facade. Each concrete tool has a focused package, such as `readfile`, `writefile`, `grep`, `bash`, and `websearch`. The `task` package owns the four related task operations. The `permission` package is the shared workspace rule library, and shared containment and mutation mechanics remain private under `internal`.

All README snippets are compiled by `example_readme_test.go` at the module root.

## Tool preparation

Every tool is a `tool.CallPreparer`. `PrepareCall` owns the whole preparation boundary: it decodes and validates the untrusted arguments once, normalizes commands, URLs, and paths, resolves canonical resource identities, and emits one typed `tool.Request` listing every capability `Requirement` the call needs. Invalid input fails during preparation and never reaches the permission gate. Execution consumes the typed prepared artifact bound to the call — the raw arguments are never reparsed — and a prepared tool that runs without its artifact fails closed.

Tools classify capabilities; they never decide Deny, Gated, or Allow. That three-state decision belongs to the harness gate evaluator, which consumes requests structurally without any tool-specific field extraction.

## The permission package

`permission` implements the single hardened workspace permission store of the access-profile specification. It stores capability rules — kind (`command.execute`, `network`, `filesystem.read`, `filesystem.write`), effect (`allow` or `deny`, deny always beats allow), enforcement class, and match — under the strict schema-version-2 JSON codec.

```go
store, diagnostics, err := permission.NewWorkspaceStore(permission.Config{
	Path: permissionFilePath, // one explicit absolute path; never discovered
})
```

Hardening: the store serves exactly one explicit permission-file path (it never computes HOME-relative or implicit locations), requires owner-only `0600` files, bounds file size, re-reads the file per query in interactive mode (so concurrent processes observe each other's atomically renamed updates), loads one immutable snapshot in read-only headless mode, and persists approved allow candidates atomically under an interprocess lock. Any load failure fails closed as an error.

Bash command rules come in three enforcement classes: an exact normalized command, the wildcard `Bash(*)`, and the token-prefix family `Bash(git log:*)`. Family matching is per shell segment: the normalized command is split at `&&`, `||`, `;`, `|`, `|&`, `&`, newline, and subshell boundaries, and a family covers a segment only when the segment is a provably simple command whose leading bare literal tokens equal the family tokens exactly — token equality, never string prefix. Anything the conservative grammar cannot prove simple (substitution, redirection, dynamic expansion, ambiguous quoting, …) is matchable only by a wildcard or an exact rule. Everything fails closed.

The automatic-family eligibility catalog is injected by the consumer (`Config.FamilyEligible`); a manually authored allow family outside the catalog stays authoritative but produces a non-fatal `Diagnostic` the consumer must surface. Deny families never warn.

The harness gate consumes the store structurally as its rule matcher and writer; deny-before-allow ordering belongs to the gate, and the store answers both queries independently.

## Bash access declarations

A Bash call may carry a structured `access` declaration of the filesystem and network deltas the command needs. The declaration **requests** authority — it never grants it. Each declared delta becomes one more requirement in the same typed request, so a gated command and its deltas share a single combined approval; an omitted gated delta stays OS-blocked by the sandbox at run time, and the model retries with a new call that declares the needed capability. Grants are minted only after the gate's decision, and command issuance is always exact-command even when a wildcard or family rule satisfied the decision.

```go
tools.Bash(
	bash.WithRunner(confinedRunner),
	bash.WithFamilyCatalog(familyEligible),
)
```

## Shared network capability

Bash network deltas, Fetch, and WebSearch all emit the same `network` capability kind with the same canonical target match encoding, so one saved workspace rule for a host and port serves all three tools. Fetch derives its single endpoint from the validated URL; WebSearch emits one requirement per endpoint its injected `SearchProvider` declares, and the provider fails closed on any secondary target outside that declaration.

## Fail-closed properties

- Invalid or unparseable arguments fail during preparation; nothing reaches the gate or the filesystem.
- A prepared tool invoked without its typed artifact refuses to run.
- Permission-file load failures (missing when required, wrong mode, oversized, malformed, unsupported schema or normalization version) are errors, never empty-rule successes, in interactive mode.
- Unsegmentable or unprovably simple shell input never matches a family rule.
- Definition builders reject nil (including typed-nil) dependencies at build time with a `DefinitionBuildError`.

See the access-profile specification (`carbon/docs/specs/access-profiles.md`) for the cross-module design, and the historical [module specification](docs/specs/module.md) for the original extraction plan.

Run the full local security suite with:

```bash
make secure
go test -race ./...
```
