package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/looprig/core/content"
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
			return &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}, newModelFactoryFor(testModel()), nil
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
	bindings := make(map[sessionwire.TenantID]sessionstore.SessionBinding)
	for _, tenant := range []sessionwire.TenantID{"tenant-a", "tenant-b"} {
		id, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		binding := sessionstore.SessionBinding{StorageBindingID: "carbon-test", BindingVersion: "v1", RuntimeSessionID: id.String(), ProtocolMode: sessionstore.ProtocolModeDisposition}
		bindings[tenant] = binding
		_, _, err = other.CreateCatalogEntry(ctx, sessionstore.CreateCatalogEntryRequest{
			TenantID: tenant, SessionID: "same-session-name", AgentID: CarbonAgentID,
			RuntimeCompatibilityID: string(compatibility), CreatedAt: now, LastActiveAt: now,
			State: sessionwire.SessionStateRunning, Residency: sessionwire.SessionResidencyCold,
			DesiredPlacement: sessionwire.HostPlacementPooled, IdempotencyKey: "create-" + string(tenant),
			Binding: binding,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", tenant, err)
		}
		journal, err := launcher.JournalStoreForTenant(tenant)
		if err != nil {
			t.Fatalf("journal for %s: %v", tenant, err)
		}
		journalStores[host.EvidenceKey{TenantID: tenant, StorageBindingID: "carbon-test"}] = journal
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
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	for _, resident := range residences {
		tenant := resident.TenantID
		commandID := sessionwire.CommandID("create-" + string(tenant))
		request := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: commandID},
			SessionID: resident.SessionID, AgentID: CarbonAgentID, Blocks: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		runtimeCommand, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if _, _, err := other.AdmitDispositionCommand(ctx, sessionstore.AdmitDispositionCommandRequest{
			TenantID: tenant, SessionID: resident.SessionID, CommandID: commandID, Binding: bindings[tenant],
			ProposedRuntimeCommandID: sessionstore.RuntimeCommandID(runtimeCommand.String()), Kind: "create", Payload: payload,
			AcceptedAt: now, ApplyDeadline: now.Add(time.Minute),
		}); err != nil {
			t.Fatalf("admit %s: %v", tenant, err)
		}
		link := dialPooledHostLink(t, server.URL, tenant)
		bind := sessionwire.HostLinkBindRequest{Version: sessionwire.CurrentWireVersion, TenantID: tenant, SessionID: resident.SessionID,
			HostID: "pooled-carbon", HostGeneration: 1, LeaseEpoch: resident.LeaseEpoch,
			RuntimeCompatibilityID: string(compatibility), IdempotencyKey: "bind-" + string(tenant)}
		pooledHostRPC(t, link, 2, sessionwire.HostLinkMethodBind, bind)
		pooledHostRPC(t, link, 3, sessionwire.HostLinkChannel(tenant, resident.SessionID), sessionwire.HostLinkCommandDelivery{CommandID: commandID})
		_ = link.Close()
		var entry sessionstore.DispositionInboxEntry
		deadline := time.Now().Add(15 * time.Second)
		for {
			entry, err = other.GetDispositionCommand(ctx, sessionstore.GetDispositionCommandRequest{TenantID: tenant, SessionID: resident.SessionID, CommandID: commandID})
			if err != nil {
				t.Fatal(err)
			}
			if entry.Record.State == sessionstore.InboxStateApplied || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if entry.Record.State != sessionstore.InboxStateApplied {
			t.Fatalf("%s command state %s, want applied", tenant, entry.Record.State)
		}
		if entry.Record.Attempt == nil {
			t.Fatalf("%s applied without attempt", tenant)
		}
		evidence, err := journalStores[host.EvidenceKey{TenantID: tenant, StorageBindingID: "carbon-test"}].ReadDispositionEvidence(ctx, sessionstore.DispositionEvidenceRequest{
			TenantID: tenant, SessionID: resident.SessionID, CommandID: commandID, Kind: "create",
			RuntimeCommandID: entry.Record.Descriptor.RuntimeCommandID, Binding: bindings[tenant], Attempt: *entry.Record.Attempt,
		})
		if err != nil || evidence.Kind != sessionstore.DispositionApplied {
			t.Fatalf("%s journal evidence %+v, %v", tenant, evidence, err)
		}
	}
}

func dialPooledHostLink(t *testing.T, serverURL string, tenant sessionwire.TenantID) *websocket.Conn {
	t.Helper()
	endpoint, err := sessionwire.HostLinkEndpoint(sessionwire.InternalEndpoint("ws"+strings.TrimPrefix(serverURL, "http")), tenant)
	if err != nil {
		t.Fatal(err)
	}
	link, _, err := websocket.DefaultDialer.Dial(string(endpoint), http.Header{"Sec-WebSocket-Protocol": {"centrifuge-json"}})
	if err != nil {
		t.Fatal(err)
	}
	negotiation, _ := json.Marshal(sessionwire.VersionNegotiationRequest{SupportedVersions: []sessionwire.WireVersion{1}})
	pooledHostFrame(t, link, 1, map[string]any{"connect": map[string]any{"token": "test", "data": json.RawMessage(negotiation)}})
	return link
}

func pooledHostRPC(t *testing.T, link *websocket.Conn, id uint32, method string, body any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	reply := pooledHostFrame(t, link, id, map[string]any{"rpc": map[string]any{"method": method, "data": json.RawMessage(data)}})
	if err := pooledHostRPCAccepted(reply); err != nil {
		t.Fatalf("HostLink %s refused: %v", method, err)
	}
}

type pooledHostRPCData struct {
	Data json.RawMessage `json:"data"`
}
type pooledHostReply struct {
	ID    uint32             `json:"id"`
	RPC   *pooledHostRPCData `json:"rpc"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func pooledHostRPCAccepted(reply pooledHostReply) error {
	if reply.Error != nil {
		return fmt.Errorf("transport error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	if reply.RPC == nil {
		return errors.New("missing RPC reply")
	}
	data := strings.TrimSpace(string(reply.RPC.Data))
	if data == "" || data == "null" {
		return nil
	}
	var refusal sessionwire.HostLinkError
	if err := json.Unmarshal(reply.RPC.Data, &refusal); err == nil {
		return fmt.Errorf("Core refusal %s", refusal.Code)
	}
	return fmt.Errorf("unexpected RPC body %s", data)
}

func pooledHostFrame(t *testing.T, link *websocket.Conn, id uint32, command map[string]any) pooledHostReply {
	t.Helper()
	command["id"] = id
	if err := link.WriteJSON(command); err != nil {
		t.Fatal(err)
	}
	if err := link.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, frame, err := link.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var reply pooledHostReply
		if err := json.Unmarshal(frame, &reply); err != nil {
			t.Fatalf("frame %s: %v", frame, err)
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			t.Fatalf("HostLink RPC %d refused: %+v", id, reply.Error)
		}
		return reply
	}
}

func TestPooledHostRPCAcceptanceRejectsBareCoreRefusal(t *testing.T) {
	refusal, err := json.Marshal(sessionwire.HostLinkError{Code: sessionwire.HostLinkErrorEpochMismatch, CurrentLeaseEpoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := pooledHostRPCAccepted(pooledHostReply{RPC: &pooledHostRPCData{Data: refusal}}); err == nil {
		t.Fatalf("bare Core refusal %s was accepted", refusal)
	}
	if err := pooledHostRPCAccepted(pooledHostReply{RPC: &pooledHostRPCData{Data: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("unknown nonempty RPC body was accepted")
	}
	if err := pooledHostRPCAccepted(pooledHostReply{RPC: &pooledHostRPCData{}}); err != nil {
		t.Fatalf("empty RPC acceptance rejected: %v", err)
	}
}
