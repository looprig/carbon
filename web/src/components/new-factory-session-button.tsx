import { useState } from "react";
import { textBlock, type CreateCommandRequest, type FactoryCommands, type PendingCommand } from "@looprig/client";
import { toError } from "../lib/to-error";

export interface NewFactorySessionButtonProps {
  create: FactoryCommands["create"];
  onCreated: (sessionId: string) => void;
}

/** A retry keeps the same pending command and therefore the same IDs and bytes. */
export function NewFactorySessionButton({ create, onCreated }: NewFactorySessionButtonProps): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const [goal, setGoal] = useState("");
  const [creating, setCreating] = useState(false);
  const [pending, setPending] = useState<PendingCommand<CreateCommandRequest> | null>(null);
  const [error, setError] = useState<Error | null>(null);

  async function submit(event: React.FormEvent): Promise<void> {
    event.preventDefault();
    const text = goal.trim();
    if (creating || (pending === null && text === "")) return;
    setCreating(true);
    setError(null);
    try {
      const command = pending ?? create({ agentId: "carbon", blocks: [textBlock(text)] });
      if (pending === null) setPending(command);
      const result = await command.attempt();
      if (result.outcome === "resolved" && result.status.status !== "rejected") {
        setPending(null);
        setGoal("");
        setOpen(false);
        onCreated(command.request.session_id);
      } else if (result.outcome === "rejected" || (result.outcome === "resolved" && result.status.status === "rejected")) {
        setPending(null);
        setError(result.outcome === "rejected" ? result.error : new Error(result.status.error?.message ?? "Session creation was rejected"));
      } else {
        setError(new Error("Creation is unconfirmed. Retry uses the same command and session IDs."));
      }
    } catch (cause) {
      setError(toError(cause));
    } finally {
      setCreating(false);
    }
  }

  if (!open) return <button type="button" data-testid="new-session-open" onClick={() => setOpen(true)} className="rounded-md bg-loop px-3 py-1.5 text-sm font-medium text-bg">New session</button>;
  return <form data-testid="new-session-form" onSubmit={(event) => { void submit(event); }} className="flex flex-wrap items-center gap-2">
    <label className="sr-only" htmlFor="new-session-goal">Goal</label>
    <input id="new-session-goal" value={goal} onChange={(event) => setGoal(event.target.value)} disabled={pending !== null || creating} className="rounded border border-border bg-card px-2 py-1.5 text-sm" />
    <button type="submit" data-testid="new-session-submit" disabled={creating || (pending === null && goal.trim() === "")} className="rounded-md bg-loop px-3 py-1.5 text-sm font-medium text-bg">{pending === null ? "Create" : "Retry create"}</button>
    {error === null ? null : <p role="alert" className="basis-full text-xs text-fail">{error.message}</p>}
  </form>;
}
