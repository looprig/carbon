package sessionruntime

import (
	"context"
	"errors"
	"io"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
)

type restoredGatePlan struct {
	open        map[gate.ID]gateEntry
	unavailable []event.GateOpened
}

type restoredPreparedGate struct {
	prepared event.GatePrepared
	payload  gate.Payload
}

// drainRecordReplay opens a cold record cursor for req and reads it to io.EOF,
// preserving ledger order. Restore uses record replay, not event replay, because
// the private journal.GatePreparedRecord is intentionally invisible to event
// consumers but is required to recover gate state fail-securely.
func drainRecordReplay(ctx context.Context, replayer journal.RecordReplayer, req journal.ReplayRequest) ([]journal.JournalRecord, error) {
	cursor, err := replayer.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close() }()

	var out []journal.JournalRecord
	for {
		rec, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
}

func eventsFromRecords(records []journal.JournalRecord, loopID uuid.UUID) []event.Event {
	out := make([]event.Event, 0, len(records))
	for _, rec := range records {
		evRec, ok := rec.(journal.EventRecord)
		if !ok {
			continue
		}
		ev := evRec.Event()
		if deliverRestoredEvent(ev, loopID) {
			out = append(out, ev)
		}
	}
	return out
}

func deliverRestoredEvent(ev event.Event, loopID uuid.UUID) bool {
	if loopID.IsZero() {
		return true
	}
	if ev.Scope() == event.ScopeSession {
		return true
	}
	return ev.EventHeader().LoopID == loopID
}

func foldRestoredGates(records []journal.JournalRecord) restoredGatePlan {
	prepared := make(map[gate.ID]restoredPreparedGate)
	opened := make(map[gate.ID]event.GateOpened)
	closed := make(map[gate.ID]bool)
	openOrder := make([]gate.ID, 0)

	for _, rec := range records {
		switch r := rec.(type) {
		case journal.GatePreparedRecord:
			preparedEvent := r.Prepared()
			id := preparedEvent.Gate.ID
			if id == (gate.ID{}) {
				id = r.Payload().GateID
			}
			if id == (gate.ID{}) {
				continue
			}
			prepared[id] = restoredPreparedGate{prepared: preparedEvent, payload: r.Payload().Payload}
		case journal.EventRecord:
			switch ev := r.Event().(type) {
			case event.GateOpened:
				id := ev.Gate.ID
				if id == (gate.ID{}) || closed[id] {
					continue
				}
				if _, seen := opened[id]; !seen {
					openOrder = append(openOrder, id)
				}
				opened[id] = ev
			case event.GateResolved:
				closed[ev.GateID] = true
				delete(opened, ev.GateID)
			}
		}
	}

	plan := restoredGatePlan{open: make(map[gate.ID]gateEntry)}
	for _, id := range openOrder {
		openedEvent, ok := opened[id]
		if !ok || closed[id] {
			continue
		}
		preparedGate, ok := prepared[id]
		if !ok || !openedEvent.Gate.Restorable || !gateRestoreHookSupported(openedEvent.Gate) {
			plan.unavailable = append(plan.unavailable, openedEvent)
			continue
		}
		coords := preparedGate.prepared.EventHeader().Coordinates
		if coords == (identity.Coordinates{}) {
			coords = openedEvent.EventHeader().Coordinates
		}
		plan.open[id] = gateEntry{
			gate:        openedEvent.Gate,
			route:       gate.Route{GateID: id, LoopID: gate.ID(openedEvent.EventHeader().LoopID), ToolExecutionID: openedEvent.Gate.Subject.ToolExecutionID},
			payload:     preparedGate.payload,
			coordinates: coords,
			state:       gateOpen,
		}
	}
	return plan
}

// gateRestoreHookSupported reports whether restore may reinstall an opened,
// unresolved gate as a live, answerable directory entry rather than closing
// it CloseRestoreUnavailable. KindPermission is supported (design §15: "the
// permission gate restores normally... the gate remains answerable by a
// human"): foldRestoredGates never restores reviewBasis (gateEntry's zero
// value) or any cancellation handle (reviewLifecycle always starts zero on a
// fresh Session — see session.go's review field), so a restored permission
// gate is structurally incapable of receiving a classifier-originated
// response until a NEW review starts, and design §15 additionally requires
// that no new review is ever started from restored/guessed context — restore
// never calls StartPermissionReview. Every other kind remains unsupported
// pending its own restore design.
func gateRestoreHookSupported(g gate.Gate) bool {
	switch g.Kind {
	case gate.KindPermission:
		return true
	case gate.KindAskUser:
		return false
	default:
		return false
	}
}

func cloneGateEntries(in map[gate.ID]gateEntry) map[gate.ID]gateEntry {
	out := make(map[gate.ID]gateEntry, len(in))
	for id, entry := range in {
		out[id] = entry
	}
	return out
}

func appendRestoreUnavailableGates(ctx context.Context, j journal.SessionJournal, factory *event.Factory, opened []event.GateOpened) error {
	for _, open := range opened {
		stamped, err := factory.Stamp(event.Header{Coordinates: open.EventHeader().Coordinates})
		if err != nil {
			return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
		}
		resolved := event.GateResolved{
			Header:   stamped,
			GateID:   open.Gate.ID,
			Resolver: open.Gate.Resolver,
			Reason:   gate.CloseRestoreUnavailable,
		}
		if _, err := j.Append(ctx, journal.NewEventRecord(resolved)); err != nil {
			return err
		}
	}
	return nil
}
