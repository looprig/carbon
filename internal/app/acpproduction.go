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

	// acpClaudeAdapterName is the current Claude ACP adapter binary,
	// installed by @agentclientprotocol/claude-agent-acp. It is the only
	// adapter identity the steering gate in
	// github.com/looprig/foreignloops/driver/acp recognizes for Claude, so
	// it must be preferred over any older name.
	acpClaudeAdapterName = "claude-agent-acp"
	// acpDeprecatedClaudeAdapterName is the pre-rename Claude ACP adapter
	// binary, installed by @zed-industries/claude-code-acp. npm has marked
	// that package deprecated ("renamed to
	// @agentclientprotocol/claude-agent-acp") and it is no longer the
	// upstream release line. It stays only as a last-resort discovery
	// fallback so an upgrade does not silently strip Claude ACP delegation
	// from an operator who has not migrated yet -- and discovering it
	// always emits acpDiagnosticDeprecatedClaudeAdapter, because a silent
	// fallback to a deprecated package is exactly how this drifted.
	acpDeprecatedClaudeAdapterName = "claude-code-acp"
	// acpCodexAdapterName is the current Codex ACP adapter binary,
	// installed by @agentclientprotocol/codex-acp. It has never been
	// renamed, so it has no deprecated alias.
	acpCodexAdapterName = "codex-acp"
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

// newProductionACPCompositionWithPreflight is retained as a lower-level test
// seam for callers that used to inject a live probe. The callback is ignored:
// startup now performs only static checks and defers ACP availability to the
// selected child launch.
func newProductionACPCompositionWithPreflight(_ context.Context, accessProfile AccessProfile, configured productionModels, _ func(context.Context, ACPExecutableProbe) ACPPreflightResult) (*ACPComposition, error) {
	return newProductionACPCompositionWithCollabRequired(context.Background(), accessProfile, configured, "", false)
}

func newProductionACPCompositionWithCollabRequired(_ context.Context, accessProfile AccessProfile, configured productionModels, collabExecutable string, requireCollabMCP bool) (*ACPComposition, error) {
	effectiveProfile, err := normalizeAccessProfile(accessProfile)
	if err != nil {
		return nil, errACPAccessProfileUnavailable
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("carbon: resolve ACP workspace root: %w", err)
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
	claudeExecutable, claudeWellKnownName := resolveACPExecutable(
		os.Getenv(acpClaudeExecutableEnv), configured.ACPLaunchers["claude-code"],
		acpClaudeAdapterName, acpDeprecatedClaudeAdapterName,
	)
	codexExecutable, _ := resolveACPExecutable(
		os.Getenv(acpCodexExecutableEnv), configured.ACPLaunchers["codex"],
		acpCodexAdapterName,
	)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       catalog,
		AccessProfile: effectiveProfile,
		Executables: map[loop.AgentHarnessName]string{
			"claude-code": claudeExecutable,
			"codex":       codexExecutable,
		},
		CollabMCPExecutable: collabExecutable,
		requireCollabMCP:    requireCollabMCP,
		WorkspaceRoot:       root,
		Env:                 os.Environ(),
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	})
	if err != nil {
		return nil, err
	}
	// Only warn when the deprecated adapter actually survived the static
	// checks and stayed in the catalog: a harness that was dropped already
	// carries its own, more actionable diagnostic.
	if claudeWellKnownName == acpDeprecatedClaudeAdapterName && composition.Catalog.HasProfile("acp/claude-code") {
		composition.Diagnostics = append(composition.Diagnostics, acpDiagnosticDeprecatedClaudeAdapter())
	}
	return composition, nil
}

// acpDiagnosticDeprecatedClaudeAdapter reports that PATH discovery fell back
// to the renamed, deprecated Claude ACP adapter. Like every other ACP
// diagnostic it is a fixed, secret-free category string with no filesystem
// path, stderr, or provider content.
func acpDiagnosticDeprecatedClaudeAdapter() string {
	return fmt.Sprintf(
		"acp: claude-code is using the deprecated %q adapter (@zed-industries/claude-code-acp, renamed upstream); install @agentclientprotocol/claude-agent-acp, or point acp_launchers in models.json or %s at it",
		acpDeprecatedClaudeAdapterName, acpClaudeExecutableEnv,
	)
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
// names in the order given. It performs no existence or executability check;
// NewACPComposition's static checks own that. A relative PATH match is
// resolved to an absolute path so the clean-absolute-path invariant used by
// the rest of the ACP pipeline holds.
//
// wellKnownNames is ordered most-current-first and the first PATH hit wins,
// so a machine with both the current and a deprecated adapter installed
// always gets the current one. The second return value is the well-known
// name that matched, or "" when the path came from the environment override
// or acp_launchers -- an explicitly configured path is the operator's own
// choice and is never reported as a deprecated discovery.
func resolveACPExecutable(envValue, configuredPath string, wellKnownNames ...string) (executable, matchedName string) {
	if envValue != "" {
		return envValue, ""
	}
	if configuredPath != "" {
		return configuredPath, ""
	}
	for _, wellKnownName := range wellKnownNames {
		found, err := exec.LookPath(wellKnownName)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(found) {
			if abs, absErr := filepath.Abs(found); absErr == nil {
				found = abs
			}
		}
		return found, wellKnownName
	}
	return "", ""
}

func cleanACPWorkspaceRoot(root string) bool {
	return root != "" && filepath.IsAbs(root) && filepath.Clean(root) == root
}
