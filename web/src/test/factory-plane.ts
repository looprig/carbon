/**
 * A programmable durable Factory plane, answered over the probe's `fetch`.
 *
 * ## Why this is not the React package's `FakeFactoryReads`
 *
 * `packages/react/src/testing/fake-reads.ts` replaces `FactoryReads` — the
 * seven-method interface — with a double. That is the right seam for a hook
 * test, and the wrong one for an APPLICATION test: it removes
 * `FactoryRestReads` from the tree, and with it the URL spelling, the
 * `?tail=`/`?limit=`/`?cursor=` query construction, the Core error envelope
 * decode, the `206`/`Content-Range` bound on an object read and the schema
 * validation of every response. Those are exactly the layers an app-level
 * criterion is about, so this double sits UNDER them, at `fetch`, and the real
 * `FactoryRestReads` runs.
 *
 * It answers only the durable read plane. Realtime is `FakeClientLink`; the
 * bootstrap and recent-session routes stay on `FactoryLinkProbe`, which already
 * owns them.
 *
 * ## What it is stricter about than a hand-written responder
 *
 * Every response is built from a Core contract shape, and a failure is a real
 * `error_envelope` at a real HTTP status, so `errorFromCoreEnvelope` decides
 * the error class rather than a test naming one. A held read honours its
 * `AbortSignal` by rejecting with an `AbortError`, because the production
 * mapping in `FactoryRestReads.request` turns exactly that into
 * `RequestAbortedError` — a fake that left an aborted read pending forever
 * would make a cancellation path untestable rather than passing.
 */
import type {
  EnduringPublication,
  FactorySessionStatus,
  PublicGatePage,
  PublicJournalPage,
} from "@looprig/client";

/** The tenant `FactoryLinkProbe.bootstrapResult` hands the application. */
const TENANT = "tenant-1";

export function publicEvent(sequence: number, text = `public event ${sequence}`): PublicJournalPage["events"][number] {
  return { event_id: `event-${sequence}`, journal_seq: sequence, body: { type: "session.message", text } };
}

/** One committed public event, as the channel publishes it. */
export function enduringFor(
  sessionId: string,
  sequence: number,
  coveredThrough = sequence,
): EnduringPublication {
  return {
    type: "enduring_publication",
    tenant_id: TENANT,
    session_id: sessionId,
    event_id: `event-${sequence}`,
    journal_seq: sequence,
    covered_through: coveredThrough,
    body: publicEvent(sequence).body,
  };
}

export interface PlaneFailure {
  /** The HTTP status the envelope is served at. Any non-2xx will do. */
  httpStatus: number;
  /** A Core error code; `errorFromCoreEnvelope` maps it to the error class. */
  code: string;
  message?: string;
  retryable?: boolean;
}

interface SessionPlane {
  status: FactorySessionStatus;
  gates: PublicGatePage;
  page: PublicJournalPage;
  /** Answered for status, gates AND journal: an authoritative denial is not
   * per-route, and a plane that failed only one would let a test pass on a
   * sibling read the real Factory would have refused too. */
  statusFailure: PlaneFailure | undefined;
}

interface RetainedObject {
  bytes: Uint8Array<ArrayBuffer>;
  /** `sha256:<64 hex>`, computed from `bytes`, unless a test overrode it. */
  digest: string;
}

export interface JournalRequest {
  sessionId: string;
  tail: string | null;
  limit: string | null;
  cursor: string | null;
}

export interface ObjectRequest {
  sessionId: string;
  objectId: string;
  kind: "metadata" | "range";
  range: string | null;
}

function envelope(failure: PlaneFailure): Response {
  return new Response(
    JSON.stringify({
      error: {
        code: failure.code,
        message: failure.message ?? failure.code,
        retryable: failure.retryable ?? false,
      },
      request_id: "request-1",
    }),
    { status: failure.httpStatus, headers: { "Content-Type": "application/json" } },
  );
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
}

export async function sha256Hex(bytes: Uint8Array<ArrayBuffer>): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export class FactoryPlane {
  readonly journalRequests: JournalRequest[] = [];
  readonly objectRequests: ObjectRequest[] = [];

  readonly #sessions = new Map<string, SessionPlane>();
  readonly #objects = new Map<string, RetainedObject>();
  #heldJournal: Array<() => void> | undefined;

  /** The session record, created with an empty cold projection on first touch. */
  session(sessionId: string): SessionPlane {
    let state = this.#sessions.get(sessionId);
    if (state === undefined) {
      state = {
        status: {
          session_id: sessionId, agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 0,
        },
        gates: { journal_tip: 0, open_gate_count: 0, gates: [] },
        page: { journal_tip: 0, covered_through: 0, events: [] },
        statusFailure: undefined,
      };
      this.#sessions.set(sessionId, state);
    }
    return state;
  }

  /**
   * Moves a session's tip and the events its captured tail returns together.
   * They MUST move together: `#readCold` refuses a capture whose projection and
   * tail disagree, so setting one alone models no server.
   */
  setPage(sessionId: string, tip: number, sequences: readonly number[]): void {
    const state = this.session(sessionId);
    state.status = { ...state.status, journal_tip: tip };
    state.page = { journal_tip: tip, covered_through: tip, events: sequences.map((sequence) => publicEvent(sequence)) };
  }

  failStatus(sessionId: string, failure: PlaneFailure): void {
    this.session(sessionId).statusFailure = failure;
  }

  /** Registers a retained object's bytes and its true whole-object digest. */
  async retain(objectId: string, bytes: Uint8Array<ArrayBuffer>): Promise<void> {
    this.#objects.set(objectId, { bytes, digest: `sha256:${await sha256Hex(bytes)}` });
  }

  /** Removes the object. Later reads answer `object_not_found` at 404. */
  discard(objectId: string): void {
    this.#objects.delete(objectId);
  }

  /** Holds every subsequent journal read until `settleJournal()`. */
  holdJournal(): void {
    this.#heldJournal ??= [];
  }

  /** Releases every held journal read against the CURRENT page. */
  settleJournal(): void {
    const held = this.#heldJournal ?? [];
    this.#heldJournal = undefined;
    for (const release of held) release();
  }

  /** `undefined` when this plane does not own the route, so the probe answers. */
  respond(url: URL, init: RequestInit | undefined): Promise<Response> | undefined {
    const object = /^\/v1\/sessions\/([^/]+)\/objects\/([^/]+?)(\/metadata)?$/.exec(url.pathname);
    if (object !== null) {
      const sessionId = decodeURIComponent(object[1]!);
      const objectId = decodeURIComponent(object[2]!);
      const range = new Headers(init?.headers).get("Range");
      this.objectRequests.push({
        sessionId, objectId, kind: object[3] === undefined ? "range" : "metadata", range,
      });
      const retained = this.#objects.get(objectId);
      if (retained === undefined) {
        return Promise.resolve(envelope({
          httpStatus: 404, code: "object_not_found", message: "The retained object no longer exists.",
        }));
      }
      if (object[3] !== undefined) {
        return Promise.resolve(json({
          reference: { object_id: objectId }, size_bytes: retained.bytes.length, digest: retained.digest,
          media_type: "text/plain",
        }));
      }
      const bounds = /^bytes=(\d+)-(\d+)$/.exec(range ?? "");
      if (bounds === null) return Promise.resolve(new Response(null, { status: 416 }));
      const start = Number(bounds[1]);
      const end = Number(bounds[2]);
      return Promise.resolve(new Response(retained.bytes.slice(start, end + 1), {
        status: 206,
        headers: {
          "Content-Range": `bytes ${start}-${end}/${retained.bytes.length}`,
          "Content-Type": "text/plain",
        },
      }));
    }

    const status = /^\/v1\/sessions\/([^/]+)\/status$/.exec(url.pathname);
    if (status !== null) {
      const state = this.session(decodeURIComponent(status[1]!));
      return Promise.resolve(state.statusFailure === undefined ? json(state.status) : envelope(state.statusFailure));
    }

    const gates = /^\/v1\/sessions\/([^/]+)\/gates$/.exec(url.pathname);
    if (gates !== null) {
      const state = this.session(decodeURIComponent(gates[1]!));
      return Promise.resolve(state.statusFailure === undefined ? json(state.gates) : envelope(state.statusFailure));
    }

    const journal = /^\/v1\/sessions\/([^/]+)\/journal$/.exec(url.pathname);
    if (journal !== null) {
      const sessionId = decodeURIComponent(journal[1]!);
      this.journalRequests.push({
        sessionId,
        tail: url.searchParams.get("tail"),
        limit: url.searchParams.get("limit"),
        cursor: url.searchParams.get("cursor"),
      });
      const answer = (): Response => {
        const state = this.session(sessionId);
        if (state.statusFailure !== undefined) return envelope(state.statusFailure);
        return json(state.page);
      };
      const held = this.#heldJournal;
      if (held === undefined) return Promise.resolve(answer());
      return new Promise<Response>((resolve, reject) => {
        const signal = init?.signal;
        const settle = (): void => resolve(answer());
        held.push(settle);
        signal?.addEventListener("abort", () => {
          // The production mapping reads exactly this: an AbortError raised by
          // `fetch` becomes `RequestAbortedError`, which the join treats as a
          // cancellation rather than a failure.
          reject(new DOMException("The operation was aborted.", "AbortError"));
        }, { once: true });
      });
    }

    return undefined;
  }
}
