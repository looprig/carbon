package browserui

// assets.go serves the embedded SPA build with the standard SPA-router
// fallback: a request for a real file under dist/ (JS, CSS, images, ...) is
// served as-is; any other request falls back to dist/index.html so client-side
// routes like /sessions/abc123 still return the app shell.
//
// dist/ is committed rather than generated at build time. //go:embed requires
// the embedded path to exist at compile time, so committing the tree is what
// keeps go build / go vet / go test green with no Node toolchain installed --
// and, because the module zip is source-only, it is also literally what a
// consumer's binary serves.
//
// .gitignore keeps exactly two files under dist/ tracked unconditionally:
// index.html, the SPA fallback target, and looprig-bundle.json, the marker
// BundleProtocolVersion reads (see bundle.go). The rest of the tree is ignored
// day to day and force-added onto a release commit by `make release-dist`, so
// what is checked in beside them is whatever that last staged. `npm run build
// -w app` overwrites it in place; `make dist-reset` restores the committed
// snapshot.

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// assetsFS is the embedded SPA build: the committed dist/ tree described
// above. Unexported: the build is an implementation detail of Assets and of
// BundleProtocolVersion, not a surface a consumer inspects.
//
// The all: prefix is required, not stylistic. A bare `//go:embed dist` applies
// the default walk rules, which silently skip every entry whose name begins
// with "_" or "." -- so a bundler that emits an underscore-prefixed chunk
// directory would produce a binary serving index.html and 404ing every asset it
// references, with no build error anywhere. `all:` disables that filtering.
//
//go:embed all:dist
var assetsFS embed.FS

const (
	// assetRoot is assetsFS's embedded subtree. Every path served by Assets is
	// confined under this prefix; see assetName.
	assetRoot = "dist"
	// indexPath is the SPA shell served for any request that does not match a
	// real embedded file.
	indexPath = "dist/index.html"
)

// Assets returns an http.Handler serving the embedded SPA build: a real asset
// under dist/ if the request path names one, otherwise dist/index.html (the
// SPA-router fallback).
//
// Every request path is cleaned and confined to the embedded dist/ root before
// it is handed to embed.FS.Open -- see assetName and assets_test.go for the
// traversal cases this defends against, and for what was empirically verified
// about embed.FS's and net/http's own path handling.
func Assets() http.Handler {
	return newAssetHandler(assetsFS)
}

// newAssetHandler builds the handler over an arbitrary fs.FS so the serving and
// traversal-confinement logic can be tested against a small controlled fixture
// without needing a second real //go:embed tree.
func newAssetHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := assetName(r.URL.Path)
		if name != indexPath && serveAsset(w, r, fsys, name) {
			return
		}
		serveIndex(w, r, fsys)
	})
}

// assetName maps an inbound request path to a path inside fsys, confined to
// assetRoot ("dist/..."). It never returns a path that can escape that root:
//
//   - "/" + reqPath forces the path to be rooted before cleaning, so
//     path.Clean's documented behavior -- "'..' elements that begin a rooted
//     path are replaced with a no-op", i.e. clamped at '/' rather than walking
//     above it -- applies unconditionally, regardless of how many ".."
//     segments reqPath contains.
//   - path.Join(assetRoot, clean) then Cleans again, so the result can only
//     ever be assetRoot itself or something rooted at "dist/".
//   - The HasPrefix check is defense in depth against that clamping behavior
//     ever regressing (e.g. a future refactor swapping path.Clean for
//     something else): any result that is not the root or root+"/" falls back
//     to the index rather than being opened.
//
// It deliberately does not percent-decode. net/http has already decoded
// URL.Path by the time a handler runs, and decoding again after cleaning would
// reintroduce the very traversal the clean just removed.
//
// io/fs additionally requires (fs.ValidPath) that names passed to Open contain
// no ".." elements and no leading "/" at all, and embed.FS enforces that itself
// -- so even a bug in this function that let a "../"-bearing name through would
// still be rejected by fsys.Open, not silently followed. This function exists
// anyway because CLAUDE.md requires paths to be cleaned and confinement-checked
// at the boundary, not because embed.FS's own check was found insufficient.
func assetName(reqPath string) string {
	clean := path.Clean("/" + reqPath)
	name := path.Join(assetRoot, clean)
	if name != assetRoot && !strings.HasPrefix(name, assetRoot+"/") {
		return indexPath
	}
	return name
}

// serveAsset attempts to serve name as a real file from fsys. It reports
// whether it did; a false return (missing, unreadable, or a directory) means
// the caller should fall back to the SPA index.
func serveAsset(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Every fs.FS this package is used with in practice (embed.FS,
		// fstest.MapFS) returns io.ReadSeeker files. Fall back rather than
		// serve without range/seek support.
		return false
	}

	http.ServeContent(w, r, path.Base(name), info.ModTime(), rs)
	return true
}

// serveIndex serves the SPA shell. It is the fallback for every request that
// does not name a real embedded asset, and also the terminal path for a
// missing/unreadable index itself -- which is a build-time invariant violation
// (dist/index.html always exists per the .gitignore rule above), not an
// attacker-reachable state, so it fails with 500 rather than panicking.
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	f, err := fsys.Open(indexPath)
	if err != nil {
		http.Error(w, "carbon browserui: embedded index.html is missing", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "carbon browserui: embedded index.html is missing", http.StatusInternalServerError)
		return
	}

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "carbon browserui: embedded index.html is missing", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", info.ModTime(), rs)
}
