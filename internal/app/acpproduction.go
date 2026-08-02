package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/looprig/coderig/internal/catalog/operator"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm/auto"
)

const (
	acpAnthropicAPIKeyEnv  = "ANTHROPIC_API_KEY"
	acpOpenAIAPIKeyEnv     = "OPENAI_API_KEY"
	acpClaudeExecutableEnv = "CLAUDE_CODE_ACP_EXECUTABLE"
	acpCodexExecutableEnv  = "CODEX_ACP_EXECUTABLE"
)

// acpChildEnvAllowlist is deliberately limited to process configuration and
// login-home location. Provider API keys and all other ambient secrets stay
// outside the ACP child environment; gateway credentials are bound only to the
// in-process provider clients.
var acpChildEnvAllowlist = []string{
	"HOME", "LANG", "LC_ALL", "LOGNAME", "PATH", "TERM", "TMPDIR", "USER",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
}

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
	catalog, err := CompileACPCatalog(ACPCatalogInput{
		// The current CodeRig topology is operator/operator-primary/reviewer;
		// the explicit role list keeps the production catalog aligned with the
		// definitions until the separate three-primer roster migration lands.
		SubagentTypes:  []identity.AgentName{operator.Name, reviewer.Name},
		GatewayClients: clients,
	})
	if err != nil {
		return nil, err
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("coderig: resolve ACP workspace root: %w", err)
	}
	return NewACPComposition(ACPChildrenConfig{
		Catalog:       catalog,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": os.Getenv(acpClaudeExecutableEnv), "codex": os.Getenv(acpCodexExecutableEnv)},
		WorkspaceRoot: root,
		Env:           os.Environ(),
		EnvAllowlist:  acpChildEnvAllowlist,
	})
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
