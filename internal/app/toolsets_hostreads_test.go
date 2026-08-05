package app

import (
	"context"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
)

// mustExecutorSet builds a minimal, real ExecutorSet under a trusted profile
// for tests that only need SOME executor set to build a role's tool
// definitions -- the actual access decision is proven separately by
// TestAcceptanceProfileGateBehavior; this only proves the built ReadFile/
// Glob/Grep tools carry WithHostReads().
func mustExecutorSet(t *testing.T, root string) *sandbox.ExecutorSet {
	t.Helper()
	profile, err := coderigProfile(AccessTrusted, root)
	if err != nil {
		t.Fatalf("coderigProfile: %v", err)
	}
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(1))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// definitionDescByName builds every definition and returns the Info().Desc of
// the one whose Name() matches.
func definitionDescByName(t *testing.T, root string, defs []tool.Definition, name string) string {
	t.Helper()
	for _, def := range defs {
		if def.Name() != name {
			continue
		}
		sessionID, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New() (session): %v", err)
		}
		loopID, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New() (loop): %v", err)
		}
		built, err := def.Build(context.Background(), tool.Bindings{
			SessionID: sessionID,
			LoopID:    loopID,
			Workspace: &tool.WorkspaceBinding{
				Root:         root,
				Observations: tool.NewWorkspaceObservations(),
				Coordinator:  noopCoordinator{},
			},
		})
		if err != nil {
			t.Fatalf("Build(%q): %v", name, err)
		}
		if len(built) != 1 {
			t.Fatalf("Build(%q) returned %d tools, want 1", name, len(built))
		}
		info, err := built[0].Info(context.Background())
		if err != nil {
			t.Fatalf("%s.Info(): %v", name, err)
		}
		return info.Desc
	}
	t.Fatalf("no definition named %q", name)
	return ""
}

// TestBuilderToolDefinitionsEnableHostReads proves builderToolDefinitions
// wires WithHostReads() into ReadFile/Glob/Grep: their advertised
// descriptions no longer claim workspace-only confinement, matching
// coderigReadGuard's doc comment ("sandbox profile access is the
// read-authority source of truth").
func TestBuilderToolDefinitionsEnableHostReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	defs := builderToolDefinitions(set, nil, nil)

	for _, tc := range []struct {
		name           string
		confinedPhrase string
	}{
		{"ReadFile", "Reads are confined to the workspace"},
		{"Glob", "Results are confined to the workspace"},
		{"Grep", "Confined to the workspace"},
	} {
		desc := definitionDescByName(t, root, defs, tc.name)
		if strings.Contains(desc, tc.confinedPhrase) {
			t.Errorf("%s.Info().Desc = %q, still advertises workspace-only confinement (WithHostReads() not wired?)", tc.name, desc)
		}
	}
}

// TestPlannerAndReviewerToolDefinitionsEnableHostReads mirrors
// TestBuilderToolDefinitionsEnableHostReads for the planner and reviewer
// rosters, which build their ReadFile/Glob/Grep definitions independently.
func TestPlannerAndReviewerToolDefinitionsEnableHostReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	plannerSet := mustExecutorSet(t, root)
	plannerDefs := plannerToolDefinitions(plannerSet, nil, nil)
	if desc := definitionDescByName(t, root, plannerDefs, "ReadFile"); strings.Contains(desc, "Reads are confined to the workspace") {
		t.Errorf("planner ReadFile.Info().Desc = %q, still advertises workspace-only confinement", desc)
	}

	reviewerSet := mustExecutorSet(t, root)
	reviewerDefs := reviewerToolDefinitions(reviewerSet, nil)
	if desc := definitionDescByName(t, root, reviewerDefs, "ReadFile"); strings.Contains(desc, "Reads are confined to the workspace") {
		t.Errorf("reviewer ReadFile.Info().Desc = %q, still advertises workspace-only confinement", desc)
	}
}

// noopCoordinator is a minimal tool.WorkspaceCoordinator fixture: definition
// Build only needs SOME non-nil coordinator to satisfy WriteFile/EditFile's
// binding; ReadFile/Glob/Grep never call it.
type noopCoordinator struct{}

func (noopCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return noopPermit{}, nil
}
func (noopCoordinator) Healthy() error { return nil }

type noopPermit struct{}

func (noopPermit) Release() {}
