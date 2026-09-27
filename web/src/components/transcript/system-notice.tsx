import type { NoticeLevel } from "@looprig/client";

/**
 * The thin divider line (capstan-spec.md §8): an out-of-band statement about
 * the session rather than a message in it.
 *
 * It carries compaction markers, a rejected turn's reason, the
 * orphaned-subagent marker and the interrupt tombstone — every one of them a
 * machine fact, so mono.
 *
 * `level` is `NoticeRow.level`, harness's own info/warn/error, not 05-app.md's
 * invented `tone: "system" | "error"`. Three levels do not fit two buckets, and
 * collapsing `warn` into either loses the distinction the fold made.
 *
 * ## Why `warn` is the foreground colour and not an amber
 *
 * §12's palette has exactly two accents — lime does, blue decides — and no
 * warning colour. Amber would be a fourth signal nobody defined; lime would
 * read as agent activity and blue as a decision waiting on the human.
 * Brightening from muted to foreground says "louder than info, not a failure"
 * with the tokens the design system actually has.
 *
 * Only `error` is announced. A rejected submit reaches the transcript ONLY as
 * an error notice (design §3b: `TurnRejected` commits a notice row, never a
 * user row), so it is the one thing here that must not scroll past silently —
 * and making every compaction marker an assertive alert would train the user to
 * ignore all of them.
 */
const levelClass: Record<NoticeLevel, string> = {
  info: "text-muted",
  warn: "text-fg",
  error: "text-fail",
};

export function SystemNotice({
  text,
  level,
  testId = "system-notice",
}: {
  text: string;
  level: NoticeLevel;
  /** Overridden by callers that reuse this divider for a row of their own. */
  testId?: string;
}): React.JSX.Element {
  return (
    <div className="flex items-center gap-3 px-4 py-2">
      <span data-testid="system-notice-rule" aria-hidden="true" className="h-px flex-1 bg-border" />
      <span
        data-testid={testId}
        data-level={level}
        {...(level === "error" ? { role: "alert" } : {})}
        className={`font-mono text-xs ${levelClass[level]}`}
      >
        {text}
      </span>
      <span data-testid="system-notice-rule" aria-hidden="true" className="h-px flex-1 bg-border" />
    </div>
  );
}
