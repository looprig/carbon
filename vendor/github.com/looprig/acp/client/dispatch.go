// dispatch.go registers this Client's client-served ACP method handlers on a
// connected protocol.Conn: session/update (always), and
// session/request_permission / fs/* / terminal/* only when the
// corresponding Options handler is configured (see options.go's capability
// doc). Every handler validates its inbound sessionId (and, where
// applicable, path or terminalId) before ever invoking the injected handler,
// per acp/CLAUDE.md's boundary-validation rule: a foreign agent's requests
// are untrusted input, not a trusted internal call.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/looprig/acp/protocol"
)

// registerClientHandlers binds every client-served method this Client
// implements onto conn. session/update is always registered (a Client
// always tracks its own sessions' updates); the rest are registered only
// when their capability is configured, so an unconfigured method is left
// entirely unregistered and falls through to Conn's own built-in
// MethodNotFound response — no separate "capability disabled" error path is
// needed.
func (c *Client) registerClientHandlers(conn *protocol.Conn) {
	conn.HandleNotifyWithSequence(string(protocol.MethodSessionUpdate), c.handleSessionUpdateNotify)

	if c.opts.Permissions != nil {
		conn.Handle(string(protocol.MethodSessionRequestPermission), c.handleRequestPermission)
	}
	if c.opts.FS != nil {
		conn.Handle(string(protocol.MethodFsReadTextFile), c.handleReadTextFile)
		conn.Handle(string(protocol.MethodFsWriteTextFile), c.handleWriteTextFile)
	}
	if c.opts.Terminal != nil {
		conn.Handle(string(protocol.MethodTerminalCreate), c.handleCreateTerminal)
		conn.Handle(string(protocol.MethodTerminalOutput), c.handleTerminalOutput)
		conn.Handle(string(protocol.MethodTerminalWaitForExit), c.handleWaitForTerminalExit)
		conn.Handle(string(protocol.MethodTerminalKill), c.handleKillTerminal)
		conn.Handle(string(protocol.MethodTerminalRelease), c.handleReleaseTerminal)
	}
}

// validateSessionID rejects an empty id, or one this Client has no
// registered Session for, before any handler (injected or built-in) is
// invoked. A foreign agent has no legitimate reason to reference a session
// id this Client never created/loaded/resumed.
func (c *Client) validateSessionID(id protocol.SessionID) error {
	if id == "" {
		return protocol.InvalidParams("sessionId is required", nil)
	}
	c.sessionsMu.Lock()
	_, ok := c.sessions[id]
	c.sessionsMu.Unlock()
	if !ok {
		return protocol.ResourceNotFound("unknown sessionId", nil)
	}
	return nil
}

// validateAbsolutePath rejects an empty path, or one that is not already a
// clean, absolute path — ACP requires fs/terminal paths to be absolute (see
// protocol/types_gen.go's ReadTextFileRequest/WriteTextFileRequest/
// CreateTerminalRequest docs) — before it ever reaches an injected handler.
func validateAbsolutePath(path string) error {
	if path == "" {
		return protocol.InvalidParams("path is required", nil)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return protocol.InvalidParams("path must be a clean, absolute path", nil)
	}
	return nil
}

// validateTerminalID rejects an empty terminal id before it reaches an
// injected handler.
func validateTerminalID(id protocol.TerminalID) error {
	if id == "" {
		return protocol.InvalidParams("terminalId is required", nil)
	}
	return nil
}

// wrapHandlerError normalizes an error returned by an injected Options
// handler into a wire-appropriate fault: a *protocol.Fault the handler
// itself constructed (for example protocol.ResourceNotFound for a missing
// file) is passed through unchanged so it reaches the agent with its
// intended code; any other error is reported as InternalError, with the
// original error retained only for local diagnosis (see protocol.Fault's
// own doc — it is never serialized to the peer).
func wrapHandlerError(op string, err error) error {
	if err == nil {
		return nil
	}
	var fault *protocol.Fault
	if errors.As(err, &fault) {
		return fault
	}
	return protocol.InternalError(op+": "+err.Error(), err)
}

// handleSessionUpdateNotify decodes an inbound session/update notification
// and routes it to the tracked Session it names. An update naming a
// sessionId this Client has no (or no longer has a) registered Session for
// cannot be routed anywhere useful; it is dropped and counted (see
// DroppedUpdates), mirroring protocol.Conn.DroppedNotifications rather than
// silently vanishing with no way to observe it. A malformed notification
// (fails to decode) is dropped the same way: there is no reliable sessionId
// to attribute it to.
func (c *Client) handleSessionUpdateNotify(_ context.Context, _ string, params json.RawMessage, receiveSequence uint64) {
	var n protocol.SessionNotification
	if err := json.Unmarshal(params, &n); err != nil {
		c.countDroppedUpdate()
		return
	}

	c.sessionsMu.Lock()
	sess, ok := c.sessions[n.SessionID]
	c.sessionsMu.Unlock()
	if !ok {
		c.countDroppedUpdate()
		return
	}

	sess.deliver(Update{SessionUpdate: n.Update, Meta: DecodeUpdateMeta(n.Meta), ReceiveSequence: receiveSequence})
}

func (c *Client) countDroppedUpdate() {
	c.sessionsMu.Lock()
	c.droppedUpdates++
	c.sessionsMu.Unlock()
}

func (c *Client) handleRequestPermission(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.RequestPermissionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("session/request_permission: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	resp, err := c.opts.Permissions.RequestPermission(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("session/request_permission", err)
	}
	return resp, nil
}

func (c *Client) handleReadTextFile(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.ReadTextFileRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("fs/read_text_file: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateAbsolutePath(req.Path); err != nil {
		return nil, err
	}
	resp, err := c.opts.FS.ReadTextFile(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("fs/read_text_file", err)
	}
	return resp, nil
}

func (c *Client) handleWriteTextFile(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.WriteTextFileRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("fs/write_text_file: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateAbsolutePath(req.Path); err != nil {
		return nil, err
	}
	resp, err := c.opts.FS.WriteTextFile(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("fs/write_text_file", err)
	}
	return resp, nil
}

func (c *Client) handleCreateTerminal(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.CreateTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("terminal/create: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if req.Command == "" {
		return nil, protocol.InvalidParams("terminal/create: command is required", nil)
	}
	resp, err := c.opts.Terminal.CreateTerminal(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("terminal/create", err)
	}
	return resp, nil
}

func (c *Client) handleTerminalOutput(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.TerminalOutputRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("terminal/output: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateTerminalID(req.TerminalID); err != nil {
		return nil, err
	}
	resp, err := c.opts.Terminal.TerminalOutput(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("terminal/output", err)
	}
	return resp, nil
}

func (c *Client) handleWaitForTerminalExit(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.WaitForTerminalExitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("terminal/wait_for_exit: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateTerminalID(req.TerminalID); err != nil {
		return nil, err
	}
	resp, err := c.opts.Terminal.WaitForTerminalExit(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("terminal/wait_for_exit", err)
	}
	return resp, nil
}

func (c *Client) handleKillTerminal(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.KillTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("terminal/kill: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateTerminalID(req.TerminalID); err != nil {
		return nil, err
	}
	resp, err := c.opts.Terminal.KillTerminal(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("terminal/kill", err)
	}
	return resp, nil
}

func (c *Client) handleReleaseTerminal(ctx context.Context, _ string, params json.RawMessage) (any, error) {
	var req protocol.ReleaseTerminalRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, protocol.InvalidParams("terminal/release: decode params", err)
	}
	if err := c.validateSessionID(req.SessionID); err != nil {
		return nil, err
	}
	if err := validateTerminalID(req.TerminalID); err != nil {
		return nil, err
	}
	resp, err := c.opts.Terminal.ReleaseTerminal(ctx, req)
	if err != nil {
		return nil, wrapHandlerError("terminal/release", err)
	}
	return resp, nil
}
