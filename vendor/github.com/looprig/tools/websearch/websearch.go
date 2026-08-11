// Package websearch implements the WebSearch tool and its SearchProvider seam,
// with no filesystem access. Preparation turns every HTTPS endpoint the bound
// provider declares into one shared `network` capability requirement (the same
// capability kind Bash network deltas and Fetch use); at run time the provider
// must fail closed on any target outside its declaration.
package websearch

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
	"github.com/looprig/tools/permission"
)

// websearch.go implements the WebSearch tool and its provider seam: the
// SearchProvider interface (Dependency Inversion — the tool depends on the
// abstraction, not a concrete scraper) and the SearchResult value the provider
// yields (design §4b, row WebSearch).
//
// LEAST PRIVILEGE: WebSearch takes only a SearchProvider — NO filesystem access.
//
// The tool itself is a thin formatter: it validates the query, clamps the result
// count, calls the injected provider under the caller's context, and formats the
// results (or the provider's error) into a tool result. The concrete network +
// HTML-parsing concern lives entirely in the provider (duckduckgo.go), so the
// tool can be unit-tested with a fake provider and the provider's parser can be
// unit-tested against static HTML — neither needs the network.
//
// SECURITY — log events, not secrets: the query is the only thing the user
// approves, so AuditSummary surfaces exactly the query (and nothing else).
//
// Failure model: a parse error, an empty query, or a provider error is a
// tool-result error STRING — InvokableRun never returns a Go error.

// webSearchToolName is the EXACT tool name carried by every prepared request
// (tool.Request.ToolName) and shown at the gate — it MUST stay "WebSearch".
const webSearchToolName = "WebSearch"

// defaultWebSearchResults is the result count used when the caller omits (or
// supplies a non-positive) results field.
const defaultWebSearchResults = 5

// maxWebSearchResults caps how many results WebSearch will return so a single
// search cannot flood the model context (design §4b: results ≤10).
const maxWebSearchResults = 10

const webSearchSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query."},
    "results": {"type": "integer", "minimum": 1, "maximum": 10, "description": "Maximum number of results to return (optional; default 5, hard cap 10)."}
  },
  "required": ["query"]
}`

const webSearchDesc = "Search the web and return a list of result titles, URLs, and snippets (default 5, max 10). Has no filesystem access. Requires approval before each search."

// SearchResult is one web search hit: a human title, the result URL, and a short
// snippet. It is the provider-agnostic value the SearchProvider yields and the
// WebSearch tool formats.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Endpoint is one HTTPS network endpoint a SearchProvider talks to: the
// normalized hostname (lowercase, no trailing dot) and TCP port. Preparation
// turns every declared endpoint into a shared `network` capability
// requirement; at run time the provider must fail closed on any secondary
// target (a redirect or auxiliary service) outside this declaration.
type Endpoint struct {
	Host string
	Port int
}

// SearchProvider is the seam between the WebSearch tool and a concrete search
// backend (DuckDuckGo today; pluggable tomorrow). Search runs under ctx (the
// implementation MUST honor its deadline/cancellation), takes the query and a
// caller-validated max (already clamped to (0, maxWebSearchResults]), and returns
// up to max results or a typed error. An implementation must never panic on a
// malformed upstream response — it returns what it could parse.
//
// Endpoints declares every network endpoint Search may contact. It must be
// stable and non-empty; a provider that cannot honestly enumerate its
// endpoints cannot be prepared and fails closed.
type SearchProvider interface {
	Search(ctx context.Context, query string, max int) ([]SearchResult, error)
	Endpoints() []Endpoint
}

// webSearchArgs is the typed decode of WebSearch's untrusted argsJSON.
type webSearchArgs struct {
	Query   string `json:"query"`
	Results int    `json:"results"`
}

// WebSearch performs a web search via an injected SearchProvider. It has no
// filesystem access (least privilege).
type WebSearch struct {
	provider SearchProvider
}

// NewWebSearch constructs a WebSearch tool bound to a SearchProvider.
func NewWebSearch(provider SearchProvider) *WebSearch {
	return &WebSearch{provider: provider}
}

// Info returns WebSearch's self-description. Name MUST equal "WebSearch".
func (w *WebSearch) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   webSearchToolName,
		Desc:   webSearchDesc,
		Schema: json.RawMessage(webSearchSchema),
	}, nil
}

// AuditSummary returns "WebSearch: <query>" — the query is exactly what the user
// approves at the gate, so it is the right (and only) summary. An unparseable
// args document yields a generic summary.
func (w *WebSearch) AuditSummary(argsJSON string) string {
	var a webSearchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Query == "" {
		return "WebSearch (unparsable args)"
	}
	return "WebSearch: " + a.Query
}

// webSearchArtifact binds the validated query and clamped result count to one
// call. Execution consumes it verbatim — the raw args are never reparsed.
type webSearchArtifact struct {
	tool.TokenArtifact
	query string
	max   int
}

// PrepareCall decodes and validates one WebSearch call and emits the shared
// `network` capability requirement for EVERY endpoint the bound provider
// declares. The grant pair is empty: WebSearch is a direct tool and its
// provider confines its own requests to the declared endpoints (secondary
// targets fail closed at run time).
func (w *WebSearch) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	a, err := parseWebSearchArgs(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	endpoints, err := normalizedEndpoints(w.provider.Endpoints())
	if err != nil {
		return tool.Request{}, nil, err
	}
	requirements := make([]tool.Requirement, 0, len(endpoints))
	for _, endpoint := range endpoints {
		match := permission.NetworkTargetMatch("tcp", endpoint.Host, endpoint.Port)
		description := "network egress to " + endpoint.Host + ":" + strconv.Itoa(endpoint.Port)
		requirements = append(requirements, tool.Requirement{
			Kind:        permission.CapabilityNetwork,
			Match:       match,
			Description: description,
			Candidates: []tool.RuleCandidate{{
				Kind:        permission.CapabilityNetwork,
				Match:       match,
				Description: description,
			}},
		})
	}
	request := tool.Request{
		ToolName:     webSearchToolName,
		ExecutionID:  executionID.String(),
		Requirements: requirements,
	}
	return request, &webSearchArtifact{query: a.Query, max: clampWebSearchResults(a.Results)}, nil
}

// normalizedEndpoints validates and normalizes a provider's declared endpoints,
// deduplicating exact repeats. Zero declared or any invalid endpoint fails the
// preparation — a network tool without an honest declaration cannot run.
func normalizedEndpoints(declared []Endpoint) ([]Endpoint, error) {
	if len(declared) == 0 {
		return nil, &webSearchError{reason: "search provider declares no network endpoints"}
	}
	seen := make(map[Endpoint]struct{}, len(declared))
	out := make([]Endpoint, 0, len(declared))
	for _, endpoint := range declared {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(endpoint.Host), "."))
		if host == "" || strings.ContainsFunc(host, func(r rune) bool { return r <= 0x20 || r == 0x7f }) {
			return nil, &webSearchError{reason: "search provider declared an invalid endpoint host"}
		}
		if endpoint.Port < 1 || endpoint.Port > 65535 {
			return nil, &webSearchError{reason: "search provider declared an invalid endpoint port"}
		}
		normalized := Endpoint{Host: host, Port: endpoint.Port}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed; without its artifact the tool fails closed. It
// calls the provider under ctx and formats the results. A provider error is a
// tool-result error STRING; it never returns a Go error.
func (w *WebSearch) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	art, ok := prepared.FromContext[*webSearchArtifact](ctx)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: WebSearch requires its prepared call artifact"), nil
	}

	results, err := w.provider.Search(ctx, art.query, art.max)
	if err != nil {
		// Surface a generic provider failure — never echo upstream internals that
		// might embed request details.
		return tool.TextResult("error: web search failed"), nil
	}
	return tool.TextResult(formatSearchResults(results)), nil
}

// parseWebSearchArgs decodes + validates the args. A non-object document or an
// empty query is a typed *webSearchError.
func parseWebSearchArgs(argsJSON string) (webSearchArgs, error) {
	var a webSearchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return webSearchArgs{}, &webSearchError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if strings.TrimSpace(a.Query) == "" {
		return webSearchArgs{}, &webSearchError{reason: "a non-empty 'query' is required"}
	}
	return a, nil
}

// clampWebSearchResults maps a caller-supplied count into (0, maxWebSearchResults]:
// ≤0 → default; otherwise min(count, max).
func clampWebSearchResults(n int) int {
	if n <= 0 {
		return defaultWebSearchResults
	}
	if n > maxWebSearchResults {
		return maxWebSearchResults
	}
	return n
}

// formatSearchResults renders the results as a numbered list of title / URL /
// snippet. An empty list yields a friendly "no results" message.
func formatSearchResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(". ")
		sb.WriteString(r.Title)
		sb.WriteString("\n   ")
		sb.WriteString(r.URL)
		if r.Snippet != "" {
			sb.WriteString("\n   ")
			sb.WriteString(r.Snippet)
		}
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// webSearchError is the typed failure for WebSearch arg parsing/validation. It
// carries a non-secret reason; PrepareCall returns it so the runner treats the
// call as invalid, and InvokableRun maps run failures to tool-result strings.
type webSearchError struct {
	reason string
	cause  error
}

func (e *webSearchError) Error() string { return e.reason }

func (e *webSearchError) Unwrap() error { return e.cause }

// compile-time assertions: WebSearch is an InvokableTool, a CallPreparer, and
// Auditable. It is NOT a WriteTarget.
var (
	_ tool.InvokableTool = (*WebSearch)(nil)
	_ tool.CallPreparer  = (*WebSearch)(nil)
	_ tool.Auditable     = (*WebSearch)(nil)
)
