import { page } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";
import { StatusDot, toneFor } from "./status-dot";

describe("toneFor", () => {
  it("maps every state harness's catalog actually emits", () => {
    // harness/pkg/sessionstore/catalog.go's SessionState is a closed enum, and
    // these six strings are the whole of it. They are asserted by name because
    // the wire type is a bare string (session_summary.schema.json has no
    // enum), so a mismatch is invisible until a user sees a gray dot on a
    // session that is blocked waiting for them.
    expect(toneFor("running")).toBe("running");
    expect(toneFor("waiting_on_gate")).toBe("waiting");
    expect(toneFor("failed")).toBe("failed");
    expect(toneFor("idle")).toBe("idle");
    expect(toneFor("interrupted")).toBe("idle");
    expect(toneFor("stopped")).toBe("idle");
  });

  it("falls back to the neutral tone for an absent or unknown state", () => {
    // SessionState is `omitempty` and additive: a catalog entry written before
    // the field existed decodes as "". A future harness release can add a
    // state too. Neither is an error, and neither may crash or mis-colour.
    expect(toneFor(undefined)).toBe("idle");
    expect(toneFor("")).toBe("idle");
    expect(toneFor("something-new")).toBe("idle");
  });
});

describe("StatusDot", () => {
  // Every assertion below settles FIRST (one `await` on presence) and then
  // asserts synchronously. `await expect.element(...).toHaveClass(...)` retries
  // until its timeout, so under a mutation it burns ~15s per test before
  // failing — measured. Waiting once and asserting after turns the same
  // mutation into an instant, readable failure.
  it("renders running as lime with the pulse animation", async () => {
    render(<StatusDot state="running" />);
    const dot = page.getByTestId("status-dot");
    await expect.element(dot).toBeInTheDocument();
    const el = dot.element();
    expect(el.getAttribute("data-status")).toBe("running");
    expect(el.className).toContain("animate-loop-pulse");
    expect(el.getAttribute("aria-label")).toBe("running");
    expect(getComputedStyle(el).backgroundColor).toBe("rgb(212, 248, 77)");
  });

  it("renders waiting as blue and failed as red, neither pulsing", async () => {
    render(
      <>
        <div data-testid="w">
          <StatusDot state="waiting_on_gate" />
        </div>
        <div data-testid="f">
          <StatusDot state="failed" />
        </div>
      </>,
    );
    await expect.element(page.getByTestId("w")).toBeInTheDocument();
    const waiting = document.querySelector("[data-testid=w] [data-testid=status-dot]");
    const failed = document.querySelector("[data-testid=f] [data-testid=status-dot]");
    if (!waiting || !failed) throw new Error("both dots must render");
    expect(waiting.getAttribute("data-status")).toBe("waiting");
    expect(waiting.className).not.toContain("animate-loop-pulse");
    expect(failed.className).not.toContain("animate-loop-pulse");
    expect(getComputedStyle(waiting).backgroundColor).toBe("rgb(162, 210, 255)");
    expect(getComputedStyle(failed).backgroundColor).toBe("rgb(255, 107, 107)");
  });

  it("keeps the raw state string as the accessible name, never a rewritten one", async () => {
    // Colour is our interpretation; the state string is a machine fact and is
    // reported verbatim. An unknown state must still be readable.
    render(<StatusDot state="something-new" />);
    const dot = page.getByTestId("status-dot");
    await expect.element(dot).toBeInTheDocument();
    const el = dot.element();
    expect(el.getAttribute("data-status")).toBe("idle");
    expect(el.getAttribute("aria-label")).toBe("something-new");
    expect(getComputedStyle(el).backgroundColor).toBe("rgb(139, 144, 150)");
  });

  it("names an absent state 'unknown' rather than rendering an unlabelled dot", async () => {
    render(<StatusDot state={undefined} />);
    const dot = page.getByTestId("status-dot");
    await expect.element(dot).toBeInTheDocument();
    expect(dot.element().getAttribute("aria-label")).toBe("unknown");
  });
});
