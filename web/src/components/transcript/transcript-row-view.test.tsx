import { useState } from "react";
import { page, userEvent } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import type { TranscriptRow } from "@looprig/client";
import { TranscriptRowView } from "./transcript-row-view";

const common = {
  ordinal: 0,
  loopId: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
  turnId: "3e4f5a6b-7c8d-4e9f-8a0b-1c2d3e4f5a6b",
  journalSeq: 1,
  live: false,
  orphanedLoop: false,
} as const;

const rows = {
  user: { ...common, kind: "user", blocks: [{ type: "text", text: "hi" }] },
  assistant: { ...common, kind: "assistant", thinking: "", text: "hello", refusal: "", redactedThinking: false },
  tool: {
    ...common,
    kind: "tool",
    toolUseId: "toolu_1",
    toolExecutionId: "",
    toolName: "bash",
    summary: "ls",
    status: "ok",
    result: "a\nb",
    spawnedLoopId: "",
  },
  notice: { ...common, kind: "notice", level: "info", text: "Context compacted" },
  tombstone: { ...common, kind: "tombstone" },
} satisfies Record<string, TranscriptRow>;

describe("TranscriptRowView", () => {
  it.each([
    ["user", rows.user, "user-bubble"],
    ["assistant", rows.assistant, "agent-prose"],
    ["tool", rows.tool, "tool-step-line"],
    ["notice", rows.notice, "system-notice"],
    ["tombstone", rows.tombstone, "tombstone-row"],
  ] as Array<[string, TranscriptRow, string]>)("routes a %s row to its own component", async (_name, row, testId) => {
    render(<TranscriptRowView row={row} />);
    await expect.element(page.getByTestId(testId)).toBeInTheDocument();
  });

  it("renders an interrupted turn's tombstone as a divider that says so", async () => {
    // TombstoneRow is content-less by construction; the only thing to render is
    // the fact that a turn was cut short. Rendering nothing would leave an
    // interrupted turn looking like one that simply ended.
    render(<TranscriptRowView row={rows.tombstone} />);
    const tombstone = page.getByTestId("tombstone-row");
    await expect.element(tombstone).toBeInTheDocument();
    expect(tombstone.element().textContent).toContain("interrupted");
  });

  it("names a stamped interrupter but preserves the old wording when absent", async () => {
    render(<TranscriptRowView row={{ ...rows.tombstone, principal: { tenant: "acme", subject: "user_alex", kind: "actor" } }} />);
    await expect.element(page.getByTestId("tombstone-row")).toHaveTextContent("Turn interrupted by user_alex");
    expect(rows.tombstone).not.toHaveProperty("principal");
  });

  it("carries a notice's own level through rather than flattening it", async () => {
    render(<TranscriptRowView row={{ ...rows.notice, level: "error", text: "Turn rejected" }} />);
    const notice = page.getByTestId("system-notice");
    await expect.element(notice).toBeInTheDocument();
    expect(notice.element().getAttribute("data-level")).toBe("error");
  });

  it("marks a row whose loop was never observed rather than passing it off as the main loop", async () => {
    // rows.ts: a loop whose LoopStarted fell off the journal page has no parent
    // anchor, and such rows render top-level WITH a marker instead of being
    // dropped. In a flat transcript the marker is the only thing that stops a
    // subagent's work reading as the primary loop's.
    render(<TranscriptRowView row={{ ...rows.assistant, orphanedLoop: true }} />);
    const marker = page.getByTestId("orphaned-loop-marker");
    await expect.element(marker).toBeInTheDocument();
    expect(marker.element().textContent).toContain("subagent");
  });

  it("shows no marker for an ordinary row", async () => {
    render(<TranscriptRowView row={rows.assistant} />);
    await expect.element(page.getByTestId("agent-prose")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=orphaned-loop-marker]")).toBeNull();
  });
});

/**
 * Counts every property read of a row, which is how "did this component's body
 * run again" is observed from outside without instrumenting the component.
 */
function counting(row: TranscriptRow): { proxy: TranscriptRow; reads: () => number } {
  let reads = 0;
  const proxy = new Proxy(row, {
    get(target, property, receiver) {
      reads += 1;
      return Reflect.get(target, property, receiver);
    },
  }) as TranscriptRow;
  return { proxy, reads: () => reads };
}

describe("TranscriptRowView memoization", () => {
  it("does not re-render when its parent re-renders with the same row object", async () => {
    // Rows are copy-on-write in @looprig/client, so an unchanged row is
    // reference-identical. Without `memo`, appending one row to a 10,000-row
    // transcript re-renders all 10,000 — the per-row selector alone does NOT
    // prevent that, because a parent's own re-render descends into every child
    // regardless of what any store did.
    const { proxy, reads } = counting(rows.assistant);
    function Harness(): React.JSX.Element {
      const [tick, setTick] = useState(0);
      return (
        <>
          <button type="button" data-testid="tick" onClick={() => setTick(tick + 1)}>
            {tick}
          </button>
          <TranscriptRowView row={proxy} />
        </>
      );
    }
    render(<Harness />);
    await expect.element(page.getByTestId("agent-prose")).toBeInTheDocument();

    const before = reads();
    expect(before).toBeGreaterThan(0);
    await userEvent.click(page.getByTestId("tick"));
    await expect.element(page.getByTestId("tick")).toHaveTextContent("1");
    expect(reads()).toBe(before);
  });

  it("DOES re-render when the row object is replaced", async () => {
    // The other half: memo must not be so sticky that a completed tool call
    // keeps saying "running". Copy-on-write is what makes this fire.
    function Harness(): React.JSX.Element {
      const [status, setStatus] = useState<"running" | "ok">("running");
      return (
        <>
          <button type="button" data-testid="finish" onClick={() => setStatus("ok")}>
            finish
          </button>
          <TranscriptRowView row={{ ...rows.tool, status, result: status === "ok" ? "done" : "" }} />
        </>
      );
    }
    render(<Harness />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(
      document.querySelector("[data-testid=tool-step-line] [data-testid=status-dot]")?.getAttribute("data-status"),
    ).toBe("running");

    await userEvent.click(page.getByTestId("finish"));
    await expect.element(page.getByTestId("tool-step-toggle")).toBeInTheDocument();
    expect(
      document.querySelector("[data-testid=tool-step-line] [data-testid=status-dot]")?.getAttribute("data-status"),
    ).toBe("idle");
  });
});
