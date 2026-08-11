package client

import (
	"errors"
	"fmt"
	"time"

	"github.com/looprig/acp/protocol"
)

// ClosedError reports that a Client or Session operation was attempted after
// the underlying connection closed — whether by an explicit Close, the
// subprocess dying, or a transport failure. Cause, when set, is the
// concrete reason (typically a *protocol.ConnClosedError); Unwrap exposes it
// for errors.Is/errors.As.
type ClosedError struct {
	Cause error
}

func (e *ClosedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("acp/client: connection closed: %v", e.Cause)
	}
	return "acp/client: connection closed"
}

func (e *ClosedError) Unwrap() error { return e.Cause }

// NotDialedError reports that a Client method was called before Dial ever
// completed successfully.
type NotDialedError struct{}

func (e *NotDialedError) Error() string { return "acp/client: not dialed" }

// DuplicateSessionError reports that an operation attempted to register a
// Session ID that is already tracked by this Client. The ID is retained for
// programmatic inspection, but the bounded error text does not echo it.
type DuplicateSessionError struct {
	SessionID protocol.SessionID
}

func (e *DuplicateSessionError) Error() string {
	return "acp/client: session already registered"
}

// LoadTimeoutError reports that session/load's response did not arrive
// within the client's load timeout, even though replay updates may have
// already been consumed (see the design doc's "Replay-idle tolerance in the
// client"). No response is ever synthesized: a hung load is reported as a
// failure, typed so callers can distinguish it from an ordinary transport or
// protocol error.
type LoadTimeoutError struct {
	SessionID protocol.SessionID
	Timeout   time.Duration
}

func (e *LoadTimeoutError) Error() string {
	return fmt.Sprintf("acp/client: session/load: sessionId %q did not resolve within %s", e.SessionID, e.Timeout)
}

// SetModelUnsupportedError reports that Session.SetModel was called without
// a granted SetModelCapability: the connected agent's initialize response
// _meta never proved (see Client.ProveSetModelCapability) that it
// advertises the non-standard session/set_model extension, so the call was
// refused before ever reaching the wire.
type SetModelUnsupportedError struct{}

func (e *SetModelUnsupportedError) Error() string {
	return "acp/client: session/set_model: no proven _meta capability for this extension"
}

// wrapConnError normalizes an error returned by a protocol.Conn-backed call
// (AgentConn.*, Conn.Notify) into this package's typed vocabulary: a
// *protocol.ConnClosedError becomes a *ClosedError so callers of acp/client
// never need to import acp/protocol just to classify "the connection is
// gone." Any other error (in particular a *protocol.Fault carrying a real
// ACP error response) is returned unchanged, since it is already a
// meaningful typed error in its own right.
func wrapConnError(err error) error {
	if err == nil {
		return nil
	}
	var closedErr *protocol.ConnClosedError
	if errors.As(err, &closedErr) {
		return &ClosedError{Cause: closedErr}
	}
	return err
}
