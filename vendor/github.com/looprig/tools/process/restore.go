package process

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// RestoreError reports one persisted process resource that Restore could not
// reconcile -- most commonly a manifest that fails to load (manifest.go's
// CodeManifestCorrupt) or whose disk spool fails to reopen
// (CodeSpoolCorrupt). Restore isolates and reports every such failure rather
// than aborting its whole scan, so one corrupt entry never hides every other
// healthy one (spec combined-acceptance: "corrupt entries are isolated and
// reported without hiding healthy entries").
type RestoreError struct {
	Handle Handle
	Err    error
}

func (e *RestoreError) Error() string {
	return fmt.Sprintf("process: restore %s: %v", e.Handle, e.Err)
}

// Unwrap returns the underlying reconciliation failure, so errors.As/errors.Is
// can see through a RestoreError to a wrapped *Error (e.g. CodeManifestCorrupt).
func (e *RestoreError) Unwrap() error { return e.Err }

// RestoreReport is Restore's summary of one scan of the resource root: every
// Handle it successfully reconciled and registered into s.entries, and every
// Handle it could not (see RestoreError).
type RestoreReport struct {
	Reconciled []Handle
	Errors     []RestoreError
}

// Restore is the session-restore entrypoint (spec "Manifests and
// durability": "On normal session restore ..."). It scans every manifest
// durably persisted beneath the Supervisor's ManifestStore root and
// reconciles each one into a queryable, non-running s.entries record:
//
//   - a manifest already in a terminal State (state.go's State.Terminal())
//     is reopened as-is: its completed output remains readable through its
//     Spool, with no live tool.Process, goroutine, lease, or quota
//     reservation (TestRestoreCompletedOutput);
//   - a manifest still in StateStarting or StateRunning means the supervisor
//     process that was running it no longer exists -- a session restore
//     implies exactly that (spec: "a manifest marked running or starting
//     becomes lost_on_restore"). It is durably transitioned to the terminal
//     StateLostOnRestore first (markLostOnRestore), publishing the lost
//     lifecycle event and completion notification through the manifest's
//     already-persisted stable LifecycleEventIDs.Lost/CommandID -- never a
//     freshly minted ID (TestRestoreRunningBecomesLost,
//     TestRestorePublicationCrashRetriesStableID) -- and only then reopened
//     the same way as an already-terminal manifest.
//
// Restore is intended to be called once, immediately after NewSupervisor and
// before any Start/Wait call, as the caller's session-restore step; it is not
// a periodic reconciliation loop, and it never touches quota bookkeeping
// (runningByLoop/runningBySession/reservedMemoryBytes/reservedSpoolBytes):
// nothing here was ever actually reserved by this process instance, and
// nothing will ever call releaseQuota for a restored entry.
//
// Restore never constructs, obtains, or references a tool.Process,
// tool.PreparedProcess, or tool.AsyncProcessRunner: there is no live process
// handle for a restored entry, by construction, so there is no signal-capable
// value anywhere in this reconciliation path that could ever be handed a
// persisted PID (spec: "Restore never signals a PID recovered from persisted
// metadata"; TestRestoreNeverSignalsPersistedPID). The manifest's unexported
// osMetadata field is never read here at all -- see manifest.go's osMetadata
// doc comment.
//
// A manifest that fails to load, or whose disk spool fails to reopen, does
// not abort the scan: it is recorded in the returned RestoreReport.Errors and
// every other manifest is still reconciled normally.
func (s *Supervisor) Restore(ctx context.Context) (RestoreReport, error) {
	handles, err := s.listManifestHandles()
	if err != nil {
		return RestoreReport{}, err
	}

	var report RestoreReport
	for _, h := range handles {
		e, err := s.reconcileManifest(ctx, h)
		if err != nil {
			report.Errors = append(report.Errors, RestoreError{Handle: h, Err: err})
			continue
		}

		s.mu.Lock()
		s.entries[h] = e
		s.mu.Unlock()

		report.Reconciled = append(report.Reconciled, h)
	}
	return report, nil
}

// listManifestHandles scans the Supervisor's ManifestStore root directory
// for every persisted manifest file and returns the Handle each one is keyed
// by. This is Restore's own directory scan: manifest.go's ManifestStore
// exposes no listing method of its own, and this microtask does not add one
// (it is scoped to entry.go/supervisor.go only), so restore.go reads
// s.manifests.root directly -- same-package field access, exactly like
// supervisor_test.go's loadOnlyManifest test helper already does for a
// single-manifest scan.
func (s *Supervisor) listManifestHandles() ([]Handle, error) {
	dirEntries, err := os.ReadDir(s.manifests.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var handles []Handle
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, manifestSuffix) {
			continue
		}
		handles = append(handles, Handle(strings.TrimSuffix(name, manifestSuffix)))
	}
	return handles, nil
}

// reconcileManifest loads h's manifest, transitions it to StateLostOnRestore
// first if it is not already terminal, and returns the queryable,
// non-running *entry Restore registers for it.
func (s *Supervisor) reconcileManifest(ctx context.Context, h Handle) (*entry, error) {
	m, err := s.manifests.Load(h)
	if err != nil {
		return nil, err
	}

	if !m.State.Terminal() {
		m, err = s.markLostOnRestore(ctx, m)
		if err != nil {
			return nil, err
		}
	}

	return s.reopenEntry(m)
}

// markLostOnRestore durably transitions m -- a manifest still in
// StateStarting or StateRunning at restore time -- to the terminal
// StateLostOnRestore. It mirrors entry.doTerminalize's terminal-write
// pattern (set FinishedAt/Result, bump CompletionPublished, Save) but needs
// no terminalOnce guard of its own: restore reconciliation runs
// synchronously, once per handle, over a manifest with no concurrent live
// writer (there is no entry/tool.Process racing this Save the way a live
// process's own natural-exit and stop/timeout/shutdown paths could race
// entry.terminalize). It then publishes the lost lifecycle event and
// completion notification via publishLostOnRestore, reusing exactly the
// manifest's already-persisted LifecycleEventIDs.Lost/CommandID.
func (s *Supervisor) markLostOnRestore(ctx context.Context, m Manifest) (Manifest, error) {
	finishedAt := time.Now().UTC()
	m.State = StateLostOnRestore
	m.FinishedAt = &finishedAt
	m.Result = Result{Reason: reasonString(tool.ProcessTerminalLostOnRestore)}
	m.CompletionPublished++

	if err := s.manifests.Save(m); err != nil {
		return Manifest{}, err
	}

	s.publishLostOnRestore(ctx, m)
	return m, nil
}

// publishLostOnRestore publishes the lost lifecycle event and completion
// notification for m -- a manifest already durably persisted in
// StateLostOnRestore -- through the Supervisor's own session-scoped
// lifecycle/notifications capabilities (supervisor.go's fields; nil-tolerant,
// exactly like entry.doTerminalize's identical dependencies). EventID and
// CommandID always come from m.Events, m's already-persisted stable
// LifecycleEventIDs -- restore never mints a fresh one anywhere -- so calling
// this more than once for the same manifest (e.g. an original publish
// attempt and a post-crash retry) always reuses the exact same IDs
// (TestRestorePublicationCrashRetriesStableID; spec "lifecycle and
// notification publication reuses the stable persisted IDs"). The durable
// Harness journal index (Task 24) is the actual duplicate-suppression
// boundary; this call's only job is to keep resending the same IDs.
func (s *Supervisor) publishLostOnRestore(ctx context.Context, m Manifest) {
	lifecycle, notifications := s.servicesLocked()

	if lifecycle != nil {
		var startedAt time.Time
		if m.StartedAt != nil {
			startedAt = *m.StartedAt
		}
		_ = lifecycle.publish(ctx, lifecycleTerminalEvent{
			EventID:    m.Events.Lost,
			Kind:       tool.ProcessLifecycleLost,
			Identity:   m.Identity,
			State:      m.State,
			Result:     m.Result,
			CreatedAt:  m.CreatedAt,
			StartedAt:  startedAt,
			FinishedAt: *m.FinishedAt,
		})
	}

	if notifications != nil {
		_ = notifications.notify(ctx, completionEvent{
			CommandID: m.Events.CommandID,
			Owner:     m.Owner,
			Handle:    m.Handle,
			State:     m.State,
			Result:    m.Result,
		})
	}
}

// reopenEntry builds the queryable, non-running *entry Restore registers for
// an already-terminal manifest m: no live tool.Process, no goroutine, no
// lease, and no quota reservation -- just enough state (Identity, a
// manifestSaver reference so a future read can reload the latest manifest,
// and a reopened Spool) for a future ProcessOutput-style read (Task 16) to
// still read this process's completed output and terminal result. done is
// pre-closed: the process is already in a terminal state, by construction,
// the instant it is restored, so every caller that treats a closed done
// channel as "terminal" (wait.go's snapshotTargets) already sees this entry
// correctly without any generation/wake plumbing ever firing for it.
func (s *Supervisor) reopenEntry(m Manifest) (*entry, error) {
	spool, err := OpenSpool(s.spoolRoot, m.Handle, s.cfg.MaxProcessSpoolBytes)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	close(done)

	return &entry{
		identity:  m.Identity,
		manifests: s.manifests,
		buffer:    NewBuffer(s.cfg.MaxProcessInMemoryBytes),
		spool:     spool,
		done:      done,
		// exited is pre-closed too, for the same reason done is: this entry
		// is already terminal, by construction, the instant it is restored.
		// Reusing done itself (rather than allocating a second, separately
		// closed channel) is fine -- both are already closed and neither is
		// ever closed again -- and keeps this entry consistent with the
		// invariant every Supervisor.Start-registered entry also holds:
		// exited closes at or before done. Supervisor.Shutdown never
		// actually reaches this field for a restored entry (its own
		// snapshotRunningEntries step filters out every already-terminal
		// entry via done before exited is ever touched); this is purely for
		// hygiene, not a load-bearing requirement.
		exited: done,
	}, nil
}
