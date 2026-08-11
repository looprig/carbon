package presentation

import (
	"context"
	"reflect"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// DisplayProjection is the committed TUI projection of a fold over all delivered
// Enduring events — the "displayed" transcript the event-persistence design's headline
// property compares (displayed == stored == restored). It bundles the pure reducer states
// (transcript, pending-gate interaction, and compaction activity) so the
// restore-repaint path and the persistence property tests build the displayed view
// through one named seam. It is value-typed and immutable; FoldDisplay is its only
// constructor.
type DisplayProjection struct {
	transcript  transcriptModel
	interaction interactionModel
	compaction  compactionProjection
	runtime     runtimeProjection
	eventCount  int
	eventIDs    map[uuid.UUID]struct{}
}

// FoldDisplay folds events through the SAME pure reducers the live path and the
// cold-restore repaint use, starting
// from the zero reducer state scoped to loopID, and returns the resulting
// displayed projection. It is the single fold the TUI uses to turn a slice of Enduring
// events into a repaintable transcript: restoreBacklogCmd folds the restored backlog
// through it, and the persistence property tests fold both a restored ReplayBacklog and
// the original live Enduring sequence through it to assert the two displayed views are
// identical. The fold is order-sensitive and side-effect-free — folding the same events
// twice yields an EqualTranscript pair.
func FoldDisplay(events []event.Event) DisplayProjection {
	tr := transcriptModel{}
	in := newInteractionModel()
	compaction := compactionProjection{}
	runtime := newRuntimeProjection()
	ids := make(map[uuid.UUID]struct{})
	for _, ev := range events {
		tr = tr.ApplyEvent(ev)
		in = in.ApplyEvent(ev)
		compaction = compaction.ApplyEvent(ev)
		runtime = runtime.ApplyEvent(ev)
		if id := ev.EventHeader().EventID; !id.IsZero() {
			ids[id] = struct{}{}
		}
	}
	return DisplayProjection{transcript: tr, interaction: in, compaction: compaction, runtime: runtime, eventCount: len(events), eventIDs: ids}
}

// EqualTranscript reports whether p and other have the byte-for-byte identical committed
// transcript (the displayed scrollback), via reflect.DeepEqual over the transcript reducer
// state — IGNORING the live-only thinking DURATION. It is the headline-property comparator:
// a restored session's repainted transcript EqualTranscript the original session's live
// transcript iff the repaint reproduced the displayed view exactly. The interaction surface
// (its input editor carries cursor state and completion-panel closures that are not value-
// comparable) is intentionally NOT part of this equality — assert PendingPrompts for the
// pending-gate dimension instead.
//
// The thinking duration (entry.thinkDur and the live segment's streaming timestamps) is
// EXCLUDED from the comparison: it is measured from streaming TokenDelta timestamps, which
// are Ephemeral and NEVER journaled, so a cold-restore fold replays only the persisted
// StepDone events and legitimately produces dur == 0 (the restored row correctly shows
// "│ thought" with no number) while the same row folded live shows "│ thought for 10sec".
// That divergence is the ACCEPTED display behavior, not a repaint bug, so it is normalized
// out (normalizeThinkTiming) before DeepEqual; every OTHER field (the committed rows,
// ordering, blocks, tool cards, gate state) is compared exactly.
//
// This is a TEST / RESTORE-VERIFICATION comparator, NOT a cheap runtime equality check:
// normalizeThinkTiming allocates fresh copies of both models (committed slices + projection
// map/pointers) on every call, and reflect.DeepEqual walks the whole reducer state. Do NOT
// wire it into a render loop or a per-event hot path expecting it to be free.
func (p DisplayProjection) EqualTranscript(other DisplayProjection) bool {
	return reflect.DeepEqual(normalizeThinkTiming(p.transcript), normalizeThinkTiming(other.transcript))
}

// normalizeThinkTiming returns a copy of m with every LIVE-ONLY thinking timing datum
// zeroed — each committed entry's thinkDur in every per-loop projection and
// the live segment's streaming timestamps (root + projections) — so EqualTranscript can
// ignore the duration when comparing a live fold against a cold-restore fold (see
// EqualTranscript). It NEVER mutates m: the committed slices and the projection map/pointers
// are rebuilt fresh, so the input model's state is untouched. Only the duration is
// normalized; every other field is carried through unchanged for the DeepEqual.
func normalizeThinkTiming(m transcriptModel) transcriptModel {
	m.global = zeroThinkDur(m.global)
	m.fold = nil
	if m.projections != nil {
		next := make(map[uuid.UUID]*loopProjection, len(m.projections))
		for k, p := range m.projections {
			if p == nil {
				next[k] = nil
				continue
			}
			cp := *p
			cp.committed = zeroThinkDur(cp.committed)
			cp.live = zeroLiveThinkTiming(cp.live)
			next[k] = &cp
		}
		m.projections = next
	}
	return m
}

// zeroThinkDur returns a fresh copy of the committed entries with every entry's thinkDur
// zeroed (nil in → nil out, so the nil/empty distinction DeepEqual cares about is
// preserved). The input slice is never mutated.
func zeroThinkDur(in []entry) []entry {
	if in == nil {
		return nil
	}
	out := make([]entry, len(in))
	copy(out, in)
	for i := range out {
		out[i].thinkDur = 0
	}
	return out
}

// zeroLiveThinkTiming returns a copy of a live segment with its streaming thinking
// timestamps cleared. These are reset with the segment at every StepDone/terminal, so they
// are already zero on a well-formed fold; clearing them here makes the exclusion defensive
// against a comparison captured mid-thinking.
func zeroLiveThinkTiming(s liveSeg) liveSeg {
	s.thinkStart, s.thinkLast, s.thinkEnd = time.Time{}, time.Time{}, time.Time{}
	return s
}

// CommittedLen is the number of finalized transcript entries across the global stream
// and every loop projection.
func (p DisplayProjection) CommittedLen() int { return p.transcript.committedLen() }

// EventCount reports how many backlog events were folded. Unlike CommittedLen it remains
// non-zero for lifecycle-only history that rebuilds loop metadata without transcript rows.
func (p DisplayProjection) EventCount() int { return p.eventCount }

// PendingPrompts is the number of pending prompts (permission gates + AskUser requests)
// the projection's interaction surface holds — the gate dimension a transcript deep-
// equal does not cover.
func (p DisplayProjection) PendingPrompts() int { return p.interaction.PendingCount() }

// RestoreBacklogError reports a failure to read a restored session's historical
// Enduring backlog for repaint (the Agent.ReplayBacklog call failed). It is a
// NON-FATAL restore error: the live subscription is unaffected, so the Screen
// surfaces it as a faint error notice and continues with an empty transcript rather
// than a dead surface. It wraps the underlying replay cause so a caller can errors.As
// both this and the journal's typed read error.
type RestoreBacklogError struct {
	Cause error
}

func (e *RestoreBacklogError) Error() string {
	if e.Cause == nil {
		return "tui: restore backlog read failed"
	}
	return "tui: restore backlog read failed: " + e.Cause.Error()
}

func (e *RestoreBacklogError) Unwrap() error { return e.Cause }

// restoredMsg carries the result of the background restore fold (restoreBacklogCmd):
// the rebuilt transcript, interaction model, and compaction projection for a cold-restore
// repaint, OR a non-nil err when the backlog read failed. The reducers were applied
// PER-EVENT inside the command (off the update loop), so the Screen applies this state
// ONCE — it never folds per event on the loop. A new (non-restored) session yields an
// empty transcript here, which the Screen installs as a no-op (no repaint). It is a
// tea.Msg.
type restoredMsg struct {
	transcript  transcriptModel
	interaction interactionModel
	compaction  compactionProjection
	runtime     runtimeProjection
	eventCount  int
	eventIDs    map[uuid.UUID]struct{}
	err         error
}

// restoreBacklogCmd is the background-fold command (Task 10.1): OFF the Bubble Tea
// update loop, it reads the restored session's historical Enduring backlog via
// Agent.ReplayBacklog, then folds EVERY event through the SAME pure reducers the live
// path uses to build the FINAL reducer
// state, and returns a SINGLE restoredMsg. This is the no-UI-hang property: a large
// backlog is folded once here, not delivered as N per-event messages through the live
// Subscribe 256-buffer and not folded per event on the update loop. A read failure
// returns a restoredMsg carrying a typed *RestoreBacklogError (non-fatal). A new session
// (empty backlog) folds to an empty transcript, which the Screen installs as a no-op.
func restoreBacklogCmd(ctx context.Context, agent Agent) tea.Cmd {
	return func() tea.Msg {
		backlog, err := agent.ReplayBacklog(ctx)
		if err != nil {
			return restoredMsg{err: &RestoreBacklogError{Cause: err}}
		}
		proj := FoldDisplay(backlog)
		return restoredMsg{transcript: proj.transcript, interaction: proj.interaction, compaction: proj.compaction, runtime: proj.runtime, eventCount: proj.eventCount, eventIDs: proj.eventIDs}
	}
}

// compile-time guard: a restoredMsg is a tea.Msg (any value satisfies tea.Msg, but the
// assignment documents intent and fails loudly if the alias ever narrows).
var _ tea.Msg = restoredMsg{}

// compile-time guard: *RestoreBacklogError is an error.
var _ error = (*RestoreBacklogError)(nil)
