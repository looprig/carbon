import { page, userEvent } from "vitest/browser";
import { expect, test, vi } from "vitest";
import { render } from "vitest-browser-react";
import type { FactoryLiveToolStep, FactoryReads } from "@looprig/client";
import type { UseFactorySessionViewResult } from "@looprig/react";
import { FactorySessionDetailPage, type FactoryDetailComposer, type FactoryDetailGate } from "./factory-session-detail-page";

const inertReads = {} as FactoryReads;

test("shows transient text and reasoning, then leaves one durable message", async () => {
  const preview = { loopId: "loop-1", turnId: "turn-1", text: "Draft answer" };
  const reasoning = { loopId: "loop-1", turnId: "turn-1", text: "Working through it" };
  const { rerender } = await render(<FactorySessionDetailPage sid="session-1" view={view({
    events: [], liveText: [preview], liveReasoning: [reasoning],
  })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("factory-live-text")).toHaveTextContent("Draft answer");
  await expect.element(page.getByTestId("factory-live-reasoning")).toHaveTextContent("Working through it");

  await rerender(<FactorySessionDetailPage sid="session-1" view={view({
    events: [{ event_id: "event-1", journal_seq: 1, body: { type: "StepDone", text: "Final answer" } }],
    liveText: [], liveReasoning: [],
  })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("factory-event-1")).toHaveTextContent("Final answer");
  await expect.element(page.getByTestId("factory-live-text")).not.toBeInTheDocument();
  await expect.element(page.getByTestId("factory-live-reasoning")).not.toBeInTheDocument();
});

function toolStep(overrides: Partial<FactoryLiveToolStep> = {}): FactoryLiveToolStep {
  return {
    phase: "started", loopId: "loop-1", turnId: "turn-1", stepId: "step-1",
    toolExecutionId: "execution-1", toolUseId: "toolu_1", toolName: "Bash",
    summary: "ls -la", isError: false, resultPreview: "",
    ...overrides,
  };
}

test("shows a live tool step running, then completed, then leaves only the committed step", async () => {
  const preview = { loopId: "loop-1", turnId: "turn-1", text: "Let me look" };
  const { rerender } = await render(<FactorySessionDetailPage sid="session-1" view={view({
    events: [], liveText: [preview], liveToolSteps: [toolStep()],
  })} reads={inertReads} gates={[]} />);
  const line = page.getByTestId("tool-step-line");
  await expect.element(line).toHaveTextContent("Bash · ls -la");
  await expect.element(page.getByTestId("factory-live-tool")).toHaveAttribute("data-preview-key", "tool:execution-1");
  await expect.element(page.getByRole("img", { name: "running" })).toBeInTheDocument();
  // Nothing to expand while it runs.
  await expect.element(page.getByTestId("tool-step-toggle")).not.toBeInTheDocument();
  // Within the turn the tool step follows the text written before the call.
  const order = [...document.querySelectorAll("[data-testid=factory-live-text], [data-testid=factory-live-tool]")].map((node) => node.getAttribute("data-testid"));
  expect(order).toStrictEqual(["factory-live-text", "factory-live-tool"]);

  await rerender(<FactorySessionDetailPage sid="session-1" view={view({
    events: [], liveText: [preview], liveToolSteps: [toolStep({ phase: "completed", isError: true, resultPreview: "permission denied" })],
  })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("tool-step-status")).toHaveTextContent("failed");
  await userEvent.click(page.getByTestId("tool-step-toggle"));
  await expect.element(page.getByTestId("tool-step-output")).toHaveTextContent("permission denied");

  await rerender(<FactorySessionDetailPage sid="session-1" view={view({
    events: [{ event_id: "event-1", journal_seq: 1, body: { type: "StepDone", loop_id: "loop-1", turn_id: "turn-1" } }],
    liveText: [], liveToolSteps: [],
  })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();
  await expect.element(page.getByTestId("factory-live-tool")).not.toBeInTheDocument();
  expect(document.querySelectorAll("[data-testid=tool-step-line]")).toHaveLength(0);
});

test("shows who sent a message, interrupted, and answered a gate", async () => {
  const alex = { tenant: "acme", subject: "user_alex", kind: "actor" };
  render(<FactorySessionDetailPage sid="session-1" view={view({ events: [
    { event_id: "event-1", journal_seq: 1, body: { type: "TurnStarted", input: { principal: alex, prefix: 1 } } },
    { event_id: "event-2", journal_seq: 2, body: { type: "TurnInterrupted", principal: alex } },
    { event_id: "event-3", journal_seq: 3, body: { type: "GateResolved", principal: { ...alex, subject: "svc", kind: "service" } } },
    { event_id: "event-4", journal_seq: 4, body: { type: "SessionIdle" } },
  ] })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();
  const chip = (seq: number) => document.querySelectorAll(`[data-testid=factory-event-${seq}] [data-testid=factory-event-from]`);
  expect(chip(1)[0]?.textContent).toBe("from user_alex");
  expect(chip(2)[0]?.textContent).toBe("from user_alex");
  expect(chip(3)[0]?.textContent).toBe("from service svc");
  expect(chip(4)).toHaveLength(0);
});

/**
 * A folded board entry, as `useFactoryGateBoard` + `useFactoryGate` produce
 * one. The page renders THIS, not `view.gates`: the raw page keeps a gate that
 * resolved live and re-lists one whose read crossed the resolve.
 */
function gate(overrides: Partial<FactoryDetailGate> = {}): FactoryDetailGate {
  return {
    sessionId: "session-1",
    gateId: "gate-1",
    kind: "harness.permission",
    openedEventId: "event-1",
    openedJournalSeq: 1,
    deadline: "2026-09-05T13:00:00Z",
    answerability: "resident",
    answerable: true,
    prompt: { title: "Allow shell?", body: "", origin: "", controls: [] },
    ...overrides,
  };
}

function view(overrides: Partial<UseFactorySessionViewResult> = {}): UseFactorySessionViewResult {
  return {
    state: "ready",
    liveState: "live",
    status: {
      session_id: "session-1", agent_id: "agent-1", state: "idle", residency: "cold", journal_tip: 2,
    },
    gates: { journal_tip: 2, open_gate_count: 0, gates: [] },
    liveText: [],
    liveReasoning: [],
    liveToolSteps: [],
    events: [
      { event_id: "event-1", journal_seq: 1, body: { type: "TurnStarted" } },
      { event_id: "event-2", journal_seq: 2, body: { type: "SessionIdle" } },
    ],
    coveredThrough: 2,
    error: null,
    earlierState: "idle",
    browseEarlier: vi.fn(() => Promise.resolve()),
    ...overrides,
  };
}

test.each([
  ["cold", "idle", "cold"],
  ["attaching", "running", "placing"],
  ["resident", "running", "resident"],
  ["resident", "waiting_on_gate", "waiting on gate"],
  ["releasing", "idle", "releasing"],
  ["cold", "failed", "failed"],
] as const)("renders durable residency %s and execution %s as %s", async (residency, state, label) => {
  render(<FactorySessionDetailPage sid="session-1" view={view({
    status: { session_id: "session-1", agent_id: "agent-1", residency, state, journal_tip: 2 },
  })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("detail-durable-state")).toHaveTextContent(label);
});

test("keeps transport repair separate while durable content remains rendered", async () => {
  render(<FactorySessionDetailPage sid="session-1" view={view({ liveState: "repairing" })} reads={inertReads} gates={[]} />);
  await expect.element(page.getByTestId("detail-live-state")).toHaveTextContent("repairing");
  await expect.element(page.getByTestId("factory-event-2")).toHaveTextContent("SessionIdle");
});

test("loads older history only from the explicit user control", async () => {
  const browseEarlier = vi.fn(() => Promise.resolve());
  render(<FactorySessionDetailPage sid="session-1" view={view({ browseEarlier })} reads={inertReads} gates={[]} />);
  expect(browseEarlier).not.toHaveBeenCalled();

  await userEvent.click(page.getByTestId("browse-earlier-history"));
  expect(browseEarlier).toHaveBeenCalledTimes(1);
});

test("renders multiple gates while separating supported resident actions", async () => {
  const gates = [
    gate(),
    gate({ gateId: "gate-2", openedJournalSeq: 2, answerability: "suspended", answerable: false,
      prompt: { title: "Allow edit?", body: "", origin: "", controls: [] } }),
    gate({ gateId: "gate-3", openedJournalSeq: 3, kind: "harness.ask_user",
      prompt: { title: "Choose one", body: "", origin: "", controls: [] } }),
  ];
  render(<FactorySessionDetailPage sid="session-1" view={view()} reads={inertReads} gates={gates} onGateRespond={vi.fn()} />);

  await expect.element(page.getByTestId("factory-gate-stack")).toBeInTheDocument();
  expect(document.querySelectorAll("[data-testid=factory-gate-card]")).toHaveLength(3);
  expect(document.querySelectorAll("[data-testid=factory-gate-actions]")).toHaveLength(1);
  expect(document.querySelectorAll("[data-testid=factory-gate-unavailable]")).toHaveLength(2);
});

test("answers the gate the card was rendered from, so no card can act on a gate the board dropped", async () => {
  // The two used to come from different places: cards from `view.gates` (the
  // raw page) and the respond path from the folded board. A card whose gate the
  // fold had dropped rendered a button whose handler found nothing and returned
  // without effect.
  const onGateRespond = vi.fn();
  render(
    <FactorySessionDetailPage
      sid="session-1"
      view={view({ gates: { journal_tip: 4, open_gate_count: 1, gates: [] } })}
      reads={inertReads}
      gates={[gate({ gateId: "gate-live" })]}
      onGateRespond={onGateRespond}
    />,
  );

  await expect.element(page.getByTestId("factory-gate-actions")).toBeInTheDocument();
  await userEvent.click(page.getByRole("button", { name: "Deny", exact: true }));
  expect(onGateRespond.mock.calls[0]).toStrictEqual(["gate-live", "Deny"]);
});

test("renders no gate stack for a page whose gates the board has closed", async () => {
  render(<FactorySessionDetailPage sid="session-1" view={view({
    gates: { journal_tip: 4, open_gate_count: 1, gates: [{
      gate_id: "gate-1", kind: "harness.permission", prompt: { title: "Allow shell?" },
      opened_event_id: "event-1", opened_journal_seq: 1,
      deadline: "2026-09-05T13:00:00Z", answerability: "resident" as const,
    }] },
  })} reads={inertReads} gates={[]} />);

  await expect.element(page.getByTestId("detail-session-id")).toBeInTheDocument();
  expect(document.querySelector("[data-testid=factory-gate-stack]")).toBeNull();
});

test("renders repeated provider tool-use captures as distinct durable event/index viewers", async () => {
  const capture = (execution: string) => ({
    tool_use_id: "reused-tool-id",
    tool_execution_id: execution,
    reference: { object_id: `object-${execution}` },
    captured_bytes: 5,
    original_bytes: 8,
    truncated: true,
    truncation_reason: "source_limit",
    encoding: "utf-8",
  });
  render(<FactorySessionDetailPage sid="session-1" reads={inertReads} gates={[]} view={view({ events: [
    { event_id: "event-1", journal_seq: 1, body: { type: "StepDone", captures: [capture("one"), capture("two")] } },
  ] })} />);

  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();
  expect(document.querySelectorAll("[data-testid=tool-capture-viewer]")).toHaveLength(2);
  expect(document.querySelectorAll("[data-capture-instance='event-1:0']")).toHaveLength(1);
  expect(document.querySelectorAll("[data-capture-instance='event-1:1']")).toHaveLength(1);
});

test("does not interpret a captures lookalike on a non-StepDone public event", async () => {
  render(<FactorySessionDetailPage sid="session-1" reads={inertReads} gates={[]} view={view({ events: [{
    event_id: "event-1", journal_seq: 1, body: { type: "OtherEvent", captures: [{
      tool_use_id: "tool-1", tool_execution_id: "execution-1", reference: { object_id: "object-1" },
      captured_bytes: 5, original_bytes: 5, truncated: false, encoding: "utf-8",
    }] },
  }] })} />);

  await expect.element(page.getByTestId("factory-event-1")).toBeInTheDocument();
  expect(document.querySelector("[data-testid=tool-capture-viewer]")).toBeNull();
});

test.each(["not_authorized", "session_not_found"])("session %s invalidation unmounts already displayed capture bytes", async (code) => {
  const source = new TextEncoder().encode("verified private result");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", source));
  const digestHex = Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
  const reads = {
    readObjectMetadata: vi.fn(async () => ({
      reference: { object_id: "object-1" }, size_bytes: source.length, digest: `sha256:${digestHex}`,
    })),
    readObjectRange: vi.fn(async (_sid: string, _oid: string, options: { start: number; end: number }) => ({
      bytes: source.slice(options.start, options.end + 1),
      contentRange: `bytes ${options.start}-${options.end}/${source.length}`,
      mediaType: "text/plain",
    })),
  } as unknown as FactoryReads;
  const event = {
    event_id: "event-1", journal_seq: 1, body: { type: "StepDone", captures: [{
      tool_use_id: "tool-1", tool_execution_id: "execution-1", reference: { object_id: "object-1" },
      captured_bytes: source.length, original_bytes: source.length, truncated: false, encoding: "utf-8",
    }] },
  };
  const screen = await render(<FactorySessionDetailPage sid="session-1" reads={reads} gates={[]} view={view({ events: [event] })} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("verified private result");

  await screen.rerender(<FactorySessionDetailPage sid="session-1" reads={reads} gates={[]} view={view({
    state: "failed", status: null, events: [], gates: null,
    error: Object.assign(new Error(code), { code }),
  })} />);
  expect(document.querySelector("[data-testid=tool-capture-output]")).toBeNull();
  await expect.element(page.getByTestId("detail-read-error")).toBeInTheDocument();
});

test("an unavailable gate offers no answer and says it is waiting for the session, not gone", async () => {
  render(<FactorySessionDetailPage sid="session-1" view={view()} reads={inertReads} onGateRespond={vi.fn()}
    gates={[gate({ answerability: "unavailable", answerable: false })]} />);
  await expect.element(page.getByTestId("factory-gate-card")).toBeInTheDocument();
  expect(document.querySelector("[data-testid=factory-gate-actions]")).toBeNull();
  await expect.element(page.getByTestId("factory-gate-answerability")).toHaveTextContent("unavailable");
  await expect.element(page.getByTestId("factory-gate-unavailable"))
    .toHaveTextContent("Waiting for the session to be resident again");
});

function composerProps(overrides: Partial<FactoryDetailComposer> = {}): FactoryDetailComposer {
  return {
    onSubmit: vi.fn(async () => true),
    submitting: false,
    error: null,
    awaiting: [],
    own: new Set(),
    unconfirmed: null,
    onRetry: vi.fn(),
    onDiscard: vi.fn(),
    ...overrides,
  };
}

test("marks the events this tab's own admitted command caused, by cause.command_id", async () => {
  const mine = "11111111-2222-4333-8444-555555555555";
  render(<FactorySessionDetailPage sid="session-1" reads={inertReads} gates={[]} composer={composerProps({ own: new Set([mine]) })}
    view={view({ events: [
      { event_id: "event-1", journal_seq: 1, body: { type: "TurnStarted", cause: { command_id: mine } } },
      { event_id: "event-2", journal_seq: 2, body: { type: "TurnStarted", cause: { command_id: "someone-else" } } },
      { event_id: "event-3", journal_seq: 3, body: { type: "SessionIdle" } },
    ] })} />);
  await expect.element(page.getByTestId("factory-event-1")).toHaveAttribute("data-own-command", mine);
  expect(page.getByTestId("factory-event-2").element().hasAttribute("data-own-command")).toBe(false);
  expect(document.querySelectorAll("[data-testid=factory-event-own]")).toHaveLength(1);
});

test("sends input through the composer and shows it as waiting until the journal names its command", async () => {
  const onSubmit = vi.fn(async () => true);
  render(<FactorySessionDetailPage sid="session-1" reads={inertReads} gates={[]} view={view()}
    composer={composerProps({ onSubmit, awaiting: [{ commandId: "command-1", text: "and then?" }] })} />);
  await expect.element(page.getByTestId("factory-awaiting-input")).toHaveTextContent("and then?");
  await userEvent.fill(page.getByTestId("composer-input"), "keep going");
  await userEvent.click(page.getByTestId("composer-submit"));
  expect(onSubmit).toHaveBeenCalledWith("keep going");
});

test("an unconfirmed input offers retry of the same command and locks a second send", async () => {
  const onRetry = vi.fn();
  render(<FactorySessionDetailPage sid="session-1" reads={inertReads} gates={[]} view={view()}
    composer={composerProps({ unconfirmed: "first message", onRetry })} />);
  await expect.element(page.getByTestId("composer-unconfirmed")).toHaveTextContent("first message");
  await expect.element(page.getByTestId("composer-input")).toBeDisabled();
  await userEvent.click(page.getByRole("button", { name: "Retry", exact: true }));
  expect(onRetry).toHaveBeenCalledTimes(1);
});
