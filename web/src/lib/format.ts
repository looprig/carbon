/**
 * Session-list formatting helpers.
 *
 * Duration is derived from the only two time fields `SessionSummary` carries
 * (`created_at` and `last_active_at`). Both are `omitzero` on the Go side, so
 * absent is a real wire case and "we don't know" is a first-class outcome,
 * rendered as an em dash rather than a fabricated 0s — which would be
 * indistinguishable from a genuinely instantaneous session.
 */
export function formatDuration(from: string | undefined, to: string | undefined): string {
  if (!from || !to) return EM_DASH;
  const start = Date.parse(from);
  const end = Date.parse(to);
  if (Number.isNaN(start) || Number.isNaN(end)) return EM_DASH;
  // Clamped rather than signed: last_active_at before created_at is a clock
  // artefact, and "-3m" in a list cell reads as a bug rather than as one.
  const seconds = Math.max(0, Math.round((end - start) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

const EM_DASH = "—";

/**
 * The leading UUID segment, for a list cell that must stay one line. The full
 * id is always in the row's `title`, so this is an abbreviation and never the
 * only copy. A value whose first segment is empty degrades to the whole string
 * rather than to a blank cell.
 */
export function shortId(sessionId: string): string {
  const [head] = sessionId.split("-");
  return head === undefined || head === "" ? sessionId : head;
}
