package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/sessionstore"
	"github.com/looprig/tools/bash"
)

// Carbon's Bash runs through an injected sandbox runner, whose CommandRunner
// returns the whole output as one []byte. Its truthful capture-safety
// declaration is therefore materialized + high output (tools v0.14.0, M1) --
// and it must be the one tools itself derives for a runner configuration, not
// a hand-copied constant that could drift from it.
func TestCarbonBashDeclaresTheRunnerCaptureSafetyToolsDerives(t *testing.T) {
	definition := bashDefinition(nil, nil)
	declarer, ok := definition.(tool.CaptureSafetyDeclarer)
	if !ok {
		t.Fatalf("Carbon's Bash definition (%T) declares no capture safety: an undeclared definition projects as low output", definition)
	}
	probe, err := bash.NewSupervisedFactory(bash.WithRunner(grantedExecutor{}), bash.WithFamilyCatalog(productFamilyEligibility()))
	if err != nil {
		t.Fatalf("NewSupervisedFactory: %v", err)
	}
	want := probe.DeclaredCaptureSafety()
	if want != (tool.DeclaredCaptureSafety{Streaming: false, HighOutput: true}) {
		t.Fatalf("tools' runner Bash declares %+v; this test's premise (materialized + high output) no longer holds", want)
	}
	if got := declarer.DeclaredCaptureSafety(); got != want {
		t.Fatalf("Carbon's Bash declares %+v, want tools' runner declaration %+v", got, want)
	}
	if definition.Name() != "Bash" || definition.Requirements() != tool.RequiresWorkspace|tool.RequiresProcessServices {
		t.Fatalf("wrapping changed the definition: name %q requirements %v", definition.Name(), definition.Requirements())
	}
}

// The roster is unsafe to place without a finite materialized maximum and safe
// with harness's: department.CaptureSafetyBoundedMaterialized is Carbon's
// declaration precisely because the rig always projects with one.
func TestCarbonRosterIsBoundedOnlyByTheMaterializedMaximum(t *testing.T) {
	definitions := carbonToolDefinitions(nil, http.DefaultClient, nil)
	unbounded := tool.ProjectCaptureSafety(definitions, 0)
	unsafe := unbounded.Unsafe()
	if !contains(unsafe, "Bash") {
		t.Fatalf("with no materialized maximum the unsafe rows are %v, want Bash among them", unsafe)
	}
	bounded := tool.ProjectCaptureSafety(definitions, loop.DefaultMaterializedToolResultBytes)
	if !bounded.Safe() {
		t.Fatalf("under harness's %d-byte maximum the roster is unsafe: %v", loop.DefaultMaterializedToolResultBytes, bounded.Unsafe())
	}
	if got := carbonCapabilities().CaptureSafety; got != department.CaptureSafetyBoundedMaterialized {
		t.Fatalf("Carbon's Department declares %q, want %q", got, department.CaptureSafetyBoundedMaterialized)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// The capture ceiling Factory's D7 check is fed is the one Carbon's loops
// declare: a finite preview (so oversized output is retained at all) and a
// ceiling the loop resolves exactly as the glue does.
func TestServeToolResultLimitsAreTheLoopsOwn(t *testing.T) {
	limits := carbonToolLimits()
	if limits.ResultBytes <= 0 {
		t.Fatalf("ResultBytes = %d: with no finite preview nothing is ever retained", limits.ResultBytes)
	}
	if got := ServeToolResultCaptureBytes(); got != limits.CaptureBytes {
		t.Fatalf("ServeToolResultCaptureBytes() = %d, want the loop's declared %d", got, limits.CaptureBytes)
	}
	policy, err := newConversationContextPolicy(testModel(), nil, nil)
	if err != nil {
		t.Fatalf("newConversationContextPolicy: %v", err)
	}
	if policy.toolLimits != limits {
		t.Fatalf("the loop's tool limits %+v are not carbonToolLimits() %+v", policy.toolLimits, limits)
	}
}

func TestPooledLauncherPreparesAnOwnerOnlySpillBaseOutsideEveryWorkspace(t *testing.T) {
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(context.Background(), Config{HomeDir: t.TempDir()}, data)
	if err != nil {
		t.Fatalf("OpenPooledLauncher: %v", err)
	}
	t.Cleanup(func() { _ = launcher.Close(context.Background()) })
	want := filepath.Join(data, toolResultSpillDirName)
	if launcher.spillBase != want {
		t.Fatalf("spill base = %q, want %q", launcher.spillBase, want)
	}
	info, err := os.Lstat(want)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("spill base = (%v, %v), want an owner-only directory", info, err)
	}
	root, err := sessionWorkspaceRoot(data, "tenant-a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(root, want+string(filepath.Separator)) || strings.HasPrefix(want, root+string(filepath.Separator)) {
		t.Fatalf("spill base %q overlaps workspace root %q", want, root)
	}
	// Reopening over an existing base is not a refusal.
	again, err := OpenPooledLauncher(context.Background(), Config{HomeDir: t.TempDir()}, data)
	if err != nil {
		t.Fatalf("reopen over an existing spill base: %v", err)
	}
	_ = again.Close(context.Background())
}

// A spill base Carbon cannot vouch for is refused at open -- not repaired, and
// not left for the first oversized tool result to discover.
func TestPooledLauncherRefusesAnUnsafeSpillBase(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, path string){
		"group writable": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o770); err != nil {
				t.Fatal(err)
			}
		},
		"world writable": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o702); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink": func(t *testing.T, path string) {
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
		},
		"a regular file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, toolResultSpillDirName)
			plant(t, path)
			launcher, err := OpenPooledLauncher(context.Background(), Config{HomeDir: t.TempDir()}, data)
			if launcher != nil {
				_ = launcher.Close(context.Background())
			}
			var initErr *StoreInitError
			if !errors.As(err, &initErr) || initErr.Stage != "tool-result-spill" {
				t.Fatalf("OpenPooledLauncher = %v, want a tool-result-spill StoreInitError", err)
			}
		})
	}
}

// Every tenant's rig retains into that tenant's OWN harness journal store --
// the one Launch journals through -- and registers read_tool_result exactly
// where retention is wired.
func TestPooledTenantStoresWireRetentionOverTheirOwnJournalStore(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	bundle, err := launcher.tenantStores("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	retention := bundle.stores.toolResults
	if retention == nil || retention.spillBase != launcher.spillBase || retention.objects != bundle.stores.session.ToolResultObjects() {
		t.Fatalf("tenant retention = %+v, want the tenant journal store's objects over the launcher's spill base", retention)
	}
	if len(retention.rigOptions()) != 1 {
		t.Fatalf("retention contributes %d rig options, want exactly WithToolResultObjects", len(retention.rigOptions()))
	}
	defs := retention.toolDefinitions()
	if len(defs) != 1 || defs[0].Name() != loop.ReadToolResultToolName {
		t.Fatalf("retention registers %v, want exactly %s", defs, loop.ReadToolResultToolName)
	}
	var none *toolResultRetention
	if none.rigOptions() != nil || none.toolDefinitions() != nil {
		t.Fatal("no retention must register neither the capture option nor read_tool_result")
	}
}

// read_tool_result reaches the model of a pooled Carbon session, and nothing
// else in the roster changed.
func TestPooledLaunchOffersReadToolResult(t *testing.T) {
	ctx := context.Background()
	client := &fakeLLM{chunks: []content.Chunk{&content.TextChunk{Text: "ok"}}}
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return client, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id := mustNewUUID(t)
	sess, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "s1", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: id})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = sess.Shutdown(ctx) })
	if _, err := sess.Submit(ctx, []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	req := waitFirstStreamRequest(t, client)
	names := map[string]bool{}
	for _, info := range req.Tools {
		names[info.Name] = true
	}
	if !names[loop.ReadToolResultToolName] || !names["Bash"] {
		t.Fatalf("model was offered %v, want Bash and %s", names, loop.ReadToolResultToolName)
	}
}

// The Factory object reader is the served tenant's runtime store and refuses
// every other tenant before touching storage; evidence is the very store the
// rig journals into.
func TestServeRuntimeObjectsAndEvidenceAreServedTenantOnly(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	control := &sessionstore.Store{}
	reader, err := NewServeSessionReader(control, launcher, "local", "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if objects, ok := reader.RuntimeObjects("other"); ok || objects != nil {
		t.Fatalf("RuntimeObjects(other) = (%v, %v), want a refusal", objects, ok)
	}
	if evidence, ok := reader.RuntimeEvidence("other"); ok || evidence != nil {
		t.Fatalf("RuntimeEvidence(other) = (%v, %v), want a refusal", evidence, ok)
	}
	evidence, ok := reader.RuntimeEvidence("local")
	journal, err := launcher.JournalStoreForTenant("local")
	if !ok || err != nil || evidence != journal {
		t.Fatalf("RuntimeEvidence(local) = (%p, %v), want the rig's own journal store %p (%v)", evidence, ok, journal, err)
	}
	objects, ok := reader.RuntimeObjects("local")
	if !ok || objects == nil {
		t.Fatalf("RuntimeObjects(local) = (%v, %v), want the served tenant's reader", objects, ok)
	}
	// A request naming another tenant, or another object kind, is refused by
	// the reader itself rather than answered from the served tenant's store.
	runtime := mustNewUUID(t).String()
	ref := sessionwire.ObjectReference{ObjectID: "v1:tool-result:00000000000000000000000000:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	if _, err := objects.GetObjectMetadata(ctx, sessionstore.GetObjectMetadataRequest{TenantID: "other", SessionID: sessionwire.SessionID(runtime), ExpectedKind: sessionstore.ObjectKindToolResult, Reference: ref}); !errors.As(err, new(*ServeObjectScopeError)) {
		t.Fatalf("foreign-tenant metadata read = %v, want ServeObjectScopeError", err)
	}
	if _, err := objects.GetObjectMetadata(ctx, sessionstore.GetObjectMetadataRequest{TenantID: "local", SessionID: sessionwire.SessionID(runtime), ExpectedKind: sessionstore.ObjectKindCommandPayload, Reference: ref}); !errors.As(err, new(*ServeObjectScopeError)) {
		t.Fatalf("command-payload metadata read = %v, want ServeObjectScopeError", err)
	}
	if _, err := objects.GetObject(ctx, sessionstore.GetObjectRequest{TenantID: "other", SessionID: sessionwire.SessionID(runtime), ExpectedKind: sessionstore.ObjectKindToolResult}); !errors.As(err, new(*ServeObjectScopeError)) {
		t.Fatalf("foreign-tenant object read = %v, want ServeObjectScopeError", err)
	}
	if _, err := objects.GetObject(ctx, sessionstore.GetObjectRequest{TenantID: "local", SessionID: sessionwire.SessionID(runtime), ExpectedKind: sessionstore.ObjectKindWorkspaceCheckpoint}); !errors.As(err, new(*ServeObjectScopeError)) {
		t.Fatalf("checkpoint object read = %v, want ServeObjectScopeError", err)
	}
	// The served tenant's own store answers: an unknown object is absent there.
	_, err = objects.GetObjectMetadata(ctx, sessionstore.GetObjectMetadataRequest{TenantID: "local", SessionID: sessionwire.SessionID(runtime), ExpectedKind: sessionstore.ObjectKindToolResult, Reference: ref})
	var objectErr *sessionstore.ObjectError
	if !errors.As(err, &objectErr) || objectErr.Code != sessionstore.ObjectErrorMetadataUnavailable {
		t.Fatalf("served-tenant metadata read of an unknown object = %v, want metadata_unavailable", err)
	}
}

func mustNewUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// waitFirstStreamRequest polls the fake's recorded requests for the first one.
func waitFirstStreamRequest(t *testing.T, client *fakeLLM) inference.Request {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		if len(client.streamRequests) > 0 {
			req := client.streamRequests[0]
			client.mu.Unlock()
			return req
		}
		client.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the model was never asked")
	return inference.Request{}
}

// harness v0.40.1: the session catalog lists sessions whose store also holds
// tool-result objects. Under v0.40.0 ListSessions decoded the nested
// object-metadata key as a catalog row and failed whole, and on fsstore v0.5.x
// the object could not be written at all.
func TestPooledTenantCatalogListsASessionThatRetainedAnObject(t *testing.T) {
	ctx := context.Background()
	client := &fakeLLM{chunks: []content.Chunk{&content.TextChunk{Text: "ok"}}}
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return client, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id := mustNewUUID(t)
	sess, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "s1", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: id})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = sess.Shutdown(ctx) })
	if _, err := sess.Submit(ctx, []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFirstStreamRequest(t, client)
	bundle, err := launcher.tenantStores("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("retained ", 10000)
	if _, err := bundle.stores.toolResults.objects.PublishToolResultObject(ctx, id, strings.NewReader(body), uint64(len(body)), sha256.Sum256([]byte(body))); err != nil {
		t.Fatalf("publish into the tenant's real fsstore: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		metas, err := bundle.stores.catalog.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions after an object publish: %v", err)
		}
		if len(metas) == 1 && metas[0].SessionID == id {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ListSessions = %+v, want exactly session %s", metas, id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
