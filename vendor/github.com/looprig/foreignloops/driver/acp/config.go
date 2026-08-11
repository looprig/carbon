package acp

import (
	"path/filepath"

	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/loop"
)

// Harness identifies the ACP adapter contract selected by Config.
type Harness string

const (
	HarnessClaudeCode Harness = "claude-code"
	HarnessCodex      Harness = "codex"
)

// Config is the caller-owned configuration for an ACP driver.
type Config struct {
	// Harness selects the adapter contract.
	Harness Harness
	// Executable is the clean absolute path to the ACP adapter binary.
	Executable string
	// Env is the complete child environment supplied by the caller.
	Env []string
	// Credential selects whether the child uses the product gateway or its
	// own harness login state.
	Credential loop.CredentialMode
	// Binding is borrowed caller-owned proxy data. The driver never starts or
	// closes anything represented by this value. It is required for
	// gateway-backed children and must be zero for native-auth children.
	Binding launch.ProxyBinding
	// ModelAlias is the primary harness-facing model alias.
	ModelAlias string
	// Effort is the neutral native reasoning-effort selector. Native explicit
	// effort requires ModelAlias; an empty or "none" value leaves effort
	// selection to the selected model. When both ModelAlias and Effort are
	// empty, the harness owns model and effort selection. Gateway launches do
	// not use this field and reject non-empty values.
	Effort string
	// SmallModelAlias is required by Claude Code and forbidden by Codex.
	SmallModelAlias string
	// Posture is the neutral access posture for the child.
	Posture driver.Posture
	// AgentSessionID resumes an existing agent-side session when non-empty.
	AgentSessionID string
	// WorkspaceRoot is the absolute session working directory.
	WorkspaceRoot string
	// McpServers is the complete MCP server list supplied to ACP session/new
	// and session/load. New snapshots this value before retaining it so nested
	// caller-owned slices and metadata cannot be mutated after construction.
	McpServers []protocol.McpServer
}

// ConfigError reports invalid caller-supplied ACP configuration.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	if e == nil {
		return "acp: config: invalid"
	}
	if e.Reason == "" {
		return "acp: config: " + e.Field
	}
	return "acp: config: " + e.Field + ": " + e.Reason
}

// validate rejects configuration before any ACP child or proxy lifecycle is
// entered. It only reports fixed field-level reasons; caller values and
// underlying errors never become part of the returned error string.
func (c Config) validate() error {
	switch c.Harness {
	case HarnessClaudeCode, HarnessCodex:
	default:
		return &ConfigError{Field: "Harness", Reason: "must be a supported harness"}
	}

	switch c.Credential {
	case loop.CredentialGatewayBacked, loop.CredentialNativeAuth:
	default:
		return &ConfigError{Field: "Credential", Reason: "must be a supported credential mode"}
	}

	if !cleanAbsolutePath(c.Executable) {
		return &ConfigError{Field: "Executable", Reason: "must be a clean absolute path"}
	}
	if c.Credential == loop.CredentialGatewayBacked {
		if c.Binding.BaseURL == "" || c.Binding.Token == "" {
			return &ConfigError{Field: "Binding", Reason: "is required"}
		}
	} else if c.Binding != (launch.ProxyBinding{}) {
		return &ConfigError{Field: "Binding", Reason: "must be empty for native authentication"}
	}
	if c.gatewayBacked() && c.Effort != "" {
		return &ConfigError{Field: "Effort", Reason: "is not supported for gateway-backed launches"}
	}
	if !c.Posture.Valid() {
		return &ConfigError{Field: "Posture", Reason: "must be a supported posture"}
	}
	if !cleanAbsolutePath(c.WorkspaceRoot) {
		return &ConfigError{Field: "WorkspaceRoot", Reason: "must be a clean absolute path"}
	}

	switch c.Harness {
	case HarnessClaudeCode:
		if c.gatewayBacked() {
			if c.ModelAlias == "" {
				return &ConfigError{Field: "ModelAlias", Reason: "is required"}
			}
			if c.SmallModelAlias == "" {
				return &ConfigError{Field: "SmallModelAlias", Reason: "is required for Claude Code"}
			}
			break
		}
		if c.ModelAlias == "" && c.SmallModelAlias == "" {
			break
		}
		if c.ModelAlias == "" {
			return &ConfigError{Field: "ModelAlias", Reason: "must be provided with SmallModelAlias for Claude Code"}
		}
		if c.SmallModelAlias == "" {
			return &ConfigError{Field: "SmallModelAlias", Reason: "must be provided with ModelAlias for Claude Code"}
		}
	case HarnessCodex:
		if c.SmallModelAlias != "" {
			return &ConfigError{Field: "SmallModelAlias", Reason: "is not supported by Codex"}
		}
		if c.gatewayBacked() && c.ModelAlias == "" {
			return &ConfigError{Field: "ModelAlias", Reason: "is required"}
		}
	}

	if !c.gatewayBacked() && c.ModelAlias == "" && c.Effort != "" {
		return &ConfigError{Field: "ModelAlias", Reason: "must be provided with Effort for native selection"}
	}
	return nil
}

func (c Config) gatewayBacked() bool {
	return c.Credential == loop.CredentialGatewayBacked
}

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
