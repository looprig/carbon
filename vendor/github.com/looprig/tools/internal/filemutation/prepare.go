// Package filemutation implements the shared mechanics of the two direct
// mutation tools, WriteFile and EditFile: single-step preparation and
// canonicalization, workspace containment, atomic publication, optimistic
// file-freshness concurrency, and permit-scoped cross-loop serialization. The
// public writefile and editfile packages are thin facades over this package.
package filemutation

// prepare.go holds the preparation seam shared by WriteFile and EditFile (the
// two direct mutation tools). Preparation is the SINGLE argument-parsing and
// canonicalization step: PrepareCall decodes the untrusted argsJSON once,
// proves workspace containment once, and binds the validated result into a
// typed artifact the tool executes WITHOUT ever reparsing the raw JSON. The
// same preparation resolves the canonical write scheduling key, so the
// runner's WriteTarget grouping and the requirement Scope can never diverge.
//
// The emitted requirement is a DIRECT filesystem.write: Scope and Match are
// both the canonical resolved target path and the grant pair is EMPTY — the
// tool enforces the approved resolved path itself at run time by re-resolving
// the path and refusing to proceed when the resolution no longer equals the
// approved canonical target (a parent-directory symlink swap between prepare
// and run redirects the write nowhere).

import (
	"path/filepath"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/permission"
)

// mutationTarget is the validated, canonicalized target every prepared
// mutation artifact carries. abs is the canonical symlink-resolved path (the
// requirement Scope/Match, the scheduling key, and the observation key);
// contained reports whether abs lives inside the workspace root (always true
// unless the tool was constructed WithHostWrites() and the input was
// absolute); lexical is the on-disk name the atomic write targets (never
// following a final-component symlink); display is the model-supplied path
// used only in messages.
type mutationTarget struct {
	abs       string
	contained bool
	lexical   string
	display   string
}

// resolveMutationTarget is the SINGLE resolution step both PrepareCall (via
// prepareWrite/prepareEdit) and InvokableRun's stage-1 recheck
// (enforceApprovedResolution) use, so the two can never drift on how a path
// resolves between approval and execution. Without hostWrites, it is exactly
// workspace.ContainedPath -- any non-nil error means "outside the workspace",
// matching the historical error message, and contained is always true. With
// hostWrites, it is workspace.ResolvedPath -- a non-nil error here means
// resolution itself failed (e.g. an unresolvable symlink chain), never merely
// "outside the workspace", since an uncontained-but-resolved target is not an
// error at this layer; the filesystem.write gate decides it. display must be
// non-empty (callers validate).
func resolveMutationTarget(root, display string, hostWrites bool) (mutationTarget, error) {
	if !hostWrites {
		abs, err := workspace.ContainedPath(root, display)
		if err != nil {
			return mutationTarget{}, &writeFileError{reason: "path is outside the workspace", cause: err}
		}
		return mutationTarget{abs: abs, contained: true, lexical: workspace.JoinedPath(root, display), display: display}, nil
	}
	abs, contained, err := workspace.ResolvedPath(root, display)
	if err != nil {
		// A RELATIVE input's error always comes from the exact same
		// workspace.ContainedPath call the non-widened branch above makes
		// (workspace.ResolvedPath forwards it verbatim for relative input) --
		// keep its historical message. Only an ABSOLUTE input can fail for the
		// NEW reason (its existing-prefix chain itself could not be resolved),
		// which is a materially different cause and gets its own message.
		reason := "path is outside the workspace"
		if filepath.IsAbs(display) {
			reason = "path could not be resolved"
		}
		return mutationTarget{}, &writeFileError{reason: reason, cause: err}
	}
	if filepath.IsAbs(display) {
		// display is already the correct on-disk name; workspace.JoinedPath
		// would wrongly re-anchor an absolute path underneath root.
		return mutationTarget{abs: abs, contained: contained, lexical: filepath.Clean(display), display: display}, nil
	}
	return mutationTarget{abs: abs, contained: contained, lexical: workspace.JoinedPath(root, display), display: display}, nil
}

// writeRequirement builds the single direct filesystem.write requirement for
// one prepared mutation target. A CONTAINED target keeps the historical shape
// unchanged: Description is "write "+abs and Candidates carries the one
// reusable exact-path allow candidate. An UNCONTAINED (host) target NEVER
// carries a candidate -- see the design note on WithHostWrites(): under a
// Gated decision the approval UI can persist an attached candidate as a
// standing allow rule, and the store's rule validator does not re-check
// workspace containment, so a persisted host-write candidate would silently
// authorize every future write to that host path. Every host write must get
// its own fresh gate decision, every time. Its Description is instead
// create-vs-overwrite aware (via the same classification commit itself uses),
// so the approval prompt tells the human which of those it is; the
// classification here is DISPLAY-only, commit re-classifies for real.
func writeRequirement(target mutationTarget) tool.Requirement {
	if target.contained {
		return tool.Requirement{
			Kind:        permission.CapabilityFilesystemWrite,
			Scope:       target.abs,
			Match:       target.abs,
			Description: "write " + target.abs,
			Candidates: []tool.RuleCandidate{{
				Kind:        permission.CapabilityFilesystemWrite,
				Match:       target.abs,
				Description: "write " + target.abs,
			}},
		}
	}
	desc := "overwrite existing file outside the workspace: " + target.abs
	if classifyWriteTarget(target.lexical) == writeTargetAbsent {
		desc = "create new file outside the workspace: " + target.abs
	}
	return tool.Requirement{
		Kind:        permission.CapabilityFilesystemWrite,
		Scope:       target.abs,
		Match:       target.abs,
		Description: desc,
	}
}

// mutationRequest assembles the prepared request for one direct mutation
// call: the write requirement for target, plus any extra requirements the
// caller supplies. extra is variadic so it is backward compatible -- every
// existing WriteFile call site (which never passes extra) is unaffected.
// EditFile uses it to append a paired filesystem.read requirement for an
// UNCONTAINED target (see pairedReadRequirement's doc comment).
func mutationRequest(toolName, executionID string, target mutationTarget, extra ...tool.Requirement) tool.Request {
	reqs := append([]tool.Requirement{writeRequirement(target)}, extra...)
	return tool.Request{
		ToolName:     toolName,
		ExecutionID:  executionID,
		Requirements: reqs,
	}
}

// pairedReadRequirement builds the paired direct filesystem.read requirement
// EditFile emits alongside its filesystem.write requirement for an
// UNCONTAINED target only (EditFile always performs an in-process read via
// readForEdit before writing). Candidates is nil for the same reason an
// uncontained write requirement's Candidates is nil (see writeRequirement):
// a persisted "approve always" read rule for a host path would silently
// authorize every future read there with no further prompt.
func pairedReadRequirement(abs string) tool.Requirement {
	return tool.Requirement{
		Kind:        permission.CapabilityFilesystemRead,
		Scope:       abs,
		Match:       abs,
		Description: "read " + abs,
		Candidates:  nil,
	}
}

// enforceApprovedResolution re-proves at RUN time that the display path still
// resolves to the APPROVED canonical target, using the SAME resolver split as
// resolveMutationTarget (hostWrites must match how target was prepared, or the
// re-derivation would compare apples to oranges). Preparation resolved once
// for the permission decision; a parent-directory swap between prepare and run
// would silently redirect the lexical write, so a changed resolution refuses
// the mutation (fail closed) rather than writing anywhere else.
func enforceApprovedResolution(root string, target mutationTarget, hostWrites bool) error {
	redone, err := resolveMutationTarget(root, target.display, hostWrites)
	if err != nil {
		return err
	}
	if redone.abs != target.abs {
		return &writeFileError{reason: "path resolution changed since approval"}
	}
	return nil
}
