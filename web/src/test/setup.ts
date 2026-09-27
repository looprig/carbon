// Importing the default vitest-browser-react entry (not /pure) registers
// `page.render` / `page.renderHook` and a global `beforeEach(cleanup)`.
// StrictMode stays off by default; a test that means to exercise double-
// mounting puts <StrictMode> at the ROOT of what it renders, because a
// `wrapper` sits INSIDE the root and React only runs the mount/unmount/remount
// simulation for a root-level StrictMode (measured in Phase 4).
import "vitest-browser-react";

// The theme stylesheet is a real deliverable (theme.test.tsx asserts computed
// colors against it), so every browser test loads it exactly as index.html's
// entry point does.
import "../styles/theme.css";
