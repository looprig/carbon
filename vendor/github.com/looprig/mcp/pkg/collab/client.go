package collab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

// DialFunc is the narrow connection seam used by Client. Production clients
// use a Unix-domain socket; tests and embedders may provide an equivalent
// local connection without changing the framing or authentication policy.
type DialFunc func(context.Context, string) (net.Conn, error)

// Client performs one authenticated request per broker connection. It is safe
// for concurrent use because no mutable connection is shared between calls.
type Client struct {
	cfg  ClientConfig
	dial DialFunc
}

// NewClient validates configuration and creates a Unix-domain broker client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	return newClient(cfg, dialEndpoint)
}

// New is a concise alias for NewClient.
func New(cfg ClientConfig) (*Client, error) { return NewClient(cfg) }

// NewClientWithDialer creates a client with an injected local dialer. The
// endpoint and capability are still validated exactly as in NewClient.
func NewClientWithDialer(cfg ClientConfig, dial DialFunc) (*Client, error) {
	return newClient(cfg, dial)
}

func newClient(cfg ClientConfig, dial DialFunc) (*Client, error) {
	if dial == nil {
		return nil, ErrInvalidConfig
	}
	normalized, err := cfg.normalized()
	if err != nil {
		if errors.Is(err, ErrInvalidCapability) {
			return nil, ErrInvalidCapability
		}
		return nil, ErrInvalidConfig
	}
	return &Client{cfg: normalized, dial: dial}, nil
}

// Config returns a defensive copy of the immutable client policy.
func (c *Client) Config() ClientConfig {
	if c == nil {
		return ClientConfig{}
	}
	cfg := c.cfg
	cfg.Capability = append([]byte(nil), c.cfg.Capability...)
	cfg.Token = nil
	return cfg
}

// Call sends one MessageAgent request and decodes its public result. Request
// validation completes before dialing the broker.
func (c *Client) Call(ctx context.Context, request MessageAgentRequest) (DelegateResult, error) {
	raw, err := c.CallJSON(ctx, request)
	if err != nil {
		return DelegateResult{}, err
	}
	return DecodeDelegateResult(raw)
}

// MessageAgent is an explicit operation-named alias for Call.
func (c *Client) MessageAgent(ctx context.Context, request MessageAgentRequest) (DelegateResult, error) {
	return c.Call(ctx, request)
}

// Send is a concise operation-named alias for Call.
func (c *Client) Send(ctx context.Context, request MessageAgentRequest) (DelegateResult, error) {
	return c.Call(ctx, request)
}

// CallJSON returns the bounded, validated public result JSON without exposing
// broker-internal request or capability data. Keeping the original bytes also
// preserves the distinction between an omitted and an explicitly empty
// response field in the public envelope.
func (c *Client) CallJSON(ctx context.Context, request MessageAgentRequest) (json.RawMessage, error) {
	if c == nil || c.dial == nil {
		return nil, ErrInvalidConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateMessageAgentRequest(request); err != nil {
		return nil, err
	}
	encodedRequest, err := json.Marshal(request)
	if err != nil || len(encodedRequest) > c.cfg.MaxFrameBytes {
		return nil, ErrInputLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, redactWireError(ErrDeadline, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.ConnectTimeout)
	conn, err := c.dial(dialCtx, c.cfg.Endpoint)
	dialCause := dialCtx.Err()
	cancel()
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return nil, redactWireError(ErrDeadline, cause)
		}
		if dialCause != nil {
			return nil, redactWireError(ErrDeadline, dialCause)
		}
		return nil, ErrConnection
	}
	if conn == nil {
		return nil, ErrConnection
	}
	defer conn.Close()

	if err := c.admit(ctx, conn, encodedRequest); err != nil {
		if ctx.Err() != nil {
			return nil, redactWireError(ErrDeadline, ctx.Err())
		}
		return nil, err
	}

	response, err := readResponse(ctx, conn, c.cfg.MaxFrameBytes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, redactWireError(ErrDeadline, ctx.Err())
		}
		if errors.Is(err, io.EOF) {
			// The broker deliberately closes unauthenticated peers without
			// returning a diagnostic. Keep that failure categorical and secret-free.
			return nil, ErrAuthentication
		}
		if errors.Is(err, ErrFrameLimit) {
			return nil, ErrFrameLimit
		}
		return nil, ErrResponse
	}
	if _, err := DecodeDelegateResult(response); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), response...), nil
}

// CallRaw is an explicit raw-JSON alias for CallJSON.
func (c *Client) CallRaw(ctx context.Context, request MessageAgentRequest) (json.RawMessage, error) {
	return c.CallJSON(ctx, request)
}

func (c *Client) admit(ctx context.Context, conn net.Conn, request []byte) error {
	stop := closeOnCancel(ctx, conn)
	defer stop()
	deadline := time.Now().Add(c.cfg.AdmissionTimeout)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return redactWireError(ErrAdmission, err)
	}
	if err := WriteHandshake(conn, c.cfg.Capability); err != nil {
		return redactWireError(ErrAdmission, err)
	}
	if err := WriteFrameLimit(conn, request, c.cfg.MaxFrameBytes); err != nil {
		return redactWireError(ErrAdmission, err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return redactWireError(ErrAdmission, err)
	}
	return nil
}

func closeOnCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// WriteFrameLimit is the configurable-bound counterpart to WriteFrame. It is
// exported so the Harness broker can use the same framing without importing a
// transport implementation.
func WriteFrameLimit(w io.Writer, payload []byte, max int) error {
	if max < 1 || max > MaxFrameBytes {
		return ErrInvalidConfig
	}
	return writeFrameLimit(w, payload, max)
}

func readResponse(ctx context.Context, conn net.Conn, max int) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, redactWireError(ErrResponse, err)
		}
	}
	if done := ctx.Done(); done != nil {
		readDone := make(chan struct{})
		go func() {
			select {
			case <-done:
				_ = conn.Close()
			case <-readDone:
			}
		}()
		response, err := readFrameLimit(conn, max)
		close(readDone)
		return response, err
	}
	return readFrameLimit(conn, max)
}
