package app

import (
	"context"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/sessionstore"
	"github.com/looprig/storage/memstore"
)

type pooledHostAuth struct{}

func (pooledHostAuth) VerifyTenant(context.Context, sessionwire.TenantID, string) error { return nil }

type pooledHostCheckpointer struct{}

func (pooledHostCheckpointer) Checkpoint(context.Context, sessionwire.TenantID, sessionwire.SessionID) error {
	return nil
}

func TestRealPooledHostHoldsTwoCarbonTenantsConcurrently(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	const compatibility department.CompatibilityID = "carbon-pooled-test"
	dept, err := NewCarbonDepartment(launcher, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	target, err := dept.Target(CarbonAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if !target.Capabilities().SupportsPooled {
		t.Fatal("Carbon target did not advertise pooled")
	}
	backend := memstore.New()
	other, err := sessionstore.Open(ctx, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(ctx) })
	journalStores := make(map[host.EvidenceKey]sessionstore.DispositionEvidenceReader)
	for _, tenant := range []sessionwire.TenantID{"tenant-a", "tenant-b"} {
		id, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		_, _, err = other.CreateCatalogEntry(ctx, sessionstore.CreateCatalogEntryRequest{
			TenantID: tenant, SessionID: "same-session-name", AgentID: CarbonAgentID,
			RuntimeCompatibilityID: string(compatibility), CreatedAt: now, LastActiveAt: now,
			State: sessionwire.SessionStateRunning, Residency: sessionwire.SessionResidencyCold,
			DesiredPlacement: sessionwire.HostPlacementPooled, IdempotencyKey: "create-" + string(tenant),
			Binding: sessionstore.SessionBinding{StorageBindingID: "carbon-test", BindingVersion: "v1", RuntimeSessionID: id.String(), ProtocolMode: sessionstore.ProtocolModeDisposition},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", tenant, err)
		}
		journalStores[host.EvidenceKey{TenantID: tenant, StorageBindingID: "carbon-test"}] = launcher.stores.session
	}
	blueprint := host.Composition{
		Options: host.Options{HostID: "pooled-carbon", InternalEndpoint: "ws://127.0.0.1:7100", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second, CommandQueueSize: 16,
			ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
		Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
		Collaborators: host.Collaborators{Backend: backend, JournalStores: journalStores,
			Registrar: host.RegistrarFunc(func(context.Context) ([]department.Registration, error) {
				return []department.Registration{{AgentID: CarbonAgentID, Target: target}}, nil
			}),
			Checkpointer: pooledHostCheckpointer{}, Auth: pooledHostAuth{}, Workspaces: launcher,
			NamespaceLayout: func(tenant sessionwire.TenantID, sid sessionwire.SessionID) string {
				return string(tenant) + "/" + string(sid)
			},
		},
	}
	service, err := host.Compose(ctx, blueprint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.Stop(ctx) })
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var residences []host.Residency
	for _, tenant := range []sessionwire.TenantID{"tenant-a", "tenant-b"} {
		resident, err := service.Attach(ctx, host.AttachRequest{TenantID: tenant, SessionID: "same-session-name", AgentID: CarbonAgentID,
			Mode: sessionwire.HostLinkAttachModeCreate, RuntimeCompatibilityID: string(compatibility), ActorID: "test-factory"})
		if err != nil {
			t.Fatalf("attach %s: %v", tenant, err)
		}
		residences = append(residences, resident)
	}
	for _, resident := range residences {
		if !resident.Attached || resident.LeaseEpoch == 0 {
			t.Fatalf("not resident: %+v", resident)
		}
	}
	if residences[0].TenantID == residences[1].TenantID {
		t.Fatal("two residences name one tenant")
	}
}
