// This file wires Handlers.Roots to the connection: it turns the application's
// RootsProvider into the neutral callback a transport installs, so that a server
// asking roots/list learns the roots the provider supplies — and only those.
//
// Roots are unlike elicitation and sampling in one way that shapes this file: a
// server does not initiate a request this module then serves. The SDK answers
// roots/list from a set the client hands it at connect time, so the provider is
// consulted once per connection (see internal/protocol installRoots), not on
// every server request. Consulting it there rather than here keeps the SDK the
// single answerer and this the single source of what it answers with.

package client

import (
	"context"

	"github.com/looprig/mcp/internal/protocol"
)

// rootsAdapter wraps the application's roots provider into the neutral callback
// the connection takes. A nil provider stays nil, which is what makes "no
// provider, no capability" true end to end rather than only at Connect: the
// protocol layer advertises nothing for a nil callback, so the SDK cannot offer
// roots on this client's behalf.
//
// The roots it returns are exactly the provider's — converted to neutral types
// and nothing more. This module never adds a host filesystem root of its own: a
// server learns the workspace view the host chose to expose, and no wider one.
func (c *Client) rootsAdapter() func(context.Context) ([]protocol.Root, error) {
	if c.rootsProvider == nil {
		return nil
	}
	return func(ctx context.Context) ([]protocol.Root, error) {
		roots, err := c.rootsProvider.Roots(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]protocol.Root, 0, len(roots))
		for _, r := range roots {
			out = append(out, protocol.Root{URI: r.URI, Name: r.Name})
		}
		return out, nil
	}
}
