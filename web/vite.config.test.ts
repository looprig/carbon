import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer as createHttpServer, request as httpRequest, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { createServer, type ConfigEnv, type Plugin, type UserConfig, type ViteDevServer } from "vite";
import config, {
  BUNDLE_MANIFEST_PLUGIN_NAME,
  BUNDLE_RELEASE_ENV,
  bundleManifestPlugin,
  DEFAULT_DEV_FACTORY_TARGET,
  DEV_API_PREFIX,
  DEV_FACTORY_TARGET_ENV,
  DEV_REALTIME_PATH,
  devProxy,
  resolveFactoryTarget,
  servesDevProxy,
} from "./vite.config";
import { BUNDLE_MANIFEST_NAME } from "./scripts/write-bundle-manifest.mjs";

/**
 * The three invocations Vite distinguishes, exactly as it hands them over —
 * measured by logging the env from the config function under `vite build` and
 * under `vitest run`, not assumed from the documentation.
 */
const BUILD: ConfigEnv = { command: "build", mode: "production", isSsrBuild: false, isPreview: false };
const TEST: ConfigEnv = { command: "serve", mode: "test", isSsrBuild: false, isPreview: false };
const DEV: ConfigEnv = { command: "serve", mode: "development", isSsrBuild: false, isPreview: false };

/** The default export is a config FUNCTION now; this is the one call site shape. */
function resolved(env: ConfigEnv): UserConfig {
  return config(env) as UserConfig;
}

/**
 * Runs `body` with `CARBON_DEV_FACTORY_TARGET` set to `value`, restored after.
 *
 * The default export reads the AMBIENT environment — `resolveFactoryTarget()`
 * defaults its parameter to `process.env` — so a test about what a real
 * invocation gets has to state the environment rather than pass a record. It
 * restores rather than deletes, because the variable may legitimately be set in
 * the shell that started vitest and the other tests here would then be reading
 * a value this file invented.
 */
function withTargetEnv<T>(value: string, body: () => T): T {
  const previous = process.env[DEV_FACTORY_TARGET_ENV];
  process.env[DEV_FACTORY_TARGET_ENV] = value;
  try {
    return body();
  } finally {
    if (previous === undefined) delete process.env[DEV_FACTORY_TARGET_ENV];
    else process.env[DEV_FACTORY_TARGET_ENV] = previous;
  }
}

/**
 * The build and dev-server settings are load-bearing in ways nothing else can
 * observe:
 *
 *  - `emptyOutDir: false` is what keeps `../dist/index.html` — the tracked
 *    placeholder `//go:embed all:dist` needs at compile time — alive across a
 *    build. Flipping it to true breaks `go build` on any machine without Node
 *    the moment a build fails partway.
 *  - `outDir` IS the embed path. A copy step is what a divergence would need.
 *  - `assetsDir` must not start with `_` or `.`; the bare `//go:embed dist`
 *    form silently skips such entries (see 00-plan §6.10).
 *  - the dev proxy carries the whole Factory plane, REST and realtime both. A
 *    wrong target, a rewritten path or a missing WebSocket upgrade fails only
 *    at runtime, and only for someone running `npm run dev`.
 *
 * All of them are single tokens in a config file no other test loads, so this
 * is the only place they can be pinned.
 */
describe("vite config", () => {
  it("builds into the //go:embed directory without emptying it", () => {
    expect(resolved(BUILD).build?.outDir).toBe("../internal/browserui/dist");
    expect(resolved(BUILD).build?.emptyOutDir).toBe(false);
  });

  it("names an asset directory //go:embed will not skip", () => {
    const assetsDir = resolved(BUILD).build?.assetsDir;
    expect(assetsDir).toBe("assets");
    expect(assetsDir?.startsWith("_")).toBe(false);
    expect(assetsDir?.startsWith(".")).toBe(false);
  });
});

/**
 * The marker has to be written by the BUILD, not by a separate ritual.
 *
 * `make release-dist` builds into a temporary `--outDir` with `--emptyOutDir`
 * and then replaces `dist/` wholesale with that tree, so anything a build does
 * not produce is DELETED from the released bundle. A manifest written by a
 * hand-run CLI survives development and disappears from the tag — which is the
 * v0.1.0 failure again, one level up: a marker that says the right thing in the
 * working tree and is absent from what consumers embed.
 *
 * So the writer is a build plugin, and these are its two load-bearing
 * properties: it writes into the RESOLVED output directory (not the literal
 * `../dist` in the config), and the release flag is an input to the build
 * rather than a constant.
 */
describe("bundle manifest plugin", () => {
  const temporaries: string[] = [];

  afterEach(() => {
    while (temporaries.length > 0) rmSync(temporaries.pop()!, { recursive: true, force: true });
  });

  function temporaryDirectory(): string {
    const created = mkdtempSync(join(tmpdir(), "looprig-wui-vite-manifest-"));
    temporaries.push(created);
    return created;
  }

  /** Invokes a Vite hook whether it is declared as a function or as `{handler}`. */
  function callHook(hook: unknown, ...args: unknown[]): void {
    const handler = typeof hook === "function"
      ? hook
      : (hook as { handler: (...rest: unknown[]) => unknown }).handler;
    (handler as (...rest: unknown[]) => unknown).apply({}, args);
  }

  function runPlugin(
    outDir: string,
    env: Record<string, string | undefined>,
    root = "/nonexistent-root",
  ): void {
    const plugin = bundleManifestPlugin(env);
    callHook(plugin.configResolved, { root, build: { outDir } });
    callHook(plugin.closeBundle);
  }

  function writtenManifest(directory: string): Record<string, unknown> {
    return JSON.parse(readFileSync(join(directory, BUNDLE_MANIFEST_NAME), "utf8")) as Record<string, unknown>;
  }

  /**
   * Vite's `plugins` is an arbitrarily nested array of plugins, falsey holes
   * and promises — `@vitejs/plugin-react` alone contributes an array. Flattened
   * by hand rather than with `flat(Infinity)`, whose recursive return type is
   * what tsc rejects with TS2589 here.
   */
  function pluginsOf(config: UserConfig): Plugin[] {
    const flatten = (value: unknown): Plugin[] =>
      Array.isArray(value) ? value.flatMap(flatten) : value ? [value as Plugin] : [];
    return flatten(config.plugins);
  }

  it("is registered on the build, and only on the build", () => {
    const found = pluginsOf(resolved(BUILD)).filter(
      (plugin) => plugin?.name === BUNDLE_MANIFEST_PLUGIN_NAME,
    );
    expect(found).toHaveLength(1);
    // `apply: "build"` is what keeps `npm run dev` and every vitest run — both
    // of which load this same config — from writing into the committed dist.
    expect(found[0]?.apply).toBe("build");
  });

  it("writes the marker into the output directory the build produced", () => {
    const out = temporaryDirectory();
    runPlugin(out, {});
    expect(writtenManifest(out)).toEqual({
      core_version: "v0.12.0",
      client_version: "0.3.0",
      react_version: "0.3.0",
      release: false,
      sessionwire_version: 1,
    });
  });

  it("resolves a relative outDir against the project root, not the process directory", () => {
    // This is the real config's shape: `root` is `app/` and `build.outDir` is
    // the literal "../internal/browserui/dist". Taking `outDir` as written would put the marker
    // wherever the build happened to be invoked from — beside the bundle only
    // by coincidence, and silently nowhere at all under `make release-dist`,
    // which passes an absolute `--outDir` but runs from the repository root.
    const root = temporaryDirectory();
    const out = join(root, "nested-out");
    mkdirSync(out);
    runPlugin(out, {}, root);
    expect(writtenManifest(out).release).toBe(false);

    const relativeRoot = temporaryDirectory();
    const relativeOut = join(relativeRoot, "from-relative");
    mkdirSync(relativeOut);
    runPlugin("from-relative", {}, relativeRoot);
    expect(writtenManifest(relativeOut).release).toBe(false);
  });

  it("declares a release build a release, and every other build not one", () => {
    const release = temporaryDirectory();
    runPlugin(release, { [BUNDLE_RELEASE_ENV]: "1" });
    expect(writtenManifest(release).release).toBe(true);

    const ordinary = temporaryDirectory();
    runPlugin(ordinary, { [BUNDLE_RELEASE_ENV]: "0" });
    expect(writtenManifest(ordinary).release).toBe(false);
  });

  it("reads the flag from the environment it is given, not the ambient one", () => {
    // The plugin defaults its parameter to `process.env`, so a test that could
    // only state the ambient environment would leak into the whole node
    // project. Passing the record is also what makes the case above a pair.
    const out = temporaryDirectory();
    runPlugin(out, { [BUNDLE_RELEASE_ENV]: "yes" });
    expect(writtenManifest(out).release).toBe(false);
  });
});

describe("dev proxy target", () => {
  it("defaults to the documented loopback Factory address", () => {
    expect(resolveFactoryTarget({})).toBe(DEFAULT_DEV_FACTORY_TARGET);
    expect(new URL(DEFAULT_DEV_FACTORY_TARGET).hostname).toBe("127.0.0.1");
  });

  it("takes an explicit override from the environment", () => {
    expect(resolveFactoryTarget({ [DEV_FACTORY_TARGET_ENV]: "http://127.0.0.1:9999" })).toBe(
      "http://127.0.0.1:9999",
    );
    // Whitespace-only is the shape a shell hands over for an unset-but-exported
    // variable; it means "no override", not "proxy to the empty string" — and
    // not a refusal either, which is what an untrimmed value would produce and
    // which `.not.toThrow()` is here to read as an assertion rather than as an
    // uncaught error.
    expect(() => resolveFactoryTarget({ [DEV_FACTORY_TARGET_ENV]: "   " })).not.toThrow();
    expect(resolveFactoryTarget({ [DEV_FACTORY_TARGET_ENV]: "   " })).toBe(DEFAULT_DEV_FACTORY_TARGET);
  });

  it("refuses a target it cannot proxy rather than starting and failing per request", () => {
    expect(() => resolveFactoryTarget({ [DEV_FACTORY_TARGET_ENV]: "127.0.0.1:8722" })).toThrow(
      DEV_FACTORY_TARGET_ENV,
    );
    expect(() => resolveFactoryTarget({ [DEV_FACTORY_TARGET_ENV]: "ws://127.0.0.1:8722" })).toThrow(
      DEV_FACTORY_TARGET_ENV,
    );
  });

  it("carries the upgrade on the realtime entry, lists it first, and rewrites neither", () => {
    const proxy = devProxy("http://127.0.0.1:1");
    // Vite matches HTTP proxy entries in key insertion order and takes the
    // FIRST whose context the path starts with, so most-specific-first is the
    // order that stays correct if the two entries ever diverge. It is NOT what
    // makes the upgrade work — Vite's upgrade handler skips entries without
    // `ws`, so swapping these two leaves the behavioural tests below green.
    // This assertion pins the shape; `ws: true` is what has behaviour.
    expect(Object.keys(proxy)).toEqual([DEV_REALTIME_PATH, DEV_API_PREFIX]);
    // Exact, not `toMatchObject`: a rewrite is what would strip the `/v1`
    // Factory routes on, and `changeOrigin` left off is what keeps the
    // browser's Origin and Host reaching Factory unmodified — an origin or CSRF
    // check reads exactly those. Both are absences, and only an exact
    // comparison can assert an absence it was not told to look for.
    expect(proxy[DEV_REALTIME_PATH]).toEqual({ target: "http://127.0.0.1:1", ws: true, changeOrigin: false });
    expect(proxy[DEV_API_PREFIX]).toEqual({ target: "http://127.0.0.1:1", changeOrigin: false });
  });

  it("is what the dev server actually installs, and nothing else installs it", () => {
    // The environment is STATED, not read: `toEqual(devProxy(resolveFactoryTarget()))`
    // would pass for a config that resolved the target some other way, because
    // both sides would be the same call. A value only this test knows is what
    // shows the env reaches the installed record.
    withTargetEnv("http://127.0.0.1:9999", () => {
      expect(resolved(DEV).server?.proxy).toEqual(devProxy("http://127.0.0.1:9999"));
      // A build writes files and a vitest run drives a browser over its own
      // fixtures; neither forwards anything to Factory, so neither carries the
      // proxy — and, below, neither pays for a malformed target either.
      expect(resolved(BUILD).server).toBeUndefined();
      expect(resolved(TEST).server).toBeUndefined();
    });
    expect(servesDevProxy(DEV)).toBe(true);
    expect(servesDevProxy(BUILD)).toBe(false);
    expect(servesDevProxy(TEST)).toBe(false);
  });

  it("confines a malformed target's refusal to the dev server", () => {
    // The defect: `resolveFactoryTarget()` was called in the default export's
    // object literal, so it ran at MODULE EVALUATION and a bad value failed
    // `vite build` and every `vitest` run — neither of which touches the proxy.
    // A lazy `get proxy()` is not the fix either; Vite's `resolveServerOptions`
    // reads `server.proxy` while resolving a build config too.
    //
    // Measured end to end at both ends of the fix, not only here:
    // `CARBON_DEV_FACTORY_TARGET=nonsense npx vite build` exited 1 before and
    // exits 0 after, and `npx vite` with the same value exits 1 with this
    // message in both.
    withTargetEnv("127.0.0.1:8722", () => {
      expect(() => resolved(BUILD)).not.toThrow();
      expect(() => resolved(TEST)).not.toThrow();
      expect(() => resolved(DEV)).toThrow(DEV_FACTORY_TARGET_ENV);
      // Still the WHOLE config, not a stub with the proxy chopped out: the
      // build path a bad target must not break is the one that emits the
      // //go:embed bundle.
      expect(resolved(BUILD).build?.outDir).toBe("../internal/browserui/dist");
    });
  });
});

/**
 * The structural assertions above read the config object; these read the
 * BEHAVIOUR, by running the real dev server over the real proxy record against
 * a fixture origin standing in for Factory. Every mistake this catches —
 * a stripped prefix, a dropped query string, an unproxied upgrade, a swallowed
 * realtime route — is invisible in the object and fatal at runtime.
 */
describe("dev proxy behaviour", () => {
  const paths: string[] = [];
  const received: Array<{ host?: string; origin?: string }> = [];
  const upgrades: string[] = [];
  let factory: Server;
  let vite: ViteDevServer;
  let port = 0;
  let factoryOrigin = "";

  beforeAll(async () => {
    factory = createHttpServer((req, res) => {
      paths.push(req.url ?? "");
      received.push({ host: req.headers.host, origin: req.headers.origin });
      res.writeHead(200, { "content-type": "text/plain", connection: "close" });
      res.end("factory");
    });
    factory.on("upgrade", (req, socket) => {
      upgrades.push(req.url ?? "");
      socket.end("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n");
    });
    await new Promise<void>((resolve) => factory.listen(0, "127.0.0.1", resolve));
    const target = `http://127.0.0.1:${(factory.address() as AddressInfo).port}`;
    factoryOrigin = target;

    vite = await createServer({
      configFile: false,
      root: import.meta.dirname,
      logLevel: "silent",
      server: { host: "127.0.0.1", port: 0, proxy: devProxy(target) },
    });
    await vite.listen();
    port = (vite.httpServer!.address() as AddressInfo).port;
  });

  afterAll(async () => {
    await vite?.close();
    factory?.closeAllConnections();
    await new Promise<void>((resolve) => factory.close(() => resolve()));
  });

  function get(path: string, extra: Record<string, string> = {}): Promise<number> {
    return new Promise((resolve, reject) => {
      const req = httpRequest(
        { host: "127.0.0.1", port, path, headers: { connection: "close", ...extra } },
        (res: IncomingMessage) => {
          res.resume();
          res.on("end", () => resolve(res.statusCode ?? 0));
        },
      );
      req.on("error", reject);
      req.end();
    });
  }

  it("forwards the API path to Factory verbatim, prefix and query intact", async () => {
    const path = "/v1/sessions/44444444-4444-4444-4444-444444444444/events?since=7";
    expect(await get(path)).toBe(200);
    expect(paths).toContain(path);
  });

  it("hands Factory the browser's own Host and Origin rather than the target's", async () => {
    // `changeOrigin: false` is load-bearing and this is its behavioural reader.
    // http-proxy's `changeOrigin: true` rewrites the outbound Host header to the
    // TARGET's authority, and Factory's guard runs a HOST rule — `Guard.Check`
    // calls `hostTrusted` first and answers `ReasonHostNotTrusted` — before its
    // Origin rule, so the rewrite lands on exactly what that rule reads.
    //
    // The discriminator is that the Vite port and the fixture-Factory port are
    // different: asserting the VITE authority is what fails the moment
    // changeOrigin flips, where asserting "some host" would not.
    const browserOrigin = `http://127.0.0.1:${port}`;
    const before = received.length;
    expect(await get("/v1/capabilities", { origin: browserOrigin })).toBe(200);
    const seen = received.slice(before);
    expect(seen).toEqual([{ host: `127.0.0.1:${port}`, origin: browserOrigin }]);
    // And not the target's, which is the value the rewrite would substitute.
    expect(seen[0]?.host).not.toBe(new URL(factoryOrigin).host);
  });

  it("does not proxy a path outside the API prefix", async () => {
    const before = paths.length;
    // The SPA's own routes are served by Vite; only `/v1` belongs to Factory.
    await get("/sessions/44444444-4444-4444-4444-444444444444");
    expect(paths.slice(before)).toEqual([]);
  });

  it("proxies the WebSocket upgrade for the realtime path", async () => {
    // Raced against a deadline rather than left to the runner's timeout: an
    // unproxied upgrade produces neither a response nor an upgrade, and a test
    // killed by timeout has asserted nothing.
    const outcome = await new Promise<string>((resolve, reject) => {
      const deadline = setTimeout(() => resolve("no upgrade and no response"), 2_000);
      const settle = (value: string): void => {
        clearTimeout(deadline);
        resolve(value);
      };
      const req = httpRequest({
        host: "127.0.0.1",
        port,
        path: DEV_REALTIME_PATH,
        headers: {
          connection: "Upgrade",
          upgrade: "websocket",
          "sec-websocket-version": "13",
          "sec-websocket-key": "dGhlIHNhbXBsZSBub25jZQ==",
        },
      });
      req.on("upgrade", (_res, socket) => {
        socket.destroy();
        settle("upgraded");
      });
      req.on("response", (res) => {
        res.resume();
        settle(`response ${res.statusCode}`);
      });
      req.on("error", (error) => {
        clearTimeout(deadline);
        reject(error);
      });
      req.end();
    });
    expect(outcome).toBe("upgraded");
    expect(upgrades).toEqual([DEV_REALTIME_PATH]);
  });
});
