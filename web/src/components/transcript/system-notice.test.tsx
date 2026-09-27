import { page } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import { SystemNotice } from "./system-notice";

function node(testId: string): Element {
  const found = document.querySelector(`[data-testid=${testId}]`);
  if (!found) throw new Error(`no element with data-testid=${testId}`);
  return found;
}

describe("SystemNotice", () => {
  it("renders an info event as a thin muted divider carrying its text", async () => {
    render(<SystemNotice text="Context compacted" level="info" />);
    const notice = page.getByTestId("system-notice");
    await expect.element(notice).toBeInTheDocument();
    expect(notice.element().textContent).toBe("Context compacted");
    expect(notice.element().getAttribute("data-level")).toBe("info");
    expect(getComputedStyle(notice.element()).color).toBe("rgb(139, 144, 150)"); // muted
    expect(notice.element().getAttribute("role")).toBeNull();
    // Thin divider, not a card: the rules either side are what make it read as
    // a break in the thread rather than a message in it (capstan-spec.md §8).
    expect(document.querySelectorAll("[data-testid=system-notice-rule]").length).toBe(2);
  });

  it("lifts a warning out of the muted register without inventing a colour", async () => {
    // §12's palette has exactly two accents — lime does, blue decides — and no
    // amber. A warning painted lime would read as agent activity and painted
    // blue as a decision waiting on the human; brightening to the foreground
    // colour says "louder than info, not a failure" with the tokens that exist.
    render(<SystemNotice text="Live frames were dropped" level="warn" />);
    const notice = page.getByTestId("system-notice");
    await expect.element(notice).toBeInTheDocument();
    expect(notice.element().getAttribute("data-level")).toBe("warn");
    expect(getComputedStyle(notice.element()).color).toBe("rgb(231, 233, 234)"); // fg
    expect(notice.element().getAttribute("role")).toBeNull();
  });

  it("renders an error-level notice in red and announces it", async () => {
    // A rejected turn arrives ONLY as a notice row (design §3b: TurnRejected
    // commits a notice, never a user row), so this is the single place a
    // refused submit is ever reported. It must not scroll past silently.
    render(<SystemNotice text="Turn rejected: queue full" level="error" />);
    const notice = page.getByTestId("system-notice");
    await expect.element(notice).toBeInTheDocument();
    expect(notice.element().getAttribute("role")).toBe("alert");
    expect(getComputedStyle(notice.element()).color).toBe("rgb(255, 107, 107)"); // fail
  });

  it("keeps the text mono — a notice is a machine fact, not prose", async () => {
    render(<SystemNotice text="Context compacted" level="info" />);
    await expect.element(page.getByTestId("system-notice")).toBeInTheDocument();
    expect(getComputedStyle(node("system-notice")).fontFamily).toContain("JetBrains Mono");
  });

  it("accepts a caller-supplied test id so a reused divider stays identifiable", async () => {
    render(<SystemNotice text="Turn interrupted" level="info" testId="tombstone-row" />);
    const notice = page.getByTestId("tombstone-row");
    await expect.element(notice).toBeInTheDocument();
    expect(notice.element().textContent).toBe("Turn interrupted");
    expect(document.querySelector("[data-testid=system-notice]")).toBeNull();
  });
});
