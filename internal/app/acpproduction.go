package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/acp/transport/stdio"
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

type acpNativeModelMetadata struct {
	alias string
	name  string
}

type acpNativeSessionProbe func(context.Context, ACPNativeAuthProbe, []acpNativeModelMetadata) ([]acpNativeModelMetadata, error)

const (
	acpNativeProbeTimeout = 5 * time.Second
	acpNativeModelLimit   = 32
	acpNativeFieldLimit   = 128
)

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

// discoverProductionACPNativeAuth treats the bounded model projection only as
// candidates. It advertises them only after the configured executable starts,
// initializes as ACP, and creates a native-auth session. Every process/login/
// protocol failure is intentionally collapsed to an empty result: raw child
// errors can contain login paths or credentials and must not cross into the
// composition or model-facing surface.
func discoverProductionACPNativeAuth(ctx context.Context, probe ACPNativeAuthProbe) ([]ACPNativeAuthSource, error) {
	return discoverProductionACPNativeAuthWithSessionProbe(ctx, probe, probeProductionACPNativeSession)
}

func discoverProductionACPNativeAuthWithSessionProbe(ctx context.Context, probe ACPNativeAuthProbe, sessionProbe acpNativeSessionProbe) ([]ACPNativeAuthSource, error) {
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
	candidates, ok := parseACPNativeModelMetadata(value)
	if !ok || sessionProbe == nil {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, acpNativeProbeTimeout)
	defer cancel()
	verified, err := sessionProbe(probeCtx, probe, candidates)
	if err != nil || probeCtx.Err() != nil || len(verified) == 0 || len(verified) > acpNativeModelLimit {
		return nil, nil
	}
	allowed := make(map[acpNativeModelMetadata]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate] = struct{}{}
	}
	result := make([]ACPNativeAuthSource, 0, len(verified))
	var claudeSmall string
	seen := make(map[acpNativeModelMetadata]struct{}, len(verified))
	for _, metadata := range verified {
		if _, ok := allowed[metadata]; !ok || !validACPNativeMetadata(metadata) {
			return nil, nil
		}
		if _, duplicate := seen[metadata]; duplicate {
			continue
		}
		seen[metadata] = struct{}{}
		source := ACPNativeAuthSource{
			Harness:       probe.Harness,
			Alias:         loop.ModelAlias(metadata.alias),
			Model:         model.CustomModel(provider, format, "", metadata.name, model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortNone,
			Efforts:       []model.Effort{model.EffortNone},
		}
		if probe.Harness == "claude-code" {
			if claudeSmall == "" {
				claudeSmall = metadata.name
			}
			source.SmallModel = claudeSmall
		}
		result = append(result, source)
	}
	return result, nil
}

func parseACPNativeModelMetadata(value string) ([]acpNativeModelMetadata, bool) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > acpNativeModelLimit {
		return nil, false
	}
	result := make([]acpNativeModelMetadata, 0, len(parts))
	for _, raw := range parts {
		alias, name, ok := strings.Cut(strings.TrimSpace(raw), "=")
		metadata := acpNativeModelMetadata{alias: strings.TrimSpace(alias), name: strings.TrimSpace(name)}
		if !ok || !validACPNativeMetadata(metadata) {
			return nil, false
		}
		result = append(result, metadata)
	}
	return result, true
}

func validACPNativeMetadata(metadata acpNativeModelMetadata) bool {
	return validACPNativeField(metadata.alias) && validACPNativeField(metadata.name)
}

func validACPNativeField(value string) bool {
	if value == "" || len(value) > acpNativeFieldLimit || strings.ContainsAny(value, "\x00\r\n\t =") {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:/-", r)) {
			return false
		}
	}
	return true
}

func cleanACPWorkspaceRoot(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root
}

func probeProductionACPNativeSession(ctx context.Context, probe ACPNativeAuthProbe, candidates []acpNativeModelMetadata) ([]acpNativeModelMetadata, error) {
	if len(candidates) == 0 || !preflightACPExecutable(probe.Executable) || !cleanACPWorkspaceRoot(probe.WorkspaceRoot) {
		return nil, fmt.Errorf("native ACP session probe has no candidates")
	}
	var connector launch.HarnessAdapter
	switch probe.Harness {
	case "claude-code":
		connector = launch.ClaudeCode(launch.ClaudeModels{Default: candidates[0].name, Small: candidates[0].name})
	case "codex":
		codex := launch.Codex(candidates[0].name)
		codex.Posture = launch.CodexPosture{ApprovalPolicy: "never", SandboxMode: "read-only"}
		connector = codex
	default:
		return nil, fmt.Errorf("unsupported native ACP harness")
	}
	managed, err := launch.Dial(ctx, launch.Config{
		NoProxy: true,
		Harness: connector,
		Command: stdio.Command{Path: probe.Executable, Env: filterACPEnv(probe.Env, acpNativeAuthEnvAllowlist)},
		Client:  client.Options{},
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = managed.Close(closeCtx)
	}()
	session, err := managed.Client().NewSession(ctx, client.NewSessionParams{Cwd: probe.WorkspaceRoot})
	if err != nil {
		return nil, err
	}
	if probe.Harness == "codex" {
		// Codex model selection is process-scoped rather than a stable ACP
		// config option. Successful native session creation validates the
		// bounded candidates supplied to that process.
		return append([]acpNativeModelMetadata(nil), candidates...), nil
	}
	return verifiedClaudeNativeModels(session.ConfigOptions(), candidates), nil
}

func verifiedClaudeNativeModels(options []protocol.SessionConfigOption, candidates []acpNativeModelMetadata) []acpNativeModelMetadata {
	advertised := make(map[string]struct{})
	for _, option := range options {
		if option.Category == nil || *option.Category != protocol.SessionConfigOptionCategoryModel || option.Select == nil {
			continue
		}
		for _, item := range option.Select.Options.Ungrouped {
			if validACPNativeField(string(item.Value)) {
				advertised[string(item.Value)] = struct{}{}
			}
		}
		for _, group := range option.Select.Options.Grouped {
			for _, item := range group.Options {
				if validACPNativeField(string(item.Value)) {
					advertised[string(item.Value)] = struct{}{}
				}
			}
		}
	}
	verified := make([]acpNativeModelMetadata, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := advertised[candidate.name]; ok {
			verified = append(verified, candidate)
		}
	}
	return verified
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
