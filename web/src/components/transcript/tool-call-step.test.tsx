import { page, userEvent } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import type { ToolRow } from "@looprig/client";

import { ToolCallStep } from "./tool-call-step";

const base: ToolRow = {
  kind: "tool",
  ordinal: 3,
  loopId: "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f",
  turnId: "3e4f5a6b-7c8d-4e9f-8a0b-1c2d3e4f5a6b",
  journalSeq: 7,
  live: false,
  orphanedLoop: false,
  toolUseId: "toolu_1",
  toolExecutionId: "",
  toolName: "bash",
  summary: "go test ./...",
  status: "ok",
  result: "ok  github.com/looprig/wui\t0.4s",
  spawnedLoopId: "",
};

function row(overrides: Partial<ToolRow> = {}): ToolRow {
  return { ...base, ...overrides };
}

function node(selector: string): Element {
  const found = document.querySelector(selector);
  if (!found) throw new Error(`no element matching ${selector}`);
  return found;
}

function dot(): Element {
  return node("[data-testid=tool-step-line] [data-testid=status-dot]");
}

describe("ToolCallStep", () => {
  it("renders collapsed as one line and hides the result", async () => {
    render(<ToolCallStep row={row()} />);
    const summary = page.getByTestId("tool-step-summary");
    await expect.element(summary).toBeInTheDocument();
    expect(summary.element().textContent).toBe("bash · go test ./...");
    expect(document.querySelector("[data-testid=tool-step-toggle]")?.getAttribute("aria-expanded")).toBe(
      "false",
    );
    expect(document.querySelector("[data-testid=tool-step-output]")).toBeNull();
  });

  it("expands to the result block on click and collapses again", async () => {
    render(<ToolCallStep row={row()} />);
    await userEvent.click(page.getByTestId("tool-step-toggle"));
    const output = page.getByTestId("tool-step-output");
    await expect.element(output).toBeInTheDocument();
    expect(output.element().textContent).toContain("ok  github.com/looprig/wui");
    expect(document.querySelector("[data-testid=tool-step-toggle]")?.getAttribute("aria-expanded")).toBe(
      "true",
    );

    await userEvent.click(page.getByTestId("tool-step-toggle"));
    await expect.element(page.getByTestId("tool-step-summary")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=tool-step-output]")).toBeNull();
  });

  it("offers no expand affordance for a call that produced no result", async () => {
    // A running call's `result` is "". A toggle that expands to nothing is a
    // control that lies about having something behind it.
    render(<ToolCallStep row={row({ status: "running", result: "" })} />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(document.querySelector("[data-testid=tool-step-toggle]")).toBeNull();
  });

  it("pulses lime while running", async () => {
    render(<ToolCallStep row={row({ status: "running", result: "" })} />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(dot().getAttribute("data-status")).toBe("running");
    expect(dot().className).toContain("animate-loop-pulse");
    expect(getComputedStyle(dot()).backgroundColor).toBe("rgb(212, 248, 77)");
  });

  it("colors a failed call red and names the failure in the line", async () => {
    render(<ToolCallStep row={row({ status: "error", result: "exit status 1" })} />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(dot().getAttribute("data-status")).toBe("failed");
    expect(getComputedStyle(dot()).backgroundColor).toBe("rgb(255, 107, 107)");
    expect(document.querySelector("[data-testid=tool-step-status]")?.textContent).toBe("failed");
  });

  it("distinguishes a cancelled call from a successful one", async () => {
    // `cancelled` and `ok` are BOTH terminal and neither is a failure, so they
    // share the gray dot — the word is what tells them apart, and dropping it
    // would report an interrupted tool call as a completed one.
    render(<ToolCallStep row={row({ status: "cancelled", result: "" })} />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(dot().getAttribute("data-status")).toBe("idle");
    expect(document.querySelector("[data-testid=tool-step-status]")?.textContent).toBe("cancelled");
  });

  it("says nothing extra about a successful call", async () => {
    render(<ToolCallStep row={row()} />);
    await expect.element(page.getByTestId("tool-step-line")).toBeInTheDocument();
    expect(dot().getAttribute("data-status")).toBe("idle");
    expect(document.querySelector("[data-testid=tool-step-status]")).toBeNull();
  });

  it("renders a tool with no summary as a bare name rather than a dangling separator", async () => {
    // `summary` is "" for a tool the redacting summariser does not know.
    render(<ToolCallStep row={row({ summary: "" })} />);
    const summary = page.getByTestId("tool-step-summary");
    await expect.element(summary).toBeInTheDocument();
    expect(summary.element().textContent).toBe("bash");
  });

  it("keeps the whole line mono — every part of it is a machine fact", async () => {
    render(<ToolCallStep row={row()} />);
    await expect.element(page.getByTestId("tool-step-summary")).toBeInTheDocument();
    expect(getComputedStyle(node("[data-testid=tool-step-summary]")).fontFamily).toContain("JetBrains Mono");
  });
});
