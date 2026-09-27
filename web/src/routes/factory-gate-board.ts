import { useRef } from "react";
import {
  decodeEnduring,
  emptyPublicGateBoard,
  foldPublicGateEvent,
  foldPublicGatePage,
  publicGateKey,
  type EventEnvelope,
  type PublicGateBoard,
  type PublicGatePage,
  type StatusEvent,
} from "@looprig/client";
import type { PublicJournalEvent, UseFactorySessionViewResult } from "@looprig/react";

/**
 * The open gates of one Factory session, folded from the two sources that
 * disagree about them: the bounded `GET /gates` page, and the live journal.
 *
 * ## Why this exists at all
 *
 * `foldPublicGatePage`'s own comment states the race it cannot fix and names
 * the two shapes that can: a TOMBSTONE keyed by `(SessionID, GateID)`, or a
 * REBUILD from `emptyPublicGateBoard()` per page set. This module is the
 * tombstone, and the choice is not a preference:
 *
 *  - A rebuild does close page resurrection, and that is what the detail route
 *    did before this file existed. But `useFactorySessionView` reads
 *    `listGates(sessionId, { limit })` — a BOUNDED window with no continuation
 *    — so rebuilding makes "absent from this page" mean "closed". That is
 *    exactly the inference the requirement forbids: a gate a human still has to
 *    answer would vanish from the screen because a later page was truncated,
 *    and nothing would ever bring it back.
 *  - A rebuild also folds no live resolve. Nothing in the view removes a gate
 *    from `view.gates`; only the next `listGates` does. A gate answered in
 *    another tab therefore stayed on screen — with its attested `resident`
 *    answerability, so `acceptsResidentResponse` reported a closed gate as
 *    answerable — until a status read happened to refresh the page.
 *
 * A tombstone is the shape that says both things at once: a page MERGE never
 * removes (absence is not resolution), and a `GateResolved` this client
 * actually observed removes its own key and keeps removing it, so a page read
 * before the resolve and merged after it cannot resurrect the gate.
 *
 * ## What a tombstone is allowed to outlive
 *
 * A gate id is a durable single-use identity: it is opened once and resolved
 * once, so within one journal a permanent tombstone needs no sequence
 * comparison — and this layer has none to make, since a page carries no fetch
 * time and no tip. What it may NOT outlive is the journal it was taken from.
 * `generation` is that scope: a superseded fold applies nothing, and a NEW
 * generation drops the board and every tombstone with it, because a rewound or
 * re-scoped journal is not the one the resolve was observed in.
 */
export interface FactoryGateBoardState {
  /** The journal authority these entries and tombstones were observed under. */
  readonly generation: number;
  readonly board: PublicGateBoard;
  /** `publicGateKey(sessionId, gateId)` for every resolve this view observed. */
  readonly resolved: ReadonlySet<string>;
}

export interface FactoryGateFold {
  readonly generation: number;
  /** The session the page was READ FOR; a page does not name its own. */
  readonly sessionId: string;
  readonly page: PublicGatePage | null;
  readonly events: readonly PublicJournalEvent[];
}

const NO_TOMBSTONES: ReadonlySet<string> = new Set();

export function emptyFactoryGateBoardState(generation = 0): FactoryGateBoardState {
  return { generation, board: emptyPublicGateBoard(), resolved: NO_TOMBSTONES };
}

/**
 * Folds one observation of a session's gates.
 *
 * RETURNS THE IDENTICAL STATE when nothing applies, and the identical BOARD
 * inside it when only tombstones moved. `useFactoryGate` memoises
 * `publicGates(board)` on board identity, so a board rebuilt on every unchanged
 * poll re-renders every gate card forever.
 *
 * The whole event window is re-folded on every call rather than only its new
 * tail. That is deliberate: every operation here is idempotent — a tombstone is
 * a set member, a duplicate `GateOpened` is a referential no-op in
 * `foldPublicGateEvent`, and a page merge that changes nothing returns its own
 * argument — so re-folding cannot double-count, and a high-water mark would
 * silently skip an older `GateResolved` that arrives through `browseEarlier`.
 */
export function foldFactoryGateView(
  state: FactoryGateBoardState,
  input: FactoryGateFold,
): FactoryGateBoardState {
  if (input.generation < state.generation) return state;
  const base = input.generation > state.generation
    ? emptyFactoryGateBoardState(input.generation)
    : state;

  let board = base.board;
  let resolved = base.resolved;
  let tombstones: Set<string> | undefined;

  for (const event of input.events) {
    const body = event.body;
    // Cheap discrimination before the decode: a session's journal is mostly
    // turns and steps, and this runs on every render of the detail route.
    if (typeof body !== "object" || body === null || Array.isArray(body)) continue;
    const raw = body as Record<string, unknown>;
    const type = raw["type"];
    if (type !== "GateOpened" && type !== "GateResolved") continue;
    const sessionId = typeof raw["session_id"] === "string" ? raw["session_id"] : "";
    // An event that names no session is IGNORED rather than keyed under "",
    // which would merge every unaddressed gate in the process into one bucket.
    if (sessionId === "") continue;
    const envelope = body as unknown as EventEnvelope;
    const item: StatusEvent = { journal_seq: event.journal_seq, event: envelope };
    const decoded = decodeEnduring(envelope);
    if (decoded.payload.kind === "GateResolved") {
      const gateId = decoded.payload.gateId;
      if (gateId === "") continue;
      const key = publicGateKey(sessionId, gateId);
      if (!resolved.has(key)) {
        tombstones ??= new Set(resolved);
        tombstones.add(key);
        resolved = tombstones;
      }
      board = foldPublicGateEvent(board, { segment: "history", event: item });
      continue;
    }
    if (decoded.payload.kind === "GateOpened") {
      // A gate this view already watched close does not re-open because its
      // opening event is still in the retained window.
      if (resolved.has(publicGateKey(sessionId, decoded.payload.gate.id))) continue;
      board = foldPublicGateEvent(board, { segment: "history", event: item });
    }
  }

  if (input.page !== null) {
    board = foldPublicGatePage(board, suppressResolved(input.page, input.sessionId, resolved), input.sessionId);
  }

  if (board === state.board && resolved === state.resolved && input.generation === state.generation) {
    return state;
  }
  return { generation: input.generation, board, resolved };
}

/**
 * The page without the records this view has already watched close.
 *
 * Filtering BEFORE the merge rather than deleting after it is what keeps board
 * identity stable: `foldPublicGatePage` returns its own argument when a page
 * applies nothing, so a page whose only new record is tombstoned leaves the
 * board untouched. Merging and then deleting produced a fresh map on every
 * poll of an unchanged page, which re-renders every gate card forever.
 *
 * Returns the page itself when nothing is suppressed, for the same reason.
 */
function suppressResolved(
  page: PublicGatePage,
  sessionId: string,
  resolved: ReadonlySet<string>,
): PublicGatePage {
  if (resolved.size === 0) return page;
  const records: unknown = (page as unknown as Record<string, unknown>)["gates"];
  if (!Array.isArray(records)) return page;
  const kept = records.filter((record) => {
    const gateId = isRecord(record) && typeof record["gate_id"] === "string" ? record["gate_id"] : "";
    return gateId === "" || !resolved.has(publicGateKey(sessionId, gateId));
  });
  return kept.length === records.length ? page : { ...page, gates: kept } as PublicGatePage;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * The view the detail route hands `useFactoryGate`.
 *
 * ## Generations, and how they are observed here
 *
 * `useFactorySessionView` does not expose a generation, so this derives one
 * from the two transitions that mean "the journal these gates came from is no
 * longer the journal I am reading":
 *
 *  - `gates` going NULL after being non-null. `FactoryColdJoin` nulls the whole
 *    projection on an authoritative `unauthenticated`, `not_authorized` or
 *    `session_not_found`. Keeping a tombstone across that would let a decision
 *    taken about a session Factory says is gone suppress a gate in whatever
 *    the next authorized read returns. This condition is REDUNDANT DEFENCE
 *    today and is named as such rather than left to be mistaken for a live
 *    guard: `#rejectAccess` publishes `coveredThrough: 0` in the same snapshot
 *    that nulls `gates`, and a tombstone can only exist if a `GateResolved` at
 *    journal_seq >= 1 was observed, so the floor always falls too. Removing it
 *    alone is an equivalent mutant, measured; removing BOTH conditions fails
 *    "an authoritative invalidation drops the board and its tombstones". It
 *    stays because the two facts are independent — a projection reset that did
 *    not zero coverage would still be one — and because the intent is the
 *    invalidation, not the arithmetic.
 *  - `coveredThrough` FALLING. A lower committed floor is the protocol's own
 *    reset signal — a Factory replica change or an overflow — and the view
 *    prunes its event map on exactly the same condition.
 *
 * Both are compared against what the previous render observed, and a new
 * `sessionId` starts a fresh cell, so the derivation is idempotent: React may
 * render this component twice for one commit (StrictMode does it always), and
 * the second pass sees no transition and folds the same inputs onto a state
 * that already absorbed them. Every operation in `foldFactoryGateView` is
 * idempotent for the same reason, which is what makes a render-phase
 * accumulator safe here rather than merely convenient.
 */
export function useFactoryGateBoard(
  sessionId: string,
  view: Pick<UseFactorySessionViewResult, "gates" | "events" | "coveredThrough">,
): PublicGateBoard {
  const cell = useRef<{
    sessionId: string;
    gates: PublicGatePage | null;
    coveredThrough: number;
    state: FactoryGateBoardState;
  } | undefined>(undefined);

  const previous = cell.current !== undefined && cell.current.sessionId === sessionId
    ? cell.current
    : { sessionId, gates: null, coveredThrough: view.coveredThrough, state: emptyFactoryGateBoardState() };

  const invalidated = view.gates === null && previous.gates !== null;
  const rewound = view.coveredThrough < previous.coveredThrough;
  const generation = previous.state.generation + (invalidated || rewound ? 1 : 0);

  const state = foldFactoryGateView(previous.state, {
    generation,
    sessionId,
    page: view.gates,
    events: view.events,
  });
  cell.current = { sessionId, gates: view.gates, coveredThrough: view.coveredThrough, state };
  return state.board;
}
