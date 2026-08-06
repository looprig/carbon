package app

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// TestBuilderToolDefinitionsEnableHostWrites proves builderToolDefinitions
// wires WithHostWrites() into WriteFile/EditFile: their advertised
// descriptions no longer claim workspace-only confinement, and instead
// document that an absolute path may resolve outside the workspace and that
// such writes are NOT covered by session checkpoint/undo. Mirrors
// TestBuilderToolDefinitionsEnableHostReads on the read side.
func TestBuilderToolDefinitionsEnableHostWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	defs := builderToolDefinitions(set, nil, nil)

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

// TestPlannerAndReviewerToolDefinitionsHaveNoMutationTools documents the
// invariant that the planner and reviewer rosters carry no file-mutation
// tools at all -- WriteFile/EditFile (and therefore any WithHostWrites()
// wiring) is a builder-only concern. Mirrors
// TestPlannerAndReviewerToolDefinitionsEnableHostReads's per-role structure.
func TestPlannerAndReviewerToolDefinitionsHaveNoMutationTools(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	plannerSet := mustExecutorSet(t, root)
	plannerDefs := plannerToolDefinitions(plannerSet, nil, nil)
	assertNoMutationDefinitions(t, "planner", plannerDefs)

	reviewerSet := mustExecutorSet(t, root)
	reviewerDefs := reviewerToolDefinitions(reviewerSet, nil)
	assertNoMutationDefinitions(t, "reviewer", reviewerDefs)
}

// assertNoMutationDefinitions fails if defs contains a WriteFile or EditFile
// definition.
func assertNoMutationDefinitions(t *testing.T, role string, defs []tool.Definition) {
	t.Helper()
	for _, def := range defs {
		if def.Name() == "WriteFile" || def.Name() == "EditFile" {
			t.Errorf("%s roster contains mutation tool %q, want none (planner/reviewer must stay read-only)", role, def.Name())
		}
	}
}
