import { useState } from "react";

export interface InterruptButtonProps {
  /** Resolves harness's own `interrupted`: whether any RUNNING turn was actually cancelled. */
  onInterrupt: () => Promise<boolean>;
  interrupting: boolean;
  error: Error | null;
}

/**
 * Stops every in-flight turn in the session.
 *
 * Deliberately not lime. §12's rule is "lime does, blue decides"; interrupting
 * is neither — it is the human taking something away — so it wears the failure
 * colour rather than the one a user reaches for to make the agent go.
 *
 * `interrupted: false` is a NORMAL answer, not a failure: an idle session had
 * nothing to cancel, and harness reports that rather than erroring. It never
 * reaches `error`, so without saying it out loud the button would look broken
 * every time a user hit it a moment too late. The note is cleared on the next
 * attempt, so it can never describe an older click than the last one.
 */
export function InterruptButton({
  onInterrupt,
  interrupting,
  error,
}: InterruptButtonProps): React.JSX.Element {
  const [nothingRunning, setNothingRunning] = useState(false);

  async function interrupt(): Promise<void> {
    setNothingRunning(false);
    setNothingRunning(!(await onInterrupt()));
  }

  return (
    <div className="flex items-center gap-2">
      <button
        type="button"
        data-testid="interrupt-button"
        disabled={interrupting}
        onClick={() => void interrupt()}
        className="rounded-md border border-fail px-3 py-1 text-xs font-medium text-fail disabled:opacity-50"
      >
        {interrupting ? "Interrupting…" : "Interrupt"}
      </button>
      {error === null ? (
        nothingRunning ? (
          <span data-testid="interrupt-noop" role="status" className="font-mono text-xs text-muted">
            Nothing was running.
          </span>
        ) : null
      ) : (
        <span role="alert" data-testid="interrupt-error" className="font-mono text-xs text-fail">
          {error.message}
        </span>
      )}
    </div>
  );
}
