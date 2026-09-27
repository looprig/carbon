import { useState } from "react";
import { page, userEvent } from "vitest/browser";
import { describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";
import type { RecentSessionPage } from "@looprig/client";
import { SessionRow } from "./session-row";

const summary: RecentSessionPage["sessions"][number] = {
  agent_id: "agent-1", session_id: "44444444-4444-4444-4444-444444444444",
  state: "running",
  title: "Fix the parser",
  created_at: "2026-08-27T10:00:00Z",
  last_active_at: "2026-08-27T10:07:30Z",
};

function text(testId: string): string {
  const node = document.querySelector(`[data-testid=${testId}]`);
  if (!node) throw new Error(`no element with data-testid=${testId}`);
  return node.textContent ?? "";
}

describe("SessionRow", () => {
  it("shows the id, status dot, duration and goal, and links to the session", async () => {
    render(<SessionRow session={summary} href={`/sessions/${summary.session_id}`} />);
    const link = page.getByTestId("session-row-link");
    await expect.element(link).toBeInTheDocument();

    expect(text("session-id")).toBe("44444444");
    expect(document.querySelector("[data-testid=session-id]")?.className).toContain("font-mono");
    expect(document.querySelector("[data-testid=status-dot]")?.getAttribute("data-status")).toBe("running");
    expect(text("session-duration")).toBe("7m");
    expect(text("session-goal")).toBe("Fix the parser");
    expect(link.element().getAttribute("href")).toBe("/sessions/44444444-4444-4444-4444-444444444444");
  });

  it("keeps the full id reachable even though the cell is abbreviated", async () => {
    render(<SessionRow session={summary} href="/sessions/x" />);
    await expect.element(page.getByTestId("session-id")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=session-id]")?.getAttribute("title")).toBe(
      "44444444-4444-4444-4444-444444444444",
    );
  });

  it("falls back to 'Untitled session' when the summary carries no title", async () => {
    render(<SessionRow session={{ ...summary, title: undefined, created_at: undefined }} href="/sessions/x" />);
    await expect.element(page.getByTestId("session-goal")).toBeInTheDocument();
    expect(text("session-goal")).toBe("Untitled session");
    expect(text("session-duration")).toBe("—");
  });

  it("hands a plain left click to the router and leaves modified clicks to the browser", async () => {
    // The href is real so the row is middle-clickable, copyable and
    // refreshable — the whole reason the router uses browser history rather
    // than hash history. onActivate is what stops a plain click from throwing
    // the SPA away and reloading the bundle.
    const onActivate = vi.fn();
    render(<SessionRow session={summary} href="/sessions/x" onActivate={onActivate} />);
    const link = page.getByTestId("session-row-link");
    await expect.element(link).toBeInTheDocument();

    await userEvent.click(link);
    expect(onActivate).toHaveBeenCalledTimes(1);

    // A meta/ctrl-click means "open in a new tab" and must reach the browser
    // untouched: intercepting it would silently break that gesture.
    //
    // Assert that on the EVENT, never by letting the navigation happen. The
    // modifier that means "new tab" is platform-specific — Meta on macOS, Ctrl
    // on Linux — so on a Linux runner Chromium treats a Meta-click as an
    // ordinary navigation, follows the real href, and destroys vitest's test
    // iframe ("Cannot connect to the iframe. Did you change the location?").
    // That is a CI-only failure the local macOS run cannot reproduce. So we
    // capture the click ourselves, record whether the component prevented it,
    // and prevent it here to keep the frame alive.
    let modifiedClickWasPrevented: boolean | undefined;
    const capture = (event: MouseEvent) => {
      modifiedClickWasPrevented = event.defaultPrevented;
      event.preventDefault();
    };
    document.addEventListener("click", capture);
    try {
      await userEvent.click(link, { modifiers: ["Meta"] });
    } finally {
      document.removeEventListener("click", capture);
    }

    expect(modifiedClickWasPrevented).toBe(false);
    expect(onActivate).toHaveBeenCalledTimes(1);
  });

  it("does not re-render when its parent re-renders with the same session", async () => {
    // Measured in Phase 4: a per-row store selector's Object.is bail-out
    // suppresses only STORE-driven re-renders. A parent re-render — a keystroke
    // in the search box, a status filter change — re-renders every child
    // regardless, so the row needs `memo` as well.
    //
    // The probe is a counting getter on the summary rather than an instrumented
    // component, so nothing about the production component changes to make it
    // observable.
    let reads = 0;
    const probed: RecentSessionPage["sessions"][number] = {
      session_id: summary.session_id,
      agent_id: summary.agent_id,
      state: summary.state,
      created_at: summary.created_at,
      last_active_at: summary.last_active_at,
      get title(): string {
        reads += 1;
        return "Fix the parser";
      },
    };

    function Harness(): React.JSX.Element {
      const [bumps, setBumps] = useState(0);
      return (
        <>
          <button data-testid="bump" type="button" onClick={() => setBumps((n) => n + 1)}>
            {`bumps:${bumps}`}
          </button>
          <SessionRow session={probed} href="/sessions/x" />
        </>
      );
    }

    render(<Harness />);
    await expect.element(page.getByTestId("session-goal")).toBeInTheDocument();
    const afterMount = reads;
    expect(afterMount).toBeGreaterThan(0);

    await userEvent.click(page.getByTestId("bump"));
    // Settle on the parent's own re-render BEFORE asserting, so the assertion
    // observes a completed render pass rather than racing one.
    await expect.element(page.getByTestId("bump")).toHaveTextContent("bumps:1");
    expect(reads).toBe(afterMount);
  });
});
