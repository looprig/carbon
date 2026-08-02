//go:build integration

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
)

// textChunk wraps s as a streamed text chunk for the fake LLM. (The fake_test fakeLLM is
// shared with the non-tagged unit tests; this helper is only used by the persisted
// integration tests, which need to drive a turn to a terminal.)
func textChunk(s string) content.Chunk { return &content.TextChunk{Text: s} }

// newIntegrationFactory opens the production session-store factory over a temp fsstore backend
// (session ledger/journal/blobs + workspace snapshots all under one root) and registers its
// Close. It is the CLI-shaped composition — one on-disk store shared by every session — and the
// seam the integration tests drive with an injected fake client via openWithClient.
func newIntegrationFactory(t *testing.T) *SessionStoreFactory {
	t.Helper()
	f, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func persistedVisibilityEvents(t *testing.T, sessionID, loopID uuid.UUID) []event.Event {
	t.Helper()
	definition, err := newConversationCompactionDefinition()
	if err != nil {
		t.Fatalf("newConversationCompactionDefinition() error = %v", err)
	}
	internalHeader := func() event.Header {
		return event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID},
			EventID:         mustUUID(t),
			EventVisibility: event.Internal,
		}
	}
	run := func() event.HustleRunDescriptor {
		return event.HustleRunDescriptor{Definition: definition.Descriptor(), RunID: hustle.RunID(mustUUID(t))}
	}
	completedRun := run()
	completedRun.Runtime = event.ModelRuntime{
		Key:    model.ModelKey{Provider: "test", Model: "test"},
		Limits: model.ContextLimits{WindowTokens: 100},
	}
	publicHeader := event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
		EventID:     mustUUID(t),
	}
	return []event.Event{
		event.HustleStarted{Header: internalHeader(), Run: run()},
		event.HustleCompleted{Header: internalHeader(), Run: completedRun},
		event.HustleFailed{
			Header: internalHeader(),
			Run:    run(), Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled,
		},
		event.CompactionRejected{
			Header:           publicHeader,
			AttemptID:        event.CompactAttemptID(mustUUID(t)),
			WaiterCommandIDs: []uuid.UUID{mustUUID(t)},
			Reason:           event.CompactionReasonManual,
			Basis:            event.ContextBasis{Revision: 1, ThroughEventID: mustUUID(t)},
			RejectReason:     event.CompactRejectUnavailable,
		},
	}
}

func drainEventReplay(t *testing.T, replayer journal.EventReplayer) []event.Event {
	t.Helper()
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var events []event.Event
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			return events
		}
		if nextErr != nil {
			t.Fatalf("cursor.Next() error = %v", nextErr)
		}
		events = append(events, ev)
	}
}

func TestPersistedVisibilityFiltersPublicBacklogAndRetainsAudit(t *testing.T) {
	factory := newIntegrationFactory(t)
	sessionID, loopID := mustUUID(t), mustUUID(t)
	lease, err := factory.stores.session.AcquireLease(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })
	writer, err := factory.stores.session.OpenJournal(context.Background(), sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	for _, ev := range persistedVisibilityEvents(t, sessionID, loopID) {
		if _, err := writer.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T) error = %v", ev, err)
		}
	}

	tests := []struct {
		name      string
		load      func(*testing.T) []event.Event
		wantTypes []string
	}{
		{
			name: "adapter restore backlog exposes only public compaction terminal",
			load: func(t *testing.T) []event.Event {
				controller := &fakeController{sessionID: sessionID, sub: newFakeSub()}
				agent, err := newSessionAdapter(context.Background(), controller, factory.stores.session, true)
				if err != nil {
					t.Fatalf("newSessionAdapter() error = %v", err)
				}
				backlog, err := agent.ReplayBacklog(context.Background())
				if err != nil {
					t.Fatalf("ReplayBacklog() error = %v", err)
				}
				return backlog
			},
			wantTypes: []string{"event.CompactionRejected"},
		},
		{
			name: "privileged replay retains internal audit and public terminal",
			load: func(t *testing.T) []event.Event {
				replayer, err := factory.stores.session.OpenInternalEventReplayer(sessionID, sessionstore.ReplayRequest{})
				if err != nil {
					t.Fatalf("OpenInternalEventReplayer() error = %v", err)
				}
				return drainEventReplay(t, replayer)
			},
			wantTypes: []string{"event.HustleStarted", "event.HustleCompleted", "event.HustleFailed", "event.CompactionRejected"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := typeNames(tt.load(t))
			if !reflect.DeepEqual(got, tt.wantTypes) {
				t.Errorf("event types = %v, want %v", got, tt.wantTypes)
			}
		})
	}
}

// drainTurn submits input through the persisted agent and drains a fresh subscription to the
// turn terminal — deterministic (unlike a WaitIdle that can race the fire-and-forget submit). The
// subscription is created BEFORE the submit so the terminal is never missed.
func drainTurn(t *testing.T, a tui.Agent, text string) {
	t.Helper()
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(20 * time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a terminal")
			}
			switch d.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return
			}
		case <-timeout:
			t.Fatal("no terminal within deadline")
		}
	}
}

// drainUntilCheckpoint submits a turn and drains a fresh subscription until the workspace
// checkpoint at quiescence lands (event.WorkspaceCheckpointed, published by CheckpointWorkspace
// after the Active→Idle edge). The subscription is created before the submit so the checkpoint is
// never missed.
func drainUntilCheckpoint(t *testing.T, a tui.Agent, text string) {
	t.Helper()
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(30 * time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a workspace checkpoint")
			}
			if _, ok := d.Event.(event.WorkspaceCheckpointed); ok {
				return
			}
		case <-timeout:
			t.Fatal("no workspace checkpoint within deadline")
		}
	}
}

// TestNewSessionStoreFactoryOpensAndCloses proves the exported constructor opens a store over an
// explicit data dir, lists an empty store as zero rows, and closes cleanly.
func TestNewSessionStoreFactoryOpensAndCloses(t *testing.T) {
	f, err := NewSessionStoreFactory(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStoreFactory: %v", err)
	}
	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List (empty): %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("fresh store List = %d entries, want 0", len(metas))
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSessionStoreNewSessionBasics proves openWithClient with a ZERO selector builds a NEW
// persisted session over the shared store: it has a non-zero SessionID (the factory minted +
// injected it) and, being a fresh session, its cold-repaint backlog carries only the session
// initialization events (SessionStarted + the primer's LoopStarted) — no turn or content
// history to repaint.
func TestSessionStoreNewSessionBasics(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if a.SessionID().IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}
	backlog, err := a.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("new-session ReplayBacklog: %v", err)
	}
	if got := typeNames(backlog); !reflect.DeepEqual(got, []string{"event.SessionStarted", "event.LoopStarted"}) {
		t.Errorf("new-session ReplayBacklog = %v, want only the session initialization events", got)
	}
}

// TestSessionStoreClearReopenReleasesExclusiveWorkspace is the real factory-level /clear
// regression. The first rig owns the exclusive checkout lease; after its session adapter is
// shut down, the same shared factory can build a distinct NewSession over that checkout.
func TestSessionStoreClearReopenReleasesExclusiveWorkspace(t *testing.T) {
	f := newIntegrationFactory(t)
	ctx := context.Background()

	first, err := f.openWithClient(ctx, &fakeLLM{}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("first NewSession: %v", err)
	}
	firstID := first.SessionID()
	blocked, err := f.openWithClient(ctx, &fakeLLM{}, newModelFactory(), SessionSelector{}, Config{})
	if err == nil {
		_ = blocked.Close(ctx)
		t.Fatal("second NewSession succeeded before first shutdown; exclusive workspace lease was not enforced")
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first session: %v", err)
	}

	second, err := f.openWithClient(ctx, &fakeLLM{}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("second NewSession after /clear handoff: %v", err)
	}
	if second.SessionID() == firstID {
		t.Errorf("second SessionID = first SessionID %v, want a fresh /clear session", firstID)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatalf("close second session: %v", err)
	}
}

// primaryLoopAgentName returns the AgentName the LoopStarted for loopID carries in evs, or
// the empty string if no such LoopStarted is present.
func primaryLoopAgentName(evs []event.Event, loopID uuid.UUID) identity.AgentName {
	for _, e := range evs {
		ls, ok := e.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.EventHeader().LoopID == loopID {
			return ls.EventHeader().AgentName
		}
	}
	return ""
}

// TestSessionStoreRoundTrip is the headline CLI-shaped wiring smoke: a NEW persisted session
// runs a turn that persists, the agent is Closed (which releases the session lease), and the
// SAME session is RESUMED over the same store. The restored session's ReplayBacklog reproduces
// the committed Enduring events and a fresh turn continues, proving the durable log survived a
// full close/reopen cycle.
func TestSessionStoreRoundTrip(t *testing.T) {
	f := newIntegrationFactory(t)

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	if sessionID.IsZero() {
		t.Fatal("new persisted session has a zero SessionID")
	}

	drainTurn(t, a, "hello")

	// A clean Close releases the lease, so a resume can reopen the same session without contending
	// the lease or waiting out its expiry.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	a2, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	if a2.SessionID() != sessionID {
		t.Errorf("resumed SessionID = %v, want %v", a2.SessionID(), sessionID)
	}

	backlog, err := a2.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("resumed ReplayBacklog: %v", err)
	}
	if !hasType(backlog, event.TurnStarted{}) {
		t.Errorf("resumed backlog missing TurnStarted: %v", typeNames(backlog))
	}
	// ReplayBacklog includes all-loop Enduring history, including restore brackets.
	if !hasType(backlog, event.RestoreStarted{}) {
		t.Errorf("restored all-loop backlog missing RestoreStarted: %v", typeNames(backlog))
	}
	if got := primaryLoopAgentName(backlog, a2.ActiveLoopID()); got != builder.Name {
		t.Errorf("restored active primer LoopStarted AgentName = %q, want %q", got, builder.Name)
	}

	drainTurn(t, a2, "continue")
}

// TestSessionStoreWorkspaceRoundTrip is the acceptance test the whole extraction exists for: a
// session's workspace is checkpointed at quiescence (SessionIdle), and a later restore
// materializes it — so both the conversation state AND the workspace files come back. The
// workspace is a temp dir the session snapshots via os.Getwd(); t.Chdir points the process there
// (and forbids t.Parallel for this test).
func TestSessionStoreWorkspaceRoundTrip(t *testing.T) {
	f := newIntegrationFactory(t)

	workspace := t.TempDir()
	t.Chdir(workspace)

	const name = "notes.txt"
	const body = "workspace survived a restore"
	// Written BEFORE the session opens, so it is present in every checkpoint the session takes.
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o600); err != nil {
		t.Fatalf("seed workspace file: %v", err)
	}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("first reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (new): %v", err)
	}
	sessionID := a.SessionID()
	drainUntilCheckpoint(t, a, "make a note")
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close (original): %v", err)
	}

	// Destroy the workspace file; a restore must materialize the checkpointed copy back.
	if err := os.Remove(filepath.Join(workspace, name)); err != nil {
		t.Fatalf("remove workspace file: %v", err)
	}

	a2, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("after restore")}}, newModelFactory(), SessionSelector{Resume: sessionID}, Config{})
	if err != nil {
		t.Fatalf("openWithClient (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })

	got, err := os.ReadFile(filepath.Join(workspace, name))
	if err != nil {
		t.Fatalf("workspace file not materialized on restore: %v", err)
	}
	if string(got) != body {
		t.Errorf("materialized workspace file = %q, want %q", string(got), body)
	}
}

// TestSessionStoreListFindsSession proves the store's listing catalog enumerates a session
// created by opening it (the event tap upserts the catalog on SessionStarted).
func TestSessionStoreListFindsSession(t *testing.T) {
	f := newIntegrationFactory(t)

	// An empty store lists nothing.
	if metas, err := f.List(context.Background()); err != nil {
		t.Fatalf("List (empty): %v", err)
	} else if len(metas) != 0 {
		t.Errorf("List on a fresh store = %d entries, want 0", len(metas))
	}

	a, err := f.openWithClient(context.Background(),
		&fakeLLM{chunks: []content.Chunk{textChunk("reply")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatalf("openWithClient: %v", err)
	}
	sessionID := a.SessionID()
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	metas, err := f.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, m := range metas {
		if m.SessionID == sessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("List did not include the open session %v: %+v", sessionID, metas)
	}
}

// hasType reports whether evs contains an event of the same concrete type as want.
func hasType(evs []event.Event, want event.Event) bool {
	wt := reflect.TypeOf(want)
	for _, e := range evs {
		if reflect.TypeOf(e) == wt {
			return true
		}
	}
	return false
}

func typeNames(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = reflect.TypeOf(e).String()
	}
	return out
}
