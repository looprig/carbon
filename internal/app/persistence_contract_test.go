package app

import (
	"os"
	"strings"
	"testing"
)

// TestPersistenceAssemblyUsesTheSoleGenericDefinition documents the narrow
// composition boundary: CodeRig assembles one Generic definition directly,
// with Generic as both the only primer and active primer, and consumes the
// configured runtime catalogue without compatibility fallbacks.
func TestPersistenceAssemblyUsesTheSoleGenericDefinition(t *testing.T) {
	source, err := os.ReadFile("persistence.go")
	if err != nil {
		t.Fatalf("read persistence.go: %v", err)
	}
	text := string(source)
	for _, forbidden := range []string{
		"primer" + "Configuration",
		"effective" + "RuntimeCatalog",
		"[]loop." + "Definition",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("persistence assembly still contains arbitrary-topology abstraction %q", forbidden)
		}
	}
	for _, required := range []string{
		"WithLoops(definition)",
		"rig.WithPrimers(string(generic.Name))",
		"rig.WithActivePrimer(string(generic.Name))",
		"cfg.RuntimeCatalog.HasEntries()",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("persistence assembly omitted direct Generic contract %q", required)
		}
	}
}
