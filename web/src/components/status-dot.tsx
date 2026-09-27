import { cn } from "../lib/cn";

/**
 * `SessionSummary.state` is a bare string on the wire — `session_summary`'s
 * schema types it `{"type":"string"}` with no enum, and the Go field is
 * `omitempty`, so "" is a legitimate value from a catalog entry written before
 * the field existed. This maps the states harness's catalog actually emits
 * (harness/pkg/sessionstore/catalog.go: running, waiting_on_gate, idle, failed,
 * interrupted, stopped) onto the four visual buckets capstan-spec.md §12
 * defines, and treats anything unrecognised as the neutral gray one.
 *
 * The raw state string is still what the user reads — we never rewrite a
 * machine fact, only colour it.
 *
 * 05-app.md's version of this matched "waiting" and "waiting_gate". Harness
 * emits NEITHER; the constant is `waiting_on_gate`. Under the plan's mapping a
 * session blocked on a permission gate — the one state that most needs to pull
 * the eye — would have rendered as a gray idle dot.
 */
export type StatusTone = "running" | "waiting" | "failed" | "idle";

export function toneFor(state: string | undefined): StatusTone {
  switch (state) {
    case "running":
      return "running";
    case "waiting_on_gate":
      return "waiting";
    case "failed":
      return "failed";
    // idle, interrupted and stopped are all "done or queued" in §12's status
    // vocabulary, and share the gray dot with the unknown and absent cases.
    default:
      return "idle";
  }
}

const toneClass: Record<StatusTone, string> = {
  running: "bg-loop animate-loop-pulse",
  waiting: "bg-rig",
  failed: "bg-fail",
  idle: "bg-muted",
};

/**
 * The dot itself, given a tone directly.
 *
 * Split out because a TOOL call's status vocabulary is not a session's:
 * `ToolRow.status` is running/ok/error/cancelled, and feeding those through
 * `toneFor` would paint a failed tool call the neutral gray of an idle session,
 * which is the same class of silent mis-colouring `toneFor`'s own comment
 * records. A caller with its own vocabulary maps it to a tone and renders this;
 * `StatusDot` is that mapping for `SessionSummary.state`.
 *
 * `label` is required rather than derived: the tone is a colour bucket and
 * several distinct states share one, so only the caller knows what the dot
 * actually means.
 */
export function ToneDot({ tone, label }: { tone: StatusTone; label: string }): React.JSX.Element {
  return (
    <span
      data-testid="status-dot"
      data-status={tone}
      role="img"
      aria-label={label}
      className={cn("inline-block h-2 w-2 shrink-0 rounded-full", toneClass[tone])}
    />
  );
}

export function StatusDot({ state }: { state: string | undefined }): React.JSX.Element {
  return (
    <ToneDot tone={toneFor(state)} label={state === undefined || state === "" ? "unknown" : state} />
  );
}
