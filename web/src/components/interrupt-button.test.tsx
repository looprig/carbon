import { page, userEvent } from "vitest/browser";
import { describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";
import { InterruptButton } from "./interrupt-button";

describe("InterruptButton", () => {
  it("interrupts on click", async () => {
    const onInterrupt = vi.fn().mockResolvedValue(true);
    render(<InterruptButton onInterrupt={onInterrupt} interrupting={false} error={null} />);
    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-button")).toBeInTheDocument();
    expect(onInterrupt.mock.calls).toEqual([[]]);
  });

  it("disables itself while an interrupt is in flight", async () => {
    render(<InterruptButton onInterrupt={vi.fn()} interrupting error={null} />);
    await expect.element(page.getByTestId("interrupt-button")).toBeDisabled();
    expect(page.getByTestId("interrupt-button").element().textContent).toBe("Interrupting…");
  });

  it("shows a failure inline with the error's own message", async () => {
    // A cold session 404s with session_not_found: /interrupt resolves the sid
    // against the LIVE registry, so this is a real and reachable outcome.
    const failure = new Error("session not found");
    render(<InterruptButton onInterrupt={vi.fn()} interrupting={false} error={failure} />);
    const alert = page.getByTestId("interrupt-error");
    await expect.element(alert).toBeInTheDocument();
    expect(alert.element().textContent).toBe("session not found");
    expect(alert.element().getAttribute("role")).toBe("alert");
  });

  it("says so when harness reports there was nothing to cancel", async () => {
    // `interrupted: false` is a NORMAL answer for an idle session, not a
    // failure — it never reaches `error`. Without this the button would look
    // broken every time a user hit it a moment too late.
    const onInterrupt = vi.fn().mockResolvedValue(false);
    render(<InterruptButton onInterrupt={onInterrupt} interrupting={false} error={null} />);
    await userEvent.click(page.getByTestId("interrupt-button"));
    const note = page.getByTestId("interrupt-noop");
    await expect.element(note).toBeInTheDocument();
    expect(note.element().textContent).toContain("Nothing was running");
    expect(document.querySelector("[data-testid=interrupt-error]")).toBeNull();
  });

  it("does not say that when a turn really was cancelled", async () => {
    const onInterrupt = vi.fn().mockResolvedValue(true);
    render(<InterruptButton onInterrupt={onInterrupt} interrupting={false} error={null} />);
    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-button")).toBeEnabled();
    expect(document.querySelector("[data-testid=interrupt-noop]")).toBeNull();
  });

  it("clears the nothing-to-cancel note when the user tries again", async () => {
    const onInterrupt = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    render(<InterruptButton onInterrupt={onInterrupt} interrupting={false} error={null} />);
    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-noop")).toBeInTheDocument();

    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-button")).toBeEnabled();
    expect(document.querySelector("[data-testid=interrupt-noop]")).toBeNull();
    expect(onInterrupt.mock.calls.length).toBe(2);
  });

  it("drops the note the moment a retry starts, not when it finishes", async () => {
    // Otherwise a stale "Nothing was running." sits under a button the user has
    // just pressed again, describing the PREVIOUS attempt while the current one
    // is still in flight.
    let release: (value: boolean) => void = () => {};
    const inFlight = new Promise<boolean>((resolve) => {
      release = resolve;
    });
    const onInterrupt = vi.fn().mockResolvedValueOnce(false).mockReturnValueOnce(inFlight);
    render(<InterruptButton onInterrupt={onInterrupt} interrupting={false} error={null} />);
    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-noop")).toBeInTheDocument();

    await userEvent.click(page.getByTestId("interrupt-button"));
    await expect.element(page.getByTestId("interrupt-noop")).not.toBeInTheDocument();
    release(true);
  });

  it("reads as a warning, not as a call to action", async () => {
    // §12: lime does, blue decides. Interrupting is neither — it is the human
    // taking something away, so it wears the failure colour rather than the
    // loop colour a user reaches for to make the agent go.
    render(<InterruptButton onInterrupt={vi.fn()} interrupting={false} error={null} />);
    const button = page.getByTestId("interrupt-button");
    await expect.element(button).toBeInTheDocument();
    const style = getComputedStyle(button.element());
    expect(style.color).toBe("rgb(255, 107, 107)");
    expect(style.backgroundColor).not.toBe("rgb(212, 248, 77)");
  });
});
