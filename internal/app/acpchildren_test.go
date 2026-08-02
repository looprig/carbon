package app

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

func TestACPPostureForRole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		role string
		want string
	}{
		{role: "planner", want: "read-only"},
		{role: "reviewer", want: "read-only"},
		{role: "builder", want: "workspace-write"},
	} {
		t.Run(test.role, func(t *testing.T) {
			got, err := acpPostureFor(test.role)
			if err != nil || string(got) != test.want {
				t.Fatalf("posture = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if _, err := acpPostureFor("unknown"); err == nil {
		t.Fatal("unknown role posture succeeded")
	}
}

func TestNewACPCompositionPreflightsProfilesAndFiltersEnv(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": "relative/codex"},
		WorkspaceRoot: t.TempDir(),
		Env:           []string{"PATH=/bin", "SECRET=must-not-pass", "LANG=C"},
		EnvAllowlist:  []string{"PATH", "LANG"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("Claude profile missing: %v", err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("Codex profile registered despite failed executable preflight")
	}
	if got := filterACPEnv([]string{"PATH=/bin", "SECRET=x", "LANG=C"}, []string{"PATH", "LANG"}); len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "LANG=C" {
		t.Fatalf("filtered env = %#v", got)
	}
}

func TestACPBoundRuntimeResolutionUsesPinnedSelectors(t *testing.T) {
	t.Parallel()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{"builder"},
		GatewayClients: map[model.ProviderName]inference.Client{"anthropic": &fakeLLM{}, "openai": &fakeLLM{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("builder")),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithPolicyRevision("acp-child-test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("builder", "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	bound, err = loop.OverrideBoundRuntimeSelection(bound, resolved.Profile, resolved.ModelAlias, resolved.Target, resolved.Effort)
	if err != nil {
		t.Fatal(err)
	}
	got, harness, err := resolveACPBoundRuntime(compiled, bound)
	if err != nil {
		t.Fatal(err)
	}
	if harness != "codex" || got.ModelAlias != "gpt-5.6-luna" || got.Effort != model.EffortMax {
		t.Fatalf("resolved = %#v harness=%q", got, harness)
	}
}

func TestNewACPCompositionWithoutCatalogHasNoProfiles(t *testing.T) {
	t.Parallel()
	composition, err := NewACPComposition(ACPChildrenConfig{Executables: map[loop.AgentHarnessName]string{"codex": "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("empty catalog registered ACP profile")
	}
	if composition.Live == nil || composition.Restored == nil {
		t.Fatal("composition did not install bounded dispatchers")
	}
	var unknown *foreign.UnknownProfileError
	_, _, err = composition.Registry.Builder("acp/codex")
	if !errors.As(err, &unknown) {
		t.Fatalf("empty registry error = %v, want UnknownProfileError", err)
	}
}
