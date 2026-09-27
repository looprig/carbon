import { useState } from "react";
import type { ToolRow, ToolRowStatus } from "@looprig/client";
import { ToneDot, type StatusTone } from "../status-dot";

/**
 * `ToolRow.status`'s four values onto §12's colour buckets.
 *
 * `ok` and `cancelled` share the gray "terminal, not a failure" dot, so the
 * WORD is what tells them apart — a cancelled call rendered as a completed one
 * would report work that never happened as done.
 */
export function toolTone(status: ToolRowStatus): StatusTone {
  switch (status) {
    case "running":
      return "running";
    case "error":
      return "failed";
    default:
      return "idle";
  }
}

/** The word shown beside the dot, or "" when the dot already says everything. */
function statusWord(status: ToolRowStatus): string {
  switch (status) {
    case "error":
      return "failed";
    case "cancelled":
      return "cancelled";
    default:
      return "";
  }
}

/**
 * A tool call is one line until you ask for more (capstan-spec.md §8).
 *
 * Everything on the line is a machine fact — tool name, redacted argument
 * summary, status — so the whole line is mono per §12. There is deliberately no
 * timestamp: 05-app.md's `· 10:09` came from an `at` field, and `ToolRow`
 * carries none. `journalSeq` is an ordering key, not a wall clock, and
 * inventing a time from arrival would be a fact the wire never stated.
 *
 * The expand affordance exists only when there is a result to expand to. A
 * running call's `result` is "", and a toggle that opens onto nothing is a
 * control that lies.
 *
 * Collapse state lives in this row. A virtualiser that unmounts scrolled-away
 * rows would drop it (Capstan §7's "keep collapse state outside virtualised
 * rows" gotcha) — see `transcript.tsx` for why nothing is virtualised yet, and
 * hoist an expanded-id set into `Transcript` if that ever changes.
 */
export function ToolCallStep({ row }: { row: ToolRow }): React.JSX.Element {
  const [expanded, setExpanded] = useState(false);
  const expandable = row.result !== "";
  const word = statusWord(row.status);

  const line = (
    <>
      <ToneDot tone={toolTone(row.status)} label={row.status} />
      <span data-testid="tool-step-summary" className="truncate font-mono text-xs text-muted">
        {row.summary === "" ? row.toolName : `${row.toolName} · ${row.summary}`}
      </span>
      {word === "" ? null : (
        <span
          data-testid="tool-step-status"
          className={`font-mono text-xs ${row.status === "error" ? "text-fail" : "text-muted"}`}
        >
          {word}
        </span>
      )}
    </>
  );

  return (
    <div data-testid="tool-step-line" className="px-4 py-1">
      {expandable ? (
        <button
          type="button"
          data-testid="tool-step-toggle"
          onClick={() => setExpanded((open) => !open)}
          aria-expanded={expanded}
          className="flex w-full items-center gap-2 rounded px-2 py-1 text-left hover:bg-accent focus-visible:outline-2 focus-visible:outline-rig"
        >
          {line}
          <span aria-hidden="true" className="ml-auto font-mono text-xs text-muted">
            {expanded ? "\u2304" : "\u203a"}
          </span>
        </button>
      ) : (
        <div className="flex w-full items-center gap-2 px-2 py-1">{line}</div>
      )}
      {expanded ? (
        <pre
          data-testid="tool-step-output"
          className="mt-1 max-h-96 overflow-auto rounded border border-border bg-card p-3 font-mono text-xs whitespace-pre-wrap"
        >
          {row.result}
        </pre>
      ) : null}
    </div>
  );
}
