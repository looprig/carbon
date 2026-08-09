package app

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/looprig/acp/protocol"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/mcp/pkg/collab"
)

const collabMCPExecutableName = "coderig-collab-mcp"

var errCollabMCPExecutableUnavailable = errors.New("coderig: collaboration MCP executable unavailable")

// resolveCollabMCPExecutable resolves the collaboration MCP process before a
// session is assembled. An explicit path is authoritative; an empty path uses
// the sibling of the current CodeRig executable.
func resolveCollabMCPExecutable(configured string) (string, error) {
	if configured != "" {
		return resolveCollabMCPExecutableFrom(configured, "")
	}
	current, err := os.Executable()
	if err != nil {
		return "", errCollabMCPExecutableUnavailable
	}
	return resolveCollabMCPExecutableFrom(configured, current)
}

func resolveCollabMCPExecutableFrom(configured, current string) (string, error) {
	candidate := configured
	if candidate == "" {
		if !cleanAbsolutePath(current) {
			return "", errCollabMCPExecutableUnavailable
		}
		candidate = filepath.Join(filepath.Dir(current), collabMCPExecutableName)
	}
	if !cleanAbsolutePath(candidate) || !verifiedExecutable(candidate) {
		return "", errCollabMCPExecutableUnavailable
	}
	return candidate, nil
}

func verifiedExecutable(path string) bool {
	if !cleanAbsolutePath(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && cleanAbsolutePath(resolved)
}

// collabMCPServerFor builds the one opaque collaboration MCP descriptor for a
// foreign ACP loop. Harness owns the descriptor authority; CodeRig only maps
// its endpoint and token into the ACP stdio environment.
func collabMCPServerFor(executable string, descriptor foreign.BrokerDescriptor) (protocol.McpServer, error) {
	if !cleanAbsolutePath(executable) || !verifiedExecutable(executable) {
		return protocol.McpServer{}, errCollabMCPExecutableUnavailable
	}
	endpoint := descriptor.Endpoint()
	capability := descriptor.Capability()
	if endpoint == "" || len(capability) != collab.CapabilityBytes {
		return protocol.McpServer{}, errors.New("coderig: collaboration MCP broker unavailable")
	}
	token, err := collab.EncodeCapabilityToken(capability)
	if err != nil {
		return protocol.McpServer{}, errors.New("coderig: collaboration MCP broker unavailable")
	}
	return protocol.McpServer{Stdio: &protocol.McpServerStdio{
		Name:    collabMCPExecutableName,
		Command: executable,
		Env: []protocol.EnvVariable{
			{Name: collab.EndpointEnv, Value: endpoint},
			{Name: collab.TokenEnv, Value: token},
		},
	}}, nil
}
