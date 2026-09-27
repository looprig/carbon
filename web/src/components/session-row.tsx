import { memo, type MouseEvent } from "react";
import type { RecentSessionPage } from "@looprig/client";
import { formatDuration, shortId } from "../lib/format";
import { cn } from "../lib/cn";
import { StatusDot } from "./status-dot";

export interface SessionRowProps {
  session: RecentSessionPage["sessions"][number];
  /** The real, refreshable path this row points at. */
  href: string;
  /**
   * Client-side navigation for a plain left click. Optional: without it the
   * row is still a working link, it just costs a full document load.
   */
  onActivate?: (() => void) | undefined;
}

/**
 * One list row: id, status dot, duration, goal. Design §5 stops there —
 * RecentSessionPage["sessions"][number] has five fields and none of the others backs a chip or a
 * label, so there is no chip row and no label row to render.
 *
 * A plain `<a href>` rather than a router `<Link>`, for two reasons. It keeps
 * the component renderable outside a router, which is what lets this test and
 * the sessions page's tests run without mounting one. And it keeps the row a
 * real link: middle-clickable, copyable, and refreshable, which is the point of
 * choosing browser history over hash history in the first place. `onActivate`
 * intercepts only the unmodified left click, so "open in a new tab" still
 * reaches the browser.
 *
 * Everything machine-authored (id, state, duration) is mono per §12; the goal
 * is the row's one piece of human/agent prose and is sans.
 *
 * `memo` is load-bearing, not decoration. The page re-renders on every
 * keystroke in the search box and on every status-filter change, and the
 * summaries it maps over are the same objects each time.
 */
export const SessionRow = memo(function SessionRow({
  session,
  href,
  onActivate,
}: SessionRowProps): React.JSX.Element {
  function handleClick(event: MouseEvent<HTMLAnchorElement>): void {
    if (!onActivate) return;
    if (event.defaultPrevented) return;
    if (event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    onActivate();
  }

  return (
    <a
      data-testid="session-row-link"
      href={href}
      onClick={handleClick}
      className={cn(
        "flex items-center gap-3 border-b border-border px-4 py-3 last:border-b-0",
        "hover:bg-accent focus-visible:outline-2 focus-visible:outline-rig",
      )}
    >
      <StatusDot state={session.state} />
      <span data-testid="session-id" title={session.session_id} className="font-mono text-xs text-muted">
        {shortId(session.session_id)}
      </span>
      <span data-testid="session-duration" className="font-mono text-xs text-muted">
        {formatDuration(session.created_at, session.last_active_at)}
      </span>
      <span data-testid="session-goal" className="truncate text-sm">
        {session.title ?? "Untitled session"}
      </span>
    </a>
  );
});
