package app

// This compile-only dependency-surface probe covers both the completed rig
// migration and Task 34's context/hustle additions. It builds only when the
// planned core, inference, LLM, harness, and CLI APIs are all present through
// Carbon's published module pins. It carries no test functions because its value
// is that the package cannot compile against a partial dependency rollout.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	carbon "github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
	"github.com/looprig/tui"
)

var (
	_ = rig.Define
	_ = loop.Define
	_ session.SessionController
	_ = event.ActiveLoopChanged{}
	_ = event.LoopStarted{DisplayName: "carbon"}
	_ tui.Agent
	_ = loop.WithDisplayName
	_ = rig.WithOffloadGC

	_ content.TokenCount = 1
	_                    = model.ContextLimits{WindowTokens: 1}
	_                    = contextcount.InferenceCapability{}
	_                    = contextcount.CounterCapability{}
	_                    = contextcount.NewEstimator
	_                    = llm.ProviderChutes
	_                    = llm.ProviderPhala
	_                    = llm.ProviderLMStudio

	_                       = command.Compact{}
	_                       = event.ContextMeasured{}
	_                       = event.CompactionStarted{}
	_                       = event.CompactionCommitted{}
	_                       = event.CompactionRejected{}
	_                       = event.HustleStarted{}
	_                       = event.HustleCompleted{}
	_                       = event.HustleFailed{}
	_ event.EventVisibility = event.Public
	_                       = event.ShouldDeliver
	_                       = hustle.Define
	_                       = loop.WithContextCounter
	_                       = loop.WithInferenceCapability
	_                       = loop.WithContextObservation
	_                       = loop.WithCompaction
	_                       = rig.WithHustles
	_                       = rig.WithHustleLimits
)

// readGoMod returns the module file's text. Every pin assertion below reads the
// FILE rather than a build-time constant, because what a release is audited on is
// the committed requirement line, not a symbol that happens to resolve today.
func readGoMod(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// requiredVersion reports the version go.mod names for a module path, or "".
func requiredVersion(gomod, module string) string {
	pattern := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + ` (v\S+)`)
	match := pattern.FindStringSubmatch(gomod)
	if match == nil {
		return ""
	}
	return match[1]
}

// TestServeDependenciesArePinned proves carbon names the harness release carrying
// the attach-or-restore fix (wui design §8.1), the exported tool-result capture
// default used by Carbon's policy test, and a usable wui release. A harness below
// v0.30 restores an already-live session by overwriting the registry entry,
// orphaning every subscriber; and wui v0.1.1 is the first wui whose module zip
// carries the BUILT SPA bundle — v0.1.0 was tagged from a development tree, ships
// only the dist/index.html placeholder, and is retracted upstream, so naming it
// would serve every browser the string "build the app to replace this placeholder"
// with no downstream repair possible (module zips are source-only).
func TestServeDependenciesArePinned(t *testing.T) {
	t.Parallel()

	gomod := readGoMod(t)

	harnessVersion := requiredVersion(gomod, "github.com/looprig/harness")
	if harnessVersion == "" {
		t.Fatal("go.mod does not require github.com/looprig/harness")
	}
	if !versionAtLeast(harnessVersion, "v0.36.0") {
		t.Errorf("go.mod requires harness %s; the orchestration lane needs >= v0.36.0", harnessVersion)
	}

	wui := requiredVersion(gomod, "github.com/looprig/wui")
	if wui == "" {
		t.Fatal("go.mod does not require github.com/looprig/wui")
	}
	if wui == "v0.1.0" || wui == "v0.1.1" {
		t.Errorf("go.mod requires wui %s; v0.2.0 is the first bundle a gating consumer accepts", wui)
	}
}

// TestOrchestrationPinsAreTheReleasedOnes is R1.1's ledger. It names the exact
// released version of every module the Factory/Host composition stands on, and it
// reads go.mod rather than trusting the build, so a pin lowered by a careless
// `go get` is a red test and not a silent downgrade.
//
// EACH ROW CARRIES ITS OWN REASON. A table of versions with no reasons is a table
// nobody can audit: the next person cannot tell which rows are load-bearing and
// which were copied forward, so every one of them gets copied forward again.
func TestOrchestrationPinsAreTheReleasedOnes(t *testing.T) {
	t.Parallel()

	gomod := readGoMod(t)

	for _, row := range []struct {
		module  string
		version string
		why     string
	}{
		{
			"github.com/looprig/core", "v0.12.0",
			"defines the principal, metadata and attribution capability contract that Factory, Host and harness carry; a stamped command is not readable by the pre-feature stack",
		},
		{
			"github.com/looprig/storage", "v0.7.0",
			"the contract BlobReaderLifecycle is declared in, which sessionstore.Open consults; v0.7.0 makes 'a key and a key extending it with /' a conformance obligation, which the tool-result object index beneath a session's catalog key depends on",
		},
		{
			"github.com/looprig/fsstore", "v0.6.0",
			"a KV key and a key beneath it coexist ('@' leaf suffixes), so a session's tool-result object index can be written beside its catalog entry; ONE-WAY: v0.6.0 refuses every pre-v0.6.0 root with ErrLegacyLayout and migrates nothing, which Carbon surfaces as LegacyDataRootError",
		},
		{
			"github.com/looprig/sessionstore", "v0.14.0",
			"stores principal and metadata in v3 disposition inbox rows; ONE-WAY: after a v3 row exists, never roll Factory or Host back below sessionstore v0.14.0, whose reader accepts it",
		},
		{
			"github.com/looprig/harness", "v0.41.0",
			"Admitted carries the principal and create/input metadata and harness writes stamped or presented journal records; ONE-WAY: after such a journal record exists, never roll Carbon back below harness v0.41.0",
		},
		{
			"github.com/looprig/host", "v0.11.0",
			"RuntimeCommand carries principal and metadata through the strict pre-attempt checks; paired with harness v0.41.0 because a journal containing stamped or presented records cannot be read by older harness",
		},
		{
			"github.com/looprig/factory", "v0.12.0",
			"accepts client metadata and can stamp a verified principal with explicit WithPrincipalStamping (not Carbon's default); it refuses an incapable Host before writing a v3 inbox row, whose one-way reader floor is sessionstore v0.14.0",
		},
		{
			"github.com/looprig/wui", "v0.4.0",
			"the browser bundle can send create/input metadata and display attributed, presented messages; those messages create a one-way journal floor of harness v0.41.0 once written",
		},
		{
			"github.com/looprig/inference", "v0.14.0",
			"supports per-call unbounded execution for the harness v0.41.0 presenter/runtime work; the same release set has a one-way harness v0.41.0 floor after attributed or presented journal records are written",
		},
		{
			"github.com/looprig/tools", "v0.14.1",
			// This row read "it ships NO read_tool_result, which is why R1.2 steps
			// 6-7 were struck" until v0.14.0 shipped the tool. Steps 6-7 are now
			// met: the pooled serve path registers read_tool_result exactly where
			// it wires tool-result retention (internal/app/toolresults.go).
			"read_tool_result (tools.ReadToolResultDefinition, v0.14.0; v0.14.1 re-pins onto harness v0.40.2), registered on the pooled serve path where retention is wired; Bash streams its complete result into the capture sink and declares its capture safety; AskUser declares tool.UserInputReplaySafe (v0.13.0), so an ask_user gate survives failover",
		},
	} {
		got := requiredVersion(gomod, row.module)
		if got == "" {
			t.Errorf("go.mod does not require %s; it is needed because %s", row.module, row.why)
			continue
		}
		if got != row.version {
			t.Errorf("go.mod requires %s %s, want %s: %s", row.module, got, row.version, row.why)
		}
	}
}

// TestHostAndHarnessPinsMoveTogether is the one cross-row rule in the ledger, and
// it is stated separately because it is not a property of either version alone.
//
// host v0.5.0's §9 obligation 1 is "pin host >= v0.5.0 and harness >= v0.36.0
// TOGETHER". The pairing is not stylistic: harness v0.36.0 widens the kind
// vocabulary, and a host below v0.5.0 answers the two new kinds by refusing them
// from inside ApplyCommand — after Host has durably begun the dispatch attempt. No
// disposition frame is written, the store cannot settle the record, the deadline
// sweep skips an attempt-bearing row, and the consumer never advances past it.
func TestHostAndHarnessPinsMoveTogether(t *testing.T) {
	t.Parallel()

	gomod := readGoMod(t)
	host := requiredVersion(gomod, "github.com/looprig/host")
	harnessVersion := requiredVersion(gomod, "github.com/looprig/harness")

	if host == "" || harnessVersion == "" {
		t.Fatalf("go.mod must require both host and harness; got host=%q harness=%q", host, harnessVersion)
	}
	if !versionAtLeast(host, "v0.5.0") && versionAtLeast(harnessVersion, "v0.36.0") {
		t.Errorf("host %s with harness %s: a Host below v0.5.0 strands every create and restore harness now admits", host, harnessVersion)
	}
	if versionAtLeast(host, "v0.5.0") && !versionAtLeast(harnessVersion, "v0.36.0") {
		t.Errorf("host %s with harness %s: host v0.5.0 requires the five-kind vocabulary harness v0.36.0 introduced", host, harnessVersion)
	}
	// host v0.8.0's obligation: "Pair host v0.8.0 with harness v0.38.0; the
	// harness adapter requires both of its new capabilities to bind."
	if versionAtLeast(host, "v0.8.0") && !versionAtLeast(harnessVersion, "v0.38.0") {
		t.Errorf("host %s with harness %s: host v0.8.0 requires harness v0.38.0's persistence-fault capabilities", host, harnessVersion)
	}
	// host v0.9.0/v0.10.x pair with harness v0.39.x (gate resume across failover).
	if versionAtLeast(host, "v0.9.0") && !versionAtLeast(harnessVersion, "v0.39.0") {
		t.Errorf("host %s with harness %s: host v0.9.0 and later pair with harness v0.39.0", host, harnessVersion)
	}
	// The principal/metadata seam and its one-way journal reader floor move together.
	if versionAtLeast(host, "v0.11.0") && !versionAtLeast(harnessVersion, "v0.41.0") {
		t.Errorf("host %s with harness %s: host v0.11.0 requires harness v0.41.0's attributed command and journal records", host, harnessVersion)
	}
	if versionAtLeast(harnessVersion, "v0.41.0") && !versionAtLeast(host, "v0.11.0") {
		t.Errorf("host %s with harness %s: harness v0.41.0 requires host v0.11.0 to carry principal and metadata into the runtime", host, harnessVersion)
	}
}

// versionAtLeast compares two vMAJOR.MINOR.PATCH versions numerically. A
// lexical comparison would order v0.10.0 below v0.9.0.
func versionAtLeast(version, floor string) bool {
	parse := func(v string) [3]int {
		var out [3]int
		for i, part := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
			if cut := strings.IndexAny(part, "-+"); cut >= 0 {
				part = part[:cut]
			}
			n, _ := strconv.Atoi(part)
			out[i] = n
		}
		return out
	}
	a, b := parse(version), parse(floor)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// TestCarbonNamesNoReplaceDirective holds the workspace rule at the one place a
// release is audited from.
//
// A `replace` makes `GOWORK=off go test ./...` — the only mode that verifies this
// module against its real pinned dependencies — consult something other than the
// pinned version, which is exactly the check it exists to be. An unresolved symbol
// under GOWORK=off means a dependency release is still owed; it is a finding, not
// something to work around here.
func TestCarbonNamesNoReplaceDirective(t *testing.T) {
	t.Parallel()

	for _, line := range strings.Split(readGoMod(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") || trimmed == "replace (" {
			t.Errorf("go.mod carries a replace directive (%q); a published module file must not", trimmed)
		}
	}
}

// orchestrationRecordNames are record types Core and SessionStore own. Carbon is
// the reference COMPOSITION and is not a place to reimplement any of them: a local
// type by one of these names would be a second definition of a wire or durable
// record, and the two would diverge silently because nothing compares them.
//
// The list is of NAMES rather than of modules, because a copy is exactly what a
// build cannot see — a copied record compiles perfectly and is wrong only against
// bytes some other process wrote.
var orchestrationRecordNames = map[string]string{
	"CommandEnvelope":             "core/sessionwire/v1",
	"EnduringPublication":         "core/sessionwire/v1",
	"HostLinkCapacityReport":      "core/sessionwire/v1",
	"HostLinkRegistryObservation": "core/sessionwire/v1",
	"VersionNegotiationRequest":   "core/sessionwire/v1",
	"VersionNegotiationResponse":  "core/sessionwire/v1",
	"CatalogRecord":               "sessionstore",
	"ResidencyGrant":              "sessionstore",
	"SessionBinding":              "sessionstore",
	"DispositionEvidenceReader":   "sessionstore",
}

// TestCarbonCopiesNoOrchestrationRecords proves R1.1's second boundary: Carbon
// names sessionwire, SessionStore, placement and realtime types by IMPORT and
// declares none of them itself.
func TestCarbonCopiesNoOrchestrationRecords(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "bin" || entry.Name() == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if owner, copied := orchestrationRecordNames[typeSpec.Name.Name]; copied {
					t.Errorf("%s declares type %s, which %s owns; Carbon composes these records and must not redeclare one",
						path, typeSpec.Name.Name, owner)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// TestCarbonDeclaresExactlyOneProductIdentity holds the rule Carbon's own CLAUDE.md
// states — "the only Carbon agent identity and prompt are Carbon's" — at the place
// R1.2 turns into a Department registration.
//
// It matters more once there is a Department than it did before: department.New
// takes a SLICE of registrations and refuses a duplicate agent, so the registry
// cannot express "an open-ended product agent registry" by accident. What it CAN
// express is a second identity added here and registered there, which is the thing
// runbook 08 forbids in terms.
func TestCarbonDeclaresExactlyOneProductIdentity(t *testing.T) {
	t.Parallel()

	catalogRoot := filepath.Join("..", "catalog")
	var names []string
	err := filepath.WalkDir(catalogRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, match := range regexp.MustCompile(`identity\.AgentName\("([^"]*)"\)`).FindAllStringSubmatch(string(source), -1) {
			names = append(names, match[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan catalog: %v", err)
	}
	if len(names) != 1 || names[0] != string(carbon.Name) {
		t.Errorf("catalog declares agent identities %v, want exactly [%q]", names, carbon.Name)
	}
}
