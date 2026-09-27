import { useState } from "react";

export interface ComposerProps {
  /** Resolves `true` when the server accepted the input; `false` leaves the draft alone. */
  onSubmit: (text: string) => Promise<boolean>;
  submitting: boolean;
  /** Any open gate — answerable or not. Either kind blocks the loop. */
  gateOpen: boolean;
  error: Error | null;
}

/**
 * The message box, pinned to the bottom of the session view (capstan-spec.md
 * §8). Three behaviours are load-bearing.
 *
 * **Locked while a gate is open.** A session waiting on a human decision cannot
 * advance, so accepting input would queue it silently behind a turn that cannot
 * run. The lockout is enforced in `send`, not only through the `disabled`
 * attribute: `disabled` is a DOM state that a programmatic submit walks
 * straight past, and the rule is a rule, not a visual. It applies to
 * unanswerable gates too — an `ask_user` gate blocks the loop exactly as hard
 * as a permission gate does, whichever client eventually answers it.
 *
 * **The draft survives a failed send.** `useComposer.submit` resolves `false`
 * for a rejected send, and clearing the box anyway would destroy what the user
 * typed on a transient network error.
 *
 * **The hint says where mid-turn input lands.** `input_queued` is ephemeral and
 * carries no text (design §3b), so until `TurnStarted` this sentence is the
 * only feedback there is.
 *
 * The send button is lime: §12 puts every action that summons an agent on the
 * loop colour.
 */
export function Composer({ onSubmit, submitting, gateOpen, error }: ComposerProps): React.JSX.Element {
  const [text, setText] = useState("");
  const locked = gateOpen || submitting;

  async function send(): Promise<void> {
    if (locked || text.trim() === "") return;
    if (await onSubmit(text)) setText("");
  }

  return (
    <div className="sticky bottom-0 mx-auto w-full max-w-[760px] border-t border-border bg-bg p-3">
      <form
        data-testid="composer-form"
        className="flex items-end gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          void send();
        }}
      >
        <label className="sr-only" htmlFor="composer-input">
          Message
        </label>
        <textarea
          id="composer-input"
          data-testid="composer-input"
          rows={2}
          value={text}
          disabled={locked}
          placeholder={gateOpen ? "Waiting on your decision…" : "Send a message…"}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={(event) => {
            // Enter sends, Shift-Enter is a newline. A modified Enter belongs
            // to the browser and is left alone.
            if (event.key !== "Enter" || event.shiftKey || event.metaKey || event.ctrlKey || event.altKey) {
              return;
            }
            event.preventDefault();
            void send();
          }}
          className="flex-1 resize-none rounded-md border border-border bg-card p-2 text-sm disabled:opacity-50"
        />
        <button
          type="submit"
          data-testid="composer-submit"
          disabled={locked || text.trim() === ""}
          className="rounded-md bg-loop px-4 py-2 text-sm font-medium text-bg disabled:opacity-50"
        >
          {submitting ? "Sending…" : "Send"}
        </button>
      </form>
      <p data-testid="composer-hint" className="mt-1 text-xs text-muted">
        {gateOpen ? "Answer the gate before sending more." : "Lands at the end of the current turn."}
      </p>
      {error === null ? null : (
        <p role="alert" data-testid="composer-error" className="mt-1 font-mono text-xs text-fail">
          {error.message}
        </p>
      )}
    </div>
  );
}
