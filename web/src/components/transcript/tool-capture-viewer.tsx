import { useCallback, useEffect, useRef, useState } from "react";
import {
  CoreProtocolError,
  RequestAbortedError,
  ToolCaptureIntegrityError,
  ToolCaptureTooLargeError,
  ToolCaptureUnavailableError,
  readToolCapturePages,
  type FactoryReads,
  type ToolCaptureContent,
  type ToolResultCaptureSummary,
} from "@looprig/client";

export const TOOL_CAPTURE_PAGE_BYTES = 64 * 1024;
export const TOOL_CAPTURE_CEILING_BYTES = 1024 * 1024;

export interface ToolCaptureViewerProps {
  reads: FactoryReads;
  sessionId: string;
  toolUseId: string;
  capture: ToolResultCaptureSummary;
}

type LoadState =
  | { kind: "idle" }
  | { kind: "loading"; scope: string; reads: FactoryReads }
  | { kind: "loaded"; scope: string; reads: FactoryReads; content: ToolCaptureContent }
  | { kind: "failed"; scope: string; reads: FactoryReads; message: string };

function captureError(cause: unknown): string {
  if (cause instanceof ToolCaptureTooLargeError) return "Full result exceeds the viewer's 1 MiB limit.";
  if (cause instanceof ToolCaptureUnavailableError) return "The full result was not retained.";
  if (cause instanceof ToolCaptureIntegrityError) return "The retained result failed integrity verification.";
  if (cause instanceof CoreProtocolError) {
    if (cause.code === "unauthenticated" || cause.code === "not_authorized") return "Access denied to the retained result.";
    if (cause.code === "session_not_found" || cause.code === "object_not_found" || cause.code === "not_found") {
      return "The retained result is no longer available.";
    }
  }
  return "The retained result could not be loaded.";
}

function localPageText(content: ToolCaptureContent, encoding: ToolResultCaptureSummary["encoding"]): string[] {
  if (encoding === "binary") {
    return content.pages.map((page) =>
      Array.from(page.bytes, (byte) => byte.toString(16).padStart(2, "0")).join(" "));
  }
  const decoder = new TextDecoder("utf-8", { fatal: false });
  return content.pages.map((page, index) =>
    decoder.decode(page.bytes, { stream: index < content.pages.length - 1 }));
}

export function ToolCaptureViewer({ reads, sessionId, toolUseId, capture }: ToolCaptureViewerProps): React.JSX.Element {
  const [state, setState] = useState<LoadState>({ kind: "idle" });
  const controller = useRef<AbortController | undefined>(undefined);
  const request = useRef(0);
  const scope = JSON.stringify([
    sessionId,
    toolUseId,
    capture.toolExecutionId,
    capture.objectId ?? null,
    capture.capturedBytes,
    capture.encoding,
  ]);
  // Passive cleanup runs after a replacement render commits. Fence the data
  // during render too, so old verified bytes can never appear under new labels.
  const visibleState = state.kind === "idle" || (state.scope === scope && state.reads === reads)
    ? state : { kind: "idle" as const };

  useEffect(() => {
    // REDUNDANT, and deliberately so — measured, not assumed. React runs the
    // cleanup below before re-running this body on a dependency change, and on
    // the first mount `controller.current` is `undefined`, so these three lines
    // cover no case the cleanup does not: removing them survives the whole app
    // suite, which is what the checkpoint-5 gate measured (mutant M3). They stay
    // as belt-and-braces against a future edit that moves the cleanup, and the
    // `setState` is NOT redundant — it is what returns a superseded viewer to
    // `idle` once the replacement has committed. Do not read the two aborts as a
    // live guard; the cleanup abort (killed by "unmount aborts a pending capture
    // read") and the render-time fence above are the guards.
    ++request.current;
    controller.current?.abort();
    controller.current = undefined;
    setState({ kind: "idle" });
    return () => {
      ++request.current;
      controller.current?.abort();
    };
  }, [reads, scope]);

  const load = useCallback(() => {
    controller.current?.abort();
    const active = new AbortController();
    controller.current = active;
    const generation = ++request.current;
    setState({ kind: "loading", scope, reads });
    void readToolCapturePages(reads, sessionId, capture, {
      pageBytes: TOOL_CAPTURE_PAGE_BYTES,
      ceilingBytes: TOOL_CAPTURE_CEILING_BYTES,
      signal: active.signal,
    }).then((content) => {
      if (generation === request.current && !active.signal.aborted) setState({ kind: "loaded", scope, reads, content });
    }).catch((cause: unknown) => {
      if (generation === request.current && !active.signal.aborted && !(cause instanceof RequestAbortedError)) {
        setState({ kind: "failed", scope, reads, message: captureError(cause) });
      }
    });
  }, [reads, sessionId, capture, scope]);

  const pages = visibleState.kind === "loaded" ? localPageText(visibleState.content, capture.encoding) : [];
  return (
    <section data-testid="tool-capture-viewer" className="mt-2 rounded border border-border p-2">
      <p className="font-mono text-xs">tool result · {toolUseId}</p>
      <p data-testid="tool-capture-metadata" className="font-mono text-xs text-muted">
        {capture.capturedBytes} captured bytes · {capture.encoding}
        {capture.originalBytes === null ? " · original size unknown" : ` of ${capture.originalBytes} original bytes`}
        {capture.truncated ? ` · truncated${capture.truncationReason === "capture_ceiling" ? " at capture ceiling" : capture.truncationReason === "source_limit" ? " at source limit" : ""}` : ""}
      </p>
      <button
        type="button"
        data-testid="tool-capture-load"
        onClick={load}
        className="mt-2 rounded border border-border px-2 py-1 text-xs"
      >
        {visibleState.kind === "loading" ? "Restart load" : visibleState.kind === "loaded" ? "Reload full result" : "View full tool result"}
      </button>
      {visibleState.kind === "loading" ? <p role="status" data-testid="tool-capture-loading" className="mt-2 text-xs text-muted">Loading and verifying the whole capture…</p> : null}
      {visibleState.kind === "failed" ? <p role="alert" data-testid="tool-capture-error" className="mt-2 text-xs text-fail">{visibleState.message}</p> : null}
      {visibleState.kind === "loaded" ? (
        <div data-testid="tool-capture-output" className="mt-2">
          <p data-testid="tool-capture-verified" className="font-mono text-xs text-muted">
            whole capture verified · {visibleState.content.metadata.digest ?? "digest unavailable"}
          </p>
          {visibleState.content.pages.length === 0 ? <p className="font-mono text-xs text-muted">Empty result</p> : visibleState.content.pages.map((range, index) => (
            <pre key={range.start} data-testid="tool-capture-page" className="mt-1 max-h-96 overflow-auto whitespace-pre-wrap rounded bg-card p-2 font-mono text-xs">
              <span className="block text-muted">local page · bytes {range.start}-{range.end}</span>
              {pages[index]}
            </pre>
          ))}
        </div>
      ) : null}
    </section>
  );
}
