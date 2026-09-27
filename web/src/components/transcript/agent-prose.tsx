/**
 * The agent's turn: one assistant segment, whole.
 *
 * `AssistantRow` carries four independent things and any combination of them
 * can be present at once, so this is not "prose OR thinking" — 05-app.md's
 * version took a `thinking` BOOLEAN, which cannot express the ordinary case of
 * a step that reasoned and then narrated. All four are rendered in the order
 * they happened: sealed reasoning, the redaction marker, prose, refusal.
 *
 * The 2px lime rail is §12's origin signature — lime marks intelligence in
 * motion. Reasoning is the same rail, muted and italic, because it is the
 * model's private working rather than something it said.
 *
 * `redactedThinking` is a fact, not a style: a redacted block projects
 * `thinking === ""` (rows.ts drops the provider's continuation bytes at the
 * decoder), so without this line a step whose only content was withheld
 * reasoning renders as an empty gap in the turn.
 */
export function AgentProse({
  thinking,
  text,
  refusal,
  redactedThinking,
}: {
  thinking: string;
  text: string;
  refusal: string;
  redactedThinking: boolean;
}): React.JSX.Element {
  return (
    <div className="flex gap-3 px-4 py-2">
      <span data-testid="agent-rail" aria-hidden="true" className="w-0.5 shrink-0 rounded-full bg-loop" />
      <div className="min-w-0 flex-1">
        {thinking === "" ? null : (
          <p data-testid="agent-thinking" className="text-sm whitespace-pre-wrap text-muted italic">
            {thinking}
          </p>
        )}
        {redactedThinking ? (
          <p data-testid="agent-redacted" className="font-mono text-xs text-muted">
            Some reasoning was withheld by the provider.
          </p>
        ) : null}
        {text === "" ? null : (
          <p data-testid="agent-prose" className="text-sm whitespace-pre-wrap">
            {text}
          </p>
        )}
        {refusal === "" ? null : (
          <p data-testid="agent-refusal" className="text-sm whitespace-pre-wrap text-fail">
            {refusal}
          </p>
        )}
      </div>
    </div>
  );
}
