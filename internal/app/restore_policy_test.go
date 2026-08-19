package app

import "testing"

func TestCarbonRestoreFailurePolicyUsesDeclarativeRigOptions(t *testing.T) {
	t.Parallel()
	if got, want := len(carbonRestoreFailureOptions()), 9; got != want {
		t.Fatalf("restore failure options = %d, want %d explicit allowances", got, want)
	}
}
