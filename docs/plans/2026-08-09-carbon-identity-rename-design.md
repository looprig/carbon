# Carbon Identity Rename Design

**Date:** 2026-08-09
**Status:** Approved

## Goal

Publish CodeRig `v0.18.0` as a standalone consumable release, then make a
greenfield breaking rename of the complete product identity from CodeRig's
Generic agent to Carbon. Carbon begins a new release line at `v0.1.0` in the
new `github.com/looprig/carbon` repository.

The rename includes source, module and import paths, commands, binaries,
durable identities, product state paths, companion executables, tests,
documentation, workspace metadata, and Git remotes. There is no compatibility
layer, alias, state migration, old-session restore, deprecated MCP role, or
fallback to a CodeRig path.

## Principles

1. CodeRig `v0.18.0` is a real release, not an archival tag. Its module must
   resolve, build, and test with `GOWORK=off` and no local sibling `replace`
   directives.
2. Publish dependencies before dependents. Every version written into a
   downstream `go.mod` must already exist remotely.
3. Preserve Git provenance. The Carbon repository receives CodeRig's commit
   ancestry plus one or more explicit rename commits, but it does not receive
   CodeRig's old release tags.
4. Treat Carbon as a new product identity. Old configuration, state, sessions,
   roles, commands, and binaries are intentionally unsupported.
5. Replace product-specific uses of `generic`; do not replace the ordinary
   English or Go-language meaning of "generic" in reusable modules.
6. Never rewrite existing user work blindly. In particular, review and preserve
   the current uncommitted LLM round-tripper changes and CodeRig's untracked
   `docs/superpowers/` files.

## Release Train

### New foundational repositories

The local `secrets/` and `credentials/` directories are currently modules in
the workspace but are not independent Git worktrees. Initialize each as its own
repository after verifying its corresponding empty GitHub repository:

| Module | Repository | First release |
|---|---|---|
| Secrets | `github.com/looprig/secrets` | `v0.1.0` |
| Credentials | `github.com/looprig/credentials` | `v0.1.0` |

Secrets releases first. Credentials then removes its local Secrets replacement,
requires `github.com/looprig/secrets v0.1.0`, passes its independent release
suite, and releases second.

The first commits are complete snapshots of the reviewed current modules. Do
not attempt to manufacture or filter an independent history from the parent
workspace repository.

### Credential-aware inference releases

The current development versions of Inference and LLM consume the new modules
and expose APIs used by current CodeRig. Release them in this order:

| Module | Release | Required dependency adoption |
|---|---|---|
| Inference | `v0.9.0` | Credentials `v0.1.0`, Secrets `v0.1.0` |
| LLM | `v0.13.0` | Inference `v0.9.0`, Credentials `v0.1.0`, Secrets `v0.1.0` |

Remove the Credentials and Secrets development replacements before each
release. Other local development replacements must not affect a standalone
verification. Run each release suite with `GOWORK=off` and a clean module
cache proof after publishing prerequisites.

LLM currently has uncommitted changes in
`providers/openai/client.go` and `providers/openai/options.go`. They add a
caller-owned round-tripper option and appear related to the credential-backed
transport work. Preserve them, review them with the surrounding tests, and
include them in `v0.13.0` only after focused and full verification. Do not
discard or overwrite them.

### Runtime and presentation releases

Current CodeRig consumes APIs and behavior newer than several existing tags.
Publish these reviewed heads after their prerequisites:

| Module | Release | Reason |
|---|---|---|
| ACP | `v0.2.0` | Model/effort selection and ordered steering APIs |
| Harness | `v0.22.0` | Durable foreign collaboration and scoped broker services |
| MCP | `v0.5.0` | CodeRig collaboration MCP server and proxy |
| Foreignloops | `v0.2.0` | ACP selection, steering, and scoped foreign services |
| Tools | `v0.9.0` | Contained absolute Bash working directories |
| TUI | `v0.14.0` | Current runtime presentation and StartAgent behavior |

Update each module's direct Looprig requirements to already-published versions
before tagging it. The dependency order is:

1. Secrets.
2. Credentials.
3. Inference.
4. LLM, Harness, MCP, and Tools where their graphs permit.
5. ACP after its selected Inference/Harness versions exist.
6. Foreignloops after ACP and Harness exist.
7. TUI after its selected Harness and Inference versions exist.

Classifiers, Core, Eval, FSStore, Sandbox, Storage, and other unchanged
dependencies use their latest already-published compatible tags. Do not cut a
new release merely to make every repository version number advance.

### Final CodeRig release

Update `coderig/go.mod` to the published release train. Remove every local
`replace github.com/looprig/... => ../...` directive. Reconcile `go.sum` using
`GOWORK=off`.

The release gates are:

- no local `replace` directives;
- `GOWORK=off go mod verify`;
- `GOWORK=off go test -race ./...`;
- `GOWORK=off go test -tags integration -race ./...`;
- `make secure` with workspace resolution disabled where needed;
- native, Linux, and Windows cgo-free builds;
- a clean disposable-module install/build proof using only remote modules.

After all gates pass:

1. Commit the release dependency adoption.
2. Push CodeRig `main` to `git@github.com:looprig/coderig.git` without force.
3. Create annotated tag `v0.18.0` on the verified commit.
4. Push only `v0.18.0`.
5. Verify remote `main`, the peeled annotated tag, and an independent module
   fetch/build.

No Carbon runtime, package, command, or product-identity implementation changes
may enter CodeRig `v0.18.0`. The approved Carbon design and implementation plan
are release documentation and may be present in that tag.

## Git Transition

After the remote CodeRig release is verified, rename the local product
worktree from `coderig/` to `carbon/`. Preserve its `.git` directory and commit
history. Repair linked worktree metadata with `git worktree repair` and verify
every registered worktree before continuing.

Change the product repository's `origin` to:

```text
git@github.com:looprig/carbon.git
```

The old CodeRig repository remains the immutable home of CodeRig `v0.18.0`.
Carbon receives the shared commit ancestry through its `main` branch, followed
by explicit Carbon rename commits. Do not push CodeRig tags to Carbon. Push
Carbon `main` and Carbon's annotated `v0.1.0` tag only after the complete rename
and standalone verification pass.

Do not rewrite commit messages or historical Git objects. Historical files in
Carbon's checked-out tree are rewritten as ordinary new changes, while their
old versions remain inspectable in earlier commits.

## Identity Contract

Carbon has one agent, also named Carbon. The canonical mappings are:

| Surface | Old | New |
|---|---|---|
| Product display name | `CodeRig` | `Carbon` |
| Product token | `coderig` | `carbon` |
| Go module | `github.com/looprig/coderig` | `github.com/looprig/carbon` |
| Command | `cmd/coderig` | `cmd/carbon` |
| Binary | `coderig` | `carbon` |
| Agent package | `internal/catalog/generic` | `internal/catalog/carbon` |
| Agent name | `generic` | `carbon` |
| Agent display/persona | `Generic` | `Carbon` |
| Durable agent kind | `coderig:generic` | `carbon:carbon` |
| Product home | `~/.looprig/coderig` | `~/.looprig/carbon` |
| MCP role | `generic` | `carbon` |
| Collaboration helper | `coderig-collab-mcp` | `carbon-collab-mcp` |
| Error prefix | `coderig:` | `carbon:` |
| Access digest domain | `coderig-access-*` | `carbon-access-*` |
| Policy domains | `coderig-*` | `carbon-*` |

The system prompt becomes `<identity product="Carbon">` and says "You are
Carbon". Delegation is Carbon-to-Carbon. Runtime catalogue entries, MCP
visibility, skills, loop display names, primers, persistence fingerprints, and
event attribution all use the new Carbon name.

## Replacement Strategy

Use ordered, case-sensitive bulk replacements for mechanical text, followed by
semantic symbol and directory renames. Replace longest and most specific forms
before shorter tokens so a broad substitution cannot destroy a later match.

The ordered mechanical set includes:

1. `github.com/looprig/coderig` to `github.com/looprig/carbon`.
2. `coderig-collab-mcp` to `carbon-collab-mcp`.
3. `coderig:generic` to `carbon:carbon`.
4. `coderig-access:generic` to `carbon-access:carbon`.
5. `~/.looprig/coderig` to `~/.looprig/carbon`.
6. `/coderig/`, `cmd/coderig`, and `bin/coderig` path forms to Carbon forms.
7. Remaining product `CodeRig` to `Carbon`.
8. Remaining product token `coderig` to `carbon`.
9. Product-agent `Generic` and `generic` to `Carbon` and `carbon` only in
   product-specific contexts.

Then rename directories and Go identifiers, including:

- `cmd/coderig` to `cmd/carbon`;
- `internal/catalog/generic` to `internal/catalog/carbon`;
- the MCP module's `cmd/coderig-collab-mcp` to
  `cmd/carbon-collab-mcp`;
- `genericDefinition` and related product-local identifiers to Carbon forms;
- product-local aliases such as `coderig` to `carbon`;
- filenames whose names encode CodeRig where they remain active.

Bulk replacement may use a reviewed `sed` script or equivalent mechanical
rewriter. Every candidate file list and post-rewrite diff must be reviewed.
Never apply a repository-wide lowercase `generic` replacement to reusable Go
libraries: generic errors, generic type behavior, and ordinary prose remain
generic.

## Companion MCP Release

CodeRig `v0.18.0` uses MCP `v0.5.0` and the executable
`coderig-collab-mcp`. Carbon requires a second MCP release:

1. Rename the command directory and process diagnostics to
   `carbon-collab-mcp`.
2. Change the MCP server default name and product-specific comments/tests.
3. Preserve the underlying collaboration protocol and environment variable
   contract; only product identity changes.
4. Run MCP default, race, integration, security, and cross-platform build gates.
5. Publish MCP `v0.6.0`.
6. Make Carbon require MCP `v0.6.0`.

This keeps the final CodeRig release reproducible while allowing Carbon to have
no old executable fallback.

## Workspace and Ecosystem Updates

Update active workspace metadata from `coderig` to `carbon`, including:

- root `go.work`;
- root `repositories.mk`;
- dependency-boundary and root-layout tests;
- website command demos, glossary, package guides, roadmap, and profile;
- organization profile documentation;
- reusable-module examples and comments that name the product;
- build scripts, CI comments, command examples, and release documentation.

Historical design and implementation documents in the checked-out source tree
are rewritten too, because the requested Carbon tree should have no stale
product identity. This is an intentional new-tree cleanup, not a Git history
rewrite.

Reusable modules should prefer neutral wording when the text describes a
generic consumer seam. Use Carbon only for a real product-specific example.

The active-tree stale-reference gate excludes `.git`, vendored snapshots that
must remain byte-for-byte tied to an upstream release, generated third-party
artifacts, and external linked worktrees. All first-party checked-out source,
tests, and documentation are in scope.

## State and Compatibility

Carbon reads and writes only `~/.looprig/carbon`. It does not inspect, copy,
rename, merge, or warn about `~/.looprig/coderig`.

Carbon accepts only MCP role `carbon`. An absent roles field means Carbon.
`generic`, CodeRig role names, and former topology names fail strict validation.

Carbon does not install a `coderig` command, command alias, symlink, environment
fallback, module forwarding package, deprecated flag, or compatibility
configuration field. Old CodeRig sessions are not listed and cannot be resumed
through Carbon.

## Testing

Use test-driven changes at each semantic boundary. Before production edits,
change focused tests to demand:

- product and prompt identity `Carbon`;
- agent and primer name `carbon`;
- durable kind `carbon:carbon`;
- Carbon-only MCP roles;
- Carbon state and permission paths;
- Carbon CLI/banner/error output;
- Carbon collaboration helper discovery;
- Carbon module and dependency-boundary paths;
- absence of CodeRig fallbacks.

Mechanical documentation rewrites do not require artificial failing unit tests,
but they require stale-reference checks and diff review.

Verification proceeds from focused tests to package tests, full race suites,
integration suites, security checks, native/cross builds, and independent
remote-module proofs. No tag is created until its exact commit has passed its
release gates. No downstream module adopts a tag until the remote ref is
verified.

## Failure Handling

- If a prerequisite repository or tag is absent remotely, stop before editing
  downstream requirements.
- If a repository is dirty, preserve and classify the existing changes before
  release work. Do not reset or discard them.
- If any release gate fails, do not tag or push that release.
- If a push succeeds but remote verification fails, do not proceed to the next
  dependent.
- Never move or recreate an existing tag. Fix forward with a new patch tag when
  a published tag is wrong.
- Never force-push CodeRig, Carbon, or a dependency release branch.
- If linked worktree repair cannot prove every registered worktree, stop before
  changing the product remote.

## Acceptance Criteria

1. Secrets `v0.1.0` and Credentials `v0.1.0` exist remotely and resolve without
   local replacements.
2. Every required release-train tag exists remotely in dependency order.
3. CodeRig `v0.18.0` contains no local sibling replacements and independently
   builds/tests from remote modules.
4. CodeRig remote `main` and `v0.18.0` point to the verified final CodeRig
   commit.
5. Carbon's module, command, binary, prompt, loop, state, durable identity, MCP
   role, companion process, docs, and Git remote use Carbon naming.
6. Carbon contains no compatibility or migration path for CodeRig or Generic.
7. MCP `v0.6.0` provides `carbon-collab-mcp`, and Carbon consumes that release.
8. Carbon independently passes its complete release gates with `GOWORK=off`.
9. Carbon remote receives `main` and annotated `v0.1.0`, but no CodeRig tags.
10. First-party active-tree searches find no stale CodeRig product identity or
    Generic product-agent identity outside explicitly documented exclusions.

## Non-goals

- Migrating CodeRig configuration, credentials, permissions, or sessions.
- Preserving a CodeRig command or Go module alias.
- Rewriting existing Git objects or commit messages.
- Pushing old CodeRig tags to Carbon.
- Renaming generic concepts in reusable libraries.
- Introducing a multi-agent registry or changing Carbon's single-agent
  topology.
- Adding product behavior unrelated to the identity rename.
