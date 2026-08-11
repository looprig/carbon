package client

import (
	"encoding/json"

	"github.com/looprig/acp/protocol"
)

// InitializeMetadata is a defensive snapshot of the metadata returned by an
// agent's initialize handshake. AgentInfo and Meta are copied for each read;
// mutating either field (or AgentInfo's nested fields) never changes the
// Client's stored handshake response.
type InitializeMetadata struct {
	AgentInfo *protocol.Implementation
	Meta      json.RawMessage
}

// InitializeMetadata returns the connected agent's initialize metadata.
// Before a successful Dial it returns a *NotDialedError; after Client reaches
// its terminal closed state it returns a *ClosedError, matching the lifecycle
// behavior of the other Client accessors.
func (c *Client) InitializeMetadata() (InitializeMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case dialReady:
		return initializeMetadataSnapshot(c.initRes), nil
	case dialClosed:
		return InitializeMetadata{}, &ClosedError{}
	default:
		return InitializeMetadata{}, &NotDialedError{}
	}
}

func initializeMetadataSnapshot(resp *protocol.InitializeResponse) InitializeMetadata {
	if resp == nil {
		return InitializeMetadata{}
	}
	return InitializeMetadata{
		AgentInfo: cloneImplementation(resp.AgentInfo),
		Meta:      cloneRawMessage(resp.Meta),
	}
}

func cloneImplementation(info *protocol.Implementation) *protocol.Implementation {
	if info == nil {
		return nil
	}

	clone := *info
	clone.Meta = cloneRawMessage(info.Meta)
	if info.Title != nil {
		title := *info.Title
		clone.Title = &title
	}
	return &clone
}
