package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/sessionstore"
)

func TestServePooledHostStopBeforeStartClosesPreboundListener(t *testing.T) {
	ctx := context.Background()
	storage, err := OpenServeStorage(ctx, Config{HomeDir: t.TempDir()}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close(ctx) })
	service, err := OpenServePooledHost(ctx, storage, ServePooledHostConfig{
		ListenAddress: "127.0.0.1:0", AuthToken: "test", StorageBindingID: "carbon-local-v1",
		Options: host.Options{HostID: "carbon-local", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 1, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
			CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 1, MaxBindings: 1, MaxTenantLinks: 1},
		Drain:                host.DrainOptions{Grace: time.Second, IdleBoundary: time.Second, PublishBound: time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	address := service.listener.Addr().String()
	_, _ = service.Stop(ctx)
	rebound, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("prebound listener remains open after Stop: %v", err)
	}
	_ = rebound.Close()
}

func TestServePooledHostCancelledStopClosesActiveSocket(t *testing.T) {
	ctx := context.Background()
	storage, err := OpenServeStorage(ctx, Config{HomeDir: t.TempDir()}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close(ctx) })
	service, err := OpenServePooledHost(ctx, storage, ServePooledHostConfig{
		ListenAddress: "127.0.0.1:0", AuthToken: "test", StorageBindingID: "carbon-local-v1",
		Options: host.Options{HostID: "carbon-local", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 1, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
			CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 1, MaxBindings: 1, MaxTenantLinks: 1},
		Drain:                host.DrainOptions{Grace: time.Second, IdleBoundary: time.Second, PublishBound: time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", service.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /readyz HTTP/1.1\r\nHost: localhost\r\n")); err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = service.Stop(stopCtx)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := conn.Read(one[:]); err == nil {
		t.Fatal("active socket survived cancelled Stop")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("active socket remained open until read deadline: %v", err)
	}
}

func TestServePooledHostBindsRealBareBaseAndAuthenticates(t *testing.T) {
	ctx := context.Background()
	storage, err := OpenServeStorage(ctx, Config{HomeDir: t.TempDir()}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close(ctx) })
	service, err := OpenServePooledHost(ctx, storage, ServePooledHostConfig{
		ListenAddress: "127.0.0.1:0", AuthToken: "test", StorageBindingID: "carbon-local-v1",
		Options: host.Options{HostID: "carbon-local", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
			CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
		Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		report, err := service.Stop(ctx)
		if err != nil || len(report.Failures) != 0 {
			t.Errorf("Host Stop report=%+v err=%v", report, err)
		}
	})
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionwire.HostLinkEndpoint(service.Endpoint(), "local"); err != nil {
		t.Fatalf("advertised base: %v", err)
	}
	response, err := http.Get("http" + string(service.Endpoint())[2:] + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d", response.StatusCode)
	}
	link := dialPooledHostLink(t, "http"+strings.TrimPrefix(string(service.Endpoint()), "ws"), "local")
	_ = link.Close()
	localEndpoint, err := sessionwire.HostLinkEndpoint(service.Endpoint(), "local")
	if err != nil {
		t.Fatal(err)
	}
	capabilityLink, _, err := websocket.DefaultDialer.Dial(string(localEndpoint), http.Header{"Sec-WebSocket-Protocol": {"centrifuge-json"}})
	if err != nil {
		t.Fatal(err)
	}
	defer capabilityLink.Close()
	offer, err := sessionwire.EncodeHostLinkConnectRequest(sessionwire.VersionNegotiationRequest{SupportedVersions: []sessionwire.WireVersion{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilityLink.WriteJSON(map[string]any{"id": 1, "connect": map[string]any{"token": "test", "data": json.RawMessage(offer)}}); err != nil {
		t.Fatal(err)
	}
	_ = capabilityLink.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, frame, err := capabilityLink.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			ID      uint32 `json:"id"`
			Connect *struct {
				Data json.RawMessage `json:"data"`
			} `json:"connect"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.ID != 1 {
			continue
		}
		if envelope.Connect == nil {
			t.Fatalf("no connect reply: %s", frame)
		}
		negotiated, err := sessionwire.DecodeHostLinkConnectReply(envelope.Connect.Data)
		if err != nil {
			t.Fatal(err)
		}
		if !negotiated.Supports(sessionwire.HostLinkMethodAttach) || !negotiated.Supports(sessionwire.HostLinkCapabilityGateResponse) {
			t.Fatalf("Host did not advertise attach and gate response: %+v", negotiated.HostLinkMethods())
		}
		break
	}
	badToken, _, err := websocket.DefaultDialer.Dial(string(localEndpoint), http.Header{"Sec-WebSocket-Protocol": {"centrifuge-json"}})
	if err != nil {
		t.Fatal(err)
	}
	defer badToken.Close()
	if err := badToken.WriteJSON(map[string]any{"id": 1, "connect": map[string]any{"token": "wrong", "data": json.RawMessage(offer)}}); err != nil {
		t.Fatal(err)
	}
	_ = badToken.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, deniedFrame, deniedErr := badToken.ReadMessage()
	if deniedErr == nil {
		var denied struct {
			Connect *json.RawMessage `json:"connect"`
		}
		if json.Unmarshal(deniedFrame, &denied) == nil && denied.Connect != nil {
			t.Fatalf("wrong token accepted for default tenant: %s", deniedFrame)
		}
	}
	endpoint, err := sessionwire.HostLinkEndpoint(service.Endpoint(), "other")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := websocket.DefaultDialer.Dial(string(endpoint), http.Header{"Sec-WebSocket-Protocol": {"centrifuge-json"}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	negotiation, _ := json.Marshal(sessionwire.VersionNegotiationRequest{SupportedVersions: []sessionwire.WireVersion{1}})
	if err := other.WriteJSON(map[string]any{"id": 1, "connect": map[string]any{"token": "test", "data": json.RawMessage(negotiation)}}); err != nil {
		t.Fatal(err)
	}
	_ = other.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, frame, err := other.ReadMessage()
	if err == nil {
		var reply map[string]json.RawMessage
		if json.Unmarshal(frame, &reply) == nil && reply["connect"] != nil {
			t.Fatalf("other tenant accepted: %s", frame)
		}
	}
}

func TestServePooledHostAppliesFirstCommandWithJournalEvidence(t *testing.T) {
	ctx := context.Background()
	var created []*fakeLLM
	storage, err := OpenServeStorage(ctx, Config{HomeDir: t.TempDir()}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"},
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			client := &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}
			created = append(created, client)
			return client, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close(ctx) })
	service, err := OpenServePooledHost(ctx, storage, ServePooledHostConfig{
		ListenAddress: "127.0.0.1:0", AuthToken: "test", StorageBindingID: "carbon-local-v1",
		Options: host.Options{HostID: "carbon-local", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
			Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
			RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
			CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
		Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
		Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
		CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		report, err := service.Stop(ctx)
		if err != nil || len(report.Failures) != 0 {
			t.Errorf("Host Stop report=%+v err=%v", report, err)
		}
	})
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	runtimeID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionstore.SessionBinding{StorageBindingID: "carbon-local-v1", BindingVersion: "v1", RuntimeSessionID: runtimeID.String(), ProtocolMode: sessionstore.ProtocolModeDisposition}
	now := time.Now().UTC()
	_, _, err = storage.ControlStore().CreateCatalogEntry(ctx, sessionstore.CreateCatalogEntryRequest{
		TenantID: "local", SessionID: "session-1", AgentID: CarbonAgentID, RuntimeCompatibilityID: string(service.CompatibilityID()),
		CreatedAt: now, LastActiveAt: now, State: sessionwire.SessionStateRunning, Residency: sessionwire.SessionResidencyCold,
		DesiredPlacement: sessionwire.HostPlacementPooled, IdempotencyKey: "create-local", Binding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	link := dialPooledHostLink(t, "http"+strings.TrimPrefix(string(service.Endpoint()), "ws"), "local")
	defer link.Close()
	attach := sessionwire.HostLinkAttachRequest{Version: sessionwire.CurrentWireVersion, TenantID: "local", SessionID: "session-1",
		HostID: "carbon-local", HostGeneration: 1, AgentID: CarbonAgentID, RuntimeCompatibilityID: string(service.CompatibilityID()),
		Mode: sessionwire.HostLinkAttachModeCreate, ActorID: "test-factory", IdempotencyKey: "attach-1"}
	attachData, err := json.Marshal(attach)
	if err != nil {
		t.Fatal(err)
	}
	attachReply := pooledHostFrame(t, link, 2, map[string]any{"rpc": map[string]any{"method": sessionwire.HostLinkMethodAttach, "data": json.RawMessage(attachData)}})
	if attachReply.RPC == nil {
		t.Fatalf("attach refused: %+v", attachReply)
	}
	var observed sessionwire.HostLinkRegistryObservation
	if err := json.Unmarshal(attachReply.RPC.Data, &observed); err != nil {
		t.Fatalf("attach reply: %s: %v", attachReply.RPC.Data, err)
	}
	if !observed.Accepting || observed.LeaseEpoch == 0 || observed.HostID != "carbon-local" || observed.HostGeneration != 1 {
		t.Fatalf("attach was not accepted: %+v", observed)
	}
	request := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "create-1"},
		SessionID: "session-1", AgentID: CarbonAgentID, Blocks: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCommand, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = storage.ControlStore().AdmitDispositionCommand(ctx, sessionstore.AdmitDispositionCommandRequest{
		TenantID: "local", SessionID: "session-1", CommandID: "create-1", Binding: binding,
		ProposedRuntimeCommandID: sessionstore.RuntimeCommandID(runtimeCommand.String()), Kind: "create", Payload: payload,
		AcceptedAt: time.Now().UTC(), ApplyDeadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	pooledHostRPC(t, link, 3, sessionwire.HostLinkMethodBind, sessionwire.HostLinkBindRequest{
		Version: sessionwire.CurrentWireVersion, TenantID: "local", SessionID: "session-1", HostID: "carbon-local", HostGeneration: 1,
		LeaseEpoch: observed.LeaseEpoch, RuntimeCompatibilityID: string(service.CompatibilityID()), IdempotencyKey: "bind-1",
	})
	pooledHostRPC(t, link, 4, sessionwire.HostLinkChannel("local", "session-1"), sessionwire.HostLinkCommandDelivery{CommandID: "create-1"})
	var entry sessionstore.DispositionInboxEntry
	deadline := time.Now().Add(15 * time.Second)
	for {
		entry, err = storage.ControlStore().GetDispositionCommand(ctx, sessionstore.GetDispositionCommandRequest{TenantID: "local", SessionID: "session-1", CommandID: "create-1"})
		if err != nil {
			t.Fatal(err)
		}
		if entry.Record.State == sessionstore.InboxStateApplied || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entry.Record.State != sessionstore.InboxStateApplied || entry.Record.Attempt == nil {
		t.Fatalf("command did not settle: %+v", entry.Record)
	}
	evidence, err := storage.DefaultJournalStore().ReadDispositionEvidence(ctx, sessionstore.DispositionEvidenceRequest{
		TenantID: "local", SessionID: "session-1", CommandID: "create-1", Kind: "create",
		RuntimeCommandID: entry.Record.Descriptor.RuntimeCommandID, Binding: binding, Attempt: *entry.Record.Attempt,
	})
	if err != nil || evidence.Kind != sessionstore.DispositionApplied {
		t.Fatalf("journal evidence: %+v %v", evidence, err)
	}
	seenInput := false
	var modelBodies []string
	modelDeadline := time.Now().Add(5 * time.Second)
	for !seenInput && time.Now().Before(modelDeadline) {
		modelBodies = nil
		for _, client := range created {
			streams, _ := client.capturedRequests()
			for _, request := range streams {
				body, _ := json.Marshal(request.Messages)
				modelBodies = append(modelBodies, string(body))
				if strings.Contains(string(body), "hello") {
					seenInput = true
				}
			}
		}
		if !seenInput {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !seenInput {
		t.Fatalf("first create input never reached the model: %v", modelBodies)
	}
}
