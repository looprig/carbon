package app

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// TestCarbonAskUserIsReplaySafe pins the half of host v0.9.0's gate failover
// Carbon's roster supplies: an ask_user gate survives a restore only for a tool
// declaring tool.UserInputReplaySafe (tools >= v0.13.0's AskUser does). A
// roster that wrapped AskUser, or a tools pin below v0.13.0, would silently
// fall back to closing the gate at restore and dropping the user's answer.
func TestCarbonAskUserIsReplaySafe(t *testing.T) {
	t.Parallel()
	set := mustExecutorSet(t, t.TempDir())
	definition := findDefinitionByName(t, carbonToolDefinitions(set, nil, nil), "AskUser")
	built, err := definition.Build(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatalf("build AskUser: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("AskUser built %d tools, want 1", len(built))
	}
	safe, ok := built[0].(tool.UserInputReplaySafe)
	if !ok || !safe.UserInputReplaySafe() {
		t.Fatalf("Carbon's AskUser (%T) does not report UserInputReplaySafe; an ask_user gate would be closed at restore and its answer dropped", built[0])
	}
}
