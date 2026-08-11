// claudecode.go implements ClaudeConnector's HarnessAdapter half: the
// executable, argument, and environment contract claude-agent-acp (the ACP
// adapter for Anthropic's claude-agent SDK) requires. See
// claude_connector.go for ClaudeConnector's session-level (config option /
// permission mode) behavior.
package launch

import (
	"path/filepath"

	"github.com/looprig/acp/transport/stdio"
)

const (
	// envAnthropicBaseURL and envAnthropicAuthToken point claude-agent-acp's
	// underlying Anthropic client at the model proxy binding instead of a
	// real provider endpoint.
	envAnthropicBaseURL = "ANTHROPIC_BASE_URL"
	// #nosec G101 -- this is the NAME of an environment variable, not a
	// credential value; the real secret is a runtime ProxyBinding token,
	// never a source-code literal.
	envAnthropicAuthToken = "ANTHROPIC_AUTH_TOKEN"
	// envClaudeCodeExecutable optionally pins the underlying `claude` CLI
	// claude-agent-acp drives to a specific absolute path.
	envClaudeCodeExecutable = "CLAUDE_CODE_EXECUTABLE"
	// envClaudeCodeNestedSession is claude-agent-acp's own nested-session
	// detection variable. It must never be present in the child's
	// environment (see Configure's doc): this connector never sets it, and
	// a caller-supplied value already present is treated as a conflicting
	// security-sensitive variable, never silently stripped.
	envClaudeCodeNestedSession = "CLAUDECODE"
)

// Configure implements HarnessAdapter for claude-agent-acp: cmd.Path is the
// caller-supplied absolute path to the claude-agent-acp executable itself
// (this connector never performs PATH lookup, invokes npx, or installs
// anything -- discovery and installation are entirely the caller's
// responsibility); gateway configuration requires both ClaudeModels aliases;
// the returned Command always has an empty argument list (claude-agent-acp
// takes none) and carries ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN set to
// binding's values, plus CLAUDE_CODE_EXECUTABLE if c.CLIPath is set. cmd is
// never mutated; the returned Command is always a fresh copy (see
// buildChildCommand).
func (c *ClaudeConnector) Configure(cmd stdio.Command, binding ProxyBinding) (stdio.Command, error) {
	return c.configure(cmd, binding, true)
}

// configureWithoutProxy keeps the caller's environment and Claude executable
// validation, but never installs gateway URL or token overrides so the child
// can use its own login state.
func (c *ClaudeConnector) configureWithoutProxy(cmd stdio.Command) (stdio.Command, error) {
	return c.configure(cmd, ProxyBinding{}, false)
}

// ConfigureNative implements NativeHarnessAdapter. It leaves Claude Code's
// login and model picker in control when either or both model aliases are
// omitted.
func (c *ClaudeConnector) ConfigureNative(cmd stdio.Command) (stdio.Command, error) {
	return c.configureWithoutProxy(cmd)
}

func (c *ClaudeConnector) configure(cmd stdio.Command, binding ProxyBinding, gateway bool) (stdio.Command, error) {
	if !cleanAbsolutePath(cmd.Path) {
		return stdio.Command{}, &PathError{Field: "Path", Reason: "must be a clean absolute path to claude-agent-acp"}
	}
	if gateway && (c.Models.Default == "" || c.Models.Small == "") {
		return stdio.Command{}, &ConfigError{Reason: "ClaudeConnector.Models.Default and Small are required for gateway configuration"}
	}

	overrides := make([]envOverride, 0, 3)
	forbidden := []string{envClaudeCodeNestedSession}
	if gateway {
		overrides = append(overrides,
			envOverride{Key: envAnthropicBaseURL, Value: binding.BaseURL},
			envOverride{Key: envAnthropicAuthToken, Value: binding.Token},
		)
	} else {
		forbidden = append(forbidden, envAnthropicBaseURL, envAnthropicAuthToken)
	}
	if c.CLIPath != "" {
		if !cleanAbsolutePath(c.CLIPath) {
			return stdio.Command{}, &PathError{Field: "CLIPath", Reason: "must be empty or a clean absolute path to the claude CLI"}
		}
		overrides = append(overrides, envOverride{Key: envClaudeCodeExecutable, Value: c.CLIPath})
	}

	out, err := buildChildCommand(cmd, overrides, forbidden)
	if err != nil {
		return stdio.Command{}, err
	}
	out.Args = []string{}
	return out, nil
}

// cleanAbsolutePath mirrors acp/transport/stdio's own unexported path
// validation (Spawn will re-check this itself once the child is actually
// started; Configure validates early so a bad path fails at configure time
// rather than only once Dial reaches Spawn).
func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
