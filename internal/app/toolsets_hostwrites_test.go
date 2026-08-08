package app

import (
	"strings"
	"testing"
)

// TestGenericToolDefinitionsEnableHostWrites proves genericToolDefinitions
// wires WithHostWrites() into WriteFile/EditFile: their advertised
// descriptions no longer claim workspace-only confinement, and instead
// document that an absolute path may resolve outside the workspace and that
// such writes are NOT covered by session checkpoint/undo. Mirrors
// TestGenericToolDefinitionsEnableHostReads on the read side.
func TestGenericToolDefinitionsEnableHostWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	defs := genericToolDefinitions(set, nil, nil)

	for _, tc := range []struct {
		name           string
		confinedPhrase string
	}{
		{"WriteFile", "Writes are confined to the workspace"},
		{"EditFile", "Edits are confined to the workspace"},
	} {
		desc := definitionDescByName(t, root, defs, tc.name)
		if strings.Contains(desc, tc.confinedPhrase) {
			t.Errorf("%s.Info().Desc = %q, still advertises workspace-only confinement (WithHostWrites() not wired?)", tc.name, desc)
		}
		if !strings.Contains(desc, "outside the workspace") {
			t.Errorf("%s.Info().Desc = %q, does not mention resolving outside the workspace (WithHostWrites() not wired?)", tc.name, desc)
		}
		if !strings.Contains(desc, "NOT covered by session checkpoint/undo") {
			t.Errorf("%s.Info().Desc = %q, does not warn that host writes are not covered by session checkpoint/undo (WithHostWrites() not wired?)", tc.name, desc)
		}
	}
}
