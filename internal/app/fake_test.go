package app

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// testResourceStorageProvider is a throwaway rig.SessionResourceStorageProvider
// backed by one t.TempDir() base, auto-cleaned by the testing package. It exists
// because Generic's Bash definition
// tool.RequiresProcessServices, so ANY test that assembles the real production
// session (genericTestDefinition) and feeds it through buildRig/rig.Define now needs
// SOME session-resource-storage provider wired — exactly like production's
// persistedResourceStorageProvider (persisted sessions) and
// headlessResourceStorageProvider (headless sessions) already are. A test that
// is unconcerned with process-service tools is unaffected by its presence: it
// changes nothing for a topology that does not declare RequiresProcessServices,
// matching sessionStores.resourceStorage's own doc comment.
type testResourceStorageProvider struct{ base string }

func (p testResourceStorageProvider) StorageForSession(_ context.Context, id uuid.UUID) (rig.SessionResourceStorage, error) {
	return rig.SessionResourceStorage{
		Path:     filepath.Join(p.base, id.String()),
		Identity: "coderig:test-session-resource-storage/v1",
	}, nil
}

var _ rig.SessionResourceStorageProvider = testResourceStorageProvider{}

// openTestStores wraps openStores(memstore.New()) with a testResourceStorageProvider
// so package tests that build the real production roster can assemble a rig
// without hand-wiring persisted/headless resource storage themselves. See
// testResourceStorageProvider's doc comment for why this became necessary.
func openTestStores(t *testing.T) (*sessionStores, error) {
	t.Helper()
	stores, err := openStores(memstore.New())
	if err != nil {
		return nil, err
	}
	stores.resourceStorage = testResourceStorageProvider{base: t.TempDir()}
	return stores, nil
}

// fakeInvokeStep scripts exactly one one-shot request. entered/release are
// independent of the stream lane so tests can prove compaction blocks only the
// loop that requested it.
type fakeInvokeStep struct {
	response *inference.Response
	respond  func(inference.Request) (*inference.Response, error)
	err      error
	entered  chan struct{}
	release  <-chan struct{}
}

// fakeStreamStep scripts exactly one ordinary streamed inference request.
type fakeStreamStep struct {
	chunks  []content.Chunk
	result  *stream.StreamResult
	err     error
	entered chan struct{}
	release <-chan struct{}
}

type fakeLLMScriptError struct{ Operation string }

func (e *fakeLLMScriptError) Error() string {
	return "coderig test: fake LLM " + e.Operation + " not scripted"
}

// fakeLLM is a controllable inference.Client for tests. Stream and Invoke have
// independent scripts, captures, and barriers so compaction can never consume
// an ordinary-turn fixture. The legacy stream fields remain as a compatibility
// fallback for existing focused tests.
type fakeLLM struct {
	credential string // opaque test-only connection secret; never part of fingerprints
	chunks     []content.Chunk
	streamErr  error         // returned from Stream() before any chunk
	hold       chan struct{} // if non-nil, Next blocks on hold or ctx after chunks

	entered     chan struct{} // if non-nil, closed once when Stream is first called
	enteredOnce sync.Once

	mu             sync.Mutex
	streamSteps    []fakeStreamStep
	invokeSteps    []fakeInvokeStep
	streamRequests []inference.Request
	invokeRequests []inference.Request
}

func (f *fakeLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	f.mu.Lock()
	f.invokeRequests = append(f.invokeRequests, req)
	if len(f.invokeSteps) == 0 {
		f.mu.Unlock()
		return nil, &fakeLLMScriptError{Operation: "Invoke"}
	}
	step := f.invokeSteps[0]
	f.invokeSteps = f.invokeSteps[1:]
	f.mu.Unlock()
	if step.entered != nil {
		close(step.entered)
	}
	if step.release != nil {
		select {
		case <-step.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if step.err != nil {
		return nil, step.err
	}
	if step.respond != nil {
		return step.respond(req)
	}
	return step.response, nil
}

func (f *fakeLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	f.mu.Lock()
	f.streamRequests = append(f.streamRequests, req)
	var scripted *fakeStreamStep
	if len(f.streamSteps) != 0 {
		step := f.streamSteps[0]
		f.streamSteps = f.streamSteps[1:]
		scripted = &step
	}
	f.mu.Unlock()
	if scripted != nil {
		if scripted.entered != nil {
			close(scripted.entered)
		}
		if scripted.release != nil {
			select {
			case <-scripted.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if scripted.err != nil {
			return nil, scripted.err
		}
		return fakeStreamReader(scripted.chunks, scripted.result), nil
	}
	if f.entered != nil {
		f.enteredOnce.Do(func() { close(f.entered) })
	}
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(f.chunks) {
			c := f.chunks[i]
			i++
			return c, nil
		}
		if f.hold != nil {
			select {
			case <-f.hold:
				return nil, io.EOF
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, io.EOF
	}
	return stream.NewStreamReader(next, nil), nil
}

func fakeStreamReader(chunks []content.Chunk, result *stream.StreamResult) *stream.StreamReader[content.Chunk] {
	index := 0
	next := func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}
	if result == nil {
		return stream.NewStreamReader(next, nil)
	}
	terminal := *result
	return stream.NewStreamReaderWithResult(next, nil, func() (stream.StreamResult, bool, error) {
		return terminal, true, nil
	})
}

func (f *fakeLLM) capturedRequests() ([]inference.Request, []inference.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.Request(nil), f.streamRequests...), append([]inference.Request(nil), f.invokeRequests...)
}

// testModel is a minimal valid model.Model for fake-client tests. The fake
// ignores it, but the configured window mirrors a consumer-supplied LM Studio
// limit so context-enabled definitions have a resolvable input denominator.
func testModel() model.Model {
	selected := lmStudioLocal("fake-model")
	selected.Limits = model.ContextLimits{WindowTokens: 128_000}
	return selected
}

// headlessTestAccess builds the headless session access wiring over root (a read-only
// permission store and non-prompting gate evaluators) and registers its executor-set
// closers for cleanup. Registering the cleanup here (before the caller builds its agent)
// means the agent's own cleanup runs first (t.Cleanup is LIFO), so the session shuts down
// before its executor sets are closed. It also folds the access digest into cfg so tests
// build sessions with the production fingerprint. The updated cfg is returned.
func headlessTestAccess(t *testing.T, cfg Config, root string) (*sessionAccess, Config) {
	t.Helper()
	access, err := buildHeadlessAccess(cfg, root)
	if err != nil {
		t.Fatalf("buildHeadlessAccess() error = %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	cfg.AccessConfigRev = access.configRev
	return access, cfg
}

// newTestAgent builds an ISOLATED headless sessionAdapter over a FRESH in-memory store and a
// throwaway workspace root, so a test never contends on the process-shared headless store's
// exclusive checkout lease (which would serialize every parallel test on the real cwd) and
// never snapshots the real working tree. Each call is independent and parallel-safe; the agent
// is Closed at test cleanup. client is the fake inference client to drive turns.
func newTestAgent(t *testing.T, client inference.Client, cfg Config) *sessionAdapter {
	t.Helper()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, cfg, root)
	definition, err := genericTestDefinition(client, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("genericTestDefinition() error = %v", err)
	}
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	assembly, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	agent, err := newSessionAdapter(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatalf("newSessionAdapter() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// newModelFactory is retained only as a package-test fixture seam. Production
// composition always binds the model loaded from models.json explicitly.
func newModelFactory() ModelFactory { return newModelFactoryFor(testModel()) }
