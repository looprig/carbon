package app

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

// acceptance_test.go drives the assembled CodeRig headless (over an isolated in-memory
// store + a temp checkout, so it never contends on the real current-checkout lease with
// sibling tests). It proves the composed rig starts with builder as the active durable
// primer, that a submitted turn is observable on the whole-session event stream, and that
// the agent closes cleanly.
//
// The composed managed-delegation action flows live in managed_delegation_test.go; the
// fresh-fsstore restore and runtime-skill matrix lives in the integration-tagged tests.

// openAcceptanceAgent opens a headless CodeRig session over an isolated store + temp root and
// returns the composition-root runtime agent (which owns the session's executor-set closers).
func openAcceptanceAgent(t *testing.T) (*RuntimeAgent, *swarmStores) {
	t.Helper()
	stores := mustHeadlessTestStores(t)
	agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(testModel()), Config{}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent, stores
}

// durableRootLoops folds the session's durable log and returns every zero-parent primer.
func durableRootLoops(t *testing.T, stores *swarmStores, sessionID uuid.UUID) map[string]uuid.UUID {
	t.Helper()
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	roots := make(map[string]uuid.UUID)
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return roots
		}
		if err != nil {
			t.Fatalf("cursor.Next() error = %v", err)
		}
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID.IsZero() {
			roots[string(started.AgentName)] = started.LoopID
		}
	}
}

// TestAcceptanceActivePrimerIsBuilder proves the composed rig selects builder as
// the active durable, zero-parent primer while the other primers also exist.
func TestAcceptanceActivePrimerIsBuilder(t *testing.T) {
	t.Parallel()
	agent, stores := openAcceptanceAgent(t)

	roots := durableRootLoops(t, stores, agent.SessionID())
	for _, name := range []string{string(planner.Name), string(builder.Name), string(reviewer.Name)} {
		if roots[name].IsZero() {
			t.Errorf("durable zero-parent primer %q is missing; roots=%v", name, roots)
		}
	}
	if got, want := agent.ActiveLoopID(), roots[string(builder.Name)]; got != want {
		t.Errorf("ActiveLoopID() = %v, want builder root %v", got, want)
	}
}

// TestAcceptanceSubmitIsObservable proves a submitted turn runs on the composed rig and is
// observable on the one whole-session subscription, then the agent closes cleanly.
func TestAcceptanceSubmitIsObservable(t *testing.T) {
	t.Parallel()
	agent, _ := openAcceptanceAgent(t)

	stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	inputID, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hello"}})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if inputID.IsZero() {
		t.Error("Submit() returned a zero input id")
	}

	// The turn produces at least one enduring event; a bounded wait guards against a hang.
	select {
	case d, ok := <-stream.Events():
		if !ok {
			t.Fatal("event stream closed before any delivery")
		}
		if d.Event == nil {
			t.Error("received a nil event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event observed within the deadline after Submit")
	}
}

// TestAcceptanceLoopHandleExposesPrimerModel proves the active loop handle exposes the shared
// model identity the primer was defined with.
func TestAcceptanceLoopHandleExposesPrimerModel(t *testing.T) {
	t.Parallel()
	agent, _ := openAcceptanceAgent(t)

	handle, ok := agent.Controller().Loop(agent.ActiveLoopID())
	if !ok {
		t.Fatal("root loop handle not found")
	}
	if handle.Model().Name != testModel().Name {
		t.Errorf("root loop model = %q, want %q", handle.Model().Name, testModel().Name)
	}
	var _ loop.Handle = handle
}
