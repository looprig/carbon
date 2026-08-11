// This file holds the paginated list methods — the catalog side of the
// connection — and the neutral page types they return.
//
// A page is the unit the MCP list methods actually deal in: a slice of items
// plus an opaque cursor for the next one. It is modelled explicitly, rather
// than hidden behind an iterator that fetches transparently (the SDK offers
// one), because every bound this module cares about is a property of the
// *sequence* of pages — how many there were, how many items they totalled,
// whether the cursors ever repeated. An iterator that hides the pages hides
// exactly the thing that has to be policed, and there is no way to refuse a
// hostile server's 10000th page from inside a range loop.
//
// Conversion of the items themselves is conv.go's job; this file only adds the
// page framing and the bound on the cursor.

package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxCursorBytes bounds an opaque pagination cursor.
//
// A cursor is server-supplied, retained for the whole fetch (every cursor seen
// is kept, to detect a cycle), and handed straight back on the next request. It
// is the one value in a page that this module stores per round trip rather than
// per item, so an unbounded one is a way to make a bounded number of pages cost
// an unbounded amount of memory. 4 KiB is far beyond any real cursor, which is
// typically an offset or an opaque ID.
const MaxCursorBytes = 4 << 10

// errCursorTooLong reports a cursor over MaxCursorBytes. The cursor itself is
// never rendered: it is opaque server data with no diagnostic value, and it
// would put unbounded untrusted bytes into an error string.
var errCursorTooLong = errors.New("protocol: pagination cursor exceeds the size limit")

// ToolPage is one page of tools/list.
type ToolPage struct {
	Tools []ToolSpec
	// NextCursor is the cursor for the following page, empty when this is the
	// last one. It is opaque: nothing in this module interprets it.
	NextCursor string
	// Warnings records items dropped during conversion, bounded by MaxWarnings.
	Warnings []string
}

// PromptPage is one page of prompts/list.
type PromptPage struct {
	Prompts    []PromptSpec
	NextCursor string
	Warnings   []string
}

// ResourcePage is one page of resources/list.
type ResourcePage struct {
	Resources  []ResourceSpec
	NextCursor string
	Warnings   []string
}

// ResourceTemplatePage is one page of resources/templates/list.
type ResourceTemplatePage struct {
	Templates  []ResourceTemplateSpec
	NextCursor string
	Warnings   []string
}

// checkCursor bounds a server-supplied cursor before it is retained.
func checkCursor(cursor string) error {
	if len(cursor) > MaxCursorBytes {
		return fmt.Errorf("%w: %d bytes, max %d", errCursorTooLong, len(cursor), MaxCursorBytes)
	}
	return nil
}

// pageWarn appends a warning to a page's warning list, keeping the last slot
// free for convertItems' overflow summary.
//
// The reserved slot is what keeps MaxWarnings a bound on memory without also
// making it a bound on the truth. Filling every slot with a per-item message
// would mean the (MaxWarnings+1)-th drop is reported nowhere at all.
func pageWarn(warnings []string, msg string) []string {
	if len(warnings) >= MaxWarnings-1 {
		return warnings
	}
	return append(warnings, msg)
}

// convertItems converts a page's items, dropping the ones that cannot be
// converted rather than failing the page.
//
// Dropping is the tolerance the item converters already define: FromSDKTool
// rejects a tool whose input schema is missing or malformed, because exposing
// it would mean accepting unconstrained arguments — the *tool* is rejected, not
// the server. Failing the whole page instead would let one defective item deny
// an otherwise-working server, and would protect nothing that dropping does not
// already protect: the unusable item is not exposed either way.
//
// No drop is silent, and the COUNT is never wrong. Warnings are capped at
// MaxWarnings so a hostile server cannot turn tolerated defects into unbounded
// memory, but a cap that simply discarded the rest would make a page of 100
// dropped tools indistinguishable from a page of 8 — the caller would read the
// eight warnings as the whole story. So the individual messages are capped and
// the last slot carries a summary of what the cap hid. A caller that finds a
// tool missing can always see that it was dropped, and how many others were,
// even when it cannot see why for every one.
func convertItems[In, Out any](
	items []In,
	convert func(In) (Out, error),
	what string,
) ([]Out, []string) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]Out, 0, len(items))
	var warnings []string
	dropped := 0
	for i, item := range items {
		conv, err := convert(item)
		if err != nil {
			dropped++
			warnings = pageWarn(warnings, fmt.Sprintf("%s %d dropped: %v", what, i, err))
			continue
		}
		out = append(out, conv)
	}
	if unreported := dropped - len(warnings); unreported > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d further %s(s) were dropped without a message (%d dropped in all, over this client's %d warning cap)",
			unreported, what, dropped, MaxWarnings))
	}
	return out, warnings
}

// FromSDKToolPage converts a tools/list result.
//
// It is separate from ListTools — which only has to fetch — because this is the
// half that touches server data, and it is the half worth driving directly: a
// fuzzer can hand it any page a server could produce, where reaching the same
// code through a live session would require a hostile server to exist first.
func FromSDKToolPage(res *mcp.ListToolsResult, b Bounds) (ToolPage, error) {
	if res == nil {
		return ToolPage{}, fmt.Errorf("%w: tools/list result", errNilInput)
	}
	if err := checkCursor(res.NextCursor); err != nil {
		return ToolPage{}, err
	}
	tools, warnings := convertItems(res.Tools,
		func(t *mcp.Tool) (ToolSpec, error) { return FromSDKTool(t, b) }, "tool")
	return ToolPage{Tools: tools, NextCursor: res.NextCursor, Warnings: warnings}, nil
}

// FromSDKPromptPage converts a prompts/list result.
func FromSDKPromptPage(res *mcp.ListPromptsResult, b Bounds) (PromptPage, error) {
	if res == nil {
		return PromptPage{}, fmt.Errorf("%w: prompts/list result", errNilInput)
	}
	if err := checkCursor(res.NextCursor); err != nil {
		return PromptPage{}, err
	}
	prompts, warnings := convertItems(res.Prompts,
		func(p *mcp.Prompt) (PromptSpec, error) { return FromSDKPrompt(p, b) }, "prompt")
	return PromptPage{Prompts: prompts, NextCursor: res.NextCursor, Warnings: warnings}, nil
}

// FromSDKResourcePage converts a resources/list result.
func FromSDKResourcePage(res *mcp.ListResourcesResult, b Bounds) (ResourcePage, error) {
	if res == nil {
		return ResourcePage{}, fmt.Errorf("%w: resources/list result", errNilInput)
	}
	if err := checkCursor(res.NextCursor); err != nil {
		return ResourcePage{}, err
	}
	resources, warnings := convertItems(res.Resources,
		func(r *mcp.Resource) (ResourceSpec, error) { return FromSDKResource(r, b) }, "resource")
	return ResourcePage{Resources: resources, NextCursor: res.NextCursor, Warnings: warnings}, nil
}

// FromSDKResourceTemplatePage converts a resources/templates/list result.
func FromSDKResourceTemplatePage(res *mcp.ListResourceTemplatesResult, b Bounds) (ResourceTemplatePage, error) {
	if res == nil {
		return ResourceTemplatePage{}, fmt.Errorf("%w: resources/templates/list result", errNilInput)
	}
	if err := checkCursor(res.NextCursor); err != nil {
		return ResourceTemplatePage{}, err
	}
	templates, warnings := convertItems(res.ResourceTemplates,
		func(rt *mcp.ResourceTemplate) (ResourceTemplateSpec, error) { return FromSDKResourceTemplate(rt, b) },
		"resource template")
	return ResourceTemplatePage{Templates: templates, NextCursor: res.NextCursor, Warnings: warnings}, nil
}

// ListTools fetches one page of the server's tools. cursor is empty for the
// first page, and otherwise the NextCursor of the preceding one.
func (s *Session) ListTools(ctx context.Context, cursor string) (ToolPage, error) {
	cs, err := s.established()
	if err != nil {
		return ToolPage{}, err
	}
	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
	if err != nil {
		return ToolPage{}, fmt.Errorf("tools/list: %w", err)
	}
	return FromSDKToolPage(res, s.cfg.Bounds)
}

// ListPrompts fetches one page of the server's prompts.
func (s *Session) ListPrompts(ctx context.Context, cursor string) (PromptPage, error) {
	cs, err := s.established()
	if err != nil {
		return PromptPage{}, err
	}
	res, err := cs.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cursor})
	if err != nil {
		return PromptPage{}, fmt.Errorf("prompts/list: %w", err)
	}
	return FromSDKPromptPage(res, s.cfg.Bounds)
}

// ListResources fetches one page of the server's concrete resources.
func (s *Session) ListResources(ctx context.Context, cursor string) (ResourcePage, error) {
	cs, err := s.established()
	if err != nil {
		return ResourcePage{}, err
	}
	res, err := cs.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
	if err != nil {
		return ResourcePage{}, fmt.Errorf("resources/list: %w", err)
	}
	return FromSDKResourcePage(res, s.cfg.Bounds)
}

// ListResourceTemplates fetches one page of the server's resource templates.
func (s *Session) ListResourceTemplates(ctx context.Context, cursor string) (ResourceTemplatePage, error) {
	cs, err := s.established()
	if err != nil {
		return ResourceTemplatePage{}, err
	}
	res, err := cs.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
	if err != nil {
		return ResourceTemplatePage{}, fmt.Errorf("resources/templates/list: %w", err)
	}
	return FromSDKResourceTemplatePage(res, s.cfg.Bounds)
}
