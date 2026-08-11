package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/foreignloops/driver"
)

var errPermissionDenied = errors.New("permission request denied")

// permissionHandler answers ACP permission requests from the configured
// neutral posture. It never waits for input or inspects display text and raw
// tool input.
type permissionHandler struct {
	posture       driver.Posture
	workspaceRoot string
}

func newPermissionHandler(posture driver.Posture, workspaceRoot string) *permissionHandler {
	return &permissionHandler{posture: posture, workspaceRoot: workspaceRoot}
}

// RequestPermission implements the non-interactive ACP permission boundary.
// A request that cannot be classified, or cannot be answered with an offered
// safe option, is rejected without exposing request data to the agent.
func (h *permissionHandler) RequestPermission(_ context.Context, req protocol.RequestPermissionRequest) (protocol.RequestPermissionResponse, error) {
	if h == nil {
		return protocol.RequestPermissionResponse{}, errPermissionDenied
	}

	allow := h.allows(req.ToolCall)
	optionID, ok := selectPermissionOption(req.Options, allow)
	if !ok {
		return protocol.RequestPermissionResponse{}, errPermissionDenied
	}
	return protocol.RequestPermissionResponse{
		Outcome: protocol.RequestPermissionOutcome{
			Selected: &protocol.SelectedPermissionOutcome{OptionID: optionID},
		},
	}, nil
}

func selectPermissionOption(options []protocol.PermissionOption, allow bool) (protocol.PermissionOptionID, bool) {
	if allow {
		for _, option := range options {
			if option.Kind == protocol.PermissionOptionKindAllowOnce && option.OptionID != "" {
				return option.OptionID, true
			}
		}
	}

	for _, kind := range []protocol.PermissionOptionKind{
		protocol.PermissionOptionKindRejectOnce,
		protocol.PermissionOptionKindRejectAlways,
	} {
		for _, option := range options {
			if option.Kind == kind && option.OptionID != "" {
				return option.OptionID, true
			}
		}
	}
	return "", false
}

func (h *permissionHandler) allows(tool protocol.ToolCallUpdate) bool {
	if h == nil || tool.Kind == nil || hasConflictingContent(tool) {
		return false
	}

	switch h.posture {
	case driver.PostureReadOnly:
		return readOnlyToolAllowed(*tool.Kind)
	case driver.PostureWorkspaceWrite:
		return workspaceToolAllowed(*tool.Kind) && h.allPathsWithinWorkspace(tool)
	default:
		return false
	}
}

func readOnlyToolAllowed(kind protocol.ToolKind) bool {
	switch kind {
	case protocol.ToolKindRead, protocol.ToolKindSearch:
		return true
	default:
		return false
	}
}

func workspaceToolAllowed(kind protocol.ToolKind) bool {
	switch kind {
	case protocol.ToolKindRead,
		protocol.ToolKindSearch,
		protocol.ToolKindEdit,
		protocol.ToolKindDelete,
		protocol.ToolKindMove,
		protocol.ToolKindExecute:
		return true
	default:
		return false
	}
}

func hasConflictingContent(tool protocol.ToolCallUpdate) bool {
	var hasDiff, hasTerminal bool
	for _, content := range tool.Content {
		hasDiff = hasDiff || content.Diff != nil
		hasTerminal = hasTerminal || content.Terminal != nil
	}
	if hasDiff && hasTerminal {
		return true
	}
	if hasDiff && tool.Kind != nil && !isMutationKind(*tool.Kind) {
		return true
	}
	if hasTerminal && tool.Kind != nil && *tool.Kind != protocol.ToolKindExecute {
		return true
	}
	return false
}

func isMutationKind(kind protocol.ToolKind) bool {
	switch kind {
	case protocol.ToolKindEdit, protocol.ToolKindDelete, protocol.ToolKindMove:
		return true
	default:
		return false
	}
}

func (h *permissionHandler) allPathsWithinWorkspace(tool protocol.ToolCallUpdate) bool {
	paths := make([]string, 0, len(tool.Locations)+len(tool.Content))
	for _, location := range tool.Locations {
		paths = append(paths, location.Path)
	}
	for _, content := range tool.Content {
		if content.Diff != nil {
			paths = append(paths, content.Diff.Path)
		}
	}
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if !pathWithinWorkspace(h.workspaceRoot, path) {
			return false
		}
	}
	return true
}

func pathWithinWorkspace(root, path string) bool {
	if root == "" || path == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(root) != root || filepath.Clean(path) != path {
		return false
	}
	canonicalRoot, ok := canonicalPathWithExistingParent(root)
	if !ok {
		return false
	}
	canonicalPath, ok := canonicalPathWithExistingParent(path)
	if !ok {
		return false
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	return true
}

// canonicalPathWithExistingParent resolves every existing component of path.
// The suffix after the deepest existing parent is retained only when that
// parent resolves to a directory; any filesystem ambiguity rejects the path.
func canonicalPathWithExistingParent(path string) (string, bool) {
	current := path
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", false
			}
			if len(missing) > 0 {
				resolvedInfo, err := os.Stat(current)
				if err != nil || !resolvedInfo.IsDir() {
					return "", false
				}
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), true
		}
		if !os.IsNotExist(err) {
			return "", false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

var _ client.PermissionHandler = (*permissionHandler)(nil)
