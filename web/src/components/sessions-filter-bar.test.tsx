import { useState } from "react";
import { page, userEvent } from "vitest/browser";
import { describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";
import { SessionsFilterBar, STATUS_FILTERS, type StatusFilter } from "./sessions-filter-bar";

/**
 * The bar is fully controlled, so a test that pins `query`/`status` at a
 * constant is testing a control the page can never actually produce: React
 * would snap the input back to "" after every keystroke. This harness holds
 * the state the page holds and reports every change to the spies.
 */
function Controlled({
  onQueryChange,
  onStatusChange,
}: {
  onQueryChange?: (next: string) => void;
  onStatusChange?: (next: StatusFilter) => void;
}): React.JSX.Element {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<StatusFilter>("all");
  return (
    <SessionsFilterBar
      query={query}
      status={status}
      onQueryChange={(next) => {
        setQuery(next);
        onQueryChange?.(next);
      }}
      onStatusChange={(next) => {
        setStatus(next);
        onStatusChange?.(next);
      }}
    />
  );
}

function labelTextFor(id: string): string {
  const label = document.querySelector(`label[for=${id}]`);
  if (!label) throw new Error(`no <label for=${id}>`);
  return (label.textContent ?? "").trim();
}

describe("SessionsFilterBar", () => {
  it("reports typed search text to the caller and keeps showing it", async () => {
    const onQueryChange = vi.fn();
    render(<Controlled onQueryChange={onQueryChange} />);
    const search = page.getByTestId("sessions-search");
    await expect.element(search).toBeInTheDocument();

    await userEvent.fill(search, "parser");
    expect(onQueryChange).toHaveBeenLastCalledWith("parser");
    expect((search.element() as HTMLInputElement).value).toBe("parser");
  });

  it("reports a status selection to the caller", async () => {
    const onStatusChange = vi.fn();
    render(<Controlled onStatusChange={onStatusChange} />);
    const select = page.getByTestId("sessions-status-filter");
    await expect.element(select).toBeInTheDocument();

    await userEvent.selectOptions(select, "running");
    expect(onStatusChange).toHaveBeenLastCalledWith("running");
    expect((select.element() as HTMLSelectElement).value).toBe("running");
  });

  it("offers exactly the five filters, and no more", async () => {
    // Design §5: ONE status filter and a search. SessionSummary carries five
    // fields and none of the others backs a chip row or a label row, so a
    // sixth control here would have nothing on the wire behind it.
    render(<Controlled />);
    await expect.element(page.getByTestId("sessions-status-filter")).toBeInTheDocument();
    const options = [...document.querySelectorAll("[data-testid=sessions-status-filter] option")];
    expect(options.map((option) => option.getAttribute("value"))).toEqual([...STATUS_FILTERS]);
    expect([...STATUS_FILTERS]).toEqual(["all", "running", "waiting", "failed", "idle"]);
    expect(document.querySelectorAll("[data-testid=sessions-filter-bar] input, [data-testid=sessions-filter-bar] select")).toHaveLength(2);
  });

  it("labels both controls for assistive tech", async () => {
    render(<Controlled />);
    const search = page.getByTestId("sessions-search");
    await expect.element(search).toBeInTheDocument();
    // The accessible name is asserted as its two halves, both synchronously:
    // the <label for=X> carries the right text, AND the control under test is
    // the element that id X actually resolves to. Together those are exactly
    // what the accessible-name computation reads, and each half fails on its
    // own mutation -- dropping the label text, or dropping the control's `id`
    // so the `for` points at nothing.
    //
    // `toHaveAccessibleName` asserts the same thing, but it is a RETRYING
    // matcher: an input that has lost its id makes it retry to the full locator
    // timeout, so the mutation reports as a ~15s hang instead of a diff.
    // Measured: 14.9s for the missing-id mutation, against ~50ms here.
    expect(labelTextFor("sessions-search")).toBe("Search sessions");
    expect(labelTextFor("sessions-status-filter")).toBe("Status");
    expect(document.getElementById("sessions-search")).toBe(search.element());
    expect(document.getElementById("sessions-status-filter")).toBe(
      page.getByTestId("sessions-status-filter").element(),
    );
  });

  it("follows the caller when the caller resets either control", async () => {
    // The controlled contract, from the other direction. An uncontrolled input
    // or select (`defaultValue`) passes every assertion above -- measured for
    // both -- and then ignores a reset the page performs, leaving the controls
    // showing a filter that is no longer applied.
    function Resettable(): React.JSX.Element {
      const [query, setQuery] = useState("parser");
      const [status, setStatus] = useState<StatusFilter>("failed");
      return (
        <>
          <button
            data-testid="clear"
            type="button"
            onClick={() => {
              setQuery("");
              setStatus("all");
            }}
          >
            clear
          </button>
          <SessionsFilterBar
            query={query}
            status={status}
            onQueryChange={setQuery}
            onStatusChange={setStatus}
          />
        </>
      );
    }

    render(<Resettable />);
    const search = page.getByTestId("sessions-search");
    await expect.element(search).toBeInTheDocument();
    const select = page.getByTestId("sessions-status-filter");
    expect((search.element() as HTMLInputElement).value).toBe("parser");
    expect((select.element() as HTMLSelectElement).value).toBe("failed");

    await userEvent.click(page.getByTestId("clear"));
    // Settle on the button's own re-render before asserting, so the assertion
    // sees a completed render pass instead of racing one -- and fails in
    // milliseconds rather than after a locator timeout.
    await expect.element(page.getByTestId("clear")).toBeEnabled();
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect((search.element() as HTMLInputElement).value).toBe("");
    expect((select.element() as HTMLSelectElement).value).toBe("all");
  });
});
