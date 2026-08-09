package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/acp/transport/stdio"
	"github.com/looprig/harness/pkg/loop"
)

const (
	acpClaudeExecutableEnv = "CLAUDE_CODE_ACP_EXECUTABLE"
	acpCodexExecutableEnv  = "CODEX_ACP_EXECUTABLE"
	acpNativeProbeTimeout  = 5 * time.Second
	acpNativeFieldLimit    = 128
)

// Gateway-backed children inherit only process mechanics. Provider keys remain
// bound to the already-constructed in-process inference clients.
var acpGatewayEnvAllowlist = []string{
	"LANG", "LC_ALL", "LOGNAME", "PATH", "TERM", "TMPDIR", "USER",
}

// Native-auth children receive only the bounded login/process environment when
// an enabled native_acp profile has passed static executable checks.
var acpNativeAuthEnvAllowlist = []string{
	"HOME", "LANG", "LC_ALL", "LOGNAME", "PATH", "TERM", "TMPDIR", "USER",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME",
}

// ACPNativeAuthProbe remains the bounded lower-level ACP probe shape.
type ACPNativeAuthProbe struct {
	Harness       loop.AgentHarnessName
	Executable    string
	WorkspaceRoot string
	Env           []string
}

func withProductionACPChildren(ctx context.Context, cfg Config, configured productionModels) (Config, error) {
	collabExecutable := ""
	if cfg.CollabMCPExecutable != "" {
		resolved, err := resolveCollabMCPExecutable(cfg.CollabMCPExecutable)
		if err != nil {
			return Config{}, err
		}
		collabExecutable = resolved
	}
	composition, err := newProductionACPCompositionWithCollabRequired(ctx, cfg.AccessProfile, configured, collabExecutable, true)
	if err != nil {
		return Config{}, err
	}
	cfg.ACPChildren = composition
	cfg.CollabMCPExecutable = composition.collabMCPExecutable
	cfg.RuntimeCatalog = composition.Catalog.RuntimeCatalog
	cfg.ACPDiagnostics = composition.Diagnostics
	return cfg, nil
}

func newProductionACPComposition(ctx context.Context, accessProfile AccessProfile, configured productionModels) (*ACPComposition, error) {
	return newProductionACPCompositionWithCollabRequired(ctx, accessProfile, configured, "", true)
}

// newProductionACPCompositionWithPreflight is retained as a lower-level test
// seam for callers that used to inject a live probe. The callback is ignored:
// startup now performs only static checks and defers ACP availability to the
// selected child launch.
func newProductionACPCompositionWithPreflight(_ context.Context, accessProfile AccessProfile, configured productionModels, _ func(context.Context, ACPExecutableProbe) ACPPreflightResult) (*ACPComposition, error) {
	return newProductionACPCompositionWithCollabRequired(context.Background(), accessProfile, configured, "", false)
}

func newProductionACPCompositionWithCollab(_ context.Context, accessProfile AccessProfile, configured productionModels, collabExecutable string) (*ACPComposition, error) {
	return newProductionACPCompositionWithCollabRequired(context.Background(), accessProfile, configured, collabExecutable, true)
}

func newProductionACPCompositionWithCollabRequired(_ context.Context, accessProfile AccessProfile, configured productionModels, collabExecutable string, requireCollabMCP bool) (*ACPComposition, error) {
	effectiveProfile, err := normalizeAccessProfile(accessProfile)
	if err != nil {
		return nil, errACPAccessProfileUnavailable
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("coderig: resolve ACP workspace root: %w", err)
	}
	catalog, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: configured.ACP,
		PrimerTarget:   configuredPrimerRuntimeTarget(configured),
		ClaudeSmall:    configured.ClaudeSmall,
		NativeACP:      configured.NativeACP,
	})
	if err != nil {
		return nil, err
	}
	return NewACPComposition(ACPChildrenConfig{
		Catalog:       catalog,
		AccessProfile: effectiveProfile,
		Executables: map[loop.AgentHarnessName]string{
			"claude-code": resolveACPExecutable(os.Getenv(acpClaudeExecutableEnv), configured.ACPLaunchers["claude-code"], "claude-code-acp"),
			"codex":       resolveACPExecutable(os.Getenv(acpCodexExecutableEnv), configured.ACPLaunchers["codex"], "codex-acp"),
		},
		CollabMCPExecutable: collabExecutable,
		requireCollabMCP:    requireCollabMCP,
		WorkspaceRoot:       root,
		Env:                 os.Environ(),
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	})
}

// preflightProductionACPExecutable is an explicit diagnostic/test probe. It
// proves that a configured file speaks ACP using the same credential path and
// exact configured model arguments as the eventual child. Production startup
// does not call it; native probes use DialNative and therefore cannot carry a
// gateway proxy binding.
func preflightProductionACPExecutable(ctx context.Context, probe ACPExecutableProbe) ACPPreflightResult {
	if !preflightACPExecutable(probe.Executable) || !cleanACPWorkspaceRoot(probe.WorkspaceRoot) {
		return ACPPreflightResult{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, acpNativeProbeTimeout)
	defer cancel()
	var connector launch.HarnessAdapter
	var claude *launch.ClaudeConnector
	switch probe.Harness {
	case "claude-code":
		claude = launch.ClaudeCode(launch.ClaudeModels{Default: probe.Model, Small: probe.SmallModel})
		connector = claude
	case "codex":
		codex := launch.Codex(probe.Model)
		codex.Posture = launch.CodexPosture{ApprovalPolicy: "never", SandboxMode: "read-only"}
		connector = codex
	default:
		return ACPPreflightResult{}
	}
	command := stdio.Command{Path: probe.Executable, Dir: probe.WorkspaceRoot}
	var managed *launch.ManagedClient
	var err error
	switch probe.Credential {
	case loop.CredentialGatewayBacked:
		switch probe.Harness {
		case "claude-code":
			if probe.Model == "" || probe.SmallModel == "" {
				return ACPPreflightResult{}
			}
		case "codex":
			if probe.Model == "" || probe.SmallModel != "" {
				return ACPPreflightResult{}
			}
		default:
			return ACPPreflightResult{}
		}
		if probe.SharedProxy == nil || probe.SharedProxy.BaseURL == "" || probe.SharedProxy.Token == "" {
			return ACPPreflightResult{}
		}
		command.Env = filterACPProviderSecrets(filterACPEnv(probe.Env, acpGatewayEnvAllowlist))
		managed, err = launch.Dial(probeCtx, launch.Config{
			Harness:     connector,
			SharedProxy: probe.SharedProxy,
			Command:     command,
			Client:      client.Options{},
		})
	case loop.CredentialNativeAuth:
		if probe.SharedProxy != nil {
			return ACPPreflightResult{}
		}
		switch probe.Harness {
		case "claude-code":
			if (probe.Model == "") != (probe.SmallModel == "") {
				return ACPPreflightResult{}
			}
		case "codex":
			if probe.SmallModel != "" {
				return ACPPreflightResult{}
			}
		default:
			return ACPPreflightResult{}
		}
		native, ok := connector.(launch.NativeHarnessAdapter)
		if !ok {
			return ACPPreflightResult{}
		}
		command.Env = filterACPProviderSecrets(filterACPEnv(probe.Env, acpNativeAuthEnvAllowlist))
		managed, err = launch.DialNative(probeCtx, launch.NativeConfig{
			Harness: native,
			Command: command,
			Client:  client.Options{},
		})
	default:
		return ACPPreflightResult{}
	}
	if err != nil {
		return ACPPreflightResult{}
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		_ = managed.Close(closeCtx)
	}()
	session, err := managed.Client().NewSession(probeCtx, client.NewSessionParams{Cwd: probe.WorkspaceRoot})
	if err != nil || probeCtx.Err() != nil {
		return ACPPreflightResult{}
	}
	if probe.Credential == loop.CredentialNativeAuth && claude != nil && probe.Model != "" {
		if err := claude.SelectDefaultModel(probeCtx, session); err != nil {
			return ACPPreflightResult{}
		}
		if err := claude.SelectSmallModel(probeCtx, session); err != nil {
			return ACPPreflightResult{}
		}
	}
	result := ACPPreflightResult{Ready: true}
	if probe.Harness == "claude-code" && session != nil {
		result.AdvertisedModels = advertisedACPModelValues(session.ConfigOptions())
	}
	return result
}

func advertisedACPModelValues(options []protocol.SessionConfigOption) []string {
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, option := range options {
		if option.Category == nil || *option.Category != protocol.SessionConfigOptionCategoryModel || option.Select == nil {
			continue
		}
		for _, item := range option.Select.Options.Ungrouped {
			value := string(item.Value)
			if validACPNativeField(value) {
				if _, exists := seen[value]; !exists {
					seen[value] = struct{}{}
					values = append(values, value)
				}
			}
		}
		for _, group := range option.Select.Options.Grouped {
			for _, item := range group.Options {
				value := string(item.Value)
				if validACPNativeField(value) {
					if _, exists := seen[value]; !exists {
						seen[value] = struct{}{}
						values = append(values, value)
					}
				}
			}
		}
	}
	return values
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

// resolveACPExecutable picks one harness's ACP adapter executable by explicit
// precedence: the environment-variable override, then the configured
// acp_launchers path, then PATH discovery of the fixed well-known adapter
// name. It performs no existence or executability check; NewACPComposition's
// static checks own that. A relative PATH match is resolved to an absolute
// path so the clean-absolute-path invariant used by the rest of the ACP
// pipeline holds.
func resolveACPExecutable(envValue, configuredPath, wellKnownName string) string {
	if envValue != "" {
		return envValue
	}
	if configuredPath != "" {
		return configuredPath
	}
	found, err := exec.LookPath(wellKnownName)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(found) {
		if abs, absErr := filepath.Abs(found); absErr == nil {
			found = abs
		}
	}
	return found
}

func cleanACPWorkspaceRoot(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root
}
