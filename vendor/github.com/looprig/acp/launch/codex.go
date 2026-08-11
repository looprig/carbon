// codex.go implements CodexConnector's HarnessAdapter half: the codex-acp
// adapter's executable, argument, and environment contract. See
// codex_connector.go for CodexConnector's own type, constructor, and
// immutable selector APIs (the bounded post-session model/effort selection
// mechanism and the fresh-launch state-copy helpers).
//
// Configure never runs, spawns, or otherwise reaches for codex-acp's own
// process: version verification is a distinct, explicit preflight step
// (see version.go's ProbeCodexVersion) a caller runs itself before ever
// constructing a Config.Command whose Path this method configures -- the
// same reason the design doc's "one-shot --version invocation" is called
// out as living outside the ACP session lifecycle (see acp/CLAUDE.md).
// This file's own contract is purely the argv/env shape.
package launch

import (
	"strconv"

	"github.com/looprig/acp/transport/stdio"
)

const (
	// envLoopRigProxyToken carries the model-proxy bearer token into
	// codex-acp's own custom model provider, referenced by the
	// model_providers.looprig.env_key override below (see
	// codexConfigArgs): codex-acp reads the token from this environment
	// variable, never from an argv value or a config file.
	//
	// #nosec G101 -- this is the NAME of an environment variable, not a
	// credential value; the real secret is the runtime bearer token read
	// from a ProxyBinding, never a source-code literal.
	envLoopRigProxyToken = "LOOPRIG_PROXY_TOKEN"
	// envCodexHome must never be present in codex-acp's environment: its
	// presence would point codex-acp at a real, persistent configuration
	// directory this connector must never generate, touch, or overwrite
	// (see the design doc's "Configuration" and acp/CLAUDE.md's
	// environment-safety rule). A caller-supplied value already present
	// is rejected as a conflicting security-sensitive variable, never
	// silently stripped (see buildChildCommand).
	envCodexHome = "CODEX_HOME"

	// codexModelProviderName is the custom model_provider id this
	// connector always registers codex-acp's gateway-facing provider
	// under. Fixed, never caller-configurable: see the design doc's
	// Codex `model_provider = "looprig"` contract.
	codexModelProviderName = "looprig"
)

// Configure implements HarnessAdapter for codex-acp: cmd.Path is the
// caller-supplied absolute path to the codex-acp executable itself (this
// connector never performs PATH lookup, invokes npx, or installs anything
// -- discovery and installation are entirely the caller's responsibility,
// the same rule claude-agent-acp's Configure follows). Gateway configuration
// requires Model to be non-empty. The returned Command's Args is entirely
// replaced with the fixed, ordered `-c key=value` override sequence
// codexConfigArgs builds (never merged with cmd's own caller-supplied Args:
// codex-acp's config surface is this connector's exclusive concern, the same
// "replace rather than merge" precedent claude-agent-acp's Configure sets);
// its Env carries LOOPRIG_PROXY_TOKEN and never CODEX_HOME. cmd is never
// mutated; the returned Command is always a fresh copy (see
// buildChildCommand).
func (c *CodexConnector) Configure(cmd stdio.Command, binding ProxyBinding) (stdio.Command, error) {
	return c.configure(cmd, binding, true)
}

// configureWithoutProxy keeps the caller's environment and Codex executable
// validation, but omits the gateway token and provider/base URL settings so
// Codex uses its own login.
func (c *CodexConnector) configureWithoutProxy(cmd stdio.Command) (stdio.Command, error) {
	return c.configure(cmd, ProxyBinding{}, false)
}

// ConfigureNative implements NativeHarnessAdapter. It validates the existing
// native model/effort tuple semantics but deliberately emits neither selector
// as argv: Codex ACP session options are selected after session/new through
// SelectModel and SelectEffort. An empty Model is passed through so codex-acp
// can choose its own model.
func (c *CodexConnector) ConfigureNative(cmd stdio.Command) (stdio.Command, error) {
	return c.configureWithoutProxy(cmd)
}

func (c *CodexConnector) configure(cmd stdio.Command, binding ProxyBinding, gateway bool) (stdio.Command, error) {
	if !cleanAbsolutePath(cmd.Path) {
		return stdio.Command{}, &PathError{Field: "Path", Reason: "must be a clean absolute path to codex-acp"}
	}
	if gateway && c.Model == "" {
		return stdio.Command{}, &ConfigError{Reason: "CodexConnector.Model is required"}
	}
	if !gateway && ((c.Effort != "" && c.Model == "") || c.effortExplicit && ((c.Model == "") != (c.Effort == ""))) {
		return stdio.Command{}, &ConfigError{Reason: "CodexConnector.Model and Effort must be provided together for native configuration"}
	}

	overrides := make([]envOverride, 0, 1)
	forbidden := []string{envCodexHome}
	if gateway {
		overrides = append(overrides, envOverride{Key: envLoopRigProxyToken, Value: binding.Token})
	} else {
		forbidden = append(forbidden, envLoopRigProxyToken)
	}

	out, err := buildChildCommand(cmd, overrides, forbidden)
	if err != nil {
		return stdio.Command{}, err
	}

	posture := c.Posture.resolve()
	if gateway {
		out.Args = codexConfigArgs(c.Model, binding.BaseURL, posture)
	} else {
		out.Args = codexNativeConfigArgs(c.Model, c.Effort, posture)
	}
	return out, nil
}

func codexNativeConfigArgs(_ string, _ string, posture CodexPosture) []string {
	// Model and effort are intentionally retained on CodexConnector for
	// post-session ACP selection; native argv carries only posture/configuration
	// values. codex-acp server mode ignores the selector -c keys.
	pairs := make([][2]string, 0, 3)
	pairs = append(pairs,
		[2]string{"approval_policy", posture.ApprovalPolicy},
		[2]string{"sandbox_mode", posture.SandboxMode},
		[2]string{"sandbox_workspace_write.network_access", strconv.FormatBool(posture.SandboxNetworkAccess)},
	)
	args := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		args = append(args, "-c", p[0]+"="+p[1])
	}
	return args
}

// codexConfigArgs builds codex-acp's fixed, ordered `-c key=value` override
// sequence: the custom "looprig" model provider (base URL, env key,
// wire_api, requires_openai_auth -- see the design doc's Codex TOML
// equivalent) followed by the caller's sandbox/approval posture. Order is
// deterministic and stable across calls -- this function always appends in
// this exact literal sequence, never a map iteration -- so callers and
// tests can assert argv positionally rather than as an unordered set.
//
// wire_api's value is written with its own embedded double quotes
// (`"responses"`) and requires_openai_auth's as a bare boolean (`false`):
// codex-acp parses each `-c` value as a TOML expression, so a string value
// must carry its own quotes to parse as a TOML string rather than a bare,
// invalid TOML identifier, while a boolean must NOT be quoted to parse as
// a TOML boolean rather than the (different) TOML string "false". model,
// model_provider, the base_url/env_key overrides, and the posture values
// are passed unquoted, matching this module's own verified reference
// usage (goose's codex-acp provider passes approval_policy/sandbox_mode
// unquoted the same way).
func codexConfigArgs(model, baseURL string, posture CodexPosture) []string {
	pairs := [...][2]string{
		{"model", model},
		{"model_provider", codexModelProviderName},
		{"model_providers." + codexModelProviderName + ".base_url", baseURL + "/v1"},
		{"model_providers." + codexModelProviderName + ".env_key", envLoopRigProxyToken},
		{"model_providers." + codexModelProviderName + ".wire_api", `"responses"`},
		{"model_providers." + codexModelProviderName + ".requires_openai_auth", "false"},
		{"approval_policy", posture.ApprovalPolicy},
		{"sandbox_mode", posture.SandboxMode},
		{"sandbox_workspace_write.network_access", strconv.FormatBool(posture.SandboxNetworkAccess)},
	}
	args := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		args = append(args, "-c", p[0]+"="+p[1])
	}
	return args
}
