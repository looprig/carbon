import { StrictMode } from "react";
import { page } from "vitest/browser";
import { expect, test } from "vitest";
import { render } from "vitest-browser-react";
import {
  acceptsResidentResponse,
  publicGates,
  type PublicGatePage,
} from "@looprig/client";
import type { PublicJournalEvent, UseFactorySessionViewResult } from "@looprig/react";
import {
  emptyFactoryGateBoardState,
  foldFactoryGateView,
  useFactoryGateBoard,
} from "./factory-gate-board";
import { FactorySessionDetailPage } from "./factory-session-detail-page";

const SID = "11111111-1111-4111-8111-111111111111";
const OTHER_SID = "22222222-2222-4222-8222-222222222222";
const LOOP = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const GATE_A = "9e2f0000-0000-4000-8000-000000000001";
const GATE_B = "9e2f0000-0000-4000-8000-000000000002";

function gateRecord(gateId: string, seq: number, answerability = "resident") {
  return {
    gate_id: gateId,
    kind: "harness.permission",
    opened_event_id: `event-${seq}`,
    opened_journal_seq: seq,
    deadline: "2026-09-05T13:00:00Z",
    answerability,
    prompt: {
      title: `gate ${gateId.slice(-1)}`,
      body: "Allow it?",
      origin: "https://example.test",
      controls: [{ action: "approve", label: "Approve" }, { action: "deny", label: "Deny" }],
    },
  };
}

function gatePage(records: ReturnType<typeof gateRecord>[], tip = 12): PublicGatePage {
  return { journal_tip: tip, open_gate_count: records.length, gates: records } as unknown as PublicGatePage;
}

function resolvedEvent(gateId: string, seq: number, sessionId = SID): PublicJournalEvent {
  return {
    event_id: `resolve-${seq}`,
    journal_seq: seq,
    body: {
      type: "GateResolved",
      v: 1,
      session_id: sessionId,
      event_id: `resolve-${seq}`,
      loop_id: LOOP,
      gate_id: gateId,
      action: "Approve",
      reason: "answered",
      resolver: "loop",
      source: { kind: "user" },
      created_at: "2026-09-05T12:00:00Z",
    },
  };
}

function openedEvent(gateId: string, seq: number, sessionId = SID): PublicJournalEvent {
  return {
    event_id: `open-${seq}`,
    journal_seq: seq,
    body: {
      type: "GateOpened",
      v: 1,
      session_id: sessionId,
      event_id: `open-${seq}`,
      loop_id: LOOP,
      created_at: "2026-09-05T12:00:00Z",
      gate: {
        id: gateId,
        kind: "harness.permission",
        resolver: "loop",
        prompt: { title: "live gate", body: "Allow it?", controls: [{ action: "approve", label: "Approve" }] },
      },
    },
  };
}

function gateIds(state: { board: Parameters<typeof publicGates>[0] }): string[] {
  return publicGates(state.board).map((entry) => entry.gateId);
}

test("a page in flight across a live resolve does not resurrect the gate", () => {
  // The race foldPublicGatePage's own comment refuses to fix: a page READ
  // before the resolve, MERGED after it. The resolve is not in this fold's
  // event window at all — only the tombstone carries it — because a page can
  // arrive after the retained window has moved past the resolving event.
  const inFlight = gatePage([gateRecord(GATE_A, 6), gateRecord(GATE_B, 7)]);
  const resolved = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: null, events: [resolvedEvent(GATE_A, 8)],
  });
  expect(gateIds(resolved)).toStrictEqual([]);

  const merged = foldFactoryGateView(resolved, {
    generation: 0, sessionId: SID, page: inFlight, events: [],
  });
  expect(gateIds(merged)).toStrictEqual([GATE_B]);
});

test("either arrival order leaves the same board", () => {
  const openPage = gatePage([gateRecord(GATE_A, 6), gateRecord(GATE_B, 7)]);
  const resolve = resolvedEvent(GATE_A, 8);

  const pageFirst = foldFactoryGateView(
    foldFactoryGateView(emptyFactoryGateBoardState(), { generation: 0, sessionId: SID, page: openPage, events: [] }),
    { generation: 0, sessionId: SID, page: openPage, events: [resolve] },
  );
  const liveFirst = foldFactoryGateView(
    foldFactoryGateView(emptyFactoryGateBoardState(), { generation: 0, sessionId: SID, page: null, events: [resolve] }),
    { generation: 0, sessionId: SID, page: openPage, events: [resolve] },
  );

  expect(gateIds(pageFirst)).toStrictEqual([GATE_B]);
  expect(gateIds(liveFirst)).toStrictEqual([GATE_B]);
});

test("absence from a partial page is NOT resolution", () => {
  // listGates is read with a limit and returns a BOUNDED window. A gate the
  // next page did not carry is a gate this client has not been told about, not
  // a gate that closed — which is why the page path merges rather than
  // rebuilding, and why only an observed GateResolved removes anything.
  const first = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 6), gateRecord(GATE_B, 7)]), events: [],
  });
  const partial = foldFactoryGateView(first, {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 6)]), events: [],
  });

  expect(gateIds(partial)).toStrictEqual([GATE_A, GATE_B]);
  expect(partial.resolved.size).toBe(0);
});

test("a tombstone closes one (session, gate) pair and nothing else", () => {
  let state = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 6)]), events: [],
  });
  state = foldFactoryGateView(state, {
    generation: 0, sessionId: OTHER_SID, page: gatePage([gateRecord(GATE_A, 6)]), events: [],
  });
  state = foldFactoryGateView(state, {
    generation: 0, sessionId: SID, page: null, events: [resolvedEvent(GATE_A, 8, OTHER_SID)],
  });

  expect(publicGates(state.board).map((entry) => `${entry.sessionId}/${entry.gateId}`))
    .toStrictEqual([`${SID}/${GATE_A}`]);
});

test("a superseded generation is refused, and a newer one drops the tombstones", () => {
  const resolved = foldFactoryGateView(emptyFactoryGateBoardState(3), {
    generation: 3, sessionId: SID, page: gatePage([gateRecord(GATE_A, 6)]), events: [resolvedEvent(GATE_A, 8)],
  });
  expect(gateIds(resolved)).toStrictEqual([]);

  // A read that was already in flight when the generation advanced applies
  // nothing at all — not its page, and not its events.
  const stale = foldFactoryGateView(resolved, {
    generation: 2, sessionId: SID, page: gatePage([gateRecord(GATE_B, 9)]), events: [],
  });
  expect(stale).toBe(resolved);

  // A NEW generation is a new authority: the journal it is reporting is not the
  // one the tombstone was taken from, so the tombstone may not outlive it.
  const rebuilt = foldFactoryGateView(resolved, {
    generation: 4, sessionId: SID, page: gatePage([gateRecord(GATE_A, 6)]), events: [],
  });
  expect(gateIds(rebuilt)).toStrictEqual([GATE_A]);
  expect(rebuilt.resolved.size).toBe(0);
});

test("an unchanged fold returns the identical state and board", () => {
  // useFactoryGate memoises `publicGates(board)` on board identity, so a board
  // rebuilt on every unchanged poll re-renders every gate card forever.
  const openPage = gatePage([gateRecord(GATE_A, 6)]);
  const first = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: openPage, events: [],
  });
  const again = foldFactoryGateView(first, { generation: 0, sessionId: SID, page: openPage, events: [] });
  expect(again).toBe(first);

  const resolvedOnce = foldFactoryGateView(first, {
    generation: 0, sessionId: SID, page: openPage, events: [resolvedEvent(GATE_A, 8)],
  });
  const resolvedTwice = foldFactoryGateView(resolvedOnce, {
    generation: 0, sessionId: SID, page: openPage, events: [resolvedEvent(GATE_A, 8)],
  });
  expect(resolvedTwice).toBe(resolvedOnce);
});

test("a live GateOpened shows the gate unattested, and its own resolve still closes it", () => {
  const opened = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: null, events: [openedEvent(GATE_A, 6)],
  });
  expect(gateIds(opened)).toStrictEqual([GATE_A]);
  // An open journal event proves presentation, never that anyone can answer.
  expect(acceptsResidentResponse(publicGates(opened.board)[0]!)).toBe(false);

  const closed = foldFactoryGateView(opened, {
    generation: 0, sessionId: SID, page: null, events: [openedEvent(GATE_A, 6), resolvedEvent(GATE_A, 8)],
  });
  expect(gateIds(closed)).toStrictEqual([]);
});

function view(overrides: Partial<UseFactorySessionViewResult> = {}): UseFactorySessionViewResult {
  return {
    state: "ready",
    liveState: "live",
    status: { session_id: SID, agent_id: "agent-1", state: "waiting_on_gate", residency: "resident", journal_tip: 12 },
    gates: gatePage([gateRecord(GATE_A, 6), gateRecord(GATE_B, 7)]),
    events: [],
    liveText: [],
    liveReasoning: [],
    liveToolSteps: [],
    coveredThrough: 12,
    error: null,
    earlierState: "idle",
    browseEarlier: () => Promise.resolve(),
    ...overrides,
  };
}

function Harness({ current }: { current: UseFactorySessionViewResult }): React.JSX.Element {
  const board = useFactoryGateBoard(SID, current);
  const gates = publicGates(board).map((entry) => ({ ...entry, answerable: acceptsResidentResponse(entry) }));
  return <FactorySessionDetailPage sid={SID} view={current} reads={{} as never} gates={gates} />;
}

test("a gate resolved live leaves the screen even though the next page still lists it", async () => {
  const stale = view();
  const screen = await render(<Harness current={stale} />);
  await expect.element(page.getByTestId("factory-gate-stack")).toBeInTheDocument();
  expect(document.querySelectorAll("[data-testid=factory-gate-card]")).toHaveLength(2);

  // The resolve lands live; the gate page behind it has not been re-read.
  await screen.rerender(<Harness current={view({ events: [resolvedEvent(GATE_A, 13)], coveredThrough: 13 })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(1);

  // A later cold page read — one that was in flight across the resolve — still
  // lists it. It must not come back, and nothing else may be lost with it.
  await screen.rerender(<Harness current={view({ events: [], coveredThrough: 13 })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(1);
  await expect.element(page.getByTestId("factory-gate-stack")).toHaveTextContent("gate 2");
});

test("an authoritative invalidation drops the board and its tombstones", async () => {
  const screen = await render(<Harness current={view({ events: [resolvedEvent(GATE_A, 13)] })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(1);

  // `session_not_found` clears the cached projection: gates go null AND coverage
  // goes to zero, which is how `#rejectAccess` really publishes it. Either
  // condition alone advances the generation, so this pins the BEHAVIOUR rather
  // than one of the two branches — see the hook's comment on the redundancy.
  // What comes back afterwards is a new authority.
  await screen.rerender(<Harness current={view({ state: "failed", status: null, gates: null, coveredThrough: 0, events: [] })} />);
  await expect.element(page.getByTestId("detail-read-error")).toBeInTheDocument();

  await screen.rerender(<Harness current={view({ events: [] })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(2);
});

test("a lowered committed floor is a new generation, so an old resolve cannot suppress the new page", async () => {
  const screen = await render(<Harness current={view({ events: [resolvedEvent(GATE_A, 13)], coveredThrough: 13 })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(1);

  // A replica change or an overflow reset rewinds coverage. The gate the old
  // journal said was resolved is not the same record the new one is listing.
  await screen.rerender(<Harness current={view({ events: [], coveredThrough: 4 })} />);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(2);
});

test("StrictMode's double render folds the same board once", async () => {
  const current = view({ events: [resolvedEvent(GATE_A, 13)], coveredThrough: 13 });
  render(<StrictMode><Harness current={current} /></StrictMode>);
  await expect.poll(() => document.querySelectorAll("[data-testid=factory-gate-card]").length).toBe(1);
  await expect.element(page.getByTestId("factory-gate-stack")).toHaveTextContent("gate 2");
});

test("a gate that survives a Host failover goes unavailable and back to answerable as later pages attest it", () => {
  // harness v0.39.0 keeps the gate open across a restore; Factory reads it
  // `unavailable` while no Host owns the session. The board keeps the card and
  // takes each page's attestation — last page wins — with no journal event.
  let state = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 3, "resident")]), events: [],
  });
  const answerable = (): boolean[] => publicGates(state.board).map(acceptsResidentResponse);
  expect(answerable()).toStrictEqual([true]);
  state = foldFactoryGateView(state, {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 3, "unavailable")]), events: [],
  });
  expect(answerable()).toStrictEqual([false]);
  state = foldFactoryGateView(state, {
    generation: 0, sessionId: SID, page: gatePage([gateRecord(GATE_A, 3, "resident")]), events: [],
  });
  expect(answerable()).toStrictEqual([true]);
});

test("a resolve naming the PUBLIC session id closes the page's gate; a runtime-id body never could", () => {
  // Before host v0.10.0 a Host relayed bodies carrying the RUNTIME session id,
  // so a live GateResolved never matched the gate the page listed under the
  // public id. Bodies now carry the public id, and the tombstone applies.
  const RUNTIME_SID = "33333333-3333-4333-8333-333333333333";
  const page = gatePage([gateRecord(GATE_A, 3)]);
  const stale = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page, events: [resolvedEvent(GATE_A, 5, RUNTIME_SID)],
  });
  expect(publicGates(stale.board).filter((gate) => gate.sessionId === SID)).toHaveLength(1);
  const current = foldFactoryGateView(emptyFactoryGateBoardState(), {
    generation: 0, sessionId: SID, page, events: [resolvedEvent(GATE_A, 5)],
  });
  expect(publicGates(current.board)).toHaveLength(0);
});
