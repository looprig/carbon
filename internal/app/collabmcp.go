package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/looprig/acp/protocol"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/mcp/pkg/collab"
)

const collabMCPExecutableName = "carbon-collab-mcp"

var errCollabMCPExecutableUnavailable = errors.New("carbon: collaboration MCP executable unavailable")

// verifiedExecutableSnapshot is the immutable path identity captured before a
// child builder is registered. Every path component is retained: a directory
// replacement can otherwise redirect the same textual executable path to a
// different file even when the final entry itself has not changed yet.
type verifiedExecutableSnapshot struct {
	path       string
	components []verifiedPathComponent
}

type verifiedPathComponent struct {
	path string
	info os.FileInfo
}

// resolveCollabMCPExecutable resolves the collaboration MCP process before a
// session is assembled. An explicit path is authoritative; an empty path uses
// the sibling of the current Carbon executable.
func resolveCollabMCPExecutable(configured string) (string, error) {
	if configured != "" {
		return resolveCollabMCPExecutableFrom(configured, "")
	}
	current, err := os.Executable()
	if err != nil {
		return "", errCollabMCPExecutableUnavailable
	}
	if current, err = filepath.EvalSymlinks(current); err != nil || !cleanAbsolutePath(current) {
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
	snapshot, ok := verifiedExecutableSnapshotFor(candidate)
	if !ok {
		return "", errCollabMCPExecutableUnavailable
	}
	return snapshot.path, nil
}

func verifiedExecutable(path string) bool {
	_, ok := verifiedExecutableSnapshotFor(path)
	return ok
}

func verifiedExecutableSnapshotFor(path string) (verifiedExecutableSnapshot, bool) {
	snapshot, ok := captureVerifiedExecutable(path)
	if !ok || !snapshot.matchesCurrent() {
		return verifiedExecutableSnapshot{}, false
	}
	return snapshot, true
}

func captureVerifiedExecutable(path string) (verifiedExecutableSnapshot, bool) {
	if !cleanAbsolutePath(path) {
		return verifiedExecutableSnapshot{}, false
	}
	// macOS commonly exposes the process temporary tree through /var while
	// the real tree is /private/var. Treat that OS-owned prefix as the
	// canonical starting point, then inspect every component below it. This
	// keeps ordinary t.TempDir paths usable without allowing an application
	// supplied symlinked parent to pass through the check.
	checkedPath := canonicalTempPath(path)
	components := pathComponents(checkedPath)
	if len(components) == 0 {
		return verifiedExecutableSnapshot{}, false
	}
	infos := make([]verifiedPathComponent, 0, len(components))
	for index, component := range components {
		info, err := os.Lstat(component)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return verifiedExecutableSnapshot{}, false
		}
		if index < len(components)-1 {
			if !info.IsDir() {
				return verifiedExecutableSnapshot{}, false
			}
			infos = append(infos, verifiedPathComponent{path: component, info: info})
			continue
		}
		if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return verifiedExecutableSnapshot{}, false
		}
		infos = append(infos, verifiedPathComponent{path: component, info: info})
	}
	resolved, err := filepath.EvalSymlinks(checkedPath)
	if err != nil || !cleanAbsolutePath(resolved) || resolved != checkedPath {
		return verifiedExecutableSnapshot{}, false
	}
	return verifiedExecutableSnapshot{path: path, components: infos}, true
}

func canonicalTempPath(path string) string {
	if runtime.GOOS != "darwin" {
		return path
	}
	const (
		darwinVarPath          = "/var"
		darwinCanonicalVarPath = "/private/var"
	)
	if path != darwinVarPath && !strings.HasPrefix(path, darwinVarPath+string(filepath.Separator)) {
		return path
	}
	return darwinCanonicalVarPath + strings.TrimPrefix(path, darwinVarPath)
}

func (snapshot verifiedExecutableSnapshot) matchesCurrent() bool {
	if !cleanAbsolutePath(snapshot.path) || len(snapshot.components) == 0 {
		return false
	}
	current, ok := captureVerifiedExecutable(snapshot.path)
	if !ok || current.path != snapshot.path || len(current.components) != len(snapshot.components) {
		return false
	}
	for index := range snapshot.components {
		previous := snapshot.components[index]
		fresh := current.components[index]
		if previous.path != fresh.path || !os.SameFile(previous.info, fresh.info) {
			return false
		}
	}
	return true
}

func pathComponents(path string) []string {
	var reversed []string
	for current := path; ; current = filepath.Dir(current) {
		reversed = append(reversed, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	components := make([]string, len(reversed))
	for index := range reversed {
		components[len(reversed)-1-index] = reversed[index]
	}
	return components
}

// collabMCPServerFor builds the one opaque collaboration MCP descriptor for a
// foreign ACP loop. Harness owns the descriptor authority; Carbon only maps
// its endpoint and token into the ACP stdio environment.
func collabMCPServerFor(executable string, descriptor foreign.BrokerDescriptor) (protocol.McpServer, error) {
	if !cleanAbsolutePath(executable) || !verifiedExecutable(executable) {
		return protocol.McpServer{}, errCollabMCPExecutableUnavailable
	}
	endpoint := descriptor.Endpoint()
	capability := descriptor.Capability()
	if endpoint == "" || len(capability) != collab.CapabilityBytes {
		return protocol.McpServer{}, errors.New("carbon: collaboration MCP broker unavailable")
	}
	token, err := collab.EncodeCapabilityToken(capability)
	if err != nil {
		return protocol.McpServer{}, errors.New("carbon: collaboration MCP broker unavailable")
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
