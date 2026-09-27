import { memo } from "react";
import { principalLabel, type TranscriptRow } from "@looprig/client";
import { AgentProse } from "./agent-prose";
import { SystemNotice } from "./system-notice";
import { ToolCallStep } from "./tool-call-step";
import { UserBubble } from "./user-bubble";

/**
 * One transcript row, routed to the component that renders its kind.
 *
 * There is no `gate` row. 05-app.md's dispatcher has one, and
 * `@looprig/client`'s projection does not: the five kinds are
 * user/assistant/tool/notice/tombstone, and gates live in `SessionView.gates`
 * keyed by gate id, reached through `useGate`. A gate is not a point in the
 * transcript — it can be opened and resolved while rows keep appending around
 * it — so the page renders open gates in their own region.
 *
 * ## Why `memo`
 *
 * The transcript hands each row a per-row selector's value, and rows are
 * copy-on-write in `@looprig/client` (design §3c), so an unchanged row is
 * reference-identical and this subtree is skipped. The per-row selector ALONE
 * is not enough: `Object.is` bail-out only suppresses store-driven re-renders,
 * and a parent that re-renders because one row was appended descends into every
 * child regardless. Without `memo`, appending to a 10,000-row transcript
 * re-renders 10,000 rows.
 *
 * The other half matters just as much: `memo` must not be so sticky that a
 * completed tool call keeps saying "running". Copy-on-write is what makes the
 * replacement fire, and `transcript-row-view.test.tsx` pins both directions.
 */
export const TranscriptRowView = memo(function TranscriptRowView({
  row,
}: {
  row: TranscriptRow;
}): React.JSX.Element {
  return (
    <div data-testid="transcript-row" data-row-kind={row.kind} data-loop-id={row.loopId}>
      {row.orphanedLoop ? (
        // rows.ts: a loop whose `LoopStarted` fell off the journal page has no
        // parent anchor, and its rows render top-level with a marker rather
        // than being dropped. This transcript is flat, so the marker is the
        // only thing that stops a subagent's work reading as the primary
        // loop's. It clears retroactively if the `LoopStarted` turns up later.
        <p data-testid="orphaned-loop-marker" className="px-4 pt-2 font-mono text-xs text-muted">
          orphaned subagent
        </p>
      ) : null}
      <RowBody row={row} />
    </div>
  );
});

function RowBody({ row }: { row: TranscriptRow }): React.JSX.Element {
  switch (row.kind) {
    case "user":
      return <UserBubble blocks={row.blocks} frame={row.frame} principal={row.principal} />;
    case "assistant":
      return (
        <AgentProse
          thinking={row.thinking}
          text={row.text}
          refusal={row.refusal}
          redactedThinking={row.redactedThinking}
        />
      );
    case "tool":
      return <ToolCallStep row={row} />;
    case "notice":
      return <SystemNotice text={row.text} level={row.level} />;
    case "tombstone":
      // Content-less by construction: the only fact is that a turn was cut
      // short. Rendering nothing would make an interrupted turn look like one
      // that simply ended.
      return <SystemNotice
        text={row.principal === undefined ? "Turn interrupted" : `Turn interrupted by ${principalLabel(row.principal)}`}
        level="info" testId="tombstone-row" />;
    default:
      // Exhaustive: a row kind added to @looprig/client fails the build here
      // rather than silently rendering as nothing.
      return assertNever(row);
  }
}

function assertNever(row: never): never {
  throw new Error(`unhandled transcript row: ${JSON.stringify(row)}`);
}
