import { page, userEvent } from "vitest/browser";
import { expect, test, vi } from "vitest";
import { render } from "vitest-browser-react";
import { useLayoutEffect } from "react";
import {
  CoreProtocolError,
  ToolCaptureIntegrityError,
  ToolCaptureUnavailableError,
  type FactoryReads,
  type RequestOptions,
  type ToolResultCaptureSummary,
} from "@looprig/client";
import { ToolCaptureViewer } from "./tool-capture-viewer";

const encoder = new TextEncoder();

async function fixture(text: string, encoding: "utf-8" | "binary" = "utf-8") {
  const source = encoder.encode(text);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", source));
  const hex = Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
  const capture: ToolResultCaptureSummary = {
    toolExecutionId: "execution-1",
    objectId: "object-1",
    capturedBytes: source.length,
    originalBytes: source.length + 7,
    originalBytesLowerBound: undefined,
    capturedBytesAtCeiling: source.length,
    truncated: true,
    truncationReason: "capture_ceiling",
    encoding,
  };
  const reads = {
    readObjectMetadata: vi.fn(async () => ({
      reference: { object_id: "object-1" }, size_bytes: source.length,
      media_type: "text/plain", digest: `sha256:${hex}`,
    })),
    readObjectRange: vi.fn(async (_sid: string, _oid: string, options: { start: number; end: number }) => ({
      bytes: source.slice(options.start, options.end + 1),
      contentRange: `bytes ${options.start}-${options.end}/${source.length}`,
      mediaType: "text/plain",
    })),
  } as unknown as FactoryReads;
  return { capture, reads };
}

test("loads no object bytes until asked, then shows only whole-verified local pages", async () => {
  // One byte short of the 64 KiB page, so the two UTF-8 bytes of `é` STRADDLE the
  // page boundary: byte 65535 ends page one and byte 65536 opens page two. A
  // decoder that did not carry its state across pages would emit a replacement
  // character on each side, so the round-trip below is what pins the streaming
  // decode. `'a'.repeat(64 * 1024)` put the whole character inside page two and
  // could not tell the two apart. The capture is still 65 537 bytes, so the
  // two-range assertion below is unchanged.
  const body = `${"a".repeat(64 * 1024 - 1)}é`;
  const { capture, reads } = await fixture(body);
  render(<ToolCaptureViewer reads={reads} sessionId="session-1" toolUseId="tool-1" capture={capture} />);

  expect(reads.readObjectMetadata).not.toHaveBeenCalled();
  expect(reads.readObjectRange).not.toHaveBeenCalled();
  await expect.element(page.getByTestId("tool-capture-metadata")).toHaveTextContent("truncated at capture ceiling");

  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.poll(() => reads.readObjectRange).toHaveBeenCalledTimes(2);
  await expect.element(page.getByTestId("tool-capture-verified")).toHaveTextContent("whole capture verified");
  const localPages = [...document.querySelectorAll("[data-testid=tool-capture-page]")];
  expect(localPages).toHaveLength(2);
  expect(localPages.map((element) => element.lastChild?.textContent ?? "").join("")).toBe(body);
});

test.each([
  [new ToolCaptureUnavailableError(), "was not retained"],
  [new ToolCaptureIntegrityError(), "failed integrity verification"],
  [new CoreProtocolError({ error: { code: "session_not_found", message: "gone", retryable: false } }), "no longer available"],
  [new CoreProtocolError({ error: { code: "not_authorized", message: "denied", retryable: false } }), "Access denied"],
] as const)("reports a safe capture error for %s", async (failure, message) => {
  const { capture, reads } = await fixture("result");
  reads.readObjectMetadata = vi.fn(async () => { throw failure; });
  render(<ToolCaptureViewer reads={reads} sessionId="session-1" toolUseId="tool-1" capture={capture} />);

  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-error")).toHaveTextContent(message);
  expect(document.querySelector("[data-testid=tool-capture-output]")).toBeNull();
});

test("replacement aborts the old read and removes its verified display", async () => {
  const first = await fixture("first result");
  const second = await fixture("second result");
  const screen = await render(<ToolCaptureViewer reads={first.reads} sessionId="session-1" toolUseId="tool-1" capture={first.capture} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("first result");

  let signal: AbortSignal | undefined;
  first.reads.readObjectMetadata = vi.fn((_sid: string, _oid: string, options?: RequestOptions): Promise<never> => {
    signal = options?.signal;
    return new Promise<never>(() => undefined);
  });
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.poll(() => signal).toBeDefined();

  await screen.rerender(<ToolCaptureViewer reads={second.reads} sessionId="session-2" toolUseId="tool-2" capture={{ ...second.capture, objectId: "object-2" }} />);
  expect(signal?.aborted).toBe(true);
  expect(document.querySelector("[data-testid=tool-capture-output]")).toBeNull();
});

test("changing only durable execution identity aborts the superseded read", async () => {
  const { capture, reads } = await fixture("pending result");
  let signal: AbortSignal | undefined;
  reads.readObjectMetadata = vi.fn((_sid: string, _oid: string, options?: RequestOptions): Promise<never> => {
    signal = options?.signal;
    return new Promise<never>(() => undefined);
  });
  const screen = await render(<ToolCaptureViewer reads={reads} sessionId="session-1" toolUseId="tool-1" capture={capture} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.poll(() => signal).toBeDefined();

  await screen.rerender(<ToolCaptureViewer reads={reads} sessionId="session-1" toolUseId="tool-1" capture={{
    ...capture, toolExecutionId: "execution-2",
  }} />);
  expect(signal?.aborted).toBe(true);
});

test("never commits old verified bytes under replacement session metadata", async () => {
  const first = await fixture("first session private result");
  const second = await fixture("second session result");
  const replacementCommits: Array<string | null> = [];
  function CommitObserver({ replacement }: { replacement: boolean }) {
    useLayoutEffect(() => {
      if (replacement) replacementCommits.push(document.querySelector("[data-testid=tool-capture-output]")?.textContent ?? null);
    });
    return null;
  }
  function Subject({ replacement }: { replacement: boolean }) {
    const selected = replacement ? second : first;
    return <>
      <ToolCaptureViewer
        reads={selected.reads}
        sessionId={replacement ? "session-2" : "session-1"}
        toolUseId={replacement ? "tool-2" : "tool-1"}
        capture={replacement ? { ...selected.capture, objectId: "object-2" } : selected.capture}
      />
      <CommitObserver replacement={replacement} />
    </>;
  }
  const screen = await render(<Subject replacement={false} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("first session private result");

  await screen.rerender(<Subject replacement />);
  expect(replacementCommits).toEqual([null]);
});

test("opaque identifiers containing NUL cannot collide across capture scopes", async () => {
  const loaded = await fixture("first scope private result");
  const replacementCommits: Array<string | null> = [];
  function CommitObserver({ replacement }: { replacement: boolean }) {
    useLayoutEffect(() => {
      if (replacement) replacementCommits.push(document.querySelector("[data-testid=tool-capture-output]")?.textContent ?? null);
    });
    return null;
  }
  function Subject({ replacement }: { replacement: boolean }) {
    return <>
      <ToolCaptureViewer
        reads={loaded.reads}
        sessionId={replacement ? "a" : "a\u0000b"}
        toolUseId={replacement ? "b\u0000c" : "c"}
        capture={loaded.capture}
      />
      <CommitObserver replacement={replacement} />
    </>;
  }
  const screen = await render(<Subject replacement={false} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-output")).toHaveTextContent("first scope private result");

  await screen.rerender(<Subject replacement />);
  expect(replacementCommits).toEqual([null]);
});

test("unmount aborts a pending capture read", async () => {
  const { capture, reads } = await fixture("pending result");
  let signal: AbortSignal | undefined;
  reads.readObjectMetadata = vi.fn((_sid: string, _oid: string, options?: RequestOptions): Promise<never> => {
    signal = options?.signal;
    return new Promise<never>(() => undefined);
  });
  const screen = await render(<ToolCaptureViewer reads={reads} sessionId="session-1" toolUseId="tool-1" capture={capture} />);
  await userEvent.click(page.getByTestId("tool-capture-load"));
  await expect.element(page.getByTestId("tool-capture-loading")).toHaveTextContent("Loading and verifying");
  await expect.poll(() => signal).toBeDefined();

  await screen.unmount();
  expect(signal?.aborted).toBe(true);
});
