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
// for tests that only need SOME executor set to build Generic's tool
// definitions -- the actual access decision is proven separately by
// TestAcceptanceProfileGateBehavior; this only proves the built ReadFile tool
// carries WithHostReads().
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

// TestGenericToolDefinitionsEnableHostReads proves genericToolDefinitions
// wires WithHostReads() into ReadFile: its advertised description no longer
// claims workspace-only confinement, matching
// coderigReadGuard's doc comment ("sandbox profile access is the
// read-authority source of truth").
func TestGenericToolDefinitionsEnableHostReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	set := mustExecutorSet(t, root)
	defs := genericToolDefinitions(set, nil, nil)

	for _, tc := range []struct {
		name           string
		confinedPhrase string
	}{
		{"ReadFile", "Reads are confined to the workspace"},
	} {
		desc := definitionDescByName(t, root, defs, tc.name)
		if strings.Contains(desc, tc.confinedPhrase) {
			t.Errorf("%s.Info().Desc = %q, still advertises workspace-only confinement (WithHostReads() not wired?)", tc.name, desc)
		}
	}
}

// TestGenericToolDefinitionsExposeCompleteCodingRoster proves the one Generic
// capability set includes repository reads/search, mutation, supervised command
// and process tools, web access, interaction, and the optional Skill seam.
func TestGenericToolDefinitionsExposeCompleteCodingRoster(t *testing.T) {
	t.Parallel()
	defs := genericToolDefinitions(mustExecutorSet(t, t.TempDir()), nil, tool.NewDefinition("Skill", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) { return nil, nil }))
	want := []string{"ReadFile", "WriteFile", "EditFile", "Bash", "ProcessOutput", "ProcessInput", "ProcessStop", "WebSearch", "Fetch", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "AskUser", "Skill"}
	got := make(map[string]bool)
	for _, def := range defs {
		for _, name := range def.ProducedToolNames() {
			got[name] = true
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("Generic roster missing %q; got %v", name, got)
		}
	}
	for _, name := range []string{"Glob", "Grep"} {
		if got[name] {
			t.Errorf("Generic roster unexpectedly includes %q; got %v", name, got)
		}
	}
}

// noopCoordinator is a minimal tool.WorkspaceCoordinator fixture: definition
// Build only needs SOME non-nil coordinator to satisfy WriteFile/EditFile's
// binding; ReadFile never calls it.
type noopCoordinator struct{}

func (noopCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return noopPermit{}, nil
}
func (noopCoordinator) Healthy() error { return nil }

type noopPermit struct{}

func (noopPermit) Release() {}
