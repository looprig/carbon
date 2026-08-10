package app

import (
	"os"
	"strings"
	"testing"
)

// TestPersistenceAssemblyUsesTheSoleCarbonDefinition documents the narrow
// composition boundary: Carbon assembles one Carbon definition directly,
// with Carbon as both the only primer and active primer, and consumes the
// configured runtime catalogue without compatibility fallbacks.
func TestPersistenceAssemblyUsesTheSoleCarbonDefinition(t *testing.T) {
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
		"rig.WithPrimers(string(carbon.Name))",
		"rig.WithActivePrimer(string(carbon.Name))",
		"cfg.RuntimeCatalog.HasEntries()",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("persistence assembly omitted direct Carbon contract %q", required)
		}
	}
}
