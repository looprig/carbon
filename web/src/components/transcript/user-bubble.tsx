import { principalLabel, type ContentBlock, type MessagePrincipal, type UserFrame } from "@looprig/client";
import { cn } from "../../lib/cn";

/**
 * The human's turn: a right-aligned bubble (capstan-spec.md §8).
 *
 * Takes `UserRow.blocks` rather than a string. `UserRow` is "the exact
 * `UserMessage` the turn opened with" (rows.ts), and a user message really can
 * carry more than text — a `tool_result` is how a subagent hands work back, and
 * an image arrives as `blocks.ts`'s opaque `other` variant. Flattening to text
 * would render an EMPTY bubble for such a turn, which is indistinguishable from
 * a bug, so anything not renderable here is labelled with its wire type rather
 * than dropped.
 *
 * `pending` is the optimistic row the composer holds until the server
 * acknowledges the command (`view.commandOutcomes`). It is PER-TAB and
 * deliberately dimmed: a second tab, and the TUI, see nothing for this submit
 * until `TurnStarted` (design §3b).
 * A presenter frame was seen by the model but not typed by the user, so it is
 * dimmed around the user's own blocks. A Factory-stamped sender gets a chip.
 */
function BlockView({ block, testId, dim }: { block: ContentBlock; testId: string; dim?: boolean }): React.JSX.Element {
  return block.type === "text" ? (
    <span data-testid={testId} className={cn("block", dim && "opacity-60 text-muted")}>{block.text}</span>
  ) : (
    <span
      data-testid={dim ? testId : "user-other-block"}
      className={cn("mt-1 block w-fit rounded bg-bg px-1.5 py-0.5 font-mono text-xs text-muted", dim && "opacity-60")}
    >
      {block.type === "other" ? block.wireType : block.type}
    </span>
  );
}

export function UserBubble({
  blocks,
  pending,
  frame,
  principal,
}: {
  blocks: readonly ContentBlock[];
  pending?: boolean;
  frame?: UserFrame;
  principal?: MessagePrincipal;
}): React.JSX.Element {
  return (
    <div data-testid="user-row" className="flex flex-col items-end px-4 py-2">
      {principal === undefined ? null : (
        <span data-testid="user-from-chip" className="mb-1 rounded-full bg-bg px-2 py-0.5 font-mono text-xs text-muted">
          from {principalLabel(principal)}
        </span>
      )}
      <div
        data-testid="user-bubble"
        data-pending={pending ? "true" : "false"}
        className={cn(
          "max-w-[80%] rounded-2xl bg-accent px-4 py-2 text-sm whitespace-pre-wrap",
          pending && "opacity-60",
        )}
      >
        {frame?.prefix.map((block, index) => <BlockView key={`p${index}`} block={block} testId="user-frame-block" dim />)}
        {blocks.map((block, index) => <BlockView key={index} block={block} testId="user-text" />)}
        {frame?.suffix.map((block, index) => <BlockView key={`s${index}`} block={block} testId="user-frame-block" dim />)}
      </div>
    </div>
  );
}
