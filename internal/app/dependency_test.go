package app

// This compile-only dependency-surface probe covers both the completed rig
// migration and Task 34's context/hustle additions. It builds only when the
// planned core, inference, LLM, harness, and CLI APIs are all present through
// Carbon's retained local replaces. It carries no test functions because its value
// is that the package cannot compile against a partial dependency rollout.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	if !strings.HasPrefix(harnessVersion, "v0.3") || harnessVersion < "v0.36.0" {
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
			"github.com/looprig/core", "v0.11.0",
			"the HostLink connect framing, the capability signal and HostLinkEndpoint's per-tenant derivation all live here; a Factory below it cannot complete a connect",
		},
		{
			"github.com/looprig/storage", "v0.6.1",
			"the contract BlobReaderLifecycle is declared in, which sessionstore.Open consults",
		},
		{
			"github.com/looprig/sessionstore", "v0.12.0",
			"the ROLLOUT RULE: every ReadGates caller must be on >= v0.12.0 BEFORE any Host publishes a gate; older readers refuse these gate pages",
		},
		{
			"github.com/looprig/harness", "v0.36.0",
			"runtimecommand.Kind names five kinds. Below it a create and a restore are refused AFTER the attempt is durable and the session's whole command stream wedges",
		},
		{
			"github.com/looprig/host", "v0.6.0",
			"the release that safely disposes an unstarted Host and prechecks malformed creates before a durable attempt; Carbon's browser lifecycle relies on that unstarted-close contract",
		},
		{
			"github.com/looprig/factory", "v0.7.0",
			"the release that quiesces new public admission and joins preboundary commands while keeping reconciliation and HostLinks live for Carbon's ordered drain",
		},
		{
			"github.com/looprig/wui", "v0.2.0",
			"the first bundle whose release marker a gating consumer accepts; v0.1.0 has no marker and v0.1.1 declares itself non-release",
		},
		{
			"github.com/looprig/tools", "v0.12.0",
			// THIS ROW'S REASON WAS FALSE and is corrected rather than removed. It
			// read "the release carrying read_tool_result, which R1.2 registers
			// whenever a session object reader is bound" — and no released module
			// ships a read_tool_result definition at all (see
			// TestCarbonAdvertisesNoUnregisteredResultReader). The ledger is the
			// artifact a release is audited from, and a row whose stated reason is
			// measurably untrue is worse than an unreasoned one, because it is the
			// reason that gets copied forward. The real reason is the ordinary one.
			"the current release of Carbon's tool roster (ReadFile, WriteFile, EditFile, Bash, the process tools, WebSearch, Fetch, Task, AskUser, Skill); it ships NO read_tool_result, which is why R1.2 steps 6-7 were struck",
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
	if host < "v0.5.0" && harnessVersion >= "v0.36.0" {
		t.Errorf("host %s with harness %s: a Host below v0.5.0 strands every create and restore harness now admits", host, harnessVersion)
	}
	if host >= "v0.5.0" && harnessVersion < "v0.36.0" {
		t.Errorf("host %s with harness %s: host v0.5.0 requires the five-kind vocabulary harness v0.36.0 introduced", host, harnessVersion)
	}
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
