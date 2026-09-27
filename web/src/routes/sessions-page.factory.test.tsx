import { page, userEvent } from "vitest/browser";
import { expect, test, vi } from "vitest";
import { render } from "vitest-browser-react";
import type { FactoryCommands, FactoryPageOptions, RecentSessionPage } from "@looprig/client";
import { FactorySessionsPage } from "./sessions-page";

const inertCreate = vi.fn() as unknown as FactoryCommands["create"];

function reads(...pages: RecentSessionPage[]): {
  calls: FactoryPageOptions[];
  listRecentSessions(options?: FactoryPageOptions): Promise<RecentSessionPage>;
} {
  const calls: FactoryPageOptions[] = [];
  return {
    calls,
    listRecentSessions(options = {}) {
      calls.push(options);
      return Promise.resolve(pages.shift() ?? { sessions: [] });
    },
  };
}

const first: RecentSessionPage = {
  sessions: [{
    session_id: "session-cold", agent_id: "agent-1", state: "idle", title: "Cold work",
    created_at: "2026-09-05T11:59:00Z", last_active_at: "2026-09-05T12:00:00Z",
  }],
  next_cursor: "page-2",
};

test("renders the durable Factory list and its opaque session identity", async () => {
  const open = vi.fn();
  render(<FactorySessionsPage reads={reads(first)} create={inertCreate} onOpenSession={open} />);
  await expect.element(page.getByTestId("sessions-list")).toBeInTheDocument();
  await expect.element(page.getByTestId("session-row-link")).toHaveAttribute("href", "/sessions/session-cold");
  expect(page.getByTestId("status-dot").element().getAttribute("data-status")).toBe("idle");

  await userEvent.click(page.getByTestId("session-row-link"));
  expect(open).toHaveBeenCalledWith("session-cold");
});

test("pages only when the user follows a bounded Factory cursor", async () => {
  const second: RecentSessionPage = {
    sessions: [{
      session_id: "session-failed", agent_id: "agent-1", state: "failed",
      last_active_at: "2026-09-04T12:00:00Z",
    }],
    previous_cursor: "page-1",
  };
  const source = reads(first, second);
  render(<FactorySessionsPage reads={source} create={inertCreate} onOpenSession={vi.fn()} />);
  await expect.element(page.getByTestId("sessions-next-page")).toBeInTheDocument();
  expect(source.calls).toHaveLength(1);

  await userEvent.click(page.getByTestId("sessions-next-page"));
  await expect.element(page.getByTestId("session-row-link")).toHaveAttribute("href", "/sessions/session-failed");
  expect(source.calls[1]).toMatchObject({ cursor: "page-2", limit: 100 });
});

test("shows a failed initial Factory read instead of an endless loading state", async () => {
  const failure = new Error("catalog denied");
  const source = { listRecentSessions: () => Promise.reject(failure) };
  render(<FactorySessionsPage reads={source} create={inertCreate} onOpenSession={vi.fn()} />);

  await expect.element(page.getByTestId("sessions-error")).toHaveTextContent("catalog denied");
  await expect.element(page.getByTestId("sessions-loading")).not.toBeInTheDocument();
});
