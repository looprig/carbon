package websearch

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// duckduckgo.go implements the DuckDuckGoProvider — the default SearchProvider
// for the WebSearch tool. It GETs DuckDuckGo's no-JS HTML results endpoint under
// a bounded context and parses the response into []SearchResult using the
// APPROVED golang.org/x/net/html tokenizer (stdlib has no HTML parser).
//
// LEAST PRIVILEGE: the provider takes only an *http.Client (the same TLS-1.2+,
// timeout-bearing client configuration as Fetch) — NO filesystem access.
//
// DEFENSIVE PARSING: DuckDuckGo's HTML is an untrusted, scraped structure that
// can change or be malformed. parseDuckDuckGoHTML walks the token stream with a
// small state machine and NEVER panics on malformed input — it returns whatever
// it could extract. A wholly-unrecognized page yields zero results (the tool
// then reports "no results"), never an error or a crash.
//
// Every request runs under the caller's context (which WebSearch passes through),
// so the injected client's timeout plus the caller deadline bound the I/O.

// ddgHTMLEndpoint is DuckDuckGo's no-JS HTML results page. The query is added as
// a URL-encoded "q" parameter. This is the documented x/net/html scrape target.
const ddgHTMLEndpoint = "https://html.duckduckgo.com/html/"

// DuckDuckGo CSS class markers the parser keys on in the HTML results page.
const (
	ddgResultAnchorClass  = "result__a"       // the title link (carries the result href)
	ddgResultSnippetClass = "result__snippet" // the snippet text
)

// ddgRedirectParam is the query parameter holding the real target URL inside
// DuckDuckGo's redirect-wrapped result links (//duckduckgo.com/l/?uddg=...).
const ddgRedirectParam = "uddg"

// DuckDuckGoProvider is a SearchProvider that scrapes DuckDuckGo's HTML results
// page. It depends only on an *http.Client (least privilege — no filesystem).
type DuckDuckGoProvider struct {
	client *http.Client
}

// NewDuckDuckGoProvider constructs a DuckDuckGoProvider bound to an injected
// *http.Client. The client is expected to carry an explicit Timeout and a TLS
// 1.2+ Transport (wired at the manifest); the provider never builds a client.
func NewDuckDuckGoProvider(client *http.Client) *DuckDuckGoProvider {
	return &DuckDuckGoProvider{client: client}
}

// Endpoints declares the ONLY network endpoint this provider contacts:
// https://html.duckduckgo.com:443 (the scrape target ddgHTMLEndpoint). The
// WebSearch tool turns it into the prepared network requirement; Search
// confines its client to it so a redirect to any secondary target fails
// closed.
func (p *DuckDuckGoProvider) Endpoints() []Endpoint {
	return []Endpoint{{Host: "html.duckduckgo.com", Port: 443}}
}

// confinedClient shallow-copies base (sharing its Transport, TLS floor, and
// Timeout) and pins its redirect policy to the declared endpoints: a redirect
// to any (host, port) outside allowed fails closed. Default HTTP(S) ports are
// applied when the redirect URL carries none.
func confinedClient(base *http.Client, allowed []Endpoint) *http.Client {
	guarded := *base
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return &searchProviderError{reason: "search redirect chain too long"}
		}
		host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
		port := 80
		if strings.ToLower(req.URL.Scheme) == "https" {
			port = 443
		}
		if text := req.URL.Port(); text != "" {
			parsed, err := strconv.Atoi(text)
			if err != nil {
				return &searchProviderError{reason: "search redirected to an invalid target"}
			}
			port = parsed
		}
		for _, endpoint := range allowed {
			if host == endpoint.Host && port == endpoint.Port {
				return nil
			}
		}
		return &searchProviderError{reason: "search redirected to an undeclared target"}
	}
	return &guarded
}

// Search GETs the DuckDuckGo HTML results page for query under ctx and parses up
// to max results. A request-build, transport, or non-2xx-status failure is a
// typed *searchProviderError; a successful response is parsed defensively (a
// malformed body yields whatever parsed, never a panic).
func (p *DuckDuckGoProvider) Search(ctx context.Context, query string, max int) ([]SearchResult, error) {
	endpoint := ddgHTMLEndpoint + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &searchProviderError{reason: "could not build search request", cause: err}
	}
	// DuckDuckGo's HTML endpoint returns an empty page without a browser-like
	// User-Agent. Set a simple, non-deceptive one (no secrets).
	req.Header.Set("User-Agent", "looprig-websearch/1.0")

	resp, err := confinedClient(p.client, p.Endpoints()).Do(req)
	if err != nil {
		return nil, &searchProviderError{reason: "search request failed", cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &searchProviderError{reason: "search returned status " + http.StatusText(resp.StatusCode)}
	}

	// Cap the body read so a hostile/huge page cannot exhaust memory. The HTML
	// results page is small; 1 MiB is far beyond any legitimate size.
	limited := io.LimitReader(resp.Body, maxSearchHTMLBytes)
	return parseDuckDuckGoHTML(limited, max), nil
}

// maxSearchHTMLBytes caps the scraped HTML read (defense against an unbounded or
// hostile response body).
const maxSearchHTMLBytes int64 = 1 << 20 // 1 MiB

// parseDuckDuckGoHTML walks the HTML token stream and extracts up to max
// SearchResults. It is a small, panic-free state machine:
//
//   - on a <a class="result__a" href="…"> start tag: begin a new result, record
//     the (unwrapped) URL, and capture the following text as the title;
//   - on a <a class="result__snippet" …> start tag: capture the following text as
//     the snippet of the in-progress result;
//   - text tokens are appended to whichever field is currently "open".
//
// A result is emitted once its title link closes. Malformed HTML (an early EOF, a
// broken tag) simply ends the walk; whatever was completed is returned. The
// tokenizer never panics on bad input — html.Tokenizer surfaces errors as an
// ErrorToken, which terminates the loop.
func parseDuckDuckGoHTML(r io.Reader, max int) []SearchResult {
	z := html.NewTokenizer(r)
	var (
		results []SearchResult
		cur     SearchResult
		inTitle bool // currently inside the title anchor's text
		inSnip  bool // currently inside the snippet anchor's text
		haveCur bool // a result is in progress (title anchor opened)
	)

	flush := func() {
		if haveCur && cur.URL != "" {
			cur.Title = strings.TrimSpace(cur.Title)
			cur.Snippet = strings.TrimSpace(cur.Snippet)
			results = append(results, cur)
		}
		cur = SearchResult{}
		haveCur = false
		inTitle = false
		inSnip = false
	}

	for len(results) < max {
		switch z.Next() {
		case html.ErrorToken:
			// EOF or a tokenizer error (malformed input). Emit any in-progress
			// result and stop — never panic.
			flush()
			return capResults(results, max)

		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "a" || !hasAttr {
				continue
			}
			class, href := anchorClassAndHref(z)
			switch {
			case hasClass(class, ddgResultAnchorClass):
				// A new result begins; close out any previous one first.
				flush()
				cur.URL = unwrapDuckDuckGoRedirect(href)
				haveCur = true
				inTitle = true
				inSnip = false
			case hasClass(class, ddgResultSnippetClass):
				inSnip = true
				inTitle = false
			}

		case html.TextToken:
			if !haveCur {
				continue
			}
			text := string(z.Text())
			switch {
			case inTitle:
				cur.Title += text
			case inSnip:
				cur.Snippet += text
			}

		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) != "a" {
				continue
			}
			// Closing an anchor ends whichever text capture was open.
			inTitle = false
			inSnip = false
		}
	}
	return capResults(results, max)
}

// capResults truncates results to at most max (defensive — the loop already
// bounds by max, but a final flush can push one over).
func capResults(results []SearchResult, max int) []SearchResult {
	if len(results) > max {
		return results[:max]
	}
	return results
}

// anchorClassAndHref pulls the class and href attribute values out of the
// current <a> start tag's attribute list.
func anchorClassAndHref(z *html.Tokenizer) (class, href string) {
	for {
		key, val, more := z.TagAttr()
		switch string(key) {
		case "class":
			class = string(val)
		case "href":
			href = string(val)
		}
		if !more {
			return class, href
		}
	}
}

// hasClass reports whether a (possibly multi-valued) HTML class attribute
// contains the target class token.
func hasClass(classAttr, target string) bool {
	for _, c := range strings.Fields(classAttr) {
		if c == target {
			return true
		}
	}
	return false
}

// unwrapDuckDuckGoRedirect resolves a result href to its real target. A direct
// URL passes through unchanged; a DuckDuckGo redirect link
// (//duckduckgo.com/l/?uddg=<urlencoded target>&rut=...) is unwrapped by URL-
// decoding the "uddg" parameter. A redirect link with no usable uddg yields ""
// (the caller drops the result, since URL == ""). An unparseable href is returned
// as-is (best effort — never panics).
func unwrapDuckDuckGoRedirect(href string) string {
	if !isDuckDuckGoRedirect(href) {
		return href
	}
	// Give the scheme-relative form a scheme so url.Parse populates RawQuery.
	toParse := href
	if strings.HasPrefix(toParse, "//") {
		toParse = "https:" + toParse
	}
	u, err := url.Parse(toParse)
	if err != nil {
		return ""
	}
	target := u.Query().Get(ddgRedirectParam)
	return target // "" when uddg is absent → result dropped.
}

// isDuckDuckGoRedirect reports whether href is a DuckDuckGo redirect-wrapper link
// (either scheme-relative "//duckduckgo.com/l/" or absolute). It only treats a
// link as a redirect when it carries the "/l/" redirect path, so a plain
// duckduckgo.com result is left untouched.
func isDuckDuckGoRedirect(href string) bool {
	return strings.Contains(href, "duckduckgo.com/l/")
}

// searchProviderError is the typed failure for the DuckDuckGo provider's network
// path (request build, transport, or a non-2xx status). The WebSearch tool maps
// it to a generic tool-result error string (never echoing upstream internals).
type searchProviderError struct {
	reason string
	cause  error
}

func (e *searchProviderError) Error() string { return e.reason }

func (e *searchProviderError) Unwrap() error { return e.cause }

// compile-time assertion: DuckDuckGoProvider satisfies SearchProvider.
var _ SearchProvider = (*DuckDuckGoProvider)(nil)
