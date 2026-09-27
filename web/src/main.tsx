import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import "./styles/theme.css";
import { browserFactoryComposition, createAppRouter } from "./router";

// StrictMode is on in the real app, not just under test: every hook in
// @looprig/react is pinned against the double-mount, and the one place that
// actually exercises it is a root-level StrictMode like this one.
const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

// The one application-scoped Factory client is constructed by the router's root
// route (see createAppRouter); `browserFactoryComposition` decides only WHERE
// Factory is, and lives in router.tsx so that it has a reader.
const router = createAppRouter({ factory: browserFactoryComposition(import.meta.env) });

createRoot(root).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
);
