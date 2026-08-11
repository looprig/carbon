// This file wires Handlers.Elicitation to the connection: it is the producer
// that turns a server's elicitation/create into a call on the application's
// handler, and the handler's answer back into a result the server sees.
//
// It is the only place in this package where a server-initiated request is
// served, which makes it the only place where foreign code (a handler) runs
// while a server waits. Everything here follows from that: the call is bounded
// by the binding's elicitation timeout, the answer is validated before it can
// reach the wire, and every failure is a refusal the server is told about
// rather than a silence it waits out.
//
// One thing here is unlike every other error path in this package: the errors
// this file returns travel *to the server*, as the JSON-RPC error answering its
// request. That is why each is built with an explicit message — an *Error with a
// Msg does not render its wrapped cause (see Error.Error), so a handler's own
// error text, which is the host's business and may name anything at all, stays
// on this side of the wire while the server still learns that its request
// failed.

package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/mcp/internal/protocol"
)

// opElicit names the operation carried by this file's errors.
const opElicit = "elicit"

// elicitAdapter wraps the application's elicitation handler into the neutral
// callback the connection takes. A nil handler stays nil, which is what makes
// "no handler, no capability" true end to end rather than only at Connect: the
// protocol layer registers nothing for a nil callback, so the SDK cannot
// advertise elicitation on this client's behalf.
func (c *Client) elicitAdapter() func(context.Context, protocol.ElicitRequest) (protocol.ElicitResult, error) {
	if c.elicitHandler == nil {
		return nil
	}
	return c.serveElicit
}

// serveElicit runs one elicitation: validate, ask, validate the answer.
//
// It runs on the connection's dispatch goroutine while the server waits for a
// reply, so it holds nothing of the Client's: emit is called with no lock held,
// per its contract, and the handler is foreign code that may legitimately block
// for as long as the elicitation timeout allows.
func (c *Client) serveElicit(ctx context.Context, r protocol.ElicitRequest) (protocol.ElicitResult, error) {
	req, err := c.elicitRequest(r)
	if err != nil {
		return protocol.ElicitResult{}, err
	}

	started := time.Now()
	c.emit(ElicitationRequested{Binding: c.def.Name, Mode: req.Mode, At: started})

	// The binding's own deadline, always: a server that asks a question nobody
	// answers must not pin a dispatch goroutine and a handler forever. It is
	// derived from ctx rather than replacing it, so a connection going down
	// still cancels the wait.
	ctx, cancel := context.WithTimeout(ctx, c.def.Timeouts.Elicitation)
	defer cancel()

	res, err := c.elicitHandler.Elicit(ctx, req)
	if err != nil {
		// The zero Action, here and below: the server is being told this
		// failed, and no answer is invented for a person who never gave one.
		c.emit(c.elicitResolved(req.Mode, 0, started))
		return protocol.ElicitResult{}, c.elicitFailed(ctx, err)
	}

	action, err := toProtocolElicitAction(res.Action)
	if err != nil {
		c.emit(c.elicitResolved(req.Mode, 0, started))
		return protocol.ElicitResult{}, NewError(FailureElicitationInvalid, c.def.Name, opElicit,
			"the elicitation handler returned no valid action", err)
	}
	c.emit(c.elicitResolved(req.Mode, res.Action, started))
	return protocol.ElicitResult{Action: action, Content: res.Content}, nil
}

// elicitFailed classifies a handler's error.
//
// A handler that ran out of time is worth distinguishing from one that broke:
// the first means a person did not answer, which is ordinary, and the second
// means the host could not ask, which is not. ctx is consulted rather than the
// error alone because a handler is free to return its own error on a deadline
// rather than ctx.Err().
func (c *Client) elicitFailed(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return NewError(FailureElicitationTimeout, c.def.Name, opElicit,
			"the elicitation was not answered within this binding's timeout", err)
	}
	return NewError(FailureElicitationInvalid, c.def.Name, opElicit,
		"the elicitation handler failed", err)
}

// elicitResolved builds the outcome event, measuring the handler's own elapsed
// time from started.
func (c *Client) elicitResolved(mode ElicitMode, action ElicitAction, started time.Time) ElicitationResolved {
	now := time.Now()
	return ElicitationResolved{
		Binding:  c.def.Name,
		Mode:     mode,
		Action:   action,
		Duration: now.Sub(started),
		At:       now,
	}
}

// elicitRequest converts a neutral request into the application's, re-checking
// what the boundary already checked.
//
// This is defence in depth and is deliberate: internal/protocol bounds and
// validates every elicitation it converts, but Conn is an interface and the
// callback is installed on a config a transport is handed. The client is what
// promises Handlers.Elicitation a bounded message and a declared mode, so the
// client verifies it — the same reason start() re-checks a handshake's protocol
// version that internal/protocol has already rejected.
func (c *Client) elicitRequest(r protocol.ElicitRequest) (ElicitRequest, error) {
	mode, err := fromProtocolElicitMode(r.Mode)
	if err != nil {
		return ElicitRequest{}, NewError(FailureElicitationInvalid, c.def.Name, opElicit,
			"the server asked for an elicitation this client cannot describe to a human", err)
	}
	if len(r.Message) > c.def.Limits.MaxElicitMessageBytes {
		return ElicitRequest{}, NewError(FailureElicitationInvalid, c.def.Name, opElicit,
			fmt.Sprintf("the elicitation prompt exceeds this binding's %d byte limit",
				c.def.Limits.MaxElicitMessageBytes), nil)
	}
	return ElicitRequest{
		Binding:       c.def.Name,
		Mode:          mode,
		Message:       r.Message,
		Schema:        r.Schema,
		URL:           r.URL,
		ElicitationID: r.ElicitationID,
	}, nil
}

// fromProtocolElicitMode maps the neutral mode onto this package's. An
// unrecognized value is refused rather than passed on: this is the boundary that
// promises a handler a mode it can switch on.
func fromProtocolElicitMode(m protocol.ElicitMode) (ElicitMode, error) {
	switch m {
	case protocol.ElicitModeForm:
		return ElicitModeForm, nil
	case protocol.ElicitModeURL:
		return ElicitModeURL, nil
	default:
		return 0, fmt.Errorf("unknown elicitation mode %d", m)
	}
}

// toProtocolElicitAction maps a handler's action onto the neutral one. The zero
// action is not a valid answer — a handler must say what happened, because the
// alternative is this client choosing an answer on a person's behalf.
func toProtocolElicitAction(a ElicitAction) (protocol.ElicitAction, error) {
	switch a {
	case ElicitAccept:
		return protocol.ElicitAccept, nil
	case ElicitDecline:
		return protocol.ElicitDecline, nil
	case ElicitCancel:
		return protocol.ElicitCancel, nil
	default:
		return 0, fmt.Errorf("invalid elicitation action %d", a)
	}
}
