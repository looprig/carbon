//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage"
)

type failingSnapshotBlobs struct {
	storage.Blobs
	mu       sync.Mutex
	fail     bool
	attempts chan struct{}
}

func (b *failingSnapshotBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	select {
	case b.attempts <- struct{}{}:
	default:
	}
	if fail {
		return errors.New("injected workspace snapshot failure")
	}
	return b.Blobs.Put(ctx, key, r)
}

func (b *failingSnapshotBlobs) setFail(fail bool) {
	b.mu.Lock()
	b.fail = fail
	b.mu.Unlock()
}

// concurrentManagedScript is the channel-controlled counterpart to managedScript. It
// intentionally does not serialize Stream callbacks: async child inference must be able to
// block while the parent continues issuing list/message/stop actions.
type concurrentManagedScript struct {
	fn func(context.Context, inference.Request) ([]content.Chunk, error)
}

func (*concurrentManagedScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("concurrentManagedScript.Invoke not used")
}

func (s *concurrentManagedScript) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	chunks, err := s.fn(ctx, req)
	if err != nil {
		return nil, err
	}
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[i]
		i++
		return chunk, nil
	}, nil), nil
}

// TestRigRestoreStateWorkspaceAndContinuation exercises the CLI-shaped persistence path
// with two genuinely distinct fsstore instances. It checks every restored projection before
// the first post-restore submit, then proves Submit follows the restored active delegate.
func TestRigRestoreStateWorkspaceAndContinuation(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(workspace)

	phase := "initial"
	primaryCalls := 0
	var restoredEffort model.Effort
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, `<role name="`+string(generic.Name)+`">`) {
			primaryCalls++
			if primaryCalls == 1 {
				return startAgentCall("restore-state-child", `{"agent_type":"generic","instructions":"work"}`), nil
			}
			return finalText("generic work complete"), nil
		}
		if phase == "restored" {
			restoredEffort = req.Model.Sampling.Effort
			return finalText("continued on restored delegate"), nil
		}
		return finalText("delegate work complete"), nil
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := a1.SessionID()
	_, observed := runManagedTurnObserved(t, a1, "perform generic work")
	var childID uuid.UUID
	for _, ev := range observed {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("managed Generic work did not create a delegate")
	}
	if err := a1.sess.SetActiveLoop(context.Background(), childID); err != nil {
		t.Fatalf("SetActiveLoop(delegate): %v", err)
	}
	controller, ok := a1.sess.LoopController(childID)
	if !ok {
		t.Fatal("delegate controller not found")
	}
	changedModel := controller.Model()
	changedModel.Name = "restored-state-model"
	// CodeRig production definitions deliberately declare only their base mode. Selecting that
	// declared mode still traverses the real mode-control boundary without inventing a mode.
	if err := controller.SetMode(context.Background(), loop.ModeName("")); err != nil {
		t.Fatalf("SetMode(base): %v", err)
	}
	// Direct inference changes follow the mode selection because a mode change intentionally
	// resets model and effort; restore must reproduce that same last-write-wins precedence.
	if err := controller.Change(context.Background(), loop.ChangeModel(changedModel), loop.ChangeEffort(model.EffortHigh)); err != nil {
		t.Fatalf("Change(delegate inference): %v", err)
	}
	const filename, body = "restore-state.txt", "checkpointed before shutdown"
	if err := os.WriteFile(filepath.Join(workspace, filename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a1.sess.CheckpointWorkspace(context.Background()); err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, filename)); err != nil {
		t.Fatal(err)
	}

	phase = "restored"
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatalf("fresh NewSessionStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("restore from fresh factory: %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	// All assertions in this block intentionally precede the first restored Submit.
	if got := a2.ActiveLoopID(); got != childID {
		t.Errorf("restored active loop = %v, want delegate %v", got, childID)
	}
	child, ok := a2.sess.Loop(childID)
	if !ok {
		t.Fatal("restored delegate missing before submit")
	}
	if got := child.Model().Name; got != changedModel.Name {
		t.Errorf("restored delegate model = %q, want %q", got, changedModel.Name)
	}
	if got := child.Mode(); got != "" {
		t.Errorf("restored delegate mode = %q, want production base mode", got)
	}
	gotBody, err := os.ReadFile(filepath.Join(workspace, filename))
	if err != nil || string(gotBody) != body {
		t.Fatalf("restored workspace before submit = %q, %v; want %q", gotBody, err, body)
	}
	if got := runManagedTurn(t, a2, "continue"); got != "continued on restored delegate" {
		t.Fatalf("restored continuation = %q", got)
	}
	if restoredEffort != model.EffortHigh {
		t.Fatalf("restored continuation effort = %q, want %q", restoredEffort, model.EffortHigh)
	}
}

// TestRigRestoreDelegateOwnership uses a fresh fsstore instance to prove the durable owner
// relation, rather than relying on the in-memory restore coverage in managed_delegation_test.
func TestRigRestoreDelegateOwnership(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(workspace)
	phase := "initial"
	step := 0
	var childID uuid.UUID
	var unrelatedResult string
	var initialSyncResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !strings.Contains(req.System, `<role name="`+string(generic.Name)+`">`) {
			if phase == "initial" {
				return finalText("initial child"), nil
			}
			return finalText("restored follow-up"), nil
		}
		if phase == "initial" {
			if step == 0 {
				step++
				return startAgentCall("own-start", `{"agent_type":"generic","instructions":"first"}`), nil
			}
			initialSyncResult = lastToolText(req)
			return finalText("initial parent"), nil
		}
		switch step {
		case 0:
			step++
			return messageAgentCall("own-send", fmt.Sprintf(`{"agent_id":%q,"message":"again"}`, childID)), nil
		case 1:
			if got := lastToolText(req); got != "restored follow-up" {
				return nil, fmt.Errorf("owned follow-up = %q", got)
			}
			step++
			return messageAgentCall("own-reject", fmt.Sprintf(`{"agent_id":%q,"message":"intrude"}`, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))), nil
		default:
			unrelatedResult = lastToolText(req)
			return finalText("ownership checked"), nil
		}
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	sid := a1.SessionID()
	_, events := runManagedTurnObserved(t, a1, "start child")
	for _, ev := range events {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("no durable child")
	}
	if initialSyncResult != "initial child" {
		t.Fatalf("foreground StartAgent result = %q, want exact child final", initialSyncResult)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	phase, step = "restored", 0
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sid}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	if got := runManagedTurn(t, a2, "continue"); got != "ownership checked" {
		t.Fatalf("final = %q", got)
	}
	if unrelatedResult != "error: subagent request failed" {
		t.Fatalf("unrelated delegate result = %q", unrelatedResult)
	}
}

// TestAgentToolsFSStorePersistence drives two persistent agents through the production
// CodeRig rig over fsstore. It covers direct-child listing, foreground reuse, and stopping
// one active child without exposing request correlation IDs.
func TestAgentToolsFSStorePersistence(t *testing.T) {
	t.Chdir(t.TempDir())
	step := 0
	var first, second agentHandle
	var listResult, messageResult, stopResult string
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	childCalls := 0
	client := &concurrentManagedScript{}
	client.fn = func(ctx context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, `<role name="`+string(generic.Name)+`">`) {
			childCalls++ // serialized by the parent barriers below: child 1 enters before child 2 starts
			switch childCalls {
			case 1:
				close(firstEntered)
				select {
				case <-releaseFirst:
					return finalText("independent first child result"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case 2:
				close(secondEntered)
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				return finalText("exact follow-up child result"), nil
			}
		}
		prior := lastToolText(req)
		switch step {
		case 0:
			step++
			return startAgentCall("fs-agent-1", `{"agent_type":"generic","instructions":"first","wait_for_response":false}`), nil
		case 1:
			var err error
			first, err = parseAgentHandle(prior)
			if err != nil {
				return nil, err
			}
			select {
			case <-firstEntered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			step++
			return startAgentCall("fs-agent-2", `{"agent_type":"generic","instructions":"second","wait_for_response":false}`), nil
		case 2:
			var err error
			second, err = parseAgentHandle(prior)
			if err != nil {
				return nil, err
			}
			select {
			case <-secondEntered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			step++
			return listAgentsCall("fs-list", `{}`), nil
		case 3:
			listResult = prior
			close(releaseFirst)
			step++
			return messageAgentCall("fs-message", fmt.Sprintf(`{"agent_id":%q,"message":"follow up"}`, first.AgentID)), nil
		case 4:
			messageResult = prior
			step++
			return stopAgentCall("fs-stop", fmt.Sprintf(`{"agent_id":%q}`, second.AgentID)), nil
		default:
			stopResult = prior
			return finalText("persistent agent matrix complete"), nil
		}
	}
	f := newIntegrationFactory(t)
	a, err := f.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if got := runManagedTurn(t, a, "run two agents"); got != "persistent agent matrix complete" {
		t.Fatalf("final = %q", got)
	}
	if first.AgentID == second.AgentID {
		t.Fatalf("first=%+v second=%+v, want independent agent ids", first, second)
	}
	if !strings.Contains(listResult, first.AgentID) || !strings.Contains(listResult, second.AgentID) {
		t.Fatalf("list result = %q, want both direct agents", listResult)
	}
	if messageResult != "exact follow-up child result" {
		t.Fatalf("message result = %q", messageResult)
	}
	if !strings.Contains(stopResult, second.AgentID) || strings.Contains(stopResult, `"stopped"`) {
		t.Fatalf("stop result = %q, want agent state result without stopped", stopResult)
	}
}

// TestManagedDelegateDeclaredModeFSStore uses a Generic-only managed topology with
// the one deliberate test-only difference: the Generic child declares
// a named mode. It complements the production-definition rejection test by proving a mode
// is accepted only when present in the target definition.
func TestManagedDelegateDeclaredModeFSStore(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	t.Chdir(root)
	phase := "initial"
	primaryCalls := 0
	var childModel string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, "mode-test-primary") {
			primaryCalls++
			if primaryCalls == 1 {
				return startAgentCall("declared-mode", `{"agent_type":"generic","instructions":"plan it","agent_mode":"plan"}`), nil
			}
			return finalText("declared mode complete"), nil
		}
		if phase == "restored" {
			childModel = req.Model.Name
		}
		return finalText("mode child complete"), nil
	}
	modeModel := testModel()
	modeModel.Name = "declared-generic-model"
	definitions := func(t *testing.T) []loop.Definition {
		t.Helper()
		primer, err := loop.Define(
			loop.WithName(generic.Name), loop.WithInference(client, testModel()), loop.WithSystem("mode-test-primary"),
			loop.WithAccessGate(approveAllAccessGate{}),
			loop.WithPolicyRevision("mode-test-primary-v1"), loop.WithDelegates(generic.Name),
			loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
		)
		if err != nil {
			t.Fatal(err)
		}
		child, err := loop.Define(
			loop.WithName(generic.Name), loop.WithInference(client, testModel()),
			loop.WithModes(loop.Mode{Name: "plan"}, loop.Mode{Name: "build", Model: modeModel, Effort: model.EffortHigh}),
			loop.WithInitialMode("plan"), loop.WithPolicyRevision("mode-test-child-v1"),
		)
		if err != nil {
			t.Fatal(err)
		}
		return []loop.Definition{primer, child}
	}
	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	assembly1, err := buildRig(definitions(t), f1.stores, root, Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	controller1, err := assembly1.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a1, err := newSessionAdapter(context.Background(), controller1, f1.stores.session, false)
	if err != nil {
		t.Fatal(err)
	}
	sid := a1.SessionID()
	got, observed := runManagedTurnObserved(t, a1, "use plan mode")
	if got != "declared mode complete" {
		t.Fatalf("final = %q", got)
	}
	var childID uuid.UUID
	for _, ev := range observed {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	childController, ok := controller1.LoopController(childID)
	if !ok {
		t.Fatal("declared-mode child controller missing")
	}
	if childController.Mode() != "plan" {
		t.Fatalf("spawned mode = %q, want plan", childController.Mode())
	}
	if err := childController.SetMode(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if err := controller1.SetActiveLoop(context.Background(), childID); err != nil {
		t.Fatal(err)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	phase = "restored"
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	assembly2, err := buildRig(definitions(t), f2.stores, root, Config{}, false)
	if err != nil {
		t.Fatal(err)
	}
	controller2, err := assembly2.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := newSessionAdapter(context.Background(), controller2, f2.stores.session, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	backlog, err := a2.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var replayedChild bool
	for _, ev := range backlog {
		if started, ok := ev.(event.LoopStarted); ok && started.LoopID == childID {
			replayedChild = true
			break
		}
	}
	if !replayedChild {
		t.Fatal("restored all-loop backlog omitted delegate LoopStarted")
	}
	restoredChild, ok := controller2.Loop(childID)
	if !ok {
		t.Fatal("restored declared-mode child missing before submit")
	}
	if restoredChild.Mode() != "build" {
		t.Fatalf("restored changed mode before submit = %q, want build", restoredChild.Mode())
	}
	if restoredChild.Model().Name != modeModel.Name {
		t.Fatalf("restored changed-mode model = %q, want %q", restoredChild.Model().Name, modeModel.Name)
	}
	if got := runManagedTurn(t, a2, "continue in build mode"); got != "mode child complete" {
		t.Fatalf("restored child final = %q", got)
	}
	if childModel != modeModel.Name {
		t.Fatalf("declared-mode child model = %q, want %q", childModel, modeModel.Name)
	}
}

// TestManagedDelegateUndeclaredModeFSStore proves the production declared-mode
// definition rejects a requested mode before registering any child in the real fsstore
// journal. This is the production counterpart to the topology-equivalent acceptance test.
func TestManagedDelegateUndeclaredModeFSStore(t *testing.T) {
	t.Chdir(t.TempDir())
	calls := 0
	var result string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		calls++
		if calls == 1 {
			return startAgentCall("undeclared-mode", `{"agent_type":"generic","instructions":"must reject","agent_mode":"build"}`), nil
		}
		result = lastToolText(req)
		return finalText("rejection observed"), nil
	}
	f := newIntegrationFactory(t)
	a, err := f.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	_, observed := runManagedTurnObserved(t, a, "request undeclared mode")
	if !strings.Contains(result, "error:") {
		t.Fatalf("undeclared mode result = %q, want bounded delegation error", result)
	}
	if got := countLoopStarted(observed); got != 0 {
		t.Fatalf("undeclared mode registered %d child loops, want 0", got)
	}
}

// TestRigRestoreSnapshotFailureAdmission composes the actual fsstore session journal and
// leases with a deterministic failing workspace blob seam. This keeps the complete CodeRig
// topology/bindings while proving the two documented snapshot priorities at admission.
func TestRigRestoreSnapshotFailureAdmission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority rig.SnapshotPriority
		required bool
	}{
		{name: "required faults future admission", priority: rig.SnapshotRequired, required: true},
		{name: "best effort permits admission and retries", priority: rig.SnapshotBestEffort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			f := newIntegrationFactory(t)
			blobs := &failingSnapshotBlobs{Blobs: f.fs.Backend().Blobs, fail: true, attempts: make(chan struct{}, 4)}
			workspace, err := workspacestore.Open(blobs)
			if err != nil {
				t.Fatal(err)
			}
			client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
				return finalText("snapshot turn complete"), nil
			}}
			access, cfg := headlessTestAccess(t, Config{}, root)
			definitions, err := genericTestDefinitions(client, testModel(), cfg, access)
			if err != nil {
				t.Fatal(err)
			}
			registration, err := newConversationHustleRegistration()
			if err != nil {
				t.Fatal(err)
			}
			options := []rig.Option{
				rig.WithLoops(definitions...),
				rig.WithPrimers(string(generic.Name)),
				rig.WithActivePrimer(string(generic.Name)),
				rig.WithSessionStore(f.stores.session),
				rig.WithExclusiveWorkspace(workspace, root, f.stores.leaser),
				rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: tc.priority, Timeout: 5 * time.Second}),
				rig.WithDelegationLimits(rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}),
				rig.WithFingerprintFields(agentFingerprintFields(cfg)),
			}
			options = append(options, registration.options()...)
			assembly, err := rig.Define(options...)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := assembly.NewSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a, err := newSessionAdapter(context.Background(), controller, f.stores.session, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close(context.Background()) })
			runManagedTurn(t, a, "trigger failing snapshot")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case <-blobs.attempts:
			case <-ctx.Done():
				t.Fatalf("snapshot attempt not observed: %v", ctx.Err())
			}
			idle := controller.(interface{ WaitIdle(context.Context) error })
			idleErr := idle.WaitIdle(ctx)
			if tc.required {
				if idleErr == nil {
					t.Fatal("required snapshot failure did not fault WaitIdle")
				}
				if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: "must reject"}}); err == nil {
					t.Fatal("required snapshot failure admitted a later submit")
				}
				return
			}
			if idleErr != nil {
				t.Fatalf("best-effort WaitIdle = %v", idleErr)
			}
			blobs.setFail(false)
			runManagedTurn(t, a, "retry snapshot")
			select {
			case <-blobs.attempts:
			case <-ctx.Done():
				t.Fatalf("best-effort snapshot did not retry: %v", ctx.Err())
			}
			if err := idle.WaitIdle(ctx); err != nil {
				t.Fatalf("best-effort retry WaitIdle = %v", err)
			}
		})
	}
}

// TestRigRestoreSiblingOwnershipScopes builds two managed primer parents over the same
// real fsstore session. Each owns one worker. After a fresh-factory restore, parent A can
// still address A's worker but cannot send, wait, or interrupt parent B's real worker.
func TestRigRestoreSiblingOwnershipScopes(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	t.Chdir(root)
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
		return finalText("scoped worker complete"), nil
	}}
	var probeA, probeB *delegateProbe
	definitions := func(t *testing.T) []loop.Definition {
		t.Helper()
		parent := func(name string, capture **delegateProbe) loop.Definition {
			probe := &delegateProbe{}
			*capture = probe
			def, err := loop.Define(
				loop.WithName(identity.AgentName(name)),
				loop.WithInference(client, testModel()),
				loop.WithTools(probe.definition()),
				loop.WithAccessGate(approveAllAccessGate{}),
				loop.WithPolicyRevision(name+"-v1"),
				loop.WithDelegates("scoped-worker"),
				loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
			)
			if err != nil {
				t.Fatal(err)
			}
			return def
		}
		worker, err := loop.Define(loop.WithName("scoped-worker"), loop.WithInference(client, testModel()), loop.WithPolicyRevision("scoped-worker-v1"))
		if err != nil {
			t.Fatal(err)
		}
		return []loop.Definition{parent("parent-a", &probeA), parent("parent-b", &probeB), worker}
	}
	build := func(t *testing.T, f *SessionStoreFactory) *rig.Rig {
		t.Helper()
		assembly, err := rig.Define(
			rig.WithLoops(definitions(t)...),
			rig.WithPrimers("parent-a", "parent-b"),
			rig.WithActivePrimer("parent-a"),
			rig.WithSessionStore(f.stores.session),
			rig.WithExclusiveWorkspace(f.stores.workspace, root, f.stores.leaser),
			rig.WithSnapshots(rig.SnapshotPolicy{Trigger: rig.SnapshotOnIdle, Priority: rig.SnapshotBestEffort}),
			rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: 8}),
			rig.WithFingerprintFields(rig.ConfigFingerprintFields{AgentKind: "coderig:scoped-ownership-test"}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return assembly
	}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f1.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	s1, err := build(t, f1).NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sid := s1.SessionID()
	parentA, parentB := probeA.captured(), probeB.captured()
	if parentA == nil || parentB == nil {
		t.Fatal("initial bind did not capture both scoped controllers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aChild, err := parentA.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "scoped-worker", Message: "a", WaitForResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	bChild, err := parentB.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "scoped-worker", Message: "b", WaitForResponse: true})
	if err != nil {
		t.Fatal(err)
	}
	if aChild.AgentID == bChild.AgentID {
		t.Fatal("distinct parents received the same child")
	}
	if err := s1.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	probeA, probeB = nil, nil
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	s2, err := build(t, f2).RestoreSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	parentA, parentB = probeA.captured(), probeB.captured()
	if parentA == nil || parentB == nil {
		t.Fatal("restore did not rebind both scoped controllers")
	}
	if got, err := parentA.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: aChild.AgentID, Message: "owned", WaitForResponse: true}); err != nil || got.Response != "scoped worker complete" {
		t.Fatalf("restored own child send = %+v, %v", got, err)
	}
	for _, request := range []tool.DelegateRequest{
		{Operation: tool.DelegateSend, AgentID: bChild.AgentID, Message: "cross-owner", WaitForResponse: true},
		{Operation: tool.DelegateInterrupt, AgentID: bChild.AgentID},
	} {
		if _, err := parentA.Execute(ctx, request); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("parent A cross-owner %v error = %v, want ownership rejection", request.Operation, err)
		}
	}
}

// TestProcessAdapterResolverIndependentOfHarnessTransport is Task 26B's
// negative proof for "no Harness ProcessBinding, Rig option, lifecycle
// option, or provider is used to transport the adapter": newProcessRunnerResolver
// is built directly over the session's *sandbox.ExecutorSet -- the SAME
// sessionAccess.set field toolsets.go's own bashDefinition and
// accessGate already resolve executors from -- and driven by a LoopID
// obtained from a GENUINELY RESTORED production session over real fsstore.
// No rig.Option, tool.ProcessBinding, Harness lifecycle registration, or
// provider of any kind is constructed anywhere in this test: the resolver
// and the executor set it captures are reached entirely through
// RuntimeAgent's own unexported `access *sessionAccess` field and the
// restored session's ActiveLoopID, never through rig.Define's option list.
func TestProcessAdapterResolverIndependentOfHarnessTransport(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	t.Chdir(workspace)
	client := &managedScript{fn: func(context.Context, inference.Request) ([]content.Chunk, error) {
		return finalText("resolver-independence turn complete"), nil
	}}

	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := a1.SessionID()
	primerLoopID := a1.ActiveLoopID()
	if primerLoopID.IsZero() {
		t.Fatal("primer LoopID is zero before the first restore")
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatalf("fresh NewSessionStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a2, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("restore from fresh factory: %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	restoredLoopID := a2.ActiveLoopID()
	if restoredLoopID != primerLoopID {
		t.Fatalf("restored ActiveLoopID = %v, want the same primer LoopID %v", restoredLoopID, primerLoopID)
	}

	if a2.access == nil || a2.access.set == nil {
		t.Fatal("restored RuntimeAgent has no Generic *sandbox.ExecutorSet to resolve against")
	}
	resolver := newProcessRunnerResolver(a2.access.set)
	runner, err := resolver(context.Background(), restoredLoopID)
	if err != nil {
		t.Fatalf("resolver(restored primer LoopID): %v", err)
	}
	adapter, ok := runner.(processRunnerAdapter)
	if !ok {
		t.Fatalf("resolver returned %T, want processRunnerAdapter", runner)
	}
	direct, err := a2.access.set.For(restoredLoopID.String())
	if err != nil {
		t.Fatalf("set.For(restored primer LoopID) directly: %v", err)
	}
	if adapter.exec != direct {
		t.Fatal("resolver's wrapped executor is not the SAME per-Loop executor set.For resolves directly -- resolver and bashDefinition/accessGate must agree on the identical instance")
	}
}
