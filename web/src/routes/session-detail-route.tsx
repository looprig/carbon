import { useCallback, useMemo, useState } from "react";
import { useFactoryClient, useFactoryComposer, useFactoryGate, useFactorySessionView, useFactoryTenantId } from "@looprig/react";
import { FactorySessionDetailPage, type FactoryDetailComposer } from "./factory-session-detail-page";
import { awaitingInputs, type SentInput } from "../lib/own-commands";
import { useFactoryGateBoard } from "./factory-gate-board";

/**
 * Official Factory route: verified tenant + durable cold projection + shared
 * realtime link.
 *
 * The gate board is folded by `useFactoryGateBoard`, not rebuilt from
 * `view.gates` per page. The rebuild closed the in-flight page resurrection
 * race and nothing else: it folded no live `GateResolved`, so a gate answered
 * in another tab stayed on screen until the next `listGates`, and — because
 * `listGates` is read with a limit — it read a gate's absence from a BOUNDED
 * page as resolution. `factory-gate-board.ts` documents the tombstone that
 * replaces it.
 *
 * The cards and the respond path now read ONE list. They used to disagree: the
 * cards came from `view.gates` (the raw page) while `respond` looked the gate up
 * in the folded board, so a gate the fold dropped still rendered a button whose
 * handler found nothing and returned silently.
 */
export function FactorySessionDetailRoute({ sid }: { sid: string }): React.JSX.Element {
  const client = useFactoryClient();
  const tenantId = useFactoryTenantId();
  const view = useFactorySessionView(client.reads, { tenantId, sessionId: sid });
  const gateBoard = useFactoryGateBoard(sid, view);
  const gateControls = useFactoryGate(sid, gateBoard);
  const composer = useFactoryDetailComposer(sid, view.events);
  return (
    <FactorySessionDetailPage
      sid={sid}
      view={view}
      reads={client.reads}
      gates={gateControls.gates}
      composer={composer}
      onGateRespond={(gateId, action) => {
        const gate = gateControls.gates.find((entry) => entry.gateId === gateId);
        if (gate === undefined) return;
        void gateControls.respond(gate, action).then((result) => {
          if ("commandId" in result) composer.remember(result.commandId);
        });
      }}
    />
  );
}

/**
 * The detail page's input path over `useFactoryComposer`, plus the per-tab
 * record of which commands this tab admitted.
 *
 * An admitted input is remembered by its `command_id` and shown as "sent ·
 * waiting for the agent" until an event names that id as its `cause` — which,
 * with host >= v0.10.0, is exactly the public id Factory admitted. The record
 * is per-tab by nature: a reload forgets it, and the transcript itself (read
 * from the journal) is what survives.
 */
function useFactoryDetailComposer(
  sid: string,
  events: readonly { readonly body: unknown }[],
): FactoryDetailComposer & { remember: (commandId: string) => void } {
  const factory = useFactoryComposer(sid);
  const [sent, setSent] = useState<readonly SentInput[]>([]);
  const [own, setOwn] = useState<ReadonlySet<string>>(() => new Set());
  const remember = useCallback((commandId: string, text?: string) => {
    setOwn((prior) => (prior.has(commandId) ? prior : new Set([...prior, commandId])));
    if (text !== undefined) {
      setSent((prior) => (prior.some((input) => input.commandId === commandId)
        ? prior : [...prior, { commandId, text }]));
    }
  }, []);
  const settle = useCallback((result: Awaited<ReturnType<typeof factory.submit>>, text: string): boolean => {
    if (result.outcome === "accepted" || result.outcome === "pending") {
      remember(result.commandId, text);
      return true;
    }
    // `unknown` keeps the envelope retained (the reply was lost and admission
    // may have committed): the draft is cleared because the unconfirmed row now
    // owns that text, and Retry replays the SAME command id.
    if (result.outcome === "unknown") {
      remember(result.commandId, text);
      return true;
    }
    return false;
  }, [remember]);
  const onSubmit = useCallback(async (text: string) => settle(await factory.submit(text), text.trim()), [factory, settle]);
  const onRetry = useCallback(() => {
    const text = factory.text;
    void factory.retry().then((result) => settle(result, text));
  }, [factory, settle]);
  const onDiscard = useCallback(() => {
    // The user gave up on knowing whether it landed: stop presenting it as sent.
    const withdrawn = factory.pending?.commandId;
    if (withdrawn !== undefined) setSent((prior) => prior.filter((input) => input.commandId !== withdrawn));
    factory.cancel();
  }, [factory]);
  const awaiting = useMemo(() => awaitingInputs(sent, events), [sent, events]);
  const unconfirmed = factory.pending !== null && !factory.pending.sending ? factory.text : null;
  return {
    onSubmit,
    submitting: factory.pending?.sending === true,
    error: factory.error,
    awaiting,
    own,
    unconfirmed,
    onRetry,
    onDiscard,
    remember,
  };
}
