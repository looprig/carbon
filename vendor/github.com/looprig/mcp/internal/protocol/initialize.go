// This file holds the initialize-handshake conversion. It is separate from
// conv.go because the handshake is the one exchange whose result the client
// keeps for the life of a connection, and it is the only conversion that can
// reject the connection outright.

package protocol

import (
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
)

// FromSDKInitializeResult converts the SDK's handshake result.
//
// A missing protocol version is fatal: the whole point of the handshake is to
// learn which protocol the peer speaks, and a client that guesses one is a
// client that mis-parses everything afterwards. Everything else is tolerated —
// an anonymous server (no serverInfo) and one advertising no capabilities are
// both legal — because neither prevents the connection from working, and both
// are already visible to the caller in the converted result.
//
// Every string the server supplies — its identity, the protocol version and the
// instructions — is truncated to b.MaxTextBytes rather than rejected. All three
// are retained for the life of the connection and rendered freely afterwards,
// so none may be unbounded; but a server padding one must not be able to fail
// the connection, only to have its padding dropped.
func FromSDKInitializeResult(r *mcp.InitializeResult, b Bounds) (InitializeResult, error) {
	if r == nil {
		return InitializeResult{}, fmt.Errorf("%w: initialize result", errNilInput)
	}
	if r.ProtocolVersion == "" {
		return InitializeResult{}, errors.New("protocol: initialize result has no protocol version")
	}
	version, _ := limits.TruncateText(r.ProtocolVersion, b.MaxTextBytes)
	instructions, _ := limits.TruncateText(r.Instructions, b.MaxTextBytes)
	return InitializeResult{
		Server:          FromSDKServerIdentity(r.ServerInfo, b),
		ProtocolVersion: ProtocolVersion(version),
		Instructions:    instructions,
		Capabilities:    fromSDKServerCapabilities(r.Capabilities),
	}, nil
}

// fromSDKServerCapabilities reduces the SDK's nillable capability structs to
// presence flags. A nil ServerCapabilities means the server advertised nothing,
// which is not an error: it is a server with no optional features.
func fromSDKServerCapabilities(c *mcp.ServerCapabilities) ServerCapabilities {
	if c == nil {
		return ServerCapabilities{}
	}
	caps := ServerCapabilities{
		Tools:     c.Tools != nil,
		Prompts:   c.Prompts != nil,
		Resources: c.Resources != nil,
		//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
		Logging:     c.Logging != nil,
		Completions: c.Completions != nil,
	}
	if c.Resources != nil {
		caps.ResourcesSubscribe = c.Resources.Subscribe
	}
	return caps
}
