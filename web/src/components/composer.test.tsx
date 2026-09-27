import { useState } from "react";
import { page, userEvent } from "vitest/browser";
import { describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";
import { Composer } from "./composer";

function input(): HTMLTextAreaElement {
  const found = document.querySelector("[data-testid=composer-input]");
  if (!(found instanceof HTMLTextAreaElement)) throw new Error("no composer input");
  return found;
}

describe("Composer", () => {
  it("submits the typed text and clears the box", async () => {
    const onSubmit = vi.fn().mockResolvedValue(true);
    render(<Composer onSubmit={onSubmit} submitting={false} gateOpen={false} error={null} />);
    await userEvent.fill(page.getByTestId("composer-input"), "run the tests");
    await userEvent.click(page.getByTestId("composer-submit"));

    await expect.element(page.getByTestId("composer-input")).toHaveValue("");
    expect(onSubmit.mock.calls).toEqual([["run the tests"]]);
  });

  it("keeps the draft when the submit did not land", async () => {
    // `useComposer.submit` resolves false for a rejected send. Clearing the box
    // anyway would destroy what the user typed on a transient network error.
    const onSubmit = vi.fn().mockResolvedValue(false);
    render(<Composer onSubmit={onSubmit} submitting={false} gateOpen={false} error={null} />);
    await userEvent.fill(page.getByTestId("composer-input"), "run the tests");
    await userEvent.click(page.getByTestId("composer-submit"));

    await expect.element(page.getByTestId("composer-input")).toHaveValue("run the tests");
    expect(onSubmit.mock.calls).toEqual([["run the tests"]]);
  });

  it("sends on Enter and inserts a newline on Shift-Enter", async () => {
    const onSubmit = vi.fn().mockResolvedValue(true);
    render(<Composer onSubmit={onSubmit} submitting={false} gateOpen={false} error={null} />);
    await userEvent.fill(page.getByTestId("composer-input"), "first");
    await userEvent.keyboard("{Shift>}{Enter}{/Shift}");
    await userEvent.keyboard("second");
    expect(onSubmit.mock.calls).toEqual([]);
    expect(input().value).toBe("first\nsecond");

    await userEvent.keyboard("{Enter}");
    await expect.element(page.getByTestId("composer-input")).toHaveValue("");
    expect(onSubmit.mock.calls).toEqual([["first\nsecond"]]);
  });

  it("sends nothing for a blank draft", async () => {
    const onSubmit = vi.fn();
    render(<Composer onSubmit={onSubmit} submitting={false} gateOpen={false} error={null} />);
    await userEvent.fill(page.getByTestId("composer-input"), "   ");
    await userEvent.keyboard("{Enter}");
    expect(onSubmit.mock.calls).toEqual([]);
    expect((page.getByTestId("composer-submit").element() as HTMLButtonElement).disabled).toBe(true);
  });

  it("is disabled with an explanation while a gate is open", async () => {
    // A session waiting on a human decision cannot advance, so accepting input
    // would queue it silently behind a turn that cannot run.
    const onSubmit = vi.fn();
    render(<Composer onSubmit={onSubmit} submitting={false} gateOpen error={null} />);
    const box = page.getByTestId("composer-input");
    await expect.element(box).toBeDisabled();
    expect((page.getByTestId("composer-submit").element() as HTMLButtonElement).disabled).toBe(true);
    expect(document.querySelector("[data-testid=composer-hint]")?.textContent).toBe(
      "Answer the gate before sending more.",
    );
    expect(onSubmit.mock.calls).toEqual([]);
  });

  it("refuses to send a draft typed BEFORE the gate opened", async () => {
    // The real sequence: the user is halfway through a message when the agent
    // parks on a permission gate. The draft is already there, so the empty-text
    // guard does not cover this — only the lockout does. And `disabled` is a
    // DOM state a programmatic submit walks straight past, so the rule has to
    // live in `send`, not only in the attribute.
    const onSubmit = vi.fn();
    function Harness(): React.JSX.Element {
      const [gateOpen, setGateOpen] = useState(false);
      return (
        <>
          <button type="button" data-testid="open-gate" onClick={() => setGateOpen(true)}>
            open a gate
          </button>
          <Composer onSubmit={onSubmit} submitting={false} gateOpen={gateOpen} error={null} />
        </>
      );
    }
    render(<Harness />);
    await userEvent.fill(page.getByTestId("composer-input"), "run the tests");
    await userEvent.click(page.getByTestId("open-gate"));
    await expect.element(page.getByTestId("composer-input")).toBeDisabled();

    const form = document.querySelector("[data-testid=composer-form]");
    if (!(form instanceof HTMLFormElement)) throw new Error("no composer form");
    form.requestSubmit();
    expect(onSubmit.mock.calls).toEqual([]);
    // And the draft is still there for when the gate is answered.
    expect(input().value).toBe("run the tests");
  });

  it("tells the user where mid-turn input lands", async () => {
    // `input_queued` carries no text (design §3b), so until TurnStarted the
    // only feedback the user gets is this sentence.
    render(<Composer onSubmit={vi.fn()} submitting={false} gateOpen={false} error={null} />);
    const hint = page.getByTestId("composer-hint");
    await expect.element(hint).toBeInTheDocument();
    expect(hint.element().textContent).toBe("Lands at the end of the current turn.");
  });

  it("locks itself while a submit is in flight", async () => {
    render(<Composer onSubmit={vi.fn()} submitting gateOpen={false} error={null} />);
    await expect.element(page.getByTestId("composer-input")).toBeDisabled();
    expect(page.getByTestId("composer-submit").element().textContent).toBe("Sending…");
  });

  it("reports a failed submit as an alert carrying the error's own message", async () => {
    const failure = new Error("the workspace is busy");
    render(<Composer onSubmit={vi.fn()} submitting={false} gateOpen={false} error={failure} />);
    const alert = page.getByTestId("composer-error");
    await expect.element(alert).toBeInTheDocument();
    expect(alert.element().textContent).toBe("the workspace is busy");
    expect(alert.element().getAttribute("role")).toBe("alert");
  });
});
