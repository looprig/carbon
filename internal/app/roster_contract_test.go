package app

import (
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
)

func TestProductionAssemblyUsesOneGenericManagedPrimer(t *testing.T) {
	t.Parallel()
	defs := genericDefs(t, Config{})
	if len(defs) != 1 {
		t.Fatalf("definition count = %d, want 1", len(defs))
	}
	if got := defs[0].Name(); got != generic.Name {
		t.Fatalf("definition name = %q, want %q", got, generic.Name)
	}
	if got := defs[0].Delegation().Style; got != loop.DelegationManaged {
		t.Fatalf("delegation style = %q, want managed", got)
	}
	if got := defs[0].Delegates(); len(got) != 1 || got[0] != generic.Name {
		t.Fatalf("delegates = %v, want [%q]", got, generic.Name)
	}
}
