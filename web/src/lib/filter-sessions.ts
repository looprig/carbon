import type { RecentSessionPage } from "@looprig/client";
import { toneFor } from "../components/status-dot";
import type { StatusFilter } from "../components/sessions-filter-bar";

/**
 * Filtering is client-side over the page already fetched: harness's list route
 * exposes no server-side filter, and the local catalogue a `carbon serve` host
 * has is small. When it stops being small the fix is a server-side filter and a
 * paging control, not a cleverer predicate here.
 *
 * Status reuses `toneFor` rather than comparing the wire string, so the filter
 * and the dot can never disagree about what "waiting" means. Search covers the
 * id and the title — the only two fields `RecentSessionPage["sessions"][number]` carries that a human
 * could search on. `state` is deliberately not searchable: it has its own
 * control, and matching it in free text would make "running" mean two things.
 */
export function filterSessions(
  sessions: readonly RecentSessionPage["sessions"][number][],
  status: StatusFilter,
  query: string,
): readonly RecentSessionPage["sessions"][number][] {
  const needle = query.trim().toLowerCase();
  if (status === "all" && needle === "") return sessions;
  return sessions.filter((session) => {
    if (status !== "all" && toneFor(session.state) !== status) return false;
    if (needle === "") return true;
    return (
      session.session_id.toLowerCase().includes(needle) ||
      (session.title ?? "").toLowerCase().includes(needle)
    );
  });
}
