package client

import (
	"context"
	"time"

	"github.com/looprig/acp/protocol"
)

// FSHandler answers the client-served filesystem methods (fs/read_text_file,
// fs/write_text_file) a foreign agent may call back into this Client for. A
// nil FSHandler in Options means the filesystem capability is not advertised
// at all: InitializeRequest.ClientCapabilities.Fs is omitted, and the
// corresponding methods are never registered on the connection (so an agent
// that calls them anyway gets Conn's own MethodNotFound, exactly as if this
// Client had never heard of them).
type FSHandler interface {
	ReadTextFile(ctx context.Context, req protocol.ReadTextFileRequest) (protocol.ReadTextFileResponse, error)
	WriteTextFile(ctx context.Context, req protocol.WriteTextFileRequest) (protocol.WriteTextFileResponse, error)
}

// TerminalHandler answers the client-served terminal/* methods. A nil
// TerminalHandler means the terminal capability is not advertised
// (InitializeRequest.ClientCapabilities.Terminal is false) and none of the
// terminal/* methods are registered.
type TerminalHandler interface {
	CreateTerminal(ctx context.Context, req protocol.CreateTerminalRequest) (protocol.CreateTerminalResponse, error)
	TerminalOutput(ctx context.Context, req protocol.TerminalOutputRequest) (protocol.TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, req protocol.WaitForTerminalExitRequest) (protocol.WaitForTerminalExitResponse, error)
	KillTerminal(ctx context.Context, req protocol.KillTerminalRequest) (protocol.KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, req protocol.ReleaseTerminalRequest) (protocol.ReleaseTerminalResponse, error)
}

// PermissionHandler answers session/request_permission. A nil
// PermissionHandler means the method is never registered.
//
// Unlike FS and Terminal, the pinned ACP schema (protocol/methods_gen.go,
// protocol/types_gen.go's ClientCapabilities) has no boolean capability flag
// for permission support at all — session/request_permission is not
// something a client advertises support for or against on the wire; a client
// either implements it or does not. So "not advertised" here is purely a
// dispatch-level fact (the method is never registered, and an agent that
// calls it anyway gets MethodNotFound), not a bit flipped in
// InitializeRequest.
type PermissionHandler interface {
	RequestPermission(ctx context.Context, req protocol.RequestPermissionRequest) (protocol.RequestPermissionResponse, error)
}

// Options configures the client capabilities a Client advertises to, and
// dispatches on behalf of, a foreign agent.
//
// Elicitation is deliberately not one of these fields. The design doc's
// implementation-techniques section and this task's own contract describe an
// injectable Elicitation capability, but the pinned v1.20.0 ACP schema this
// module generates from has no elicitation method or capability at all — the
// same absence acp/internal/mockpeer/main.go already documents and, per the
// plan's own precedence rule ("when a generated-schema detail conflicts with
// this plan's exact constant names, the pinned artifact wins"), resolves the
// same way here: there is no wire method this package could dispatch an
// ElicitationHandler through, so none is invented.
type Options struct {
	// FS answers client-served filesystem methods. Nil disables the
	// capability entirely.
	FS FSHandler
	// Terminal answers client-served terminal/* methods. Nil disables the
	// capability entirely.
	Terminal TerminalHandler
	// Permissions answers session/request_permission. Nil disables it.
	Permissions PermissionHandler

	// ClientInfo identifies this client to the agent during "initialize".
	// Nil is valid: InitializeRequest.ClientInfo is optional.
	ClientInfo *protocol.Implementation

	// LoadTimeout overrides the package LoadTimeout constant as the bound
	// session/load's response is awaited under. Zero means "use LoadTimeout".
	LoadTimeout time.Duration
}

// withDefaults returns a copy of o with zero-valued fields that need a
// concrete default resolved (currently just LoadTimeout) filled in.
func (o Options) withDefaults() Options {
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = LoadTimeout
	}
	return o
}

// buildInitializeRequest constructs the "initialize" request this Client
// sends on every successful connection attempt: the pinned protocol version,
// caller-supplied ClientInfo (if any), and capability bits derived
// mechanically from which Options handlers are non-nil.
func (c *Client) buildInitializeRequest() protocol.InitializeRequest {
	return protocol.InitializeRequest{
		ProtocolVersion: protocol.CurrentProtocolVersion,
		ClientInfo:      c.opts.ClientInfo,
		ClientCapabilities: &protocol.ClientCapabilities{
			Fs:       fsCapabilities(c.opts.FS),
			Terminal: c.opts.Terminal != nil,
		},
	}
}

// fsCapabilities reports the FileSystemCapabilities this Client advertises
// for its Options.FS handler: both operations are always offered together
// (Options exposes one FSHandler covering both, not independently toggled
// read/write capabilities), or nil (omitted entirely) when no handler is
// configured.
func fsCapabilities(fs FSHandler) *protocol.FileSystemCapabilities {
	if fs == nil {
		return nil
	}
	return &protocol.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}
}
