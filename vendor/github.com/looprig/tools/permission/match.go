package permission

import (
	"strings"

	"github.com/looprig/harness/pkg/tool"
)

// match.go answers whether one durable rule matches one prepared
// requirement. The store consults it identically for deny and allow rules;
// deny-before-allow ordering belongs to the gate evaluator.
//
// A rule matches only within its own capability and enforcement class. The
// requirement side of each class contract is the canonical Match encoding
// documented in rule.go; preparation builds those strings.

// matchesRequirement reports whether rule matches the prepared requirement.
// Anything unparseable or foreign matches nothing: an incompatible record
// can tighten nothing and widen nothing.
func matchesRequirement(rule Rule, requirement tool.Requirement) bool {
	if rule.Capability != requirement.Kind {
		return false
	}
	switch rule.Class {
	case ClassCommandInvoke:
		return requirement.Match == rule.Command
	case ClassCommandInvokeWildcard:
		// Bash(*) satisfies only the command-execution decision. The issued
		// grant remains the exact-command command.start.v1.
		return true
	case ClassCommandInvokeFamily:
		// Token-aware shell-segment family matching: every segment of the
		// normalized command must lead with exactly the family tokens. The
		// store additionally combines rules per segment (bashrule.go) so a
		// compound command may be covered by several allow rules.
		return familyMatchesCommand(rule, requirement.Match)
	case ClassNetworkTarget:
		return matchesNetworkTarget(rule, requirement)
	case ClassNetworkBroad:
		return matchesCommandBound(rule, requirement, rule.Class)
	case ClassFilesystemPathRead, ClassFilesystemPathWrite:
		return isCanonicalPathRequirement(requirement) && requirement.Match == rule.Path
	case ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		return matchesTree(rule, requirement)
	case ClassFilesystemHostRead, ClassFilesystemHostWrite:
		return matchesCommandBound(rule, requirement, rule.Class)
	}
	return false
}

// matchesNetworkTarget requires every constraint present in the rule. A
// command-bound broad requirement never matches a target rule and a target
// rule never satisfies a broad requirement.
func matchesNetworkTarget(rule Rule, requirement tool.Requirement) bool {
	if requirement.GrantClass != "" && requirement.GrantClass != GrantClassNetworkProxyTarget {
		return false
	}
	transport, host, port, ok := parseNetworkTargetMatch(requirement.Match)
	if !ok {
		return false
	}
	if rule.Host != host {
		return false
	}
	if rule.Transport != "" && rule.Transport != transport {
		return false
	}
	if rule.Port != 0 && rule.Port != port {
		return false
	}
	return true
}

// matchesCommandBound implements the exact-command-bound broad classes:
// the requirement must carry the identical enforcement class and the rule's
// exact bound command; a broad rule can never satisfy a direct or
// target-scoped request.
func matchesCommandBound(rule Rule, requirement tool.Requirement, class string) bool {
	if requirement.GrantClass != class {
		return false
	}
	bound, ok := parseCommandBoundMatch(requirement.Match)
	if !ok || bound.Command != rule.Command {
		return false
	}
	if rule.Class == ClassNetworkBroad {
		return bound.Target == rule.Target && requirement.GrantTarget == rule.Target
	}
	return true
}

// matchesTree covers a canonical path at or below the root and the tree
// requirement for the identical root.
func matchesTree(rule Rule, requirement tool.Requirement) bool {
	if requirement.GrantClass != "" && !strings.HasPrefix(requirement.GrantClass, "filesystem.path.") && !strings.HasPrefix(requirement.GrantClass, "filesystem.tree.") {
		return false
	}
	match := requirement.Match
	if root, isTree := strings.CutPrefix(match, "tree:"); isTree {
		return validCanonicalPath(root) && pathWithinRoot(root, rule.Root)
	}
	return validCanonicalPath(match) && pathWithinRoot(match, rule.Root)
}

// isCanonicalPathRequirement guards the exact-path classes against
// command-bound or tree-scoped requirement encodings.
func isCanonicalPathRequirement(requirement tool.Requirement) bool {
	if requirement.GrantClass != "" && !strings.HasPrefix(requirement.GrantClass, "filesystem.path.") {
		return false
	}
	return validCanonicalPath(requirement.Match)
}

// pathWithinRoot reports whether canonical path sits at or below canonical
// root. Both are validated canonical absolute paths, so a byte comparison
// with a separator boundary is exact.
func pathWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	if root == "/" {
		return true
	}
	return strings.HasPrefix(path, root+"/")
}
