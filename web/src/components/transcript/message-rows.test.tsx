import { page } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import type { ContentBlock } from "@looprig/client";
import { AgentProse } from "./agent-prose";
import { UserBubble } from "./user-bubble";

function node(testId: string): Element {
  const found = document.querySelector(`[data-testid=${testId}]`);
  if (!found) throw new Error(`no element with data-testid=${testId}`);
  return found;
}

function text(blocks: string[]): ContentBlock[] {
  return blocks.map((value) => ({ type: "text", text: value }));
}

describe("UserBubble", () => {
  it("dims the presenter frame around the user's own blocks and names the sender", async () => {
    render(<UserBubble blocks={text(["add milk"])}
      frame={{ prefix: text(["[from: Alex]"]), suffix: text(["[end]"]) }}
      principal={{ tenant: "acme", subject: "user_alex", kind: "actor" }} />);
    await expect.element(page.getByTestId("user-from-chip")).toHaveTextContent("from user_alex");
    const frame = [...document.querySelectorAll("[data-testid=user-frame-block]")];
    expect(frame.map((item) => item.textContent)).toEqual(["[from: Alex]", "[end]"]);
    for (const item of frame) expect(item.className).toContain("opacity-60");
    const all = [...node("user-bubble").querySelectorAll("[data-testid=user-frame-block],[data-testid=user-text]")];
    expect(all.map((item) => item.textContent)).toEqual(["[from: Alex]", "add milk", "[end]"]);
  });

  it("keeps a pre-feature user bubble free of frame blocks and sender chip", async () => {
    render(<UserBubble blocks={text(["plain"])} />);
    await expect.element(page.getByTestId("user-bubble")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=user-from-chip]")).toBeNull();
    expect(document.querySelector("[data-testid=user-frame-block]")).toBeNull();
  });
  it("right-aligns the turn and marks a pending row as not yet accepted", async () => {
    render(<UserBubble blocks={text(["run the tests"])} pending />);
    const bubble = page.getByTestId("user-bubble");
    await expect.element(bubble).toBeInTheDocument();
    expect(bubble.element().textContent).toContain("run the tests");
    expect(bubble.element().getAttribute("data-pending")).toBe("true");
    expect(node("user-row").className).toContain("items-end");
  });

  it("is not marked pending once the server has acknowledged the turn", async () => {
    render(<UserBubble blocks={text(["run the tests"])} />);
    const bubble = page.getByTestId("user-bubble");
    await expect.element(bubble).toBeInTheDocument();
    expect(bubble.element().getAttribute("data-pending")).toBe("false");
  });

  it("renders every text block in order", async () => {
    render(<UserBubble blocks={text(["first", "second"])} />);
    await expect.element(page.getByTestId("user-bubble")).toBeInTheDocument();
    expect([...document.querySelectorAll("[data-testid=user-text]")].map((n) => n.textContent)).toEqual([
      "first",
      "second",
    ]);
  });

  it("marks a block it cannot render rather than dropping it", async () => {
    // A user message really can carry a tool_result (a subagent hand-back) or
    // an image. Rendering only the text blocks would show an EMPTY bubble for
    // a turn that carried content — indistinguishable from a bug, and the
    // reason blocks.ts keeps unrecognised blocks as an opaque variant rather
    // than discarding them.
    render(
      <UserBubble
        blocks={[
          { type: "text", text: "here" },
          { type: "tool_result", toolUseId: "toolu_1", content: [], isError: false },
          { type: "other", wireType: "image", raw: {} },
        ]}
      />,
    );
    await expect.element(page.getByTestId("user-bubble")).toBeInTheDocument();
    expect([...document.querySelectorAll("[data-testid=user-other-block]")].map((n) => n.textContent)).toEqual(
      ["tool_result", "image"],
    );
  });
});

describe("AgentProse", () => {
  it("renders prose against a lime origin rail", async () => {
    render(<AgentProse text="I'll start by reading the file." thinking="" refusal="" redactedThinking={false} />);
    const prose = page.getByTestId("agent-prose");
    await expect.element(prose).toBeInTheDocument();
    expect(prose.element().textContent).toBe("I'll start by reading the file.");
    // The 2px lime rail is §12's origin signature: lime marks the loop.
    expect(getComputedStyle(node("agent-rail")).backgroundColor).toBe("rgb(212, 248, 77)");
  });

  it("renders sealed reasoning muted and italic, above the prose", async () => {
    render(<AgentProse text="on it" thinking="the parser is the likely culprit" refusal="" redactedThinking={false} />);
    const thinking = page.getByTestId("agent-thinking");
    await expect.element(thinking).toBeInTheDocument();
    expect(thinking.element().textContent).toBe("the parser is the likely culprit");
    const style = getComputedStyle(thinking.element());
    expect(style.fontStyle).toBe("italic");
    expect(style.color).toBe("rgb(139, 144, 150)");
    expect(node("agent-thinking").compareDocumentPosition(node("agent-prose"))).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it("omits the reasoning block entirely when the step carried none", async () => {
    render(<AgentProse text="on it" thinking="" refusal="" redactedThinking={false} />);
    await expect.element(page.getByTestId("agent-prose")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=agent-thinking]")).toBeNull();
    expect(document.querySelector("[data-testid=agent-redacted]")).toBeNull();
  });

  it("says reasoning was withheld even when the step has no visible content at all", async () => {
    // A redacted thinking block projects thinking === "" (rows.ts: the provider
    // bytes never reach the browser, only the FACT of redaction). Without this
    // marker such a step renders as nothing and the turn has a hole in it.
    render(<AgentProse text="" thinking="" refusal="" redactedThinking />);
    const redacted = page.getByTestId("agent-redacted");
    await expect.element(redacted).toBeInTheDocument();
    expect(redacted.element().textContent).toContain("withheld");
  });

  it("renders a refusal as a refusal, never as ordinary prose", async () => {
    // blocks.ts keeps the refusal tag for exactly this: byte-identical to a
    // text block's payload, and the tag is the only thing that stops a refusal
    // reading as the agent's own narration.
    render(<AgentProse text="" thinking="" refusal="I can't help with that." redactedThinking={false} />);
    const refusal = page.getByTestId("agent-refusal");
    await expect.element(refusal).toBeInTheDocument();
    expect(refusal.element().textContent).toContain("I can't help with that.");
    expect(getComputedStyle(refusal.element()).color).toBe("rgb(255, 107, 107)");
    expect(document.querySelector("[data-testid=agent-prose]")).toBeNull();
  });
});
