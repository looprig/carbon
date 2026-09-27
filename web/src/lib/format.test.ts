import { describe, expect, it } from "vitest";
import { formatDuration, shortId } from "./format";

describe("formatDuration", () => {
  it("formats sub-minute, minute and hour spans compactly", () => {
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:00:42Z")).toBe("42s");
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:07:30Z")).toBe("7m");
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T13:20:00Z")).toBe("3h 20m");
  });

  it("switches units exactly at the boundary, not near it", () => {
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:00:59Z")).toBe("59s");
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:01:00Z")).toBe("1m");
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:59:59Z")).toBe("59m");
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T11:00:00Z")).toBe("1h 0m");
  });

  it("reports a zero-length span as 0s, not as unknown", () => {
    // A session created and never touched again has created_at ==
    // last_active_at. That is a known duration of zero, not a missing one.
    expect(formatDuration("2026-08-27T10:00:00Z", "2026-08-27T10:00:00Z")).toBe("0s");
  });

  it("returns an em dash when either timestamp is missing or unparseable", () => {
    // Both fields are `omitzero` on the Go side, so absent is a real wire case
    // and "we don't know" must not be rendered as a fabricated 0s.
    expect(formatDuration(undefined, "2026-08-27T10:00:00Z")).toBe("—");
    expect(formatDuration("2026-08-27T10:00:00Z", undefined)).toBe("—");
    expect(formatDuration(undefined, undefined)).toBe("—");
    expect(formatDuration("", "2026-08-27T10:00:00Z")).toBe("—");
    expect(formatDuration("not a date", "2026-08-27T10:00:00Z")).toBe("—");
    expect(formatDuration("2026-08-27T10:00:00Z", "not a date")).toBe("—");
  });

  it("clamps a negative span to 0s rather than printing nonsense", () => {
    expect(formatDuration("2026-08-27T10:05:00Z", "2026-08-27T10:00:00Z")).toBe("0s");
  });
});

describe("shortId", () => {
  it("keeps the first segment of a UUID and never mangles a short id", () => {
    expect(shortId("44444444-4444-4444-4444-444444444444")).toBe("44444444");
    expect(shortId("abc")).toBe("abc");
  });

  it("never renders an empty cell for a non-empty id", () => {
    // The full id is always in the row's title attribute, but the visible cell
    // is the only thing a user can scan. An id whose first segment is empty
    // must degrade to the whole string, not to nothing.
    expect(shortId("-4444-4444")).toBe("-4444-4444");
    expect(shortId("")).toBe("");
  });
});
