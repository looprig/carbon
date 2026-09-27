import { Fragment } from "react";
import {
  GATE_APPROVAL_ACTIONS,
  eventPrincipal,
  principalLabel,
  livePreviewKey,
  placeLivePreviews,
  toolResultCaptures,
  type FactoryReads,
  type FactorySessionStatus,
  type GateApprovalAction,
  type PublicGateEntry,
  type ToolResultCaptureSummary,
} from "@looprig/client";
import type { UseFactorySessionViewResult } from "@looprig/react";
import { ToolCaptureViewer } from "../components/transcript/tool-capture-viewer";
import { Composer } from "../components/composer";
import { causeCommandId, type SentInput } from "../lib/own-commands";

const TERMINAL_STATES = new Set(["failed", "interrupted", "stopped", "completed", "cancelled"]);

export function durableStateLabel(status: FactorySessionStatus): string {
  if (TERMINAL_STATES.has(status.state)) return status.state;
  if (status.state === "waiting_on_gate") return "waiting on gate";
  if (status.residency === "attaching" || status.residency === "placing") return "placing";
  return status.residency;
}

/**
 * One open gate, as the board holds it plus whether it can be answered from
 * here. `useFactoryGate`'s `FactoryOpenGate` satisfies this structurally, which
 * is the point: the cards a human sees and the gates the respond path can act
 * on are ONE list, so a card cannot offer a button for a gate the action side
 * cannot find.
 */
export interface FactoryDetailGate extends PublicGateEntry {
  readonly answerable: boolean;
}

/**
 * What a gate card says when it offers no button. Answerability is Factory's
 * attestation, re-read while it is transient, so these describe a state that
 * may change on its own — above all `unavailable`, which is what a gate reads
 * while its session has no live owner (a Host failover or release). An open
 * gate survives that (harness >= v0.39.0), so it is shown as waiting, not gone.
 */
export function gateStateLabel(answerability: string): string {
  switch (answerability) {
    case "unavailable": return "Waiting for the session to be resident again — this gate stays open";
    case "suspended": return "Suspended — not answerable right now";
    case "submitted": return "Answer submitted — waiting for the agent";
    case "expired": return "Expired";
    case "": return "Not yet attested — checking whether it can be answered";
    default: return "No resident action is available";
  }
}

/** The write half of the detail page. Optional so a read-only embedding stays read-only. */
export interface FactoryDetailComposer {
  /** Resolves `true` when the input was admitted (the draft may be cleared). */
  onSubmit: (text: string) => Promise<boolean>;
  submitting: boolean;
  error: Error | null;
  /** Inputs this tab sent that no journal event names as its cause yet. */
  awaiting: readonly SentInput[];
  /** Every command id this tab admitted, to mark the events each one caused. */
  own: ReadonlySet<string>;
  /** An input whose delivery is unconfirmed (reply lost); retry replays the same command. */
  unconfirmed: string | null;
  onRetry: () => void;
  onDiscard: () => void;
}

export interface FactorySessionDetailPageProps {
  sid: string;
  view: UseFactorySessionViewResult;
  reads: FactoryReads;
  /**
   * The FOLDED board, not `view.gates`. The raw page is one bounded window of
   * whatever the last `listGates` returned; it does not lose a gate that
   * resolved live, and it re-lists one that resolved while the read was in
   * flight. See `factory-gate-board.ts`.
   */
  gates: readonly FactoryDetailGate[];
  onGateRespond?: (gateId: string, action: GateApprovalAction) => void;
  composer?: FactoryDetailComposer;
}

interface PublicCapture {
  index: number;
  toolUseId: string;
  capture: ToolResultCaptureSummary;
}

function publicCaptures(body: unknown): PublicCapture[] {
  if (typeof body !== "object" || body === null || Array.isArray(body)) return [];
  const record = body as Record<string, unknown>;
  if (record["type"] !== "StepDone") return [];
  const raw = record["captures"];
  if (!Array.isArray(raw)) return [];
  const captures: PublicCapture[] = [];
  raw.forEach((candidate, index) => {
    for (const [toolUseId, capture] of toolResultCaptures([candidate])) {
      captures.push({ index, toolUseId, capture });
    }
  });
  return captures;
}

function previewAnchor(body: unknown): { loopId: string; turnId: string } {
  if (typeof body !== "object" || body === null) return { loopId: "", turnId: "" };
  const record = body as Record<string, unknown>;
  return {
    loopId: typeof record["loop_id"] === "string" ? record["loop_id"] : "",
    turnId: typeof record["turn_id"] === "string" ? record["turn_id"] : "",
  };
}

function LivePreview({ preview }: { preview: { kind: "text" | "reasoning"; loopId: string; turnId: string; text: string } }): React.JSX.Element {
  return <article
    data-testid={preview.kind === "text" ? "factory-live-text" : "factory-live-reasoning"}
    data-preview-key={livePreviewKey(preview)}
    className="mb-2 rounded-md border border-border bg-card p-3"
  >
    <span className="font-mono text-xs text-muted">{preview.kind === "text" ? "Writing…" : "Thinking…"}</span>
    <p className="mt-1 whitespace-pre-wrap text-sm">{preview.text}</p>
  </article>;
}

/** Durable session projection. Realtime health is metadata, never a render prerequisite. */
export function FactorySessionDetailPage({ sid, view, reads, gates, onGateRespond, composer }: FactorySessionDetailPageProps): React.JSX.Element {
  if (view.status === null && view.state !== "failed") {
    return <main className="p-6"><p role="status">Loading session…</p></main>;
  }
  if (view.status === null) {
    return (
      <main className="p-6">
        <div role="alert" data-testid="detail-read-error" className="rounded-md border border-fail/50 bg-fail/10 p-4 text-fail">
          {view.error?.message ?? "Session unavailable"}
        </div>
      </main>
    );
  }

  const previews = placeLivePreviews(view.events.map((event) => previewAnchor(event.body)), 0, view.events.length, view.liveText, view.liveReasoning);

  return (
    <main className="flex h-dvh flex-col">
      <header className="flex items-center gap-3 border-b border-border px-4 py-3">
        <span data-testid="detail-durable-state" className="font-mono text-xs">{durableStateLabel(view.status)}</span>
        <span data-testid="detail-session-id" className="font-mono text-xs text-muted">{sid}</span>
        <span data-testid="detail-live-state" className="ml-auto font-mono text-xs text-muted">{view.liveState}</span>
      </header>
      {view.error === null ? null : (
        <p role="status" data-testid="detail-transport-warning" className="m-3 rounded-md border border-border p-2 text-xs">
          {view.error.message}
        </p>
      )}
      <div role="log" aria-live="polite" className="min-h-0 flex-1 overflow-y-auto p-4">
        {view.events.length === 0 && previews.unplaced.length === 0 ? <p className="text-sm text-muted">Nothing here yet</p> : view.events.map((event, index) => {
          const captures = publicCaptures(event.body);
          const cause = causeCommandId(event.body);
          const own = cause !== "" && composer?.own.has(cause) === true;
          const from = eventPrincipal(event.body);
          return (<Fragment key={event.event_id}>
            <article
              data-testid={`factory-event-${event.journal_seq}`}
              data-own-command={own ? cause : undefined}
              className="mb-2 rounded-md border border-border bg-card p-3"
            >
              <span className="font-mono text-xs text-muted">#{event.journal_seq}</span>
              {own ? <span data-testid="factory-event-own" className="ml-2 font-mono text-xs text-loop">you</span> : null}
              {from === undefined ? null : (
                <span data-testid="factory-event-from" className="ml-2 font-mono text-xs text-muted">from {principalLabel(from)}</span>
              )}
              <pre className="mt-1 overflow-auto whitespace-pre-wrap font-mono text-xs">{JSON.stringify(event.body, null, 2)}</pre>
              {captures.map((entry) => (
                <div key={`${event.event_id}:${entry.index}`} data-capture-instance={`${event.event_id}:${entry.index}`}>
                  <ToolCaptureViewer reads={reads} sessionId={sid} toolUseId={entry.toolUseId} capture={entry.capture} />
                </div>
              ))}
            </article>
            {previews.afterRow.get(index)?.map((preview) => <LivePreview key={livePreviewKey(preview)} preview={preview} />)}
          </Fragment>);
        })}
        {previews.unplaced.map((preview) => <LivePreview key={livePreviewKey(preview)} preview={preview} />)}
        {composer?.awaiting.map((input) => (
          <article key={input.commandId} data-testid="factory-awaiting-input" data-command-id={input.commandId} className="mb-2 rounded-md border border-dashed border-border p-3">
            <span className="font-mono text-xs text-muted">sent · waiting for the agent</span>
            <p className="mt-1 whitespace-pre-wrap text-sm">{input.text}</p>
          </article>
        ))}
      </div>
      {gates.length === 0 ? null : (
        <section data-testid="factory-gate-stack" className="mx-4 border-t border-border py-3">
          {gates.map((gate) => (
            <article key={gate.gateId} data-testid="factory-gate-card" className="mb-2 rounded-md border border-border bg-card p-3">
              <p className="font-medium">{gate.prompt.title === "" ? "Decision required" : gate.prompt.title}</p>
              <p className="text-sm text-muted">{gate.prompt.body}</p>
              <p data-testid="factory-gate-answerability" className="font-mono text-xs text-muted">{gate.kind} · {gate.answerability === "" ? "unattested" : gate.answerability}</p>
              {gate.kind === "harness.permission" && gate.answerable && onGateRespond !== undefined ? (
                <div data-testid="factory-gate-actions" className="mt-2 flex gap-2">
                  {Object.values(GATE_APPROVAL_ACTIONS).map((action) => (
                    <button key={action} type="button" onClick={() => onGateRespond(gate.gateId, action)} className="rounded border border-border px-2 py-1 text-xs">
                      {action}
                    </button>
                  ))}
                </div>
              ) : (
                <p data-testid="factory-gate-unavailable" className="mt-2 text-xs text-muted">
                  {gate.answerable ? "Answer this gate in a supported client" : gateStateLabel(gate.answerability)}
                </p>
              )}
            </article>
          ))}
        </section>
      )}
      {composer === undefined ? null : (
        <>
          {composer.unconfirmed === null ? null : (
            <div role="status" data-testid="composer-unconfirmed" className="mx-4 flex items-center gap-2 rounded-md border border-border p-2 text-xs">
              <span className="flex-1">Delivery of &ldquo;{composer.unconfirmed}&rdquo; is unconfirmed.</span>
              <button type="button" onClick={composer.onRetry} className="rounded border border-border px-2 py-1">Retry</button>
              <button type="button" onClick={composer.onDiscard} className="rounded border border-border px-2 py-1">Discard</button>
            </div>
          )}
          <Composer
            onSubmit={composer.onSubmit}
            submitting={composer.submitting || composer.unconfirmed !== null}
            gateOpen={gates.length > 0}
            error={composer.error}
          />
        </>
      )}
      <button
        type="button"
        data-testid="browse-earlier-history"
        disabled={view.earlierState === "loading" || view.earlierState === "complete"}
        onClick={() => { void view.browseEarlier(); }}
        className="m-4 rounded-md border border-border px-3 py-2 text-sm disabled:opacity-40"
      >
        {view.earlierState === "loading" ? "Loading…" : "Browse earlier history"}
      </button>
    </main>
  );
}
