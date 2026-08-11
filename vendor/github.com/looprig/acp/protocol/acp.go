// acp.go is the typed protocol surface over Conn: it binds the generated
// method-name constants in methods_gen.go to the generated request/response
// types in types_gen.go, one Go method per ACP RPC. It adds no behavior of
// its own — Conn already owns id minting, framing, concurrency, and error
// mapping (see conn.go) — these are thin, mechanical wrappers around
// Conn.Call and Conn.Notify.
//
// AgentConn is the surface a client uses to call an agent. ClientConn is
// the mirror: the surface an agent uses to call a client (permission
// requests, file I/O, terminal operations, session update notifications).
// CreateTerminal returns a *TerminalHandle bundling the session and
// terminal id, so Output/WaitForExit/Kill/Release never need them
// threaded through by the caller.
package protocol

import "context"

// AgentConn is the typed surface for the methods a client calls on an
// agent: one method per entry in AgentMethods.
type AgentConn struct {
	conn *Conn
}

// NewAgentConn wraps conn as the typed agent-served method surface.
func NewAgentConn(conn *Conn) *AgentConn {
	return &AgentConn{conn: conn}
}

// Conn returns the underlying Conn, for callers that also need direct
// access to it (Close, Done, extension traffic, and so on).
func (a *AgentConn) Conn() *Conn { return a.conn }

// CallExtensionWithResult is the protocol-owned generic primitive for a
// vendor/extension request. It deliberately lives on AgentConn rather than
// in acp/client's public surface so typed client methods can use the disjoint
// extension id space without exposing arbitrary method probing to callers.
func (a *AgentConn) CallExtensionWithResult(ctx context.Context, method string, params, result any) (CallResult, error) {
	return a.conn.call(ctx, method, params, result, a.conn.mintExtID)
}

// StartExtensionCall is the asynchronous counterpart used by fixed typed
// extension surfaces. The method string stays in protocol so callers above
// this package cannot turn the typed client API into arbitrary probing.
func (a *AgentConn) StartExtensionCall(ctx context.Context, method string, params, result any) (*CallHandle, error) {
	return a.conn.startCall(ctx, method, params, result, a.conn.mintExtID)
}

// Initialize calls the agent's "initialize" method.
func (a *AgentConn) Initialize(ctx context.Context, req InitializeRequest) (*InitializeResponse, error) {
	var resp InitializeResponse
	if err := a.conn.Call(ctx, string(MethodInitialize), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Authenticate calls the agent's "authenticate" method.
func (a *AgentConn) Authenticate(ctx context.Context, req AuthenticateRequest) (*AuthenticateResponse, error) {
	var resp AuthenticateResponse
	if err := a.conn.Call(ctx, string(MethodAuthenticate), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// NewSession calls the agent's "session/new" method.
func (a *AgentConn) NewSession(ctx context.Context, req NewSessionRequest) (*NewSessionResponse, error) {
	var resp NewSessionResponse
	if err := a.conn.Call(ctx, string(MethodSessionNew), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// LoadSession calls the agent's "session/load" method.
func (a *AgentConn) LoadSession(ctx context.Context, req LoadSessionRequest) (*LoadSessionResponse, error) {
	var resp LoadSessionResponse
	if err := a.conn.Call(ctx, string(MethodSessionLoad), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ResumeSession calls the agent's "session/resume" method.
func (a *AgentConn) ResumeSession(ctx context.Context, req ResumeSessionRequest) (*ResumeSessionResponse, error) {
	var resp ResumeSessionResponse
	if err := a.conn.Call(ctx, string(MethodSessionResume), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListSessions calls the agent's "session/list" method.
func (a *AgentConn) ListSessions(ctx context.Context, req ListSessionsRequest) (*ListSessionsResponse, error) {
	var resp ListSessionsResponse
	if err := a.conn.Call(ctx, string(MethodSessionList), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CloseSession calls the agent's "session/close" method.
func (a *AgentConn) CloseSession(ctx context.Context, req CloseSessionRequest) (*CloseSessionResponse, error) {
	var resp CloseSessionResponse
	if err := a.conn.Call(ctx, string(MethodSessionClose), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteSession calls the agent's "session/delete" method.
func (a *AgentConn) DeleteSession(ctx context.Context, req DeleteSessionRequest) (*DeleteSessionResponse, error) {
	var resp DeleteSessionResponse
	if err := a.conn.Call(ctx, string(MethodSessionDelete), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Prompt calls the agent's "session/prompt" method.
func (a *AgentConn) Prompt(ctx context.Context, req PromptRequest) (*PromptResponse, error) {
	var resp PromptResponse
	if err := a.conn.Call(ctx, string(MethodSessionPrompt), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PromptWithResult is Prompt's additive ordered form. It exposes the
// response receive sequence and writer-admission fact while preserving the
// original Prompt API for callers that do not need ordering evidence.
func (a *AgentConn) PromptWithResult(ctx context.Context, req PromptRequest) (*PromptResponse, CallResult, error) {
	var resp PromptResponse
	facts, err := a.conn.CallWithResult(ctx, string(MethodSessionPrompt), req, &resp)
	if err != nil {
		return nil, facts, err
	}
	return &resp, facts, nil
}

// Cancel sends the "session/cancel" notification. It never blocks on a
// response: ACP notifications have none.
func (a *AgentConn) Cancel(ctx context.Context, n CancelNotification) error {
	return a.conn.Notify(ctx, string(MethodSessionCancel), n)
}

// SetConfigOption calls the agent's "session/set_config_option" method.
func (a *AgentConn) SetConfigOption(ctx context.Context, req SetSessionConfigOptionRequest) (*SetSessionConfigOptionResponse, error) {
	var resp SetSessionConfigOptionResponse
	if err := a.conn.Call(ctx, string(MethodSessionSetConfigOption), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetMode calls the agent's "session/set_mode" method.
func (a *AgentConn) SetMode(ctx context.Context, req SetSessionModeRequest) (*SetSessionModeResponse, error) {
	var resp SetSessionModeResponse
	if err := a.conn.Call(ctx, string(MethodSessionSetMode), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ClientConn is the typed surface for the methods an agent calls on a
// client: one method per entry in ClientMethods.
type ClientConn struct {
	conn *Conn
}

// NewClientConn wraps conn as the typed client-served method surface.
func NewClientConn(conn *Conn) *ClientConn {
	return &ClientConn{conn: conn}
}

// Conn returns the underlying Conn, for callers that also need direct
// access to it (Close, Done, extension traffic, and so on).
func (c *ClientConn) Conn() *Conn { return c.conn }

// RequestPermission calls the client's "session/request_permission" method.
func (c *ClientConn) RequestPermission(ctx context.Context, req RequestPermissionRequest) (*RequestPermissionResponse, error) {
	var resp RequestPermissionResponse
	if err := c.conn.Call(ctx, string(MethodSessionRequestPermission), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReadTextFile calls the client's "fs/read_text_file" method.
func (c *ClientConn) ReadTextFile(ctx context.Context, req ReadTextFileRequest) (*ReadTextFileResponse, error) {
	var resp ReadTextFileResponse
	if err := c.conn.Call(ctx, string(MethodFsReadTextFile), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// WriteTextFile calls the client's "fs/write_text_file" method.
func (c *ClientConn) WriteTextFile(ctx context.Context, req WriteTextFileRequest) (*WriteTextFileResponse, error) {
	var resp WriteTextFileResponse
	if err := c.conn.Call(ctx, string(MethodFsWriteTextFile), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateTerminal calls the client's "terminal/create" method and returns a
// *TerminalHandle bundling the session id (from req) and the terminal id
// (from the response), so Output/WaitForExit/Kill/Release never need to be
// given either again.
func (c *ClientConn) CreateTerminal(ctx context.Context, req CreateTerminalRequest) (*TerminalHandle, error) {
	var resp CreateTerminalResponse
	if err := c.conn.Call(ctx, string(MethodTerminalCreate), req, &resp); err != nil {
		return nil, err
	}
	return &TerminalHandle{
		conn:       c.conn,
		sessionID:  req.SessionID,
		terminalID: resp.TerminalID,
	}, nil
}

// SessionUpdate sends the "session/update" notification. It never blocks on
// a response: ACP notifications have none.
func (c *ClientConn) SessionUpdate(ctx context.Context, n SessionNotification) error {
	return c.conn.Notify(ctx, string(MethodSessionUpdate), n)
}

// TerminalHandle is a bundled handle to one terminal created via
// ClientConn.CreateTerminal. It pre-binds the session id and terminal id so
// that Output, WaitForExit, Kill, and Release never require the caller to
// thread either back through.
type TerminalHandle struct {
	conn       *Conn
	sessionID  SessionID
	terminalID TerminalID
}

// ID returns the terminal id this handle is bound to.
func (t *TerminalHandle) ID() TerminalID { return t.terminalID }

// Output calls the client's "terminal/output" method for this terminal.
func (t *TerminalHandle) Output(ctx context.Context) (*TerminalOutputResponse, error) {
	req := TerminalOutputRequest{SessionID: t.sessionID, TerminalID: t.terminalID}
	var resp TerminalOutputResponse
	if err := t.conn.Call(ctx, string(MethodTerminalOutput), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// WaitForExit calls the client's "terminal/wait_for_exit" method for this
// terminal.
func (t *TerminalHandle) WaitForExit(ctx context.Context) (*WaitForTerminalExitResponse, error) {
	req := WaitForTerminalExitRequest{SessionID: t.sessionID, TerminalID: t.terminalID}
	var resp WaitForTerminalExitResponse
	if err := t.conn.Call(ctx, string(MethodTerminalWaitForExit), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Kill calls the client's "terminal/kill" method for this terminal.
func (t *TerminalHandle) Kill(ctx context.Context) (*KillTerminalResponse, error) {
	req := KillTerminalRequest{SessionID: t.sessionID, TerminalID: t.terminalID}
	var resp KillTerminalResponse
	if err := t.conn.Call(ctx, string(MethodTerminalKill), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Release calls the client's "terminal/release" method for this terminal.
func (t *TerminalHandle) Release(ctx context.Context) (*ReleaseTerminalResponse, error) {
	req := ReleaseTerminalRequest{SessionID: t.sessionID, TerminalID: t.terminalID}
	var resp ReleaseTerminalResponse
	if err := t.conn.Call(ctx, string(MethodTerminalRelease), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
