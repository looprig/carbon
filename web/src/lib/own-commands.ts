/**
 * Correlating a session's public events with the commands THIS tab sent.
 *
 * Factory admits a command under the `command_id` the client minted, and a Host
 * (>= v0.10.0) projects every public event body so that `cause.command_id` is
 * exactly that public id — never the runtime's own. So an event whose cause
 * names a command this tab admitted IS that command's effect, and the id is the
 * only join key needed: no text matching, no timing heuristics.
 *
 * Before host v0.10.0 the cause carried the runtime command id, which matched
 * nothing a client ever held. Nothing here depends on the old shape: an event
 * with no cause, or a cause naming someone else's command, is simply not ours.
 */

/** The public command id an event body names as its cause, or `""`. */
export function causeCommandId(body: unknown): string {
  if (typeof body !== "object" || body === null || Array.isArray(body)) return "";
  const cause = (body as Record<string, unknown>)["cause"];
  if (typeof cause !== "object" || cause === null || Array.isArray(cause)) return "";
  const id = (cause as Record<string, unknown>)["command_id"];
  return typeof id === "string" ? id : "";
}

/** Every command id some event in `events` names as its cause. */
export function causedCommandIds(events: readonly { readonly body: unknown }[]): ReadonlySet<string> {
  const seen = new Set<string>();
  for (const event of events) {
    const id = causeCommandId(event.body);
    if (id !== "") seen.add(id);
  }
  return seen;
}

/** One input this tab sent, remembered until the journal shows its effect. */
export interface SentInput {
  readonly commandId: string;
  readonly text: string;
}

/** The sent inputs no event has yet named as its cause, in send order. */
export function awaitingInputs(
  sent: readonly SentInput[],
  events: readonly { readonly body: unknown }[],
): readonly SentInput[] {
  if (sent.length === 0) return sent;
  const seen = causedCommandIds(events);
  return sent.filter((input) => !seen.has(input.commandId));
}
