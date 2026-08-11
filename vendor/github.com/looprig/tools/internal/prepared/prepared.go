// Package prepared holds the small helpers the direct file/context tools share
// at the preparation boundary: reading a call's typed prepared artifact back
// from the context and building the direct filesystem requirements (empty
// grant pair — the tool enforces the approved resolved resource itself).
package prepared

import (
	"context"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

// FromContext returns the typed prepared artifact bound to this call, or
// (zero, false) when the call runs without one — the fail-closed path for
// every prepared tool.
func FromContext[T tool.PreparedArtifact](ctx context.Context) (T, bool) {
	var zero T
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return zero, false
	}
	artifact, ok := call.Artifact.(T)
	return artifact, ok
}

// PathReadRequirement builds the direct filesystem.read requirement for ONE
// canonical resolved path (Scope and Match are both the canonical path), with
// the matching reusable exact-path allow candidate.
func PathReadRequirement(abs string) tool.Requirement {
	return tool.Requirement{
		Kind:        permission.CapabilityFilesystemRead,
		Scope:       abs,
		Match:       abs,
		Description: "read " + abs,
		Candidates: []tool.RuleCandidate{{
			Kind:        permission.CapabilityFilesystemRead,
			Match:       abs,
			Description: "read " + abs,
		}},
	}
}

// TreeReadRequirement builds the direct filesystem.read requirement for a tool
// that walks the WHOLE canonical root (Glob/Grep): the Scope stays the plain
// canonical root path — the profile resolves access AT that path, and the walk
// never leaves the workspace — while the Match uses the canonical tree
// encoding so durable tree rules cover it. The candidate offers the same tree
// rule for reuse.
func TreeReadRequirement(rootAbs string) tool.Requirement {
	match := permission.TreeMatch(rootAbs)
	return tool.Requirement{
		Kind:        permission.CapabilityFilesystemRead,
		Scope:       rootAbs,
		Match:       match,
		Description: "read files under " + rootAbs,
		Candidates: []tool.RuleCandidate{{
			Kind:        permission.CapabilityFilesystemRead,
			Match:       match,
			Description: "read files under " + rootAbs,
		}},
	}
}
