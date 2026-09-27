import { describe, expect, it } from "vitest";
import type { RecentSessionPage } from "@looprig/client";
import { filterSessions } from "./filter-sessions";

const sessions: RecentSessionPage["sessions"][number][] = [
  { agent_id: "agent-1", last_active_at: "2026-09-05T12:00:00Z", session_id: "aaaaaaaa-0000-0000-0000-000000000000", state: "running", title: "Fix the parser" },
  { agent_id: "agent-1", last_active_at: "2026-09-05T12:00:00Z", session_id: "bbbbbbbb-0000-0000-0000-000000000000", state: "failed", title: "Refactor storage" },
  { agent_id: "agent-1", last_active_at: "2026-09-05T12:00:00Z", session_id: "cccccccc-0000-0000-0000-000000000000", state: "waiting_on_gate", title: "Deploy" },
  { agent_id: "agent-1", last_active_at: "2026-09-05T12:00:00Z", session_id: "dddddddd-0000-0000-0000-000000000000", state: "idle" },
];

function ids(result: readonly RecentSessionPage["sessions"][number][]): string[] {
  return result.map((session) => session.session_id.slice(0, 8));
}

describe("filterSessions", () => {
  it("returns everything for the default filter", () => {
    expect(ids(filterSessions(sessions, "all", ""))).toEqual(["aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd"]);
  });

  it("narrows by the same status buckets the dot colours by", () => {
    expect(ids(filterSessions(sessions, "running", ""))).toEqual(["aaaaaaaa"]);
    expect(ids(filterSessions(sessions, "failed", ""))).toEqual(["bbbbbbbb"]);
    // waiting_on_gate is what harness emits; a filter matching "waiting"
    // literally would return nothing here.
    expect(ids(filterSessions(sessions, "waiting", ""))).toEqual(["cccccccc"]);
    // A summary with no state at all is idle, and must be reachable.
    expect(ids(filterSessions(sessions, "idle", ""))).toEqual(["dddddddd"]);
  });

  it("searches over the id and the title, and nothing else", () => {
    expect(ids(filterSessions(sessions, "all", "bbbbbbbb"))).toEqual(["bbbbbbbb"]);
    expect(ids(filterSessions(sessions, "all", "parser"))).toEqual(["aaaaaaaa"]);
    // `state` is not a search field: it has its own control.
    expect(ids(filterSessions(sessions, "all", "running"))).toEqual([]);
  });

  it("ignores case and surrounding whitespace in the needle", () => {
    expect(ids(filterSessions(sessions, "all", "  PARSER "))).toEqual(["aaaaaaaa"]);
    expect(ids(filterSessions(sessions, "all", "BBBBBBBB"))).toEqual(["bbbbbbbb"]);
  });

  it("treats a whitespace-only search as no search rather than as no match", () => {
    expect(ids(filterSessions(sessions, "all", "   "))).toHaveLength(4);
  });

  it("applies status and search together, not either-or", () => {
    expect(ids(filterSessions(sessions, "running", "parser"))).toEqual(["aaaaaaaa"]);
    expect(ids(filterSessions(sessions, "failed", "parser"))).toEqual([]);
  });

  it("matches a titleless session by id without crashing on the absent title", () => {
    expect(ids(filterSessions(sessions, "all", "dddddddd"))).toEqual(["dddddddd"]);
  });

  it("never mutates or reorders the page it was given", () => {
    const before = [...sessions];
    filterSessions(sessions, "failed", "storage");
    expect(sessions).toEqual(before);
    expect(ids(filterSessions(sessions, "all", "0000"))).toEqual(ids(sessions));
  });
});
