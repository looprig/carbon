// Package stdio is the process-boundary transport for the Agent Client
// Protocol bridge: it carries a *protocol.Conn over a process's stdin and
// stdout. Serve wires an agent process's own standard streams to a Conn
// already built by the caller (the agent side); Spawn starts and supervises a
// child process whose stdin/stdout carry the other end of that same Conn (the
// client side, used by acp/client).
//
// This package is stdlib-only plus acp/protocol: it never imports
// github.com/looprig/harness or github.com/looprig/core, directly or
// transitively (see acp/CLAUDE.md).
package stdio
