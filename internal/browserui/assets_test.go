package browserui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// indexContent reads the real committed placeholder so tests assert against its
// actual bytes rather than a hardcoded duplicate string.
func indexContent(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(assetsFS, indexPath)
	if err != nil {
		t.Fatalf("reading embedded %s: %v", indexPath, err)
	}
	return string(b)
}

// TestEmbedFSRejectsTraversalNames documents (and locks in) the embed.FS
// guarantee assetName is layered on top of: fs.ValidPath rejects any name
// containing ".." or "." elements, an empty element, or a leading "/" outright,
// at Open time, regardless of what the caller passes in. Verified empirically
// (client/pkg/webui's task investigation) before assetName was first written;
// this test is what keeps that verification true rather than remembered.
//
// assetName exists on top of this, not instead of it: CLAUDE.md requires paths
// to be cleaned and confinement-checked at the boundary. Neither layer is
// load-bearing alone.
func TestEmbedFSRejectsTraversalNames(t *testing.T) {
	t.Parallel()
	names := []string{
		"../../etc/passwd",
		"dist/../../../etc/passwd",
		"/etc/passwd",
		"dist/../etc/passwd",
		// Escaping the embed root to a file that really does exist next to it.
		"../go.mod",
		"dist/../go.mod",
		// Non-".." forms fs.ValidPath also rejects.
		"./dist/index.html",
		"dist//index.html",
	}
	for _, name := range names {
		if _, err := assetsFS.Open(name); err == nil {
			t.Errorf("assetsFS.Open(%q): want error (invalid/not-exist per fs.ValidPath), got nil", name)
		}
	}
	// Sanity: a legitimate embedded name still opens fine.
	if _, err := assetsFS.Open(indexPath); err != nil {
		t.Fatalf("assetsFS.Open(%q): unexpected error %v", indexPath, err)
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reqPath string
		want    string
	}{
		{name: "root", reqPath: "/", want: assetRoot},
		{name: "index explicit", reqPath: "/index.html", want: "dist/index.html"},
		{name: "nested asset", reqPath: "/assets/app.js", want: "dist/assets/app.js"},
		{name: "spa client route", reqPath: "/sessions/abc123", want: "dist/sessions/abc123"},
		{name: "dotdot traversal clamps at root", reqPath: "/../../etc/passwd", want: "dist/etc/passwd"},
		{name: "deep dotdot traversal clamps at root", reqPath: "/../../../../../../../../etc/passwd", want: "dist/etc/passwd"},
		{name: "escape to a real sibling file clamps", reqPath: "/../go.mod", want: "dist/go.mod"},
		{name: "traversal back out through the root prefix clamps", reqPath: "/dist/../../go.mod", want: "dist/go.mod"},
		{name: "bare dotdot clamps to root", reqPath: "/..", want: assetRoot},
		{name: "dot segments are resolved", reqPath: "/./assets/./app.js", want: "dist/assets/app.js"},
		{name: "trailing slash is dropped", reqPath: "/assets/", want: "dist/assets"},
		{name: "mixed-dot pattern is a literal filename, not traversal", reqPath: "/....//....//etc/passwd", want: "dist/..../..../etc/passwd"},
		{name: "empty path", reqPath: "", want: assetRoot},
		{name: "double slashes collapse", reqPath: "//assets//app.js", want: "dist/assets/app.js"},
		{name: "absolute-looking leading double slash clamps", reqPath: "//etc/passwd", want: "dist/etc/passwd"},
		// net/http decodes percent-escapes into URL.Path before a handler sees
		// them, so assetName is never handed an escape sequence in practice. If
		// it is, it is a literal filename component and stays one -- assetName
		// must never decode, or "%2e%2e%2f" would become a traversal it already
		// finished cleaning.
		{name: "percent-escapes are literal, never re-decoded", reqPath: "/%2e%2e/%2e%2e/etc/passwd", want: "dist/%2e%2e/%2e%2e/etc/passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := assetName(tt.reqPath)
			if got != tt.want {
				t.Errorf("assetName(%q) = %q, want %q", tt.reqPath, got, tt.want)
			}
			// The invariant this function exists to guarantee.
			if got != assetRoot && !strings.HasPrefix(got, assetRoot+"/") {
				t.Errorf("assetName(%q) = %q escapes root %q", tt.reqPath, got, assetRoot)
			}
		})
	}
}

func TestAssetsRootServesIndex(t *testing.T) {
	t.Parallel()
	want := indexContent(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Assets().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// TestAssetsTraversal drives Assets() with httptest.NewRequest, which parses the
// target exactly as a real server would receive it off the wire -- including
// populating RawPath for percent-encoded paths -- so this exercises the same
// Path/RawPath split a live net/http server hands to any registered handler.
func TestAssetsTraversal(t *testing.T) {
	want := indexContent(t)

	cases := []struct {
		name string
		path string // request-target, as passed to httptest.NewRequest
	}{
		{name: "dotdot traversal", path: "/../../etc/passwd"},
		{name: "url-encoded slash traversal", path: "/..%2f..%2fetc%2fpasswd"},
		{name: "fully-encoded dotdot traversal", path: "/%2e%2e%2f%2e%2e%2fetc%2fpasswd"},
		{name: "double-encoded traversal stays a literal name", path: "/%252e%252e%252fgo.mod"},
		{name: "mixed-dot waf-bypass pattern", path: "/....//....//etc/passwd"},
		{name: "absolute-looking leading double slash", path: "//etc/passwd"},
		{name: "escape to a real file next to the embed root", path: "/../go.mod"},
		{name: "escape back out through the root prefix", path: "/dist/../../go.mod"},
		{name: "encoded escape to a real file next to the embed root", path: "/..%2f..%2fgo.mod"},
		{name: "backslash separators are literal, not traversal", path: `/..\..\etc\passwd`},
		{name: "unknown asset", path: "/no-such-file.js"},
		{name: "spa client route", path: "/sessions/abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()

			Assets().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Body.String(); got != want {
				t.Fatalf("body = %q, want SPA index fallback %q", got, want)
			}
		})
	}
}

// TestAssetsNeverOpensOutsideRoot asserts the confinement step itself rather
// than its currently-redundant effect.
//
// embed.FS's own fs.ValidPath check would otherwise mask a regression in
// assetName: an escaping name is rejected by Open, the handler falls back to
// the index, and every response-body assertion in TestAssetsTraversal still
// passes -- verified by mutating assetName to drop the rooting and the prefix
// check, which left that test green. Worse, "/dist/../../go.mod" resolves under
// such a mutation to plain "go.mod", which is a *valid* fs path: only its
// absence from the embed tree stops it being served, so an fs.FS backed by a
// real directory would hand out the file.
//
// Recording every name the handler passes to Open closes that gap.
func TestAssetsNeverOpensOutsideRoot(t *testing.T) {
	t.Parallel()

	hostile := []string{
		"/../../etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/....//....//etc/passwd",
		"//etc/passwd",
		"/../go.mod",
		"/dist/../../go.mod",
		"/..%2f..%2fgo.mod",
		"/../../../../../../../../etc/passwd",
		"/..",
	}

	spy := &recordingFS{inner: fstest.MapFS{"dist/index.html": {Data: []byte("<html>shell</html>")}}}
	h := newAssetHandler(spy)
	for _, target := range hostile {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (SPA fallback)", target, rec.Code)
		}
	}

	opened := spy.opened()
	if len(opened) == 0 {
		t.Fatal("no Open calls recorded; the test is not exercising the handler")
	}
	for _, name := range opened {
		if name != assetRoot && !strings.HasPrefix(name, assetRoot+"/") {
			t.Errorf("handler called Open(%q), which escapes the embed root %q", name, assetRoot)
		}
	}
}

// recordingFS wraps an fs.FS and records every name it is asked to Open.
// See TestAssetsNeverOpensOutsideRoot for why the wrapper exists.
type recordingFS struct {
	inner fs.FS

	mu    sync.Mutex
	names []string
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	return r.inner.Open(name)
}

func (r *recordingFS) opened() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

// TestAssetsTraversalOverRealHTTP repeats the traversal-shaped requests as
// literal GETs against a real listening server, so the request line is
// constructed and parsed exactly as an attacker's raw HTTP client would send it
// (as opposed to Go's http.NewRequest/URL-parsing short-circuiting anything).
// None of these responses may contain content from outside the embedded dist/.
func TestAssetsTraversalOverRealHTTP(t *testing.T) {
	want := indexContent(t)

	srv := httptest.NewServer(Assets())
	t.Cleanup(srv.Close)

	targets := []string{
		"/../../etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
		"/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/....//....//etc/passwd",
		"//etc/passwd",
		"/../go.mod",
		"/dist/../../go.mod",
		"/..%2f..%2fgo.mod",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, srv.URL+target, nil)
			if err != nil {
				t.Fatalf("NewRequest(%q): %v", target, err)
			}
			client := &http.Client{
				Timeout: 5 * time.Second,
				// Traversal must be blocked at the response for THIS request,
				// not merely by chasing a redirect elsewhere; do not let
				// redirect-following mask what this handler actually returned.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do(%q): %v", target, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if strings.Contains(string(body), "root:") {
				t.Fatalf("response body looks like it leaked /etc/passwd contents: %q", body)
			}
			if strings.Contains(string(body), "module github.com/looprig/carbon") {
				t.Fatalf("response body leaked the module's own go.mod, which sits next to the embed root: %q", body)
			}
			// A redirect (3xx) back into this same handler is acceptable:
			// net/http's ServeMux performs one for the plain "/../.." case when
			// a handler is mounted under a mux. Nothing is mounted here, so no
			// redirect is expected -- but assert the terminal behavior either
			// way.
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (SPA fallback) or a redirect back into this handler", resp.StatusCode)
			}
			if string(body) != want {
				t.Fatalf("body = %q, want SPA index fallback %q", body, want)
			}
		})
	}
}

// TestAssetsServesRealAssets proves normal asset serving works -- not just that
// attacks are blocked -- using a small in-memory fs.FS fixture rather than
// adding non-placeholder files to the committed dist/, which //go:embed would
// bake in at compile time. newAssetHandler is parameterized over fs.FS for
// exactly this.
func TestAssetsServesRealAssets(t *testing.T) {
	t.Parallel()

	const assetBody = "console.log('app');"
	fsys := fstest.MapFS{
		"dist/index.html":    {Data: []byte("<html>shell</html>")},
		"dist/assets/app.js": {Data: []byte(assetBody)},
		"dist/favicon.ico":   {Data: []byte("ico-bytes")},
		"dist/assets":        {Mode: fs.ModeDir},
	}
	h := newAssetHandler(fsys)

	get := func(t *testing.T, target string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", target, rec.Code)
		}
		return rec
	}

	t.Run("real nested asset is served as-is", func(t *testing.T) {
		t.Parallel()
		if got := get(t, "/assets/app.js").Body.String(); got != assetBody {
			t.Fatalf("body = %q, want %q", got, assetBody)
		}
	})

	t.Run("unknown path falls back to index", func(t *testing.T) {
		t.Parallel()
		if got := get(t, "/sessions/xyz").Body.String(); got != "<html>shell</html>" {
			t.Fatalf("body = %q, want index shell", got)
		}
	})

	t.Run("directory path falls back to index, not a directory listing", func(t *testing.T) {
		t.Parallel()
		if got := get(t, "/assets").Body.String(); got != "<html>shell</html>" {
			t.Fatalf("body = %q, want index shell (no directory listing)", got)
		}
	})

	t.Run("traversal against the fixture clamps and serves the in-root asset", func(t *testing.T) {
		t.Parallel()
		// path.Clean("/" + "/../assets/app.js") clamps to "/assets/app.js",
		// which IS a real, in-root asset -- so this serves the asset, not the
		// index. This case documents that clamping (not blanket rejection) is
		// the intended behavior for in-root paths containing resolvable "..".
		if got := get(t, "/../assets/app.js").Body.String(); got != assetBody {
			t.Fatalf("body = %q, want clamped-and-served asset %q", got, assetBody)
		}
	})
}
