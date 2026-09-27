import type { StatusTone } from "./status-dot";

/**
 * The list's whole filtering surface: one status select and one search box.
 * capstan-spec.md §8's chip row and label row are deliberately absent — design
 * §5 — because `SessionSummary` carries five fields and none of the others
 * backs a chip.
 *
 * Fully controlled. The page owns `query` and `status`, so the two can be set
 * from somewhere else (a URL param, a "clear filters" action) without the bar
 * and the list disagreeing about what is filtered.
 */
export type StatusFilter = "all" | StatusTone;

export const STATUS_FILTERS: readonly StatusFilter[] = ["all", "running", "waiting", "failed", "idle"];

/** What each filter is called in the UI. The values are ours, not machine facts, so they may be prose. */
const FILTER_LABELS: Record<StatusFilter, string> = {
  all: "all",
  running: "running",
  waiting: "waiting",
  failed: "failed",
  idle: "idle",
};

export interface SessionsFilterBarProps {
  query: string;
  status: StatusFilter;
  onQueryChange: (next: string) => void;
  onStatusChange: (next: StatusFilter) => void;
}

export function SessionsFilterBar({
  query,
  status,
  onQueryChange,
  onStatusChange,
}: SessionsFilterBarProps): React.JSX.Element {
  return (
    <div data-testid="sessions-filter-bar" className="mb-3 flex items-center gap-2">
      <label className="sr-only" htmlFor="sessions-search">
        Search sessions
      </label>
      <input
        id="sessions-search"
        data-testid="sessions-search"
        type="search"
        value={query}
        placeholder="Search id or goal…"
        onChange={(event) => onQueryChange(event.target.value)}
        className="flex-1 rounded-md border border-border bg-card px-3 py-1.5 text-sm focus-visible:outline-2 focus-visible:outline-rig"
      />
      <label className="sr-only" htmlFor="sessions-status-filter">
        Status
      </label>
      <select
        id="sessions-status-filter"
        data-testid="sessions-status-filter"
        value={status}
        onChange={(event) => onStatusChange(event.target.value as StatusFilter)}
        className="rounded-md border border-border bg-card px-2 py-1.5 font-mono text-xs focus-visible:outline-2 focus-visible:outline-rig"
      >
        {STATUS_FILTERS.map((filter) => (
          <option key={filter} value={filter}>
            {FILTER_LABELS[filter]}
          </option>
        ))}
      </select>
    </div>
  );
}
