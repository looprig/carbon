package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/harness/pkg/loop"
)

func TestProductionAssemblyUsesOneCarbonManagedPrimer(t *testing.T) {
	t.Parallel()
	definition := carbonDef(t, Config{})
	if got := definition.Name(); got != carbon.Name {
		t.Fatalf("definition name = %q, want %q", got, carbon.Name)
	}
	if got := definition.Delegation().Style; got != loop.DelegationManaged {
		t.Fatalf("delegation style = %q, want managed", got)
	}
	if got := definition.Delegates(); len(got) != 1 || got[0] != carbon.Name {
		t.Fatalf("delegates = %v, want [%q]", got, carbon.Name)
	}
}

func TestProductionSourceUsesOnlyCarbonAgentTopology(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))

	entries, err := os.ReadDir(filepath.Join(repoRoot, "internal", "catalog"))
	if err != nil {
		t.Fatalf("read catalog directory: %v", err)
	}
	var catalogDirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			catalogDirs = append(catalogDirs, entry.Name())
		}
	}
	if len(catalogDirs) != 1 || catalogDirs[0] != "carbon" {
		t.Fatalf("catalog directories = %v, want only [carbon]", catalogDirs)
	}

	var source strings.Builder
	err = filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		source.Write(contents)
		return nil
	})
	if err != nil {
		t.Fatalf("read production Go sources: %v", err)
	}

	text := source.String()
	if !strings.Contains(text, `internal/catalog/carbon`) {
		t.Fatal("production does not import the Carbon catalog")
	}
	if !strings.Contains(text, `AgentName("carbon")`) {
		t.Fatal("production does not define the Carbon catalog identity")
	}
	if !strings.Contains(text, `carbon:carbon`) {
		t.Fatal("production does not stamp carbon:carbon")
	}

	// These are source-contract sentinels, not runtime compatibility fixtures:
	// production must not retain the removed topology or durable identities.
	for _, forbidden := range []string{
		"internal/catalog/planner",
		"internal/catalog/builder",
		"internal/catalog/reviewer",
		"internal/catalog/operator",
		"leafBuiltin",
		"swarmDefinitions",
		"swarmStores",
		"delegate_defaults",
		"carbon:planner",
		"carbon:builder",
		"carbon:reviewer",
		"carbon:operator",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("production contains removed agent/topology symbol %q", forbidden)
		}
	}
}
