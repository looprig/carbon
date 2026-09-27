import { useMemo, useState } from "react";
import type { FactoryCommands } from "@looprig/client";
import { useFactorySessionList, type FactorySessionListReads } from "@looprig/react";
import { SessionRow } from "../components/session-row";
import { NewFactorySessionButton } from "../components/new-factory-session-button";
import { SessionsFilterBar, type StatusFilter } from "../components/sessions-filter-bar";
import { filterSessions } from "../lib/filter-sessions";

export interface FactorySessionsPageProps {
  reads: FactorySessionListReads;
  create: FactoryCommands["create"];
  onOpenSession: (sessionId: string) => void;
}

/** The official durable Factory catalogue; legacy SessionsPage remains for compatibility tests. */
export function FactorySessionsPage({ reads, create, onOpenSession }: FactorySessionsPageProps): React.JSX.Element {
  const page = useFactorySessionList(reads);
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<StatusFilter>("all");
  const visible = useMemo(
    () => filterSessions(page.sessions, status, query),
    [page.sessions, status, query],
  );

  return (
    <main data-testid="sessions-page" className="mx-auto max-w-4xl p-6">
      <div className="mb-4 flex items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Sessions</h1>
        <NewFactorySessionButton create={create} onCreated={onOpenSession} />
      </div>
      {page.error !== null ? (
        <div role="alert" data-testid="sessions-error" className="rounded-md border border-fail/50 bg-fail/10 p-4 text-fail">
          <p className="font-medium">Couldn&rsquo;t load sessions</p>
          <p className="font-mono text-sm">{page.error.message}</p>
        </div>
      ) : !page.loaded || page.loading ? (
        <div role="status" data-testid="sessions-loading" className="py-8 text-muted">Loading sessions…</div>
      ) : page.sessions.length === 0 ? (
        <div data-testid="sessions-empty" className="rounded-md border border-dashed border-border p-10 text-center text-muted">
          <p className="font-medium">No sessions yet</p>
        </div>
      ) : (
        <>
          <SessionsFilterBar query={query} status={status} onQueryChange={setQuery} onStatusChange={setStatus} />
          {visible.length === 0 ? (
            <div data-testid="sessions-no-match" className="rounded-md border border-dashed border-border p-10 text-center text-muted">
              No sessions match this filter
            </div>
          ) : (
            <div data-testid="sessions-list" className="overflow-hidden rounded-md border border-border bg-card">
              {visible.map((session) => (
                <SessionRow
                  key={session.session_id}
                  session={session}
                  href={`/sessions/${encodeURIComponent(session.session_id)}`}
                  onActivate={() => onOpenSession(session.session_id)}
                />
              ))}
            </div>
          )}
          <nav aria-label="Session pages" className="mt-4 flex justify-between">
            <button
              type="button"
              data-testid="sessions-previous-page"
              disabled={page.previousCursor === undefined || page.loading}
              onClick={() => { void page.loadPrevious(); }}
              className="rounded-md border border-border px-3 py-1 text-sm disabled:opacity-40"
            >
              Newer
            </button>
            <button
              type="button"
              data-testid="sessions-next-page"
              disabled={page.nextCursor === undefined || page.loading}
              onClick={() => { void page.loadNext(); }}
              className="rounded-md border border-border px-3 py-1 text-sm disabled:opacity-40"
            >
              Older
            </button>
          </nav>
        </>
      )}
    </main>
  );
}
