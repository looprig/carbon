import { resolve } from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { playwright } from "@vitest/browser-playwright";
import type { ConfigEnv, Plugin, ProxyOptions } from "vite";
import { defineConfig } from "vitest/config";
import { writeBundleManifest } from "./scripts/write-bundle-manifest.mjs";

/** Vite proxies Factory REST and ClientLink over one loopback development origin. */
export const DEV_API_PREFIX = "/v1";
/**
 * The realtime endpoint `createFactoryClient` derives from a `baseUrl`, and the
 * one entry that carries `ws: true`.
 *
 * It is listed FIRST because Vite matches proxy entries in key insertion order
 * and takes the first whose context the request path starts with — but measured
 * (mutation M1), the order is NOT what makes the upgrade work: Vite's upgrade
 * handler skips every entry without `ws`, so `/v1` ahead of this one still
 * leaves the upgrade to this one, and with identical targets and no rewrite the
 * two orders are indistinguishable today. Most-specific-first is the convention
 * that stays correct if the two entries ever diverge; `ws: true` is the part
 * that is load-bearing, and removing it hangs the upgrade.
 */
export const DEV_REALTIME_PATH = "/v1/realtime";
export const DEFAULT_DEV_FACTORY_TARGET = "http://127.0.0.1:8722";
export const DEV_FACTORY_TARGET_ENV = "CARBON_DEV_FACTORY_TARGET";

/** The Factory origin `npm run dev` proxies to, defaulted or stated explicitly. */
export function resolveFactoryTarget(env: Record<string, string | undefined> = process.env): string {
  const raw = env[DEV_FACTORY_TARGET_ENV]?.trim();
  if (raw === undefined || raw === "") return DEFAULT_DEV_FACTORY_TARGET;
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new Error(`${DEV_FACTORY_TARGET_ENV} must be an absolute http(s) URL, got ${JSON.stringify(raw)}`);
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new Error(
      `${DEV_FACTORY_TARGET_ENV} must be an absolute http(s) URL, got ${JSON.stringify(raw)}` +
        " — the WebSocket upgrade is proxied over the HTTP target, not a ws: one",
    );
  }
  return raw;
}

/**
 * The proxy record itself. No `rewrite` on either entry: Factory routes on the
 * `/v1` prefix, so stripping it is what would 404 every request.
 */
export function devProxy(target: string): Record<string, ProxyOptions> {
  return {
    [DEV_REALTIME_PATH]: { target, ws: true, changeOrigin: false },
    [DEV_API_PREFIX]: { target, changeOrigin: false },
  };
}

/**
 * Whether this Vite invocation is the dev server the proxy exists for.
 *
 * The proxy — and therefore `resolveFactoryTarget()`'s refusal — used to live
 * in the default export's object literal, which meant it ran at MODULE
 * EVALUATION: a malformed `CARBON_DEV_FACTORY_TARGET` failed `vite build` and
 * every `vitest` run, neither of which forwards anything to Factory. Fail-fast
 * was right; the blast radius was not.
 *
 * A lazy `get proxy()` does not fix it, measured: Vite's `resolveServerOptions`
 * reads `server.proxy` while resolving a BUILD config too, so the getter throws
 * under `vite build` exactly as the eager call did. The config-function env is
 * what actually separates the three invocations, and all three were measured:
 * `vite build` arrives as `{command: "build", mode: "production"}`, `vitest` as
 * `{command: "serve", mode: "test"}`, and `npm run dev` is the remaining case —
 * a serve that is not a test run.
 */
export function servesDevProxy(env: Pick<ConfigEnv, "command" | "mode">): boolean {
  return env.command === "serve" && env.mode !== "test";
}

/** The name `vite.config.test.ts` looks the plugin up by. */
export const BUNDLE_MANIFEST_PLUGIN_NAME = "looprig-bundle-manifest";

/**
 * The environment variable that makes a build a RELEASE build.
 *
 * `release` is an input to the build rather than a constant anywhere, because
 * the release bundle and a development bundle are produced by the same command
 * — `make release-dist` runs `npm run build` twice, into throwaway directories,
 * and compares them. Only the flag distinguishes the artefact it publishes.
 *
 * Exactly `"1"`. Anything else, including `"true"` and `"yes"`, is not a
 * release: this gates whether a consumer will serve the bundle as official, so
 * the affirmative case is one spelling and everything else fails closed.
 */
export const BUNDLE_RELEASE_ENV = "CARBON_BUNDLE_RELEASE";

/**
 * Writes `looprig-bundle.json` into whatever directory this build produced.
 *
 * It is a plugin, not a step in the `build` npm script, because the output
 * directory is not fixed: `make release-dist` builds into two temporary
 * directories with `--outDir {out} --emptyOutDir` and installs one of them over
 * `dist/`. A writer that only ever wrote to `dist/` would leave the release
 * candidate — the tree that actually becomes the tag — with no marker at all,
 * and `browserui.BundleProtocolVersion` would answer `ErrNoBundleManifest` for every
 * consumer of the published module.
 *
 * `apply: "build"` matters: `npm run dev` and every vitest project load this
 * same config, and neither should write into the committed bundle.
 *
 * @param env Environment to read {@link BUNDLE_RELEASE_ENV} from.
 */
export function bundleManifestPlugin(env: Record<string, string | undefined> = process.env): Plugin {
  let outDir = "";
  return {
    name: BUNDLE_MANIFEST_PLUGIN_NAME,
    apply: "build",
    configResolved(config) {
      // The RESOLVED directory, not `build.outDir` as written: that is the
      // relative "../internal/browserui/dist" above unless `--outDir` overrode it, and the plugin
      // must not depend on the process's working directory either way.
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      writeBundleManifest(outDir, { release: env[BUNDLE_RELEASE_ENV] === "1" });
    },
  };
}

export default defineConfig((env) => ({
  plugins: [react(), tailwindcss(), bundleManifestPlugin()],
  resolve: {
    // One React instance for everything. Without this a prebundled dependency
    // (@tanstack/react-router) and the app can end up with separate copies,
    // and every hook in the router's own components reads a null dispatcher:
    // "Cannot read properties of null (reading 'useContext')".
    dedupe: ["react", "react-dom"],
  },
  // Pre-bundled up front rather than discovered mid-run. Vitest reloads the
  // page when the optimizer finds a new dependency during a run, and warns
  // that doing so "may cause tests to fail, lead to flaky behaviour or
  // duplicated test runs". It happens on any cold cache -- a fresh `npm ci` on
  // CI, exactly where a flake is hardest to read.
  optimizeDeps: {
    include: ["react", "react-dom", "react-dom/client", "react/jsx-dev-runtime", "@tanstack/react-router"],
  },
  build: {
    // The Go side embeds this directory (internal/browserui/dist). Keeping the build output
    // and the embed path identical means there is no copy step to forget.
    outDir: "../internal/browserui/dist",
    // Explicitly false. Vite would refuse to empty an outDir outside the
    // project root anyway (and warn on every build), but stating it here is
    // what stops a later edit from deleting dist/index.html, which //go:embed
    // needs at compile time on a machine with no Node toolchain and which this
    // build only rewrites at the very end. The other file that must survive,
    // dist/looprig-bundle.json, is rewritten by bundleManifestPlugin on every
    // build, so it needs no protection here. `npm run dist:reset` clears stale
    // assets and restores the committed snapshot.
    emptyOutDir: false,
    // Default, restated: //go:embed dist (without `all:`) silently skips any
    // entry whose name starts with `_` or `.`, so the asset directory must
    // never be renamed to something like SvelteKit's `_app`. assets.go uses
    // `all:dist`, so this is belt as well as braces.
    assetsDir: "assets",
  },
  // Spread rather than `server: <cond> ? {...} : undefined`, so a build or a
  // test run carries no `server` key at all rather than one holding undefined.
  ...(servesDevProxy(env) ? { server: { proxy: devProxy(resolveFactoryTarget()) } } : {}),
  test: {
    // Same guard packages/protocol and packages/react use, and the same one
    // client/ uses: a test that asserts nothing is a test that passes for the
    // wrong reason.
    expect: { requireAssertions: true },
    projects: [
      {
        extends: "./vite.config.ts",
        test: {
          name: "app",
          browser: {
            enabled: true,
            provider: playwright(),
            instances: [{ browser: "chromium", headless: true }],
          },
          include: ["src/**/*.test.{ts,tsx}"],
          setupFiles: ["./src/test/setup.ts"],
        },
      },
      {
        // A second, node-only project for the two things that cannot run in a
        // browser: the assertions about THIS config object (importing
        // `vite.config.ts` from the browser project would drag
        // @vitejs/plugin-react, @tailwindcss/vite and node:url into a browser
        // bundle) and the build-output guard, which imports `node:fs`.
        extends: "./vite.config.ts",
        test: {
          name: "node",
          environment: "node",
          include: ["vite.config.test.ts", "scripts/**/*.test.ts"],
          // NOT vitest's 5 s default. `scripts/bundle-workflow.test.ts` drives
          // the real release script: each of its cases clones a git fixture,
          // runs two isolated builds through fake release tools and inspects
          // the index, which is 6-8 s of genuine work per case on a warm host
          // and more on a loaded one. Eleven of its cases failed here at the
          // default while passing 28/28 at 60 s, so the cap was measuring the
          // machine rather than the script.
          //
          // This is the ONLY cap those cases get, and it must stay that way. A
          // per-case third argument to `it`/`it.each` OVERRIDES this value, and
          // seven interrupt cases carried a leftover 20_000 from before this
          // line existed -- reintroducing the exact failure mode it was raised
          // to remove. They were measured red here at load 62 (SIGTERM took
          // 27.5 s against the 20 s cap) and the caps were removed. Do not add
          // a per-case timeout to this project; move this number instead.
          testTimeout: 120_000,
        },
      },
    ],
  },
}));
