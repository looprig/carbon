package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/sessionstore"
	"github.com/looprig/storage/memstore"
)

// TestRealPooledHostAbandonsAFaultedCarbonRuntime is host v0.8.0/v0.8.1's
// product obligation, proven end to end through a REAL composed Host, Carbon's
// real department adapter and a real harness session: when the runtime's
// persistence fault latches, Host gives the session up — it calls
// AbandonResidency on the runtime (which reaches harness and hands back the
// journal lease) and releases the residency — so a successor attach RESTORES the
// session under a strictly later residency instead of finding it wedged.
//
// Only the fault SIGNAL is injected (a real storage outage is the tests lane's
// job); the abandon, the lease hand-back and the restore are all real. Without
// Carbon forwarding department.PersistenceFaults, Host never sees the signal,
// logs nothing, and the session stays resident: the abandon count stays zero and
// every re-attach answers the same residency.
func TestRealPooledHostAbandonsAFaultedCarbonRuntime(t *testing.T) {
	ctx := context.Background()
	const tenant sessionwire.TenantID = "tenant-a"
	const publicID sessionwire.SessionID = "faulted-session"
	const compatibility department.CompatibilityID = "carbon-pooled-fault-test"

	pooled, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pooled.Close(ctx) })

	var (
		mu       sync.Mutex
		launched []*faultInjectingController
	)
	injecting := pooledLauncherFunc(func(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
		controller, err := pooled.Launch(ctx, scope)
		if err != nil {
			return nil, err
		}
		wrapped := newFaultInjectingController(controller)
		mu.Lock()
		launched = append(launched, wrapped)
		mu.Unlock()
		return wrapped, nil
	})
	dept, err := NewCarbonDepartment(injecting, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	target, err := dept.Target(CarbonAgentID)
	if err != nil {
		t.Fatal(err)
	}

	backend := memstore.New()
	control, err := sessionstore.Open(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(ctx) })
	runtimeID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, err := control.CreateCatalogEntry(ctx, sessionstore.CreateCatalogEntryRequest{
		TenantID: tenant, SessionID: publicID, AgentID: CarbonAgentID,
		RuntimeCompatibilityID: string(compatibility), CreatedAt: now, LastActiveAt: now,
		State: sessionwire.SessionStateRunning, Residency: sessionwire.SessionResidencyCold,
		DesiredPlacement: sessionwire.HostPlacementPooled, IdempotencyKey: "create-faulted",
		Binding: sessionstore.SessionBinding{StorageBindingID: "carbon-test", BindingVersion: "v1",
			RuntimeSessionID: runtimeID.String(), ProtocolMode: sessionstore.ProtocolModeDisposition},
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := pooled.JournalStoreForTenant(tenant)
	if err != nil {
		t.Fatal(err)
	}
	service, err := host.Compose(ctx, host.Composition{
		Options: host.Options{HostID: "pooled-carbon-faults", InternalEndpoint: "ws://127.0.0.1:7101", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second, CommandQueueSize: 16,
			ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
		Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
		Collaborators: host.Collaborators{Backend: backend,
			JournalStores: map[host.EvidenceKey]sessionstore.DispositionEvidenceReader{{TenantID: tenant, StorageBindingID: "carbon-test"}: journal},
			Registrar: host.RegistrarFunc(func(context.Context) ([]department.Registration, error) {
				return []department.Registration{{AgentID: CarbonAgentID, Target: target}}, nil
			}),
			Checkpointer: pooledHostCheckpointer{}, Auth: pooledHostAuth{}, Workspaces: pooled,
			NamespaceLayout: func(tenant sessionwire.TenantID, sid sessionwire.SessionID) string {
				return string(tenant) + "/" + string(sid)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.Stop(ctx) })
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}

	attach := func(mode sessionwire.HostLinkAttachMode) (host.Residency, error) {
		return service.Attach(ctx, host.AttachRequest{TenantID: tenant, SessionID: publicID, AgentID: CarbonAgentID,
			Mode: mode, RuntimeCompatibilityID: string(compatibility), ActorID: "test-factory"})
	}
	first, err := attach(sessionwire.HostLinkAttachModeCreate)
	if err != nil {
		t.Fatalf("attach create: %v", err)
	}
	if !first.Attached || first.LeaseEpoch == 0 {
		t.Fatalf("not resident: %+v", first)
	}
	mu.Lock()
	if len(launched) != 1 {
		mu.Unlock()
		t.Fatalf("launched %d runtimes, want 1", len(launched))
	}
	faulted := launched[0]
	mu.Unlock()

	faulted.inject()

	deadline := time.Now().Add(30 * time.Second)
	for faulted.abandons.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Host never abandoned the faulted Carbon runtime: department.PersistenceFaults did not reach Host's fault supervisor")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, held := faulted.LeaseEpoch(); held {
		t.Fatal("the faulted runtime still holds its journal lease after Host abandoned it")
	}

	// A successor attach RESTORES the session under a strictly later residency.
	// Until the give-up finishes an attach may be refused (not_admitting) or
	// answered with the dying residency, so it is retried until it moves.
	var restored host.Residency
	for {
		restored, err = attach(sessionwire.HostLinkAttachModeRestore)
		if err == nil && restored.Attached && restored.LeaseEpoch > first.LeaseEpoch {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successor residency after the give-up: last %+v, %v", restored, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launched) != 2 {
		t.Fatalf("launched %d runtimes, want the original and one restore", len(launched))
	}
	if faulted.abandons.Load() != 1 {
		t.Fatalf("the faulted runtime was abandoned %d times, want once", faulted.abandons.Load())
	}
	if _, held := launched[1].LeaseEpoch(); !held {
		t.Fatal("the restored runtime holds no journal lease")
	}
}

// pooledLauncherFunc is a launcherFunc that declares pooled support, as the
// PooledLauncher it wraps does.
type pooledLauncherFunc func(context.Context, LaunchScope) (session.SessionController, error)

func (f pooledLauncherFunc) Launch(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
	return f(ctx, scope)
}
func (pooledLauncherFunc) SupportsPooled() bool { return true }
