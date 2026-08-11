package evidence

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/looprig/harness/pkg/tool"
)

// Limits bounds every evidence tool's output AT THE SOURCE: each tool
// truncates while it reads/lists/searches, never after buffering an
// unbounded result. Zero (or a value exceeding the package's internal hard
// ceiling) falls back to a safe default — see (Limits).normalize.
type Limits struct {
	// MaxListEntries bounds evidence_filesystem_list's returned entry count.
	MaxListEntries int
	// MaxReadBytes bounds evidence_filesystem_read's default read size when
	// a call does not specify its own (smaller-or-equal) limit.
	MaxReadBytes int
	// MaxGlobMatches bounds evidence_filesystem_glob's returned match count.
	MaxGlobMatches int
	// MaxGrepMatches bounds evidence_filesystem_grep's returned match count.
	MaxGrepMatches int
	// MaxGrepFileBytes bounds how many bytes of any one file
	// evidence_filesystem_grep scans.
	MaxGrepFileBytes int
	// MaxStatusBytes bounds evidence_git_repository_status's status text.
	MaxStatusBytes int
	// MaxDiffBytes bounds evidence_git_diff's diff text.
	MaxDiffBytes int
}

// DefaultLimits returns conservative, non-zero defaults comfortably inside
// every hard ceiling this package enforces internally.
func DefaultLimits() Limits {
	return Limits{
		MaxListEntries:   500,
		MaxReadBytes:     64 << 10,
		MaxGlobMatches:   200,
		MaxGrepMatches:   200,
		MaxGrepFileBytes: 256 << 10,
		MaxStatusBytes:   64 << 10,
		MaxDiffBytes:     128 << 10,
	}
}

// normalize fills every non-positive or over-ceiling field with a safe
// value. It never returns a Limits whose values exceed this package's own
// hard*  constants, regardless of what a consumer configured.
func (l Limits) normalize() Limits {
	d := DefaultLimits()
	l.MaxListEntries = clampLimit(l.MaxListEntries, d.MaxListEntries, hardMaxListEntries)
	l.MaxReadBytes = clampLimit(l.MaxReadBytes, d.MaxReadBytes, hardMaxReadBytes)
	l.MaxGlobMatches = clampLimit(l.MaxGlobMatches, d.MaxGlobMatches, hardMaxGlobMatches)
	l.MaxGrepMatches = clampLimit(l.MaxGrepMatches, d.MaxGrepMatches, hardMaxGrepMatches)
	l.MaxGrepFileBytes = clampLimit(l.MaxGrepFileBytes, d.MaxGrepFileBytes, hardMaxGrepFileBytes)
	l.MaxStatusBytes = clampLimit(l.MaxStatusBytes, d.MaxStatusBytes, hardMaxGitOutputBytes)
	l.MaxDiffBytes = clampLimit(l.MaxDiffBytes, d.MaxDiffBytes, hardMaxGitOutputBytes)
	return l
}

// filesystemToolNames/gitToolNames are the fixed, stable produced-tool-name
// sets for each definition returned by Definitions. tool.NewEvidenceDefinition
// derives its declared names from the ToolInfo slice passed at construction,
// but Build additionally requires the factory's OWN built tool names to match
// exactly (see pkg/tool's factoryDefinition.Build) — listing them once here
// keeps the two declarations (schema-bearing ToolInfo vs. constructed tool)
// from silently drifting apart.
func filesystemToolInfos() []tool.ToolInfo {
	return []tool.ToolInfo{
		{Name: toolNameFilesystemStat, Desc: filesystemStatDesc, Schema: []byte(pathStatSchema)},
		{Name: toolNameFilesystemList, Desc: filesystemListDesc, Schema: []byte(listDirectorySchema)},
		{Name: toolNameFilesystemRead, Desc: filesystemReadDesc, Schema: []byte(readFileSchema)},
		{Name: toolNameFilesystemGlob, Desc: filesystemGlobDesc, Schema: []byte(globFilesSchema)},
		{Name: toolNameFilesystemGrep, Desc: filesystemGrepDesc, Schema: []byte(grepFilesSchema)},
	}
}

// The Desc strings below MUST stay identical, word for word, to what each
// concrete tool's Info() returns — Harness's bind step (pkg/hustle
// bindEvidenceTools) rejects a definition whose declared ToolInfo differs
// from what the built tool reports at bind time. Centralizing them as
// constants used by both Info() and this file keeps that guarantee
// structural instead of a manually-maintained invariant.
const (
	filesystemStatDesc = "Report canonical path resolution, lstat-style metadata that does not follow a " +
		"trailing symlink, and resolved-target metadata for one path inside the review " +
		"workspace. Never reads file contents."
	filesystemListDesc = "List the immediate entries of one directory inside the review workspace, bounded by count. Not recursive."
	filesystemReadDesc = "Read a bounded byte range of one file inside the review workspace."
	filesystemGlobDesc = "Find file names inside the review workspace matching a shell-style glob, bounded " +
		"by match count and scan cost. Does not descend into symlinked directories and does " +
		"not follow a symlink whose target escapes the review workspace."
	filesystemGrepDesc = "Search file contents inside the review workspace for a literal substring or RE2 " +
		"regular expression, bounded by match count and scan cost. Skips files that look " +
		"binary and does not follow a symlink whose target escapes the review workspace."
	gitRepositoryStatusDesc = "Report the review workspace's Git repository root, worktree state (bare, detached " +
		"HEAD), and bounded working-tree status restricted to the workspace directory."
	gitDiffDesc = "Report a bounded, read-only Git diff restricted to the review workspace, with " +
		"external diff drivers and textconv filters disabled."
	gitRemotesDesc = "List the review workspace repository's configured Git remotes (fetch URLs only) " +
		"with a best-effort visibility hint. Never contacts a remote."
	gitBranchDesc = "Report the review workspace repository's current branch or detached-HEAD state, " +
		"upstream tracking branch, and best-effort default branch."
)

func gitToolInfos() []tool.ToolInfo {
	return []tool.ToolInfo{
		{Name: toolNameGitRepositoryStatus, Desc: gitRepositoryStatusDesc, Schema: []byte(gitRepositoryStatusSchema)},
		{Name: toolNameGitDiff, Desc: gitDiffDesc, Schema: []byte(gitDiffSchema)},
		{Name: toolNameGitRemotes, Desc: gitRemotesDesc, Schema: []byte(gitRemotesSchema)},
		{Name: toolNameGitBranch, Desc: gitBranchDesc, Schema: []byte(gitBranchSchema)},
	}
}

// requirementKinds is the single source of truth for every
// tool.Requirement.Kind value this catalog's evidence tools declare: the
// five filesystem kinds (path.go) and the one shared Git kind (git.go).
// Nothing else derives its own copy of this set — RequirementKinds (below)
// and, transitively, pkg/commandsafety.RequiredEvidenceKinds both return
// exactly this slice, so a future evidence tool that adds or removes a Kind
// constant here is reflected everywhere that reads it without a second,
// hand-maintained list to keep in sync.
var requirementKinds = []string{
	KindFilesystemStat,
	KindFilesystemList,
	KindFilesystemRead,
	KindFilesystemGlob,
	KindFilesystemGrep,
	KindGitRead,
}

// RequirementKinds returns a fresh copy of every tool.Requirement.Kind value
// this package's evidence tools declare. See requirementKinds for why this
// is the package's sole introspection point for that set.
func RequirementKinds() []string {
	out := make([]string, len(requirementKinds))
	copy(out, requirementKinds)
	return out
}

const filesystemDefinitionName = "command-safety-evidence-filesystem"
const gitDefinitionName = "command-safety-evidence-git"

// kindDeclaringDefinition adapts a tool.Definition to additionally implement
// Harness's optional tool.EvidenceKindDeclarer capability (pkg/tool's
// EvidenceKindDeclarer). Embedding tool.Definition promotes every existing
// method — including the interface's unexported sealing method — so this
// type still satisfies tool.Definition unchanged; it only adds the one new
// method Harness's construction-time evidence-policy validation
// (internal/hustleruntime's validateEvidenceAllowedKinds) probes for via
// type assertion, so that fail-fast check stops silently skipping this
// package's evidence-tool definitions.
type kindDeclaringDefinition struct {
	tool.Definition
}

// EvidenceRequirementKinds implements tool.EvidenceKindDeclarer by
// delegating to RequirementKinds, this package's sole source of truth for
// the Requirement.Kind values its evidence tools declare (see
// requirementKinds) — never a second, hand-maintained list.
func (kindDeclaringDefinition) EvidenceRequirementKinds() []string {
	return RequirementKinds()
}

// FilesystemDefinition returns the sealed evidence definition for the five
// filesystem tools (stat/list/read/glob/grep), bounded by limits.
func FilesystemDefinition(limits Limits) tool.Definition {
	limits = limits.normalize()
	return kindDeclaringDefinition{tool.NewEvidenceDefinition(
		filesystemDefinitionName,
		tool.RequiresWorkspaceRead,
		filesystemToolInfos(),
		func(_ context.Context, bindings tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			root, err := canonicalReadRoot(bindings)
			if err != nil {
				return nil, err
			}
			return []tool.InvokableTool{
				newPathStatTool(root),
				newListDirectoryTool(root, limits.MaxListEntries),
				newReadFileTool(root, limits.MaxReadBytes),
				newGlobFilesTool(root, limits.MaxGlobMatches),
				newGrepFilesTool(root, limits.MaxGrepMatches, limits.MaxGrepFileBytes),
			}, nil
		},
	)}
}

// GitDefinition returns the sealed evidence definition for the four Git
// tools (repository status, diff, remotes, branch), bounded by limits, with
// visibility optionally resolved through resolver (nil is a safe,
// network-free default — see VisibilityResolver).
func GitDefinition(limits Limits, resolver VisibilityResolver) tool.Definition {
	limits = limits.normalize()
	return kindDeclaringDefinition{tool.NewEvidenceDefinition(
		gitDefinitionName,
		tool.RequiresWorkspaceRead,
		gitToolInfos(),
		func(_ context.Context, bindings tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			root, err := canonicalReadRoot(bindings)
			if err != nil {
				return nil, err
			}
			binary := resolveGitBinary()
			return []tool.InvokableTool{
				newGitRepositoryStatusTool(root, binary, limits.MaxStatusBytes),
				newGitDiffTool(root, binary, limits.MaxDiffBytes),
				newGitRemotesTool(root, binary, resolver),
				newGitBranchTool(root, binary),
			}, nil
		},
	)}
}

// Definitions returns the complete command-safety evidence pack: the
// filesystem definition followed by the Git definition.
func Definitions(limits Limits, resolver VisibilityResolver) []tool.Definition {
	return []tool.Definition{FilesystemDefinition(limits), GitDefinition(limits, resolver)}
}

func canonicalReadRoot(bindings tool.EvidenceFactoryBindings) (string, error) {
	if bindings.ReadWorkspace == nil {
		return "", errWorkspaceUnavailable
	}
	root := bindings.ReadWorkspace.Root
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errWorkspaceUnavailable
	}
	return root, nil
}

// resolveGitBinary resolves git from the CALLER's real PATH exactly once,
// at concrete-tool construction time (not from any model-supplied value).
// The resolved absolute binary path and the real ambient PATH (needed so
// git can still resolve ordinary coreutils it may legitimately shell out
// to, e.g. core.editor) are frozen into every subsequent git subprocess's
// sanitized environment — PATH is not a secret, unlike HOME/credential
// state, which sanitizedGitEnv deliberately excludes. A binary that cannot
// be resolved is not a construction failure — every Git tool degrades to a
// clean "git is unavailable" result (see gitRun.run) rather than making
// evidence gathering as a whole unavailable.
func resolveGitBinary() *gitBinary {
	path, err := exec.LookPath("git")
	if err != nil {
		return &gitBinary{}
	}
	return newGitBinary(path, os.Getenv("PATH"))
}
