package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
)

func TestProductionAssemblyUsesOneGenericManagedPrimer(t *testing.T) {
	t.Parallel()
	definition := genericDef(t, Config{})
	if got := definition.Name(); got != generic.Name {
		t.Fatalf("definition name = %q, want %q", got, generic.Name)
	}
	if got := definition.Delegation().Style; got != loop.DelegationManaged {
		t.Fatalf("delegation style = %q, want managed", got)
	}
	if got := definition.Delegates(); len(got) != 1 || got[0] != generic.Name {
		t.Fatalf("delegates = %v, want [%q]", got, generic.Name)
	}
}

func TestProductionSourceUsesOnlyGenericAgentTopology(t *testing.T) {
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
	if len(catalogDirs) != 1 || catalogDirs[0] != "generic" {
		t.Fatalf("catalog directories = %v, want only [generic]", catalogDirs)
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
	if !strings.Contains(text, `internal/catalog/generic`) {
		t.Fatal("production does not import the Generic catalog")
	}
	if !strings.Contains(text, `AgentName("generic")`) {
		t.Fatal("production does not define the generic catalog identity")
	}
	if !strings.Contains(text, `coderig:generic`) {
		t.Fatal("production does not stamp coderig:generic")
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
		"coderig:planner",
		"coderig:builder",
		"coderig:reviewer",
		"coderig:operator",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("production contains removed agent/topology symbol %q", forbidden)
		}
	}
}
