package app

import (
	"testing"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/loop"
)

func TestProductionRosterUsesThreeManagedPrimers(t *testing.T) {
	t.Parallel()
	defs := swarmDefs(t, Config{})
	want := []string{string(planner.Name), string(builder.Name), string(reviewer.Name)}
	if len(defs) != len(want) {
		t.Fatalf("definition count = %d, want %d", len(defs), len(want))
	}
	for i, def := range defs {
		if got := string(def.Name()); got != want[i] {
			t.Errorf("definition[%d] name = %q, want %q", i, got, want[i])
		}
		if got := def.Delegation().Style; got != loop.DelegationManaged {
			t.Errorf("definition[%d] delegation style = %q, want managed", i, got)
		}
		if got := len(def.Delegates()); got != len(want) {
			t.Errorf("definition[%d] delegate count = %d, want %d", i, got, len(want))
		}
	}
	if got := string(defs[1].Name()); got != string(builder.Name) {
		t.Fatalf("active primer definition = %q, want builder", got)
	}
}
