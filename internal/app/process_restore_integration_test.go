//go:build integration

package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
)

// process_restore_integration_test.go covers the two Task 28 scenarios that
// need a close-and-reopen across a REAL fsstore-backed SessionStoreFactory:
// scenario 12 (restore completed output) and scenario 14, whose headline
// test must be named exactly TestIntegrationProcessJournalIdempotencyReopen.
// Both reuse process_integration_test.go's fixture helpers
// (processIntegrationAgent, runProcessScript, bashCall/processOutputCall,
// decodeBashResult/decodeProcessResult) and persistence_integration_test.go's
// newIntegrationFactory/drainEventReplay/acceptanceEventsUntil.

// --- Scenario 12: restore completed output -------------------------------

// TestIntegrationProcessRestoreReadsCompletedOutput proves a background
// process's output and terminal metadata survive a real close/reopen: a
// fresh ProcessOutput call against the SAME process handle, issued from the
// RESTORED session, reads the exact output and exit status the original
// session already observed.
func TestIntegrationProcessRestoreReadsCompletedOutput(t *testing.T) {
	pia := openProcessIntegrationAgent(t)
	sessionID := pia.agent.SessionID()

	stream, err := pia.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	var processID string
	final, _ := runProcessScript(t, pia, "start a command to restore later",
		func(string) []content.Chunk {
			return bashCall("restore-output-start", `{"command": "echo restored-output-marker", "background": true}`)
		},
		func(prior string) []content.Chunk {
			started := decodeBashResult(t, prior)
			processID = started.ProcessID
			return []content.Chunk{&content.TextChunk{Text: "restore-output-started"}}
		},
	)
	if final != "restore-output-started" {
		t.Fatalf("final = %q", final)
	}
	if processID == "" {
		t.Fatal("no process_id captured from the background start")
	}

	// Drain until the process's terminal lifecycle event is durable, so the
	// completed output is guaranteed persisted before Close.
	acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		completed, ok := ev.(event.ProcessCompleted)
		return ok && completed.Process.ProcessHandle == processID
	})
	if err := stream.Close(); err != nil {
		t.Fatalf("subscription Close() error = %v", err)
	}
	if err := pia.agent.Close(context.Background()); err != nil {
		t.Fatalf("original Close() error = %v", err)
	}

	restoredClient := &managedScript{}
	cfg := Config{AccessProfile: AccessUnconfined, HomeDir: t.TempDir()}
	restored, err := pia.factory.openWithClient(context.Background(), restoredClient, newModelFactoryFor(testModel()), SessionSelector{Resume: sessionID}, cfg)
	if err != nil {
		t.Fatalf("openWithClient(resume) error = %v", err)
	}
	defer func() { _ = restored.Close(context.Background()) }()

	restoredPia := &processIntegrationAgent{factory: pia.factory, agent: restored, client: restoredClient, workspace: pia.workspace}
	var readAfterRestore processResultJSON
	restoreFinal, _ := runProcessScript(t, restoredPia, "read completed output after restore",
		func(string) []content.Chunk {
			return processOutputCall("restore-output-read", fmt.Sprintf(`{"process_id": %q, "cursor": 0, "wait": "poll"}`, processID))
		},
		func(prior string) []content.Chunk {
			readAfterRestore = decodeProcessResult(t, prior)
			return []content.Chunk{&content.TextChunk{Text: "restore-output-read-ok"}}
		},
	)
	if restoreFinal != "restore-output-read-ok" {
		t.Fatalf("restore final = %q", restoreFinal)
	}
	if readAfterRestore.Error != "" {
		t.Fatalf("restored ProcessOutput error = %q, want none (completed output must survive restore)", readAfterRestore.Error)
	}
	if readAfterRestore.Status != "exited" || readAfterRestore.ExitCode == nil || *readAfterRestore.ExitCode != 0 {
		t.Fatalf("restored ProcessOutput result = %+v, want terminal exited/0", readAfterRestore)
	}
	if got, want := readAfterRestore.Output, "restored-output-marker"; !strings.Contains(got, want) {
		t.Errorf("restored output = %q, want it to contain %q", got, want)
	}
}

// --- Scenario 14: journal idempotency across a real reopen ---------------

// commandRecordProcessNotifications drains every command.ProcessNotification
// record in sessionID's durable ledger via the SAME privileged full-stream
// seam Harness's own restore-time undelivered-notification fold uses
// (OpenInternalRecordReplayer), so a caller can prove "exactly one durable
// frame" directly against the ledger, independent of any in-memory replay
// filtering.
func commandRecordProcessNotifications(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []command.ProcessNotification {
	t.Helper()
	replayer, err := store.OpenInternalRecordReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("RecordReplayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()

	var notifications []command.ProcessNotification
	for {
		rec, _, nextErr := cursor.Next(context.Background())
		if nextErr != nil {
			break
		}
		commandRec, ok := rec.(journal.CommandRecord)
		if !ok {
			continue
		}
		if pn, ok := commandRec.Command().(command.ProcessNotification); ok {
			notifications = append(notifications, pn)
		}
	}
	return notifications
}

// processCompletedEvents drains sessionID's durable ledger for every public
// event.ProcessCompleted record, via the SAME privileged replayer restore
// uses (OpenInternalEventReplayer, persistence_integration_test.go's
// drainEventReplay).
func processCompletedEvents(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []event.ProcessCompleted {
	t.Helper()
	replayer, err := store.OpenInternalEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenInternalEventReplayer() error = %v", err)
	}
	events := drainEventReplay(t, replayer)
	var completed []event.ProcessCompleted
	for _, ev := range events {
		if c, ok := ev.(event.ProcessCompleted); ok {
			completed = append(completed, c)
		}
	}
	return completed
}

func countTurnDone(events []event.Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(event.TurnDone); ok {
			n++
		}
	}
	return n
}

// TestIntegrationProcessJournalIdempotencyReopen closes and reopens a real
// fsstore-backed SessionStore around a background process whose completion
// was already fully durable and delivered BEFORE close. Harness's own
// restore-time undelivered-notification fold (internal/sessionruntime's
// restoredStateFrom/undeliveredProcessNotifications) unconditionally
// re-seeds every process notification it finds in the durable ledger into
// the freshly restored loop's live dedupe guard, regardless of whether it
// was already fully consumed pre-close -- that re-seed is itself the
// "retry" this test's name refers to. This test proves that retry is safe
// by construction: it is a pure in-memory seed
// (internal/loopruntime/loop.go's RestoredState.PendingProcessNotifications
// -> state.processNotifications), never a re-append through
// notifyProcessCompletion and never a new model turn on its own -- so the
// persisted lifecycle EventID and notification CommandID each still name
// EXACTLY ONE durable frame after reopening, and no duplicate model turn is
// ever produced.
func TestIntegrationProcessJournalIdempotencyReopen(t *testing.T) {
	pia := openProcessIntegrationAgent(t)
	sessionID := pia.agent.SessionID()
	store := pia.factory.stores.session

	stream, err := pia.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	var processID string
	final, turnEvents := runProcessScript(t, pia, "start a command whose notification must survive reopen",
		func(string) []content.Chunk {
			return bashCall("idempotency-start", `{"command": "echo idempotency-marker", "background": true}`)
		},
		func(prior string) []content.Chunk {
			processID = decodeBashResult(t, prior).ProcessID
			return []content.Chunk{&content.TextChunk{Text: "idempotency-started"}}
		},
	)
	if final != "idempotency-started" {
		t.Fatalf("final = %q", final)
	}
	if processID == "" {
		t.Fatal("no process_id captured from the background start")
	}

	// turnEvents is runManagedTurnObserved's own accumulated event list from
	// its OWN subscription, driving the submit -- the correct source for
	// "did TurnDone happen during this turn" (this session was never
	// restored, so ReplayBacklog, a "cold repaint" seam -- see
	// persistence_integration_test.go's TestSessionStoreNewSessionBasics --
	// would return only the initial construction backlog, not this turn's
	// own history). This file's OWN separate `stream` subscription is used
	// only to wait for the process's later, asynchronous terminal lifecycle
	// event below -- entry.doTerminalize (tools/process/entry.go) publishes
	// the lifecycle event, then notifies completion, at the SAME one-shot
	// terminal transition, so observing the public ProcessCompleted event
	// here is sufficient proof the notification command was durably
	// appended too.
	turnDoneBeforeClose := countTurnDone(turnEvents)
	if turnDoneBeforeClose == 0 {
		t.Fatal("no TurnDone observed before close; the background start's own turn never completed")
	}

	acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		completed, ok := ev.(event.ProcessCompleted)
		return ok && completed.Process.ProcessHandle == processID
	})
	if err := stream.Close(); err != nil {
		t.Fatalf("subscription Close() error = %v", err)
	}

	completedBeforeClose := processCompletedEvents(t, store, sessionID)
	if len(completedBeforeClose) != 1 {
		t.Fatalf("durable event.ProcessCompleted records before close = %d, want exactly 1", len(completedBeforeClose))
	}
	originalEventID := completedBeforeClose[0].EventHeader().EventID
	if originalEventID.IsZero() {
		t.Fatal("durable ProcessCompleted EventID is zero")
	}

	notificationsBeforeClose := commandRecordProcessNotifications(t, store, sessionID)
	if len(notificationsBeforeClose) != 1 {
		t.Fatalf("durable command.ProcessNotification records before close = %d, want exactly 1", len(notificationsBeforeClose))
	}
	originalCommandID := notificationsBeforeClose[0].Header.CommandID
	if originalCommandID.IsZero() {
		t.Fatal("durable ProcessNotification CommandID is zero")
	}
	if notificationsBeforeClose[0].Notification.CommandID != originalCommandID {
		t.Fatalf("ProcessNotification.Header.CommandID = %s, Notification.CommandID = %s, want equal", originalCommandID, notificationsBeforeClose[0].Notification.CommandID)
	}

	if err := pia.agent.Close(context.Background()); err != nil {
		t.Fatalf("original Close() error = %v", err)
	}

	restoredClient := &managedScript{}
	cfg := Config{AccessProfile: AccessUnconfined, HomeDir: t.TempDir()}
	restored, err := pia.factory.openWithClient(context.Background(), restoredClient, newModelFactoryFor(testModel()), SessionSelector{Resume: sessionID}, cfg)
	if err != nil {
		t.Fatalf("openWithClient(resume) error = %v", err)
	}
	defer func() { _ = restored.Close(context.Background()) }()

	// Immediately after reopening -- BEFORE submitting anything new -- the
	// restore-time notification re-seed must not, by itself, have produced
	// any new durable frame or any new model turn.
	afterReopenBacklog, err := restored.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("ReplayBacklog() (after reopen) error = %v", err)
	}
	if got := countTurnDone(afterReopenBacklog); got != turnDoneBeforeClose {
		t.Fatalf("TurnDone count immediately after reopen = %d, want unchanged from pre-close %d (the restore-time notification re-seed must not itself produce a turn)", got, turnDoneBeforeClose)
	}

	completedAfterReopen := processCompletedEvents(t, store, sessionID)
	if len(completedAfterReopen) != 1 {
		t.Fatalf("durable event.ProcessCompleted records after reopen = %d, want exactly 1 (no duplicate frame from the restore-time retry)", len(completedAfterReopen))
	}
	if completedAfterReopen[0].EventHeader().EventID != originalEventID {
		t.Fatalf("ProcessCompleted EventID after reopen = %s, want unchanged %s (the persisted lifecycle EventID must be reused, not re-minted)", completedAfterReopen[0].EventHeader().EventID, originalEventID)
	}

	notificationsAfterReopen := commandRecordProcessNotifications(t, store, sessionID)
	if len(notificationsAfterReopen) != 1 {
		t.Fatalf("durable command.ProcessNotification records after reopen = %d, want exactly 1 (no duplicate frame from the restore-time retry)", len(notificationsAfterReopen))
	}
	if notificationsAfterReopen[0].Header.CommandID != originalCommandID {
		t.Fatalf("ProcessNotification CommandID after reopen = %s, want unchanged %s (the persisted notification CommandID must be reused, not re-minted)", notificationsAfterReopen[0].Header.CommandID, originalCommandID)
	}

	// A genuinely new, ordinary follow-up turn on the restored session must
	// still work normally: no doubled dispatch from the notification
	// redelivery that happened at restore.
	restoredPia := &processIntegrationAgent{factory: pia.factory, agent: restored, client: restoredClient, workspace: pia.workspace}
	restoreFinal, _ := runProcessScript(t, restoredPia, "continue after reopen",
		func(string) []content.Chunk {
			return []content.Chunk{&content.TextChunk{Text: "continued-after-reopen"}}
		},
	)
	if restoreFinal != "continued-after-reopen" {
		t.Fatalf("restore final = %q", restoreFinal)
	}

	completedAfterFollowUp := processCompletedEvents(t, store, sessionID)
	if len(completedAfterFollowUp) != 1 {
		t.Fatalf("durable event.ProcessCompleted records after a follow-up turn = %d, want still exactly 1", len(completedAfterFollowUp))
	}
	notificationsAfterFollowUp := commandRecordProcessNotifications(t, store, sessionID)
	if len(notificationsAfterFollowUp) != 1 {
		t.Fatalf("durable command.ProcessNotification records after a follow-up turn = %d, want still exactly 1", len(notificationsAfterFollowUp))
	}
}
