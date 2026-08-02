package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm/auto"
)

const (
	acpAnthropicAPIKeyEnv    = "ANTHROPIC_API_KEY"
	acpOpenAIAPIKeyEnv       = "OPENAI_API_KEY"
	acpClaudeExecutableEnv   = "CLAUDE_CODE_ACP_EXECUTABLE"
	acpCodexExecutableEnv    = "CODEX_ACP_EXECUTABLE"
	acpClaudeNativeModelsEnv = "CLAUDE_CODE_ACP_NATIVE_MODELS"
	acpCodexNativeModelsEnv  = "CODEX_ACP_NATIVE_MODELS"
)

// acpChildEnvAllowlist is deliberately limited to process configuration and
// login-home location. Provider API keys and all other ambient secrets stay
// outside the ACP child environment; gateway credentials are bound only to the
// in-process provider clients.
var acpNativeAuthEnvAllowlist = []string{
	"HOME", "LANG", "LC_ALL", "LOGNAME", "PATH", "TERM", "TMPDIR", "USER",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
}

// Native model discovery is allowed to consume only the connector's bounded,
// non-secret model projection in addition to the values that a native child
// may receive. These variables are intentionally not part of the child
// allowlist: discovery input and child process environment are separate
// trust boundaries.
var acpNativeDiscoveryEnvAllowlist = append(append([]string(nil), acpNativeAuthEnvAllowlist...),
	acpClaudeNativeModelsEnv, acpCodexNativeModelsEnv,
)

// Gateway-backed children must not inherit harness login locations. Provider
// credentials are bound only to the in-process gateway clients.
var acpGatewayEnvAllowlist = []string{
	"LANG", "LC_ALL", "LOGNAME", "PATH", "TERM", "TMPDIR", "USER",
}

// Compatibility name for callers that supplied one allowlist before the
// credential-specific environment split.
var acpChildEnvAllowlist = acpNativeAuthEnvAllowlist

// ACPNativeAuthProbe is the secret-free input to native-login discovery.
type ACPNativeAuthProbe struct {
	Harness       loop.AgentHarnessName
	Executable    string
	WorkspaceRoot string
	Env           []string
}

type acpNativeAuthDiscoverer func(context.Context, ACPNativeAuthProbe) ([]ACPNativeAuthSource, error)

// withProductionACPChildren installs the real process composition only at the
// public production entry points. Test seams may continue to pass an explicit
// composition (or nil) without reading ambient credentials or executable paths.
func withProductionACPChildren(cfg Config) (Config, error) {
	if cfg.ACPChildren != nil {
		return cfg, nil
	}
	composition, err := newProductionACPComposition()
	if err != nil {
		return Config{}, err
	}
	cfg.ACPChildren = composition
	return cfg, nil
}

// newProductionACPComposition binds direct provider credentials to private
// inference clients, compiles the current CodeRig role set, and preflights the
// configured ACP executables. Missing credentials or executables simply yield
// an empty/partial catalog; the resulting Subagent surface fails closed with a
// bounded no-runtime result rather than advertising an unusable child.
func newProductionACPComposition() (*ACPComposition, error) {
	return newProductionACPCompositionWithDiscovery(context.Background(), discoverProductionACPNativeAuth)
}

func newProductionACPCompositionWithDiscovery(ctx context.Context, discover acpNativeAuthDiscoverer) (*ACPComposition, error) {
	clients := make(map[model.ProviderName]inference.Client)
	anthropic, err := productionACPClient(
		model.ProviderName("anthropic"), model.APIFormatAnthropic, "claude-sonnet-5", acpAnthropicAPIKeyEnv,
	)
	if err != nil {
		return nil, err
	}
	if anthropic != nil {
		clients[model.ProviderName("anthropic")] = anthropic
	}
	openai, err := productionACPClient(
		model.ProviderName("openai"), model.APIFormatOpenAIResponses, "gpt-5.6-luna", acpOpenAIAPIKeyEnv,
	)
	if err != nil {
		return nil, err
	}
	if openai != nil {
		clients[model.ProviderName("openai")] = openai
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("coderig: resolve ACP workspace root: %w", err)
	}
	executables := map[loop.AgentHarnessName]string{
		"claude-code": os.Getenv(acpClaudeExecutableEnv),
		"codex":       os.Getenv(acpCodexExecutableEnv),
	}
	if discover == nil {
		discover = discoverProductionACPNativeAuth
	}
	native := make([]ACPNativeAuthSource, 0)
	for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
		sources, err := discover(ctx, ACPNativeAuthProbe{
			Harness: harness, Executable: executables[harness], WorkspaceRoot: root,
			Env: filterACPEnv(os.Environ(), acpNativeDiscoveryEnvAllowlist),
		})
		if err != nil {
			return nil, err
		}
		native = append(native, sources...)
	}
	catalog, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{planner.Name, builder.Name, reviewer.Name},
		GatewayClients: clients,
		NativeAuth:     native,
	})
	if err != nil {
		return nil, err
	}
	return NewACPComposition(ACPChildrenConfig{
		Catalog:             catalog,
		Executables:         executables,
		WorkspaceRoot:       root,
		Env:                 os.Environ(),
		EnvAllowlist:        acpChildEnvAllowlist,
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	})
}

// discoverProductionACPNativeAuth consumes a bounded, secret-free projection
// from the harness connector. The value is a comma-separated alias=model list
// supplied by the connector's discovery boundary; empty output means that the
// harness has no usable native-auth catalogue. No login token is read or
// forwarded, and missing native rows never fall back to gateway rows.
func discoverProductionACPNativeAuth(_ context.Context, probe ACPNativeAuthProbe) ([]ACPNativeAuthSource, error) {
	envName := acpCodexNativeModelsEnv
	provider := model.ProviderName("openai")
	format := model.APIFormatOpenAIResponses
	if probe.Harness == "claude-code" {
		envName = acpClaudeNativeModelsEnv
		provider = model.ProviderName("anthropic")
		format = model.APIFormatAnthropic
	}
	value := lookupEnv(probe.Env, envName)
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	result := make([]ACPNativeAuthSource, 0)
	var claudeSmall string
	for _, raw := range strings.Split(value, ",") {
		alias, name, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok || strings.TrimSpace(alias) == "" || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("coderig: invalid native ACP model discovery")
		}
		alias, name = strings.TrimSpace(alias), strings.TrimSpace(name)
		source := ACPNativeAuthSource{
			Harness:       probe.Harness,
			Alias:         loop.ModelAlias(alias),
			Model:         model.CustomModel(provider, format, "", name, model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortNone,
			Efforts:       []model.Effort{model.EffortNone},
		}
		if probe.Harness == "claude-code" {
			if claudeSmall == "" {
				claudeSmall = name
			}
			source.SmallModel = claudeSmall
		}
		result = append(result, source)
	}
	return result, nil
}

func lookupEnv(env []string, wanted string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == wanted {
			return value
		}
	}
	return ""
}

func productionACPClient(provider model.ProviderName, format model.APIFormat, name, envName string) (inference.Client, error) {
	key := os.Getenv(envName)
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	selected := model.CustomModel(provider, format, "", name, model.WithTools(), model.WithThinking())
	client, err := auto.New(selected, auth.APIKey(key))
	if err != nil {
		return nil, fmt.Errorf("coderig: configure ACP gateway client: %w", err)
	}
	return client, nil
}
