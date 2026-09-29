package browserui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// TestBundleProtocolVersionReadsEmbeddedManifest pins the three versions the
// embedded bundle declares, by absolute literal.
//
// Deriving these from the files write-bundle-manifest.mjs reads would pin
// nothing: the assertion would hold for whatever those files happened to say,
// including a value that had drifted out from under the committed bundle. The
// literals are the point -- moving the pinned Core or the protocol package must
// require touching this test, which is where a reviewer is told the marker
// consumers read has changed.
//
// Release IS pinned here, by the same literal rule, and that is a change of
// premise rather than a tightening. It used to be excluded for a stated reason:
// "a literal false would make the release target fail its own gate, and a
// literal true would fail on every development commit". The first half still
// holds. The second half assumed the committed bundle is ordinarily a
// development build -- true until task U6.0-shipdist, which installed a
// `make release-dist` bundle as the tree this repository ships from here on.
// The module zip is source-only, so the committed tree IS the artefact every
// consumer embeds; it does not revert to a development build between releases,
// and a commit that made it one would be the defect, not the norm.
//
// So the literal is `true`, and what it catches is the accident that is now the
// realistic one: an ordinary `npm run build` overwrites dist/ with a marker
// saying release=false (the plugin reads CARBON_BUNDLE_RELEASE, which only
// `make release-dist` sets), and that downgrade reaches a commit. `make
// dist-reset` restores the committed bundle.
func TestBundleProtocolVersionReadsEmbeddedManifest(t *testing.T) {
	t.Parallel()

	got, err := BundleProtocolVersion()
	if err != nil {
		t.Fatalf("BundleProtocolVersion(): unexpected error %v", err)
	}
	if got.CoreVersion != "v0.12.0" {
		t.Errorf("BundleProtocolVersion().CoreVersion = %q, want %q", got.CoreVersion, "v0.12.0")
	}
	if got.ProtocolVersion != "0.3.0" {
		t.Errorf("BundleProtocolVersion().ProtocolVersion = %q, want %q", got.ProtocolVersion, "0.3.0")
	}
	if !got.Release {
		t.Errorf("BundleProtocolVersion().Release = false, want true: the committed bundle is the shipped release artefact (task U6.0-shipdist); run `make dist-reset` if a development build overwrote it")
	}
	if got.SessionwireVersion != 1 {
		t.Errorf("BundleProtocolVersion().SessionwireVersion = %d, want %d", got.SessionwireVersion, 1)
	}
}

// TestEmbeddedBundleReleaseClaimMatchesItsTree is the property a consumer gates
// on: the no-Node placeholder declares itself non-release (runbook 06, task
// U5.3), and Factory's default command refuses it on that basis (runbook 05,
// task A9.2).
//
// Stated as implications rather than as an equality, because the marker's value
// alone does not distinguish a legitimate tree from an illegitimate one. What
// is never legitimate is the placeholder claiming to be a release, and that is
// what a consumer would have no way to detect for itself -- it sees the marker,
// never the tree.
//
// The check is on the parsed marker rather than on the file's bytes: a consumer
// never sees the bytes, and a reader that silently defaulted Release to true
// would pass a bytes-level assertion.
//
// WHAT THIS TEST USED TO ASSERT, AND WHY IT NO LONGER CAN. Until task
// U6.0-shipdist it required `marker.Release == (CARBON_BUNDLE_RELEASE == "1")`,
// an EQUALITY, on the premise that the committed bundle is a development build
// except during a release run. U6.0-shipdist installed a `make release-dist`
// bundle as the committed tree, which is what makes Carbon's SPA shippable --
// the module zip is source-only, so whatever is committed under dist/ is what
// every consumer embeds. Under that premise the equality is not merely weak,
// it is WRONG in the reverse direction: it makes `GOWORK=off make check` and
// `GOWORK=off go test ./...` -- the workspace's own standalone verification --
// fail for every developer and every CI run on a correct release tree. It was
// measured failing exactly that way before being replaced.
//
// The surviving direction is the one that is still invariant: a release run
// must produce a marker that claims a release. That catches the wiring defect
// release-dist.mjs's assertReleaseMarker catches from the Node side, from the
// Go side, and it is silent about a release marker outside a release run --
// which is now the ordinary state of this repository.
//
// The accident the dropped direction was aimed at ("a marker committed with
// release: true from an ordinary npm run build") is not lost: it is now covered
// better, and by its actual mechanism, in
// TestBundleProtocolVersionReadsEmbeddedManifest. An ordinary build writes
// release=FALSE, so the realistic downgrade is a false marker on a committed
// tree, and a literal pin is what sees it.
func TestEmbeddedBundleReleaseClaimMatchesItsTree(t *testing.T) {
	t.Parallel()

	got, err := BundleProtocolVersion()
	if err != nil {
		t.Fatalf("BundleProtocolVersion(): unexpected error %v", err)
	}
	built, err := treeCarriesBuiltOutput(assetsFS)
	if err != nil {
		t.Fatalf("treeCarriesBuiltOutput: %v", err)
	}
	// `make release-dist` sets CARBON_BUNDLE_RELEASE=1 on the node process
	// (Makefile) and release-dist.mjs spawns this very `go test` with
	// {...process.env}, so the variable is present in the environment of a
	// release run's test and absent in every other run.
	if fault := releaseClaimFault(got.Release, built, os.Getenv(bundleReleaseEnv) == "1"); fault != "" {
		t.Fatalf("embedded bundle: %s (marker release=%t, tree built=%t, %s=%q)",
			fault, got.Release, built, bundleReleaseEnv, os.Getenv(bundleReleaseEnv))
	}
}

// releaseClaimFault names why a release claim is incoherent with the tree it
// describes and the run observing it, or returns "" when it is coherent.
//
// Extracted from the assertion above so the rule is a value a table can cross
// rather than a branch only ever reached with one input: //go:embed fixes the
// embedded tree at compile time, so the live test can exercise exactly one of
// the eight combinations and could not tell a correct rule from `return ""`.
func releaseClaimFault(claimsRelease, treeIsBuilt, releaseRun bool) string {
	if claimsRelease && !treeIsBuilt {
		return "the tree holds only " + bundleIndexPath + " and the manifest, yet its marker claims a release build"
	}
	if releaseRun && !claimsRelease {
		return "a release run installed a tree whose marker does not claim a release, so " + bundleReleaseEnv + " did not reach the build"
	}
	return ""
}

// TestReleaseClaimFaultCrossesEveryCombination pins the rule at every point,
// including the two the live assertion can never visit and the one this repo
// deliberately stopped calling a fault.
func TestReleaseClaimFaultCrossesEveryCombination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		claimsRelease bool
		treeIsBuilt   bool
		releaseRun    bool
		wantFault     bool
	}{
		{name: "placeholder claiming a release, outside a release run", claimsRelease: true, treeIsBuilt: false, releaseRun: false, wantFault: true},
		{name: "placeholder claiming a release, inside a release run", claimsRelease: true, treeIsBuilt: false, releaseRun: true, wantFault: true},
		{name: "release run whose marker does not claim a release", claimsRelease: false, treeIsBuilt: true, releaseRun: true, wantFault: true},
		{name: "release run over a placeholder with no claim: reported as the flag fault", claimsRelease: false, treeIsBuilt: false, releaseRun: true, wantFault: true},
		{name: "release run over a built tree that claims a release", claimsRelease: true, treeIsBuilt: true, releaseRun: true},
		{
			// The combination the old equality called a fault. It is the
			// committed state of this repository after task U6.0-shipdist, and
			// calling it a fault made every ordinary check run red.
			name:          "committed release bundle read outside a release run",
			claimsRelease: true, treeIsBuilt: true, releaseRun: false,
		},
		{name: "development build outside a release run", claimsRelease: false, treeIsBuilt: true, releaseRun: false},
		{name: "no-Node placeholder making no claim", claimsRelease: false, treeIsBuilt: false, releaseRun: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := releaseClaimFault(tt.claimsRelease, tt.treeIsBuilt, tt.releaseRun)
			if (got != "") != tt.wantFault {
				t.Fatalf("releaseClaimFault(%t, %t, %t) = %q, want a fault = %t",
					tt.claimsRelease, tt.treeIsBuilt, tt.releaseRun, got, tt.wantFault)
			}
		})
	}
}

// bundleReleaseEnv is app/vite.config.ts's BUNDLE_RELEASE_ENV, the one
// variable that makes a build a release build. Duplicated as a literal rather
// than read from the TypeScript, because the Go side has no way to read it and
// a wrong value here would silently weaken the assertion above rather than
// fail: if the two ever disagree, the release run is the place it shows.
const bundleReleaseEnv = "CARBON_BUNDLE_RELEASE"

// TestBundleProtocolVersionReadsTheFileRatherThanRestatingIt pins what
// bundle.go:7-13 says the whole file exists for: the values are read out of the
// bundle, so that "a constant would keep saying the right thing while the tree
// beneath it went stale" cannot happen here.
//
// It compares BundleProtocolVersion() against the marker read from the LIVE
// working tree (os.DirFS("."), i.e. dist/looprig-bundle.json on disk) through
// the same reader, value and error both. Any implementation that answers from
// constants agrees with this only for as long as those constants happen to
// equal the file; the moment the marker moves -- which is every release and
// every protocol bump -- it fails.
//
// Stated honestly about its limit: a constant that is exactly equal to the
// current marker in all four fields is not distinguishable from the real read
// by any in-process test, because //go:embed fixes the embedded bytes at
// compile time and no test can vary them. That mutant is equivalent FOR THE
// CURRENT TREE and only for it; this test is what makes it stop being
// equivalent the first time anything in the marker changes.
func TestBundleProtocolVersionReadsTheFileRatherThanRestatingIt(t *testing.T) {
	t.Parallel()

	got, gotErr := BundleProtocolVersion()
	// The source tree beside the test, not the compiled-in copy: this is the
	// file `make release-dist` rewrites and a developer could edit.
	want, wantErr := readBundleManifest(os.DirFS("."))
	if !errors.Is(gotErr, wantErr) && !errors.Is(wantErr, gotErr) {
		t.Fatalf("BundleProtocolVersion() error = %v, reading %s from the working tree gives %v",
			gotErr, bundleManifestPath, wantErr)
	}
	if got != want {
		t.Errorf("BundleProtocolVersion() = %+v, but %s on disk says %+v", got, bundleManifestPath, want)
	}
}

// TestTreeCarriesBuiltOutputSeparatesThePlaceholderFromABuild is what
// TestEmbeddedBundleReleaseClaimMatchesItsTree's implication rests on. Without
// it that test passes for whichever answer treeCarriesBuiltOutput happens to
// give, including a constant one.
func TestTreeCarriesBuiltOutputSeparatesThePlaceholderFromABuild(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fsys fstest.MapFS
		want bool
	}{
		{
			// The shape a checkout has when only the two unconditionally
			// tracked files are present: no build has run.
			name: "index and manifest alone are the no-Node placeholder",
			fsys: fstest.MapFS{
				bundleIndexPath:    {Data: []byte("<html></html>")},
				bundleManifestPath: {Data: []byte("{}")},
			},
			want: false,
		},
		{
			name: "a hashed asset is output only a build produces",
			fsys: fstest.MapFS{
				bundleIndexPath:            {Data: []byte("<html></html>")},
				bundleManifestPath:         {Data: []byte("{}")},
				assetRoot + "/assets/i.js": {Data: []byte("export{}")},
			},
			want: true,
		},
		{
			// Not tied to assetsDir: Vite's asset directory is configurable and
			// a build emits files beside the shell too. Any file that is
			// neither of the two tracked ones is build output.
			name: "output outside the asset directory counts",
			fsys: fstest.MapFS{
				bundleIndexPath:                     {Data: []byte("<html></html>")},
				bundleManifestPath:                  {Data: []byte("{}")},
				assetRoot + "/favicon-DEADBEEF.svg": {Data: []byte("<svg/>")},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := treeCarriesBuiltOutput(tt.fsys)
			if err != nil {
				t.Fatalf("treeCarriesBuiltOutput: %v", err)
			}
			if got != tt.want {
				t.Errorf("treeCarriesBuiltOutput = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadBundleManifestRefusesAReleaseClaimOverThePlaceholder is the reader's
// half of the same property. It is enforced in the reader, not left to each
// consumer, because the consumer holds only the Bundle: by the time Factory
// compares versions the tree is no longer in its hands.
func TestReadBundleManifestRefusesAReleaseClaimOverThePlaceholder(t *testing.T) {
	t.Parallel()

	const release = `{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":1}`

	got, err := readBundleManifest(manifestFS(release))
	if err == nil {
		t.Fatalf("readBundleManifest = %+v, want an error for a release claim over the placeholder", got)
	}
	if got != (Bundle{}) {
		t.Errorf("readBundleManifest returned %+v alongside its error", got)
	}
	// Distinguishable from the absent-manifest case, which is a different
	// remedy: one needs a build, the other needs a bundle new enough to have one.
	if errors.Is(err, ErrNoBundleManifest) {
		t.Errorf("a lying release marker reported %v as a missing one", err)
	}

	// The same bytes over a built tree are fine, so the rejection is about the
	// tree and not about the word "release".
	if _, err := readBundleManifest(builtFS(release)); err != nil {
		t.Errorf("readBundleManifest over a built tree: unexpected error %v", err)
	}
}

// TestBundleManifestNamesThePinnedCore cross-checks the marker against
// contract/VERSION -- a different authority, written by a different tool, and
// already held to go.mod and the Makefile by contract/contract_test.go.
//
// Without this, the manifest could name any Core at all and only the literal
// above would notice, which a version bump silently updates.
func TestBundleManifestNamesThePinnedCore(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../web/client-contract-version")
	if err != nil {
		t.Fatalf("reading contract/VERSION: %v", err)
	}
	want := strings.TrimSpace(string(raw))
	if want == "" {
		t.Fatal("contract/VERSION is empty; this test would assert nothing")
	}

	got, err := BundleProtocolVersion()
	if err != nil {
		t.Fatalf("BundleProtocolVersion(): unexpected error %v", err)
	}
	if got.CoreVersion != want {
		t.Fatalf("BundleProtocolVersion().CoreVersion = %q, contract/VERSION = %q", got.CoreVersion, want)
	}
}

// TestReadBundleManifestAcceptsARelease proves the reader reports what the
// manifest says rather than a constant. Without it, a reader hardcoding
// Release=false would satisfy every other case in this file.
func TestReadBundleManifestAcceptsARelease(t *testing.T) {
	t.Parallel()

	fsys := builtFS(
		`{"core_version":"v9.4.1","client_version":"3.2.1","react_version":"0.2.0","release":true,"sessionwire_version":2}`,
	)
	got, err := readBundleManifest(fsys)
	if err != nil {
		t.Fatalf("readBundleManifest: unexpected error %v", err)
	}
	want := Bundle{
		CoreVersion:        "v9.4.1",
		ProtocolVersion:    "3.2.1",
		ReactVersion:       "0.2.0",
		Release:            true,
		SessionwireVersion: 2,
	}
	if got != want {
		t.Fatalf("readBundleManifest = %+v, want %+v", got, want)
	}
}

// TestReadBundleManifestFailsClosed covers every shape a consumer must not be
// handed a usable Bundle for. Factory compares the marker with Core's supported
// version and fails closed on an old, empty or future one; that comparison is
// only meaningful if a malformed or absent marker never arrives as a zero-value
// Bundle that happens to compare equal to something.
//
// Every fixture here is a BUILT tree, not the placeholder. These cases all
// carry release:true, and over a placeholder the release-coherence rule above
// would reject them before the defect under test was ever reached -- each case
// would pass for a reason it does not name.
func TestReadBundleManifestFailsClosed(t *testing.T) {
	t.Parallel()

	const valid = `{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":1}`

	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr error // sentinel the caller may classify on, or nil for "any error"
	}{
		{
			name: "no manifest at all: the pre-marker bundle wui v0.1.0 shipped",
			fsys: fstest.MapFS{bundleIndexPath: {Data: []byte("<html></html>")}},
			// A consumer must be able to tell this apart from a manifest that
			// declares itself non-release: one is an old artefact, the other is
			// a current artefact saying "not a release".
			wantErr: ErrNoBundleManifest,
		},
		{name: "empty file", fsys: builtFS("")},
		{name: "not JSON at all", fsys: builtFS("looprig")},
		{name: "JSON but not an object", fsys: builtFS(`["v0.7.0"]`)},
		{name: "truncated object", fsys: builtFS(`{"core_version":"v0.7.0"`)},
		{
			// A second document after the first would otherwise be ignored
			// entirely, so a file whose real content is appended below a decoy
			// would read as the decoy.
			name: "trailing document after the manifest",
			fsys: builtFS(valid + `{"release":false}`),
		},
		{
			// The reader and the writer ship together in one module, so an
			// unrecognised key is a malformed or foreign file, not a newer
			// manifest this build should tolerate.
			name: "unknown field",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":1,"signed":true}`),
		},
		{
			name: "absent sessionwire version reads as the zero value",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true}`),
		},
		{
			name: "zero sessionwire version",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":0}`),
		},
		{
			name: "negative sessionwire version",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":-1}`),
		},
		{
			name: "sessionwire version as a string",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":"1"}`),
		},
		{
			name: "empty core version",
			fsys: builtFS(`{"core_version":"","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":1}`),
		},
		{
			name: "absent core version",
			fsys: builtFS(`{"client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":1}`),
		},
		{
			name: "empty protocol version",
			fsys: builtFS(`{"core_version":"v0.7.0","client_version":"","react_version":"0.2.0","release":true,"sessionwire_version":1}`),
		},
		{
			name: "absent protocol version",
			fsys: builtFS(`{"core_version":"v0.7.0","react_version":"0.2.0","release":true,"sessionwire_version":1}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readBundleManifest(tt.fsys)
			if err == nil {
				t.Fatalf("readBundleManifest = %+v, want an error", got)
			}
			if got != (Bundle{}) {
				t.Errorf("readBundleManifest returned %+v alongside its error; a rejected manifest must yield no usable marker", got)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("readBundleManifest error = %v, want one matching %v", err, tt.wantErr)
			}
		})
	}
}

// TestBundleManifestErrorsAreClassifiable keeps the two failure modes apart:
// only an ABSENT manifest is ErrNoBundleManifest. A present-but-malformed one
// must not be, or a consumer could not distinguish "this build predates the
// marker" from "this build's marker is corrupt".
func TestBundleManifestErrorsAreClassifiable(t *testing.T) {
	t.Parallel()

	_, err := readBundleManifest(builtFS(`{"core_version":"v0.7.0","client_version":"0.2.0","react_version":"0.2.0","release":true,"sessionwire_version":0}`))
	if err == nil {
		t.Fatal("readBundleManifest: want an error for a zero sessionwire version")
	}
	if errors.Is(err, ErrNoBundleManifest) {
		t.Fatalf("a malformed manifest reported %v as a missing one", err)
	}
}

// manifestFS is the no-Node placeholder shape: the two unconditionally tracked
// files and nothing a build produced.
func manifestFS(body string) fstest.MapFS {
	return fstest.MapFS{
		bundleIndexPath:    {Data: []byte("<html></html>")},
		bundleManifestPath: {Data: []byte(body)},
	}
}

// builtFS is the placeholder plus one hashed asset: the shape a build leaves,
// and the only shape a manifest may claim release over.
func builtFS(body string) fstest.MapFS {
	fsys := manifestFS(body)
	fsys[assetRoot+"/assets/index-DEADBEEF.js"] = &fstest.MapFile{Data: []byte("export{}")}
	return fsys
}
