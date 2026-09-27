import { expect, test } from "vitest";
import { awaitingInputs, causeCommandId, causedCommandIds } from "./own-commands";

test("reads the public cause command id and nothing that merely looks like one", () => {
  expect(causeCommandId({ type: "TurnStarted", cause: { command_id: "c-1" } })).toBe("c-1");
  expect(causeCommandId({ type: "TurnStarted", command_id: "top-level" })).toBe("");
  expect(causeCommandId({ type: "TurnStarted", cause: { command_id: 7 } })).toBe("");
  expect(causeCommandId({ type: "TurnStarted", cause: ["c-1"] })).toBe("");
  expect(causeCommandId(null)).toBe("");
  expect(causeCommandId("c-1")).toBe("");
});

test("an input stops awaiting exactly when an event names its command as the cause", () => {
  const sent = [{ commandId: "c-1", text: "one" }, { commandId: "c-2", text: "two" }];
  const events = [
    { body: { type: "TurnStarted", cause: { command_id: "c-1" } } },
    { body: { type: "TurnStarted", cause: { command_id: "someone-else" } } },
  ];
  expect(causedCommandIds(events)).toStrictEqual(new Set(["c-1", "someone-else"]));
  expect(awaitingInputs(sent, events)).toStrictEqual([{ commandId: "c-2", text: "two" }]);
  expect(awaitingInputs([], events)).toStrictEqual([]);
});
