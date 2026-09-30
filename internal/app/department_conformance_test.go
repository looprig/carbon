package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
	"github.com/looprig/host/department/departmenttest"
	"github.com/looprig/inference"
)

// probeRecorder is a departmenttest.Recorder over the probes a launcher wrapped:
// what harness's own runtime-command applier was handed, beneath the seam.
type probeRecorder struct {
	mu     sync.Mutex
	probes []*probeController
}

func (r *probeRecorder) add(p *probeController) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, p)
}

func (r *probeRecorder) Applied(command department.RuntimeCommand) (departmenttest.Applied, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.probes {
		for _, admitted := range p.recordedAdmitted() {
			if string(admitted.CommandID) == string(command.CommandID) {
				return departmenttest.Applied{Principal: admitted.Principal, Metadata: admitted.Metadata}, true
			}
		}
	}
	return departmenttest.Applied{}, false
}

// TestCarbonTargetPassesRuntimeConformance runs Host's exported runtime
// conformance against the PRODUCTION Carbon launch: NewCarbonDepartment over the
// PooledLauncher the serve path composes (per-tenant journal store, tool-result
// capture, session-scoped access and MCP), with only the model client replaced.
//
// The probe wrapping each launched session forwards every command to harness's
// real applier and records what it was handed, so the principal and metadata
// check is made at the exact type harness receives.
func TestCarbonTargetPassesRuntimeConformance(t *testing.T) {
	ctx := context.Background()
	pooled, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}}}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pooled.Close(context.Background()) })

	recorder := &probeRecorder{}
	recording := pooledLauncherFunc(func(ctx context.Context, scope LaunchScope) (session.SessionController, error) {
		controller, err := pooled.Launch(ctx, scope)
		if err != nil {
			return nil, err
		}
		probe := newProbe(controller)
		recorder.add(probe)
		return probe, nil
	})
	target := mustCarbonTargetOver(t, recording, department.CompatibilityID("carbon-conformance"))
	departmenttest.RunRuntimeConformance(t, target, func(ctx context.Context, target department.LaunchTarget, request department.CreateRequest) (department.Runtime, departmenttest.Recorder, error) {
		// The pooled launcher serves exactly the root it materializes for the
		// session and refuses any other, so the request names that root, as
		// Host's workspace seam (EnsureWorkspace) would.
		root, err := pooled.EnsureWorkspace(ctx, request.TenantID, request.SessionID)
		if err != nil {
			return nil, nil, err
		}
		request.WorkspaceRoot = root
		runtime, err := target.Create(ctx, request)
		return runtime, recorder, err
	})
}

// TestLaunchScopeRefusesARequestItsScopeDisagreesWith pins the one derivation
// Carbon's launch shim does not apply itself: harnessruntime hands NewSession
// the rig options for the request's RigSessionID, and the launcher applies its
// own from the same id. The shim refuses any disagreement — a wrong option
// count, a restore of another id, or a create/restore cross — without launching.
func TestLaunchScopeRefusesARequestItsScopeDisagreesWith(t *testing.T) {
	t.Parallel()
	launches := 0
	launcher := launcherFunc(func(context.Context, LaunchScope) (session.SessionController, error) {
		launches++
		return nil, errors.New("carbon test: must not launch")
	})
	id, other := mustUUIDForTest(t), mustUUIDForTest(t)
	create := launchScope{launcher: launcher, scope: LaunchScope{RigSessionID: id}}
	restore := launchScope{launcher: launcher, scope: LaunchScope{RigSessionID: id, Restore: true}}
	var mismatch *LaunchOptionsMismatchError
	for name, call := range map[string]func() error{
		"a named create given no options": func() error { _, err := create.NewSession(context.Background()); return err },
		"a named create given two options": func() error {
			_, err := create.NewSession(context.Background(), rig.WithSessionID(id), rig.WithSessionID(id))
			return err
		},
		"a restore of another id":  func() error { _, err := restore.RestoreSession(context.Background(), other); return err },
		"a create scope restoring": func() error { _, err := create.RestoreSession(context.Background(), id); return err },
		"a restore scope creating": func() error { _, err := restore.NewSession(context.Background()); return err },
		"a minting create with one": func() error {
			_, err := launchScope{launcher: launcher}.NewSession(context.Background(), rig.WithSessionID(id))
			return err
		},
	} {
		if err := call(); !errors.As(err, &mismatch) {
			t.Errorf("%s: %v, want *LaunchOptionsMismatchError", name, err)
		}
	}
	if launches != 0 {
		t.Fatalf("the launcher ran %d times for requests the scope refuses", launches)
	}
}
