/**
 * Narrows a caught `unknown` to an `Error` so a component can render
 * `.message` without a type assertion.
 *
 * `@looprig/react`'s stores have their own copy of this (`stores/publisher.ts`'s
 * `asError`) but do not export it, and `@looprig/client` has none: its
 * transport only ever rejects with a real `Error` subclass. This exists for the
 * one place `app/` awaits a transport call directly — session creation, which
 * has no hook in `@looprig/react` — and nothing else in `app/` should be
 * catching transport rejections at all.
 *
 * The non-Error branch is not dead code: a `throw "boom"` anywhere below is
 * legal JavaScript, and `String(cause)` in a message beats `undefined` in the
 * UI. `cause` is preserved so the original value survives in devtools.
 */
export function toError(cause: unknown): Error {
  return cause instanceof Error ? cause : new Error(String(cause), { cause });
}
