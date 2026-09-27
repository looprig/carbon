package browserui

// bundle.go exposes the embedded SPA build's protocol marker.
//
// The module zip is source-only and `go get` runs no build step, so whatever is
// COMMITTED under dist/ is what every consumer's //go:embed all:dist serves.
// That makes the bundle a versioned artefact in its own right: the JavaScript in
// it speaks a particular sessionwire version, was built against a particular
// pinned Core, and carries a particular @looprig/protocol build. None of those
// are properties of the Go source beside it, which is why they are read out of
// the bundle rather than declared as constants here -- a constant would keep
// saying the right thing while the tree beneath it went stale, which is exactly
// what wui v0.1.0 shipped (retracted; see go.mod).
//
// app/scripts/write-bundle-manifest.mjs writes the file, from the three
// authorities that own the three values; vite.config.ts's bundleManifestPlugin
// calls it on every build so the marker is produced by the same step that
// produces the bundle. This file only reads and validates it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

// bundleManifestPath is the manifest's path inside assetsFS. It is the
// BUNDLE_MANIFEST_NAME of app/scripts/write-bundle-manifest.mjs, under
// assetRoot.
const bundleManifestPath = assetRoot + "/looprig-bundle.json"

// bundleIndexPath is the SPA shell. With the manifest it is one of the two
// files .gitignore keeps tracked unconditionally, so a tree holding these two
// and nothing else is a checkout on which no build has run.
const bundleIndexPath = assetRoot + "/index.html"

// ErrNoBundleManifest reports that the embedded bundle carries no manifest at
// all -- the shape every wui build before this marker existed has, including
// the retracted v0.1.0.
//
// It is deliberately distinguishable from a manifest that parses and declares
// itself non-release: one is an artefact that predates the marker, the other is
// a current artefact stating its own status. A consumer that cannot tell them
// apart cannot report which of the two it is refusing.
var ErrNoBundleManifest = errors.New("carbon browserui: embedded bundle carries no manifest")

// Bundle is the embedded SPA build's self-description.
//
// It is a plain comparable struct of exported fields on purpose: a consumer
// (Factory's default command) has to be able to construct one to test its own
// acceptance rule against an old, empty or future marker without needing a
// second wui build to embed.
type Bundle struct {
	// CoreVersion is the github.com/looprig/core module version whose
	// sessionwire/v1 schemas the bundle's client was built against. It is the
	// same string contract/VERSION holds.
	CoreVersion string `json:"core_version"`
	// ProtocolVersion is the @looprig/protocol package version in the bundle.
	// It is also the version string the Centrifuge connect frame advertises;
	// app/scripts/write-bundle-manifest.test.ts drives the real handshake and
	// asserts the two agree.
	ProtocolVersion string `json:"client_version"`
	ReactVersion    string `json:"react_version"`
	// Release reports whether the release process produced this tree. A
	// development build, and the tree committed so //go:embed all:dist compiles
	// with no Node toolchain installed, are both false.
	//
	// An absent key decodes to false, which is the fail-closed direction: a
	// manifest that does not say it is a release is not treated as one. The
	// converse is checked rather than trusted: readBundleManifest refuses a
	// true here over a tree that carries no build output.
	Release bool `json:"release"`
	// SessionwireVersion is the sessionwire protocol version the bundle's
	// client negotiates -- the `const` in Core's
	// version_negotiation_response.schema.json. This is the quantity a server
	// compares against its own Core support.
	SessionwireVersion int `json:"sessionwire_version"`
}

// BundleProtocolVersion returns the embedded bundle's marker.
//
// It returns an error rather than a zero Bundle for a missing or malformed
// manifest, because a zero Bundle is a value a caller could accidentally
// compare against something and pass. Callers should treat any error, and any
// Bundle whose Release is false, as "do not serve this as an official build".
func BundleProtocolVersion() (Bundle, error) {
	return readBundleManifest(assetsFS)
}

// readBundleManifest reads and validates the manifest from an arbitrary fs.FS,
// so every rejection below can be driven from a fixture rather than needing a
// second real //go:embed tree -- the same reason newAssetHandler is
// parameterized.
func readBundleManifest(fsys fs.FS) (Bundle, error) {
	data, err := fs.ReadFile(fsys, bundleManifestPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %s: %w", ErrNoBundleManifest, bundleManifestPath, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	// The reader and app/scripts/write-bundle-manifest.mjs ship in one module
	// and always move together, so an unrecognised key is a malformed or
	// foreign file rather than a newer manifest to tolerate.
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("carbon browserui: decoding %s: %w", bundleManifestPath, err)
	}
	// encoding/json stops at the end of the first value. Without this, a file
	// holding a decoy manifest followed by anything at all would read as the
	// decoy with no complaint.
	if decoder.More() {
		return Bundle{}, fmt.Errorf("carbon browserui: %s has trailing content after the manifest", bundleManifestPath)
	}

	if bundle.CoreVersion == "" {
		return Bundle{}, fmt.Errorf("carbon browserui: %s names no core version", bundleManifestPath)
	}
	if bundle.ReactVersion == "" {
		return Bundle{}, fmt.Errorf("carbon browserui: %s names no React package version", bundleManifestPath)
	}
	if bundle.ProtocolVersion == "" {
		return Bundle{}, fmt.Errorf("carbon browserui: %s names no protocol version", bundleManifestPath)
	}
	// Zero is what an absent key decodes to, so an empty marker and an
	// explicitly zero one are one rejection rather than two behaviours.
	if bundle.SessionwireVersion < 1 {
		return Bundle{}, fmt.Errorf(
			"carbon browserui: %s declares sessionwire version %d, want a positive version",
			bundleManifestPath, bundle.SessionwireVersion,
		)
	}
	if bundle.Release {
		built, err := treeCarriesBuiltOutput(fsys)
		if err != nil {
			return Bundle{}, fmt.Errorf("carbon browserui: inspecting the embedded bundle: %w", err)
		}
		if !built {
			return Bundle{}, fmt.Errorf(
				"carbon browserui: %s claims a release build, but the tree holds only %s and the manifest",
				bundleManifestPath, bundleIndexPath,
			)
		}
	}
	return bundle, nil
}

// treeCarriesBuiltOutput reports whether fsys holds anything an app build
// produced, as opposed to only the two files tracked unconditionally so
// //go:embed all:dist compiles with no Node toolchain installed.
//
// "Anything else at all" rather than "something under dist/assets": Vite's
// assetsDir is configurable and a build emits files beside the shell as well,
// so naming the asset directory here would make this answer wrong the moment
// vite.config.ts changed one string. What it actually measures is whether the
// tree is bigger than the checked-in floor.
func treeCarriesBuiltOutput(fsys fs.FS) (bool, error) {
	built := false
	err := fs.WalkDir(fsys, assetRoot, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || name == bundleIndexPath || name == bundleManifestPath {
			return nil
		}
		built = true
		return fs.SkipAll
	})
	if err != nil {
		return false, err
	}
	return built, nil
}
