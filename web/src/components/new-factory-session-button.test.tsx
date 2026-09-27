import { page, userEvent } from "vitest/browser";
import { expect, test, vi } from "vitest";
import { render } from "vitest-browser-react";
import type { FactoryCommands } from "@looprig/client";
import { NewFactorySessionButton } from "./new-factory-session-button";

test("creates a Carbon Factory session with the goal and opens its durable id", async () => {
  const open = vi.fn();
  const attempt = vi.fn().mockResolvedValue({ outcome: "resolved", status: { command_id: "command-1", status: "accepted" } });
  const create = vi.fn().mockReturnValue({ request: { session_id: "session-1" }, attempt });
  render(<NewFactorySessionButton create={create as FactoryCommands["create"]} onCreated={open} />);
  await userEvent.click(page.getByTestId("new-session-open"));
  await userEvent.fill(page.getByLabelText("Goal"), "Fix the parser");
  await userEvent.click(page.getByTestId("new-session-submit"));
  await expect.poll(() => open.mock.calls.length).toBe(1);
  expect(create).toHaveBeenCalledWith({ agentId: "carbon", blocks: [{ type: "text", Text: "Fix the parser" }] });
  expect(open).toHaveBeenCalledWith("session-1");
});

test("an unconfirmed create retries the same pending command", async () => {
  const open = vi.fn();
  const attempt = vi.fn()
    .mockResolvedValueOnce({ outcome: "unknown", error: new Error("reply lost") })
    .mockResolvedValueOnce({ outcome: "resolved", status: { command_id: "command-1", status: "accepted" } });
  const create = vi.fn().mockReturnValue({ request: { session_id: "session-1" }, attempt });
  render(<NewFactorySessionButton create={create as FactoryCommands["create"]} onCreated={open} />);
  await userEvent.click(page.getByTestId("new-session-open"));
  await userEvent.fill(page.getByLabelText("Goal"), "Fix the parser");
  await userEvent.click(page.getByTestId("new-session-submit"));
  await expect.element(page.getByRole("alert")).toHaveTextContent("unconfirmed");
  await userEvent.click(page.getByTestId("new-session-submit"));
  await expect.poll(() => open.mock.calls.length).toBe(1);
  expect(create).toHaveBeenCalledTimes(1);
  expect(attempt).toHaveBeenCalledTimes(2);
});
