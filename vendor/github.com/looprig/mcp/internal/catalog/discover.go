// This file implements initial discovery: turning a freshly initialized
// connection into the first Generation.
//
// It follows the design's discovery sequence. Steps 1 (initialize and
// negotiate) and 8-9 (publish the candidate, adopt it before the owner becomes
// ready) belong to the client, which owns the handshake and the lifecycle;
// what happens here is steps 2-7 — fetch every advertised family with
// pagination, reject duplicate cursors and page cycles, enforce the page, item,
// schema and byte limits, validate names and schemas, preserve the raw protocol
// identity, and construct the stable model-visible identity.
//
// The all-or-nothing property the design requires ("discovery failure does not
// partially replace a prior valid generation") is structural here rather than
// something to remember: Discover has no reference to whatever generation is
// currently adopted and no way to publish. It either returns one complete new
// Generation or it returns an error. A caller that only assigns on success
// cannot half-replace anything.

package catalog

import (
	"context"
	"fmt"

	"github.com/looprig/mcp/internal/limits"
	"github.com/looprig/mcp/internal/protocol"
)

// Values for limits.OverLimitError.What reported by discovery.
const (
	// WhatCatalogPages is reported when a family needs more pages than the
	// binding allows.
	WhatCatalogPages = "catalog_pages"
	// WhatCatalogItems is reported when a family carries more items than the
	// binding allows.
	WhatCatalogItems = "catalog_items"
)

// Lister is the connection surface discovery needs: the paginated list methods,
// and nothing else.
//
// It is declared here, at the consumer, rather than taken as a protocol.Conn.
// Discovery cannot initialize, call a tool, or close a connection, and an
// interface that offered it those would be an interface it could misuse. It
// also makes the fetch sequence testable against a scripted server — page
// cycles and hostile cursors are trivial to produce through this and awkward to
// produce through a real one.
//
// protocol.Conn satisfies it.
type Lister interface {
	ListTools(ctx context.Context, cursor string) (protocol.ToolPage, error)
	ListPrompts(ctx context.Context, cursor string) (protocol.PromptPage, error)
	ListResources(ctx context.Context, cursor string) (protocol.ResourcePage, error)
	ListResourceTemplates(ctx context.Context, cursor string) (protocol.ResourceTemplatePage, error)
}

// Limits is the narrow view of the client's Limits that discovery enforces.
//
// The item bounds are per family, not a shared budget: they answer "is this
// server's tool list reasonable", which is a different question per family and
// is configured separately. A non-positive bound is not "unbounded" — like
// every other bound in this module it fails closed, rejecting the first page —
// so callers pass a normalized value.
type Limits struct {
	// MaxPages caps the list round trips one family may take.
	MaxPages int
	// MaxTools caps the tools accepted from one server.
	MaxTools int
	// MaxPrompts caps the prompts accepted from one server.
	MaxPrompts int
	// MaxResources caps the resources accepted from one server. It is applied
	// to concrete resources and to templates separately: they are two lists,
	// and a server with many templates is not thereby allowed fewer resources.
	MaxResources int
}

// Config is everything discovery needs that does not come from the connection.
// The handshake fields are supplied by the caller because the caller performed
// the handshake; discovery must not repeat it.
type Config struct {
	// Binding is the configured name of the binding being discovered.
	Binding string
	// Number is the generation number to assign.
	Number uint64
	// Handshake is what initialize settled. Capabilities gates which families
	// are fetched at all.
	Handshake protocol.InitializeResult
	// Limits bounds the fetch.
	Limits Limits
	// Tolerances is the compatibility policy the generation is built under. The
	// zero value tolerates nothing.
	Tolerances Tolerances
}

// Discover fetches every advertised catalog family and builds a Generation.
//
// Only families the server advertised are fetched. This is the design's
// compatibility rule — "checks server capabilities before using a method" —
// and it is not merely polite: a server that did not advertise prompts has not
// promised prompts/list exists, and calling it produces a method-not-found
// error that is indistinguishable from a real failure. Asking would turn a
// server with no prompts into a server that failed discovery. What was skipped,
// and why, is recorded as a Decision, so "this server has no prompts" and "we
// never asked" stay distinguishable afterwards.
//
// Failures are all-or-nothing: any error returns no generation at all.
func Discover(ctx context.Context, l Lister, cfg Config) (*Generation, error) {
	if l == nil {
		return nil, &DefectError{Reason: "no connection to discover over"}
	}

	caps := cfg.Handshake.Capabilities
	b := Builder{
		Binding:         cfg.Binding,
		Number:          cfg.Number,
		Tolerances:      cfg.Tolerances,
		ProtocolVersion: cfg.Handshake.ProtocolVersion,
		Capabilities:    caps,
		Server:          cfg.Handshake.Server,
		Instructions:    cfg.Handshake.Instructions,
	}

	var warnings warningBudget

	tools, w, err := fetchFamily(ctx, FamilyTools, caps.Tools, cfg.Limits.MaxPages, cfg.Limits.MaxTools,
		func(ctx context.Context, cursor string) ([]protocol.ToolSpec, string, []string, error) {
			p, err := l.ListTools(ctx, cursor)
			return p.Tools, p.NextCursor, p.Warnings, err
		})
	if err != nil {
		return nil, err
	}
	b.Tools = tools
	warnings.merge(w)
	b.Decisions = append(b.Decisions, decisionFor(FamilyTools, caps.Tools))

	prompts, w, err := fetchFamily(ctx, FamilyPrompts, caps.Prompts, cfg.Limits.MaxPages, cfg.Limits.MaxPrompts,
		func(ctx context.Context, cursor string) ([]protocol.PromptSpec, string, []string, error) {
			p, err := l.ListPrompts(ctx, cursor)
			return p.Prompts, p.NextCursor, p.Warnings, err
		})
	if err != nil {
		return nil, err
	}
	b.Prompts = prompts
	warnings.merge(w)
	b.Decisions = append(b.Decisions, decisionFor(FamilyPrompts, caps.Prompts))

	resources, w, err := fetchFamily(ctx, FamilyResources, caps.Resources, cfg.Limits.MaxPages, cfg.Limits.MaxResources,
		func(ctx context.Context, cursor string) ([]protocol.ResourceSpec, string, []string, error) {
			p, err := l.ListResources(ctx, cursor)
			return p.Resources, p.NextCursor, p.Warnings, err
		})
	if err != nil {
		return nil, err
	}
	b.Resources = resources
	warnings.merge(w)
	b.Decisions = append(b.Decisions, decisionFor(FamilyResources, caps.Resources))

	// Templates are gated on the resources capability: MCP has no separate
	// capability for them, they are part of what a resources server offers.
	templates, w, err := fetchFamily(ctx, FamilyResourceTemplates, caps.Resources, cfg.Limits.MaxPages, cfg.Limits.MaxResources,
		func(ctx context.Context, cursor string) ([]protocol.ResourceTemplateSpec, string, []string, error) {
			p, err := l.ListResourceTemplates(ctx, cursor)
			return p.Templates, p.NextCursor, p.Warnings, err
		})
	if err != nil {
		return nil, err
	}
	b.ResourceTemplates = templates
	warnings.merge(w)
	b.Decisions = append(b.Decisions, decisionFor(FamilyResourceTemplates, caps.Resources))

	b.Warnings, b.DroppedWarnings = warnings.kept, warnings.dropped
	return b.Build()
}

// decisionFor records whether a family was fetched or skipped.
func decisionFor(f Family, advertised bool) Decision {
	if advertised {
		return Decision{Family: f, Action: ActionFetched}
	}
	return Decision{Family: f, Action: ActionSkippedNotAdvertised}
}

// fetchFamily paginates one family to completion, enforcing its bounds.
//
// A family the server did not advertise is not fetched: the zero result is
// returned with no round trip at all, which is what makes the skip observable
// in a test (no call happened) rather than merely inferable.
//
// The generic parameter is the item type. The families differ only in their
// item type and their method, and the part worth getting right — the cursor
// bookkeeping, the cycle rejection, the page and item accounting — is identical
// for all four. Writing it once means all four are policed the same way, and
// there is no fourth copy where the cycle check quietly went missing.
func fetchFamily[T any](
	ctx context.Context,
	family Family,
	advertised bool,
	maxPages, maxItems int,
	fetch func(ctx context.Context, cursor string) (items []T, next string, warnings []string, err error),
) ([]T, warningBudget, error) {
	if !advertised {
		return nil, warningBudget{}, nil
	}

	var (
		items    []T
		warnings warningBudget
		cursor   string
	)
	// seen holds every cursor already requested, including the initial empty
	// one. A server that hands back a cursor from this set is walking us in a
	// circle — either by returning its own cursor, or by returning one from
	// several pages back, which no bound on page count would catch in time.
	seen := map[string]struct{}{"": {}}

	for page := 0; ; page++ {
		if page >= maxPages {
			// The server still has more to give, but this binding will not
			// fetch it. A truncated catalog is not a safe catalog — the missing
			// tools would look identical to tools the server removed — so this
			// is an error rather than a warning.
			return nil, warningBudget{}, &limits.OverLimitError{What: WhatCatalogPages, Limit: maxPages}
		}
		if err := ctx.Err(); err != nil {
			return nil, warningBudget{}, fmt.Errorf("catalog: %s: %w", family, err)
		}

		got, next, w, err := fetch(ctx, cursor)
		if err != nil {
			return nil, warningBudget{}, fmt.Errorf("catalog: %s: %w", family, err)
		}

		// Count before appending: a page that would take the total over the
		// bound must not be retained even briefly.
		if len(items)+len(got) > maxItems {
			return nil, warningBudget{}, &limits.OverLimitError{What: WhatCatalogItems, Limit: maxItems}
		}
		items = append(items, got...)
		warnings.add(w...)

		if next == "" {
			return items, warnings, nil
		}
		if _, repeat := seen[next]; repeat {
			// The cursor never reaches a name or a log; it is opaque server
			// data with no diagnostic value and unbounded length.
			return nil, warningBudget{}, &DefectError{
				Family: family,
				Reason: fmt.Sprintf("server returned a pagination cursor it already served (page %d): the catalog does not terminate", page),
			}
		}
		seen[next] = struct{}{}
		cursor = next
	}
}
