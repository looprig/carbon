// The palette and the type split are product decisions, not styling taste:
// "mono for every machine-authored fact" (ids, statuses, timestamps, paths) is
// what makes a transcript scannable, and "lime does, blue decides" is what
// makes an approval read as a human decision rather than an agent action
// (capstan-spec.md §12). Asserting COMPUTED styles is the only way to pin
// them, because a typo in a Tailwind token name emits no rule and fails
// silently.
import { cdp, page } from "vitest/browser";
import { describe, expect, it } from "vitest";
import { render } from "vitest-browser-react";

function computed(testId: string): CSSStyleDeclaration {
  const node = document.querySelector(`[data-testid=${testId}]`);
  if (!node) throw new Error(`no element with data-testid=${testId}`);
  return getComputedStyle(node);
}

describe("theme tokens", () => {
  it("resolves the dark background and the lime/blue brand colors", async () => {
    render(
      <div data-testid="swatches">
        <div data-testid="bg" className="bg-bg h-4 w-4" />
        <div data-testid="sidebar" className="bg-sidebar h-4 w-4" />
        <div data-testid="card" className="bg-card h-4 w-4" />
        <div data-testid="accent" className="bg-accent h-4 w-4" />
        <div data-testid="border" className="bg-border h-4 w-4" />
        <div data-testid="muted" className="bg-muted h-4 w-4" />
        <div data-testid="loop" className="bg-loop h-4 w-4" />
        <div data-testid="rig" className="bg-rig h-4 w-4" />
        <div data-testid="fail" className="bg-fail h-4 w-4" />
      </div>,
    );
    await expect.element(page.getByTestId("swatches")).toBeInTheDocument();
    expect(computed("bg").backgroundColor).toBe("rgb(16, 16, 16)"); // #101010
    expect(computed("sidebar").backgroundColor).toBe("rgb(23, 25, 27)"); // #17191b
    expect(computed("card").backgroundColor).toBe("rgb(27, 29, 32)"); // #1b1d20
    expect(computed("accent").backgroundColor).toBe("rgb(36, 37, 39)"); // #242527
    expect(computed("border").backgroundColor).toBe("rgb(43, 46, 50)"); // #2b2e32
    expect(computed("muted").backgroundColor).toBe("rgb(139, 144, 150)"); // #8b9096
    expect(computed("loop").backgroundColor).toBe("rgb(212, 248, 77)"); // #D4F84D lime — the loop
    expect(computed("rig").backgroundColor).toBe("rgb(162, 210, 255)"); // #A2D2FF blue — the rig
    expect(computed("fail").backgroundColor).toBe("rgb(255, 107, 107)"); // #FF6B6B
  });

  it("uses Inter for prose and JetBrains Mono for machine facts", async () => {
    render(
      <div data-testid="type">
        <p data-testid="prose" className="font-sans">
          prose
        </p>
        <p data-testid="fact" className="font-mono">
          01H0
        </p>
      </div>,
    );
    await expect.element(page.getByTestId("type")).toBeInTheDocument();
    expect(computed("prose").fontFamily).toContain("Inter");
    expect(computed("fact").fontFamily).toContain("JetBrains Mono");
  });

  it("actually ships both font faces, not just their names", async () => {
    // A `font-family` assertion alone is hollow: it reads the declaration, not
    // the font, so it passes with no @font-face anywhere and the browser
    // silently falling back to the next family in the stack.
    //
    // `document.fonts.check()` is hollow too, and measurably so — it returned
    // true for both families while theme.css was still a bare
    // `@import "tailwindcss"` with no face registered anywhere. Chromium
    // answers "can I lay this out?", and it always can, by falling back.
    // Iterating the FontFaceSet is the assertion that bites: an unregistered
    // family is simply not in it.
    const families = new Set([...document.fonts].map((face) => face.family.replaceAll('"', "")));
    expect([...families]).toContain("Inter Variable");
    expect([...families]).toContain("JetBrains Mono Variable");
  });

  it("animates only the running pulse, and disables it under prefers-reduced-motion", async () => {
    render(<span data-testid="pulse" className="animate-loop-pulse inline-block h-2 w-2" />);
    await expect.element(page.getByTestId("pulse")).toBeInTheDocument();
    expect(computed("pulse").animationName).toBe("loop-pulse");

    // §12 lists prefers-reduced-motion as part of the design system, not an
    // afterthought. Asserting that a `@media (prefers-reduced-motion: reduce)`
    // BLOCK exists would be hollow — the block can exist and still lose the
    // cascade to Tailwind's utilities layer, which is exactly the mistake
    // worth catching. So emulate the media state through CDP and assert the
    // COMPUTED animation, which is the thing the user's vestibular system
    // actually experiences.
    await emulateReducedMotion("reduce");
    try {
      await nextFrame();
      expect(computed("pulse").animationName).toBe("none");
    } finally {
      await emulateReducedMotion("no-preference");
    }
  });
});

async function emulateReducedMotion(value: "reduce" | "no-preference"): Promise<void> {
  await cdp().send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-reduced-motion", value }],
  });
}

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}
