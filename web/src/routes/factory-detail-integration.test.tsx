/**
 * U5.2 step 5, at the APPLICATION level.
 *
 * Every case here mounts the whole composed application —
 * `createAppRouter` → `FactoryIdentityProvider` → `FactorySessionDetailRoute` →
 * `useFactorySessionView` → `FactoryLinkStore` — and asserts on the RENDERED
 * DOM. The only doubles are at the two real boundaries: `fetch`
 * (`FactoryPlane`) and the socket (`FakeClientLink`). `FactoryRestReads`,
 * `joinFactorySessionView`, the schema validation and the page components are
 * all the production code.
 *
 * That is the distinction the criteria list turns on. `packages/react`'s
 * `use-session-view.test.tsx` covers the same seven situations against
 * `FactoryColdJoin`'s snapshot, with `FactoryReads` replaced wholesale; a
 * snapshot field moving is not the same claim as a transcript staying on the
 * screen, and step 5 is about the application.
 *
 * ## What is NOT here, and why
 *
 * U3.2 step 3's "Host/workspace already gone" case wanted a REAL Host behind
 * this. It is not reachable from `wui` today and this file does not pretend
 * otherwise: `factory.Server` exposes no `http.Handler` for the API — the
 * router is `factory/internal/httpapi.Router`, unexported, and there is no
 * `factory/cmd` — so there is no Factory endpoint for a browser to talk to
 * until Factory task A9.1 ships one. What IS reachable, and is covered below,
 * is the client half of the property: with zero Hosts (`residency: "cold"`) the
 * durable projection, transcript and retained capture bytes all render, and
 * when the retained object is gone the failure is local to that object.
 */
import { page, userEvent } from "vitest/browser";
import { expect, test } from "vitest";
import { render } from "vitest-browser-react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { createAppRouter } from "../router";
import { FactoryLinkProbe, type FakeClientLink } from "../test/fakes";
import { FactoryPlane, enduringFor, publicEvent } from "../test/factory-plane";

const SID = "44444444-4444-4444-4444-444444444444";
const OTHER = "55555555-5555-5555-5555-555555555555";

interface App {
  probe: FactoryLinkProbe;
  plane: FactoryPlane;
  router: ReturnType<typeof createAppRouter>;
}

function compose(plane: FactoryPlane, path = `/sessions/${SID}`): App {
  const probe = new FactoryLinkProbe();
  probe.plane = plane;
  const router = createAppRouter({
    history: createMemoryHistory({ initialEntries: [path] }),
    factory: { options: probe.options() },
  });
  return { probe, plane, router };
}

/** The one link the application built, once its provider's effect has run. */
async function link(app: App): Promise<FakeClientLink> {
  await expect.poll(() => app.probe.links.length).toBe(1);
  return app.probe.only();
}

/** The `journal_seq` of every event currently on screen, in render order. */
function rendered(): number[] {
  return [...document.querySelectorAll("[data-testid^=factory-event-]")]
    .map((node) => Number(node.getAttribute("data-testid")!.slice("factory-event-".length)));
}

/**
 * A `StepDone` carrying one retained tool capture.
 *
 * `captured_bytes` is stated separately from the object's real length so a test
 * can describe an over-ceiling capture without allocating one.
 */
function captureEvent(sequence: number, objectId: string, capturedBytes: number) {
  return {
    event_id: `event-${sequence}`,
    journal_seq: sequence,
    body: {
      type: "StepDone",
      captures: [{
        tool_use_id: "tool-1", tool_execution_id: "execution-1",
        reference: { object_id: objectId },
        captured_bytes: capturedBytes, original_bytes: capturedBytes,
        truncated: false, encoding: "utf-8",
      }],
    },
  };
}

test("a refresh reconciles three events committed between the cold capture and authorization", async () => {
  // The browser-refresh case: REST captures an immutable tip, the journal moves
  // on before the socket is authorized, and the joined view must show BOTH.
  const plane = new FactoryPlane();
  plane.setPage(SID, 1, [1]);
  const app = compose(plane);
  const probe = app.probe;
  probe.clientLinkHook = (built) => { built.holdConnect = true; };
  render(<RouterProvider router={app.router} />);

  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();
  expect(rendered()).toStrictEqual([1]);

  plane.setPage(SID, 4, [1, 2, 3, 4]);
  (await link(app)).settleConnect();

  await expect.element(page.getByTestId("factory-event-4")).toBeInTheDocument();
  expect(rendered()).toStrictEqual([1, 2, 3, 4]);
  // A refresh re-reads a bounded TAIL. Sequence zero is never asked for.
  expect(plane.journalRequests.every((request) => request.tail === "256")).toBe(true);
});

test("a Factory replica change repairs the rendered transcript and ignores the old replica's frames", async () => {
  const plane = new FactoryPlane();
  plane.setPage(SID, 2, [1, 2]);
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("factory-event-2")).toBeInTheDocument();

  const live = await link(app);
  await expect.poll(() => live.open.length).toBe(1);
  const stale = live.open[0]!;
  plane.setPage(SID, 5, [1, 2, 3, 4, 5]);
  live.drop();

  await expect.element(page.getByTestId("factory-event-5")).toBeInTheDocument();
  expect(rendered()).toStrictEqual([1, 2, 3, 4, 5]);
  // The superseded replica's channel is closed. A frame it hands up afterwards
  // is not a durable event and must not enter the transcript.
  stale.deliver(enduringFor(SID, 99));
  await expect.poll(() => rendered()).toStrictEqual([1, 2, 3, 4, 5]);
});

test("an overflowing live buffer repairs from the durable plane rather than rendering a gap", async () => {
  const plane = new FactoryPlane();
  plane.setPage(SID, 1, [1]);
  const app = compose(plane);
  app.probe.clientLinkHook = (built) => { built.holdConnect = true; };
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();

  const live = await link(app);
  plane.holdJournal();
  live.settleConnect();
  await expect.poll(() => live.open.length).toBe(1);
  // 257 admissible frames against a 256-publication pre-join buffer, while the
  // read they would be reconciled against is held. Sequence 1 is at the cold
  // capture's floor and is dropped before buffering, so the flood must run to
  // 258 to be one over — the bound is on what is BUFFERED, not on what arrived.
  const subscription = live.open[0]!;
  for (let sequence = 1; sequence <= 258; sequence++) subscription.deliver(enduringFor(SID, sequence));

  // The durable plane's own answer is far BELOW the flood, so a buffered frame
  // that leaked into the view would be visible above this tip.
  //
  // What this row is and is not, measured rather than asserted. It is the
  // application-level statement of the criterion: a flooded live buffer renders
  // the durable page and never a fabricated one. It is NOT the sole killer of
  // `FactorySignalQueue`'s overflow branch — three mutants of that branch were
  // run against this file (drop-oldest `shift()`, discard without
  // `requiresRepair`, and both) and all three still render exactly this and
  // still rejoin, because `mergeFactoryEvents` fails closed on the same
  // non-contiguous survivors and repairs anyway. Two independent guards reach
  // one screen. The discard mechanism's own killer is in
  // `packages/protocol`'s backpressure tests, which is the right layer for it.
  plane.setPage(SID, 10, [10]);
  plane.settleJournal();

  await expect.element(page.getByTestId("factory-event-10")).toBeInTheDocument();
  expect(rendered()).toStrictEqual([1, 10]);
  // And it REPAIRED rather than quietly continuing on the connection whose
  // buffer it just threw away: the binding rejoined, so the durable page it
  // rendered was read after a fresh subscribe rather than being whatever the
  // flooded connection happened to hold.
  expect(live.subscriptions.length).toBeGreaterThan(1);
});

test.each([
  ["session_not_found", 404],
  ["not_authorized", 403],
] as const)("an authoritative %s clears the rendered transcript", async (code, httpStatus) => {
  const plane = new FactoryPlane();
  plane.setPage(SID, 2, [1, 2]);
  const app = compose(plane);
  app.probe.clientLinkHook = (built) => { built.holdConnect = true; };
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("factory-event-2")).toBeInTheDocument();

  plane.failStatus(SID, { httpStatus, code });
  (await link(app)).settleConnect();

  await expect.element(page.getByTestId("detail-read-error")).toBeInTheDocument();
  expect(rendered()).toStrictEqual([]);
  expect(document.querySelector("[data-testid=detail-durable-state]")).toBeNull();
});

test("a deleted retained object fails locally while the surrounding transcript stays rendered", async () => {
  // The OBJECT direction of the pair above, and it must not behave like it.
  // `session_not_found` is Factory saying the session is gone, so the cached
  // projection is revoked; an object that is no longer retained says nothing
  // about the session, and unrelated history has to survive it.
  const bytes = new TextEncoder().encode("retained tool output");
  const plane = new FactoryPlane();
  await plane.retain("object-1", bytes);
  plane.session(SID).status = {
    session_id: SID, agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 2,
  };
  plane.session(SID).page = {
    journal_tip: 2, covered_through: 2,
    events: [captureEvent(1, "object-1", bytes.length), publicEvent(2, "a later public event")],
  };
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("tool-capture-viewer")).toBeInTheDocument();
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("retained tool output");

  // The object is reaped between the two reads, so this is a DELETION rather
  // than an object that was never there: the same viewer, same capture summary,
  // and only the retained bytes have gone.
  plane.discard("object-1");
  await userEvent.click(page.getByTestId("tool-capture-load"));

  await expect.element(page.getByTestId("tool-capture-error"))
    .toHaveTextContent("The retained result is no longer available.");
  expect(document.querySelector("[data-testid=tool-capture-output]")).toBeNull();
  expect(rendered()).toStrictEqual([1, 2]);
  await expect.element(page.getByTestId("factory-event-2")).toHaveTextContent("a later public event");
  await expect.element(page.getByTestId("detail-durable-state")).toBeInTheDocument();
  expect(document.querySelector("[data-testid=detail-read-error]")).toBeNull();
});

test("a session with no Host renders its durable projection and its retained capture bytes", async () => {
  // U3.2 step 3's reachable half. `residency: "cold"` is zero Hosts; nothing
  // below reaches a Host, and the bytes still arrive, whole-digest verified.
  const bytes = new TextEncoder().encode("retained tool output");
  const plane = new FactoryPlane();
  await plane.retain("object-1", bytes);
  plane.session(SID).status = {
    session_id: SID, agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 1,
  };
  plane.session(SID).page = {
    journal_tip: 1, covered_through: 1, events: [captureEvent(1, "object-1", bytes.length)],
  };
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("detail-durable-state")).toHaveTextContent("cold");

  await userEvent.click(page.getByTestId("tool-capture-load"));

  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("retained tool output");
  await expect.element(page.getByTestId("tool-capture-verified")).toHaveTextContent("whole capture verified");
});

test("live callbacks for a superseded session cannot render into the one now on screen", async () => {
  const plane = new FactoryPlane();
  plane.setPage(SID, 1, [1]);
  plane.setPage(OTHER, 7, [7]);
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();

  const live = await link(app);
  await expect.poll(() => live.open.length).toBe(1);
  const stale = live.open[0]!;

  await app.router.navigate({ to: "/sessions/$sid", params: { sid: OTHER } });
  await expect.element(page.getByTestId("factory-event-7")).toBeInTheDocument();

  stale.deliver(enduringFor(SID, 99));
  await expect.poll(() => rendered()).toStrictEqual([7]);
  await expect.element(page.getByTestId("detail-session-id")).toHaveTextContent(OTHER);
});

test("a capture above the viewer's own ceiling is refused before any object request", async () => {
  // The ceiling is written as a LITERAL here, not read from the module under
  // test. Reading the constant would make this capture "one over whatever the
  // constant says", which is true for every value it could hold and therefore
  // pins none of them. With the literal, raising the constant admits this
  // capture and fails both assertions below.
  const plane = new FactoryPlane();
  plane.session(SID).status = {
    session_id: SID, agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 1,
  };
  plane.session(SID).page = {
    journal_tip: 1, covered_through: 1,
    events: [captureEvent(1, "object-1", 1024 * 1024 + 1)],
  };
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("tool-capture-load")).toBeInTheDocument();

  await userEvent.click(page.getByTestId("tool-capture-load"));

  await expect.element(page.getByTestId("tool-capture-error"))
    .toHaveTextContent("Full result exceeds the viewer's 1 MiB limit.");
  expect(plane.objectRequests).toStrictEqual([]);
});

test("a capture at exactly the ceiling is admitted and reaches the object plane", async () => {
  // The other side of the same boundary, and the reason the literal matters:
  // a LOWERED ceiling refuses this capture, so only the value 1024 * 1024
  // passes both rows.
  const plane = new FactoryPlane();
  plane.session(SID).status = {
    session_id: SID, agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 1,
  };
  plane.session(SID).page = {
    journal_tip: 1, covered_through: 1,
    events: [captureEvent(1, "object-1", 1024 * 1024)],
  };
  const app = compose(plane);
  render(<RouterProvider router={app.router} />);
  await expect.element(page.getByTestId("tool-capture-load")).toBeInTheDocument();

  await userEvent.click(page.getByTestId("tool-capture-load"));

  // The object is not retained, so the read fails AT the plane rather than
  // before it — which is the whole point: it was not refused by the ceiling.
  await expect.poll(() => plane.objectRequests.map((request) => request.kind)).toStrictEqual(["metadata"]);
});
