package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/wui"
)

// This file is R1.1's half of the pin, and it lives in cmd/carbon deliberately.
//
// A `go get` alone does not keep a module in go.mod: `go mod tidy` drops a
// requirement nothing imports, and Factory is not imported by any Carbon file
// until R1.3 composes the browser surface. A pin nothing names is a pin that
// disappears on the next tidy, silently, and the first symptom is a release audit
// that cannot explain why the ledger and the module file disagree.
//
// So the probe names the Factory, Host and WUI APIs Carbon's composition will use,
// HERE, at the product composition root — which is the only place runbook 08's
// boundary permits them. It is a compile-only surface: if a released module stops
// carrying one of these names, this package stops building, which is the report
// the release lane wants rather than a runtime surprise.
var (
	// Factory: the composition root, its options, and the public authorization
	// sentinel an injected Authorizer must wrap to produce 403/not_authorized
	// rather than 500/internal_error.
	_ = factory.New
	_ = factory.WithPendingCommands
	_ = factory.WithPublicCreates
	_ = identity.ErrUnauthorized

	// Host: the composition and the Department registry R1.2 fills.
	_ = host.Compose
	_ = department.New
	_ = department.NewRigTarget
	// Host v0.6: failed composition can dispose a service before Start without
	// closing the caller's borrowed storage backend.
	_ = (*host.Service).CloseUnstarted
	_ department.LaunchTarget
	_ department.Runtime
	// The optional capability a product runtime MUST implement. Naming it here
	// is not decoration: it is the one capability department.Adapt forwards
	// without requiring, so nothing else in a build would notice if the
	// released module removed or re-signed it.
	_ department.AttemptCloser

	// WUI: the embedded SPA bundle, injected at the process root.
	_ = wui.Assets
)

// looprigImportOffenders reports every non-test .go file under root that imports a
// path with the given prefix. It is serveImportOffenders generalized; the two exist
// separately rather than one calling the other because serveImportOffenders is
// pinned by its own test against an exact-match rule, and widening it to prefixes
// would change what that test proves.
func looprigImportOffenders(root, prefix string) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range file.Imports {
			importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
				offenders = append(offenders, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offenders, nil
}

// TestFactoryIsComposedOnlyInTheCommand holds runbook 08's boundary: the single
// local process does not erase the service boundary. Factory imports neither Host
// nor Harness, and Carbon's internal runtime packages import neither Factory nor
// its adapters — the product composition root imports every module and injects
// their interfaces.
//
// The runbook's narrower phrasing is "internal Carbon runtime packages do not
// import Factory HTTP/Centrifuge adapters", and on released factory v0.7.1 that
// rule collapses into this one: the HTTP api, routing, realtime and placement
// packages are all under factory/internal, so they are UNREACHABLE from Carbon by
// construction. Asserting the whole module is therefore the strictly stronger rule
// and the only one that can actually be violated.
func TestFactoryIsComposedOnlyAtTheBrowserProductBoundary(t *testing.T) {
	t.Parallel()

	offenders, err := looprigImportOffenders(filepath.Join("..", "..", "internal"), "github.com/looprig/factory")
	if err != nil {
		t.Fatalf("scan internal: %v", err)
	}
	for _, path := range offenders {
		t.Errorf("%s imports Factory; the Factory composition belongs to the public browser product boundary", path)
	}
	boundary, err := looprigImportOffenders(filepath.Join("..", "..", "browser"), "github.com/looprig/factory")
	if err != nil {
		t.Fatalf("scan browser: %v", err)
	}
	if len(boundary) == 0 {
		t.Fatal("browser product boundary no longer imports Factory")
	}
}

// TestHostIsComposedOnlyAtTheProductBoundary completes R1.1 step 2a, which names
// Factory, Host and WUI but whose Host half had no test.
//
// The rule is not "internal/ may not import Host": internal/app/department.go
// legitimately imports host/department, because that is where a harness session is
// made to satisfy Host's contracts and the adapter has to live inside the product
// boundary. The rule is that Host stops THERE — the catalog, which holds the one
// Carbon identity and prompt, has no business knowing a Host exists, and neither does
// any package added beside it.
//
// SO THE SCAN IS internal/ WITH internal/app ALLOW-LISTED, not internal/catalog. Until
// the R1.3 round this case scanned internal/catalog alone while its comment claimed
// "neither does any package added beside it" — and a NEW package under internal/
// importing host/department passed (regate mutant H2, exit 0). The allow-list states
// the exception once, in the one place the rule is enforced, so a third package added
// beside the two is caught the day it is added rather than the day internal/ is
// re-read. Widening the allow-list is a visible diff; widening a scan root is not.
func TestHostIsComposedOnlyAtTheProductBoundary(t *testing.T) {
	t.Parallel()

	// The ONE package permitted to name Host, and why: internal/app/department.go is
	// where a harness session.SessionController is made to satisfy host/department's
	// contracts, and that adapter has to live inside the product boundary.
	const permitted = "internal/app/"

	offenders, err := looprigImportOffenders(filepath.Join("..", "..", "internal"), "github.com/looprig/host")
	if err != nil {
		t.Fatalf("scan internal: %v", err)
	}
	var permittedSeen bool
	for _, path := range offenders {
		if strings.Contains(filepath.ToSlash(path), permitted) {
			permittedSeen = true
			continue
		}
		t.Errorf("%s imports Host; Host is composed in internal/app's launch target and cmd/carbon, nowhere else", path)
	}
	// The allow-list is held to a real importer, so the day internal/app stops naming
	// Host this case says so instead of silently becoming an exception for nothing —
	// an allow-list nobody exercises is a hole waiting for the next package.
	if !permittedSeen {
		t.Errorf("no file under %s imports Host; the allow-list exempts a package that no longer needs it", permitted)
	}
}

// TestWUIIsInjectedOnlyByTheProductBoundary keeps the embedded browser bundle at the
// public browser root. wui's Go API is http.Handler in, http.Handler out and names no
// looprig type, so an internal package importing it would compile perfectly and
// merely mean the SPA bundle had been welded to a runtime package that has no
// business embedding 2 MiB of JavaScript.
func TestWUIIsInjectedOnlyByTheProductBoundary(t *testing.T) {
	t.Parallel()

	offenders, err := looprigImportOffenders(filepath.Join("..", "..", "internal"), "github.com/looprig/wui")
	if err != nil {
		t.Fatalf("scan internal: %v", err)
	}
	for _, path := range offenders {
		t.Errorf("%s imports wui; the SPA bundle is injected by browser", path)
	}
	boundary, err := looprigImportOffenders(filepath.Join("..", "..", "browser"), "github.com/looprig/wui")
	if err != nil {
		t.Fatalf("scan browser: %v", err)
	}
	if len(boundary) == 0 {
		t.Fatal("browser product boundary no longer imports WUI")
	}
}

// TestLooprigImportOffendersFindsOnlyNonTestGoFiles is the falsifier for the guard
// above, in the shape TestServeImportOffendersFindsOnlyNonTestGoFiles already has:
// a guard that scans nothing passes for the wrong reason forever.
func TestLooprigImportOffendersFindsOnlyNonTestGoFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(rel, src string) string {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return full
	}

	const importsFactory = "package p\n\nimport _ \"github.com/looprig/factory/identity\"\n"
	const importsNeighbour = "package p\n\nimport _ \"github.com/looprig/factoryish\"\n"
	const clean = "package p\n\nimport _ \"fmt\"\n"

	offender := write("app/wiring.go", importsFactory)
	write("app/wiring_test.go", importsFactory) // a test may drive the composition
	write("app/neighbour.go", importsNeighbour) // a prefix match must not be a substring match
	write("app/clean.go", clean)

	got, err := looprigImportOffenders(root, "github.com/looprig/factory")
	if err != nil {
		t.Fatalf("looprigImportOffenders: %v", err)
	}
	if len(got) != 1 || got[0] != offender {
		t.Fatalf("offenders = %v, want exactly [%s]", got, offender)
	}
}
