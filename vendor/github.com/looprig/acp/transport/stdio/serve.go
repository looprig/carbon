package stdio

import (
	"context"
	"errors"
	"io"

	"github.com/looprig/acp/protocol"
)

// errNilConn reports that Serve was given a nil conn, which is always a
// caller error and never a valid transport state.
var errNilConn = errors.New("stdio: serve: conn is nil")

// Serve runs the agent-side stdio transport loop. conn must already be built
// by the caller over r and w (typically protocol.NewConn(r, w, opts), with
// r, w the same os.Stdin/os.Stdout) with any handlers registered; Serve's job
// is to translate ctx cancellation — of which Conn itself has no notion —
// into the transport closure that unblocks Conn's blocked read/write on r/w,
// and to return once the connection is over by either cause.
//
// Serve blocks until ctx is done or conn ends on its own (the peer
// disconnected, or something else closed conn). Either way, it ensures conn
// is closed and returns: ctx.Err() if ctx caused the return, or nil if conn
// ended on its own — ordinary peer disconnect is not itself an error worth
// reporting to Serve's caller.
func Serve(ctx context.Context, r io.Reader, w io.Writer, conn *protocol.Conn) error {
	if conn == nil {
		return errNilConn
	}

	select {
	case <-ctx.Done():
		// Conn has no context awareness of its own: closing r/w here is what
		// unblocks its read loop's pending Read (and any pending Write), and
		// conn.Close waits for that read loop to fully exit before
		// returning, so no goroutine is left running once Serve returns.
		closeIfCloser(r)
		closeIfCloser(w)
		_ = conn.Close()
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

func closeIfCloser(v any) {
	if c, ok := v.(io.Closer); ok {
		_ = c.Close()
	}
}
