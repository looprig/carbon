import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { checkDist, classifyDistEntries, classifyDistLeaks } from "./check-dist.mjs";

describe("classifyDistEntries", () => {
  it("accepts a normal Vite build tree", () => {
    expect(
      classifyDistEntries(["index.html", "assets", "assets/index-abc123.js", "favicon.svg"]),
    ).toEqual({ ok: true, missingIndex: false, skipped: [] });
  });

  it("rejects a top-level entry //go:embed dist would silently skip", () => {
    // `//go:embed dist`, without the `all:` prefix, drops every path whose name
    // begins with `_` or `.` — producing a build that serves a blank page with
    // no error anywhere. SvelteKit's `_app/` is exactly this trap; Vite's
    // default assetsDir avoids it, and this makes that a pinned property rather
    // than a coincidence.
    expect(classifyDistEntries(["index.html", "_app"])).toEqual({
      ok: false,
      missingIndex: false,
      skipped: ["_app"],
    });
    expect(classifyDistEntries(["index.html", ".vite"])).toEqual({
      ok: false,
      missingIndex: false,
      skipped: [".vite"],
    });
  });

  it("rejects a NESTED skipped entry too, not just a top-level one", () => {
    // The exclusion applies at every level of the walk, not only to the
    // pattern's immediate children, so a top-level-only check would pass a tree
    // that still loses files. 05-app.md's version reads one flat readdir.
    expect(classifyDistEntries(["index.html", "assets", "assets/.vite/manifest.json"])).toEqual({
      ok: false,
      missingIndex: false,
      skipped: ["assets/.vite/manifest.json"],
    });
  });

  it("normalises Windows separators before judging a path segment", () => {
    expect(classifyDistEntries(["index.html", "assets\\_chunk.js"])).toEqual({
      ok: false,
      missingIndex: false,
      skipped: ["assets/_chunk.js"],
    });
  });

  it("rejects a build with no index.html — the SPA fallback target", () => {
    expect(classifyDistEntries(["assets", "assets/index-abc123.js"])).toEqual({
      ok: false,
      missingIndex: true,
      skipped: [],
    });
  });

  it("reports both problems at once rather than masking one behind the other", () => {
    // 05-app.md's classifier returned `skipped: ["missing index.html"]` for the
    // missing-file case, which both conflates two different faults into one
    // field and hides any skipped entries when the index is also absent.
    expect(classifyDistEntries(["_app"])).toEqual({
      ok: false,
      missingIndex: true,
      skipped: ["_app"],
    });
  });

  it("treats an empty tree as a failed build, not an empty success", () => {
    expect(classifyDistEntries([])).toEqual({ ok: false, missingIndex: true, skipped: [] });
  });
});

describe("classifyDistLeaks", () => {
  const CLEAN = [
    { path: "index.html", content: '<link rel="modulepreload" href="/assets/index-C8jCNzUt.js">' },
    { path: "assets/index-C8jCNzUt.js", content: "console.log(1);\n" },
    { path: "assets/index-DVeYZWBb.css", content: "body{color:red}\n" },
  ];

  it("accepts a tree with no map files, no sourcemap comments and no local paths", () => {
    expect(classifyDistLeaks(CLEAN)).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("flags a .map file present anywhere in the tree", () => {
    // U5.3's runbook step 3 requires the released bundle to ship no source
    // maps at all — a .map file next to a legitimate asset is exactly the
    // regression a future build could reintroduce silently.
    expect(
      classifyDistLeaks([...CLEAN, { path: "assets/index-C8jCNzUt.js.map", content: "{}" }]),
    ).toEqual({ ok: false, mapFiles: ["assets/index-C8jCNzUt.js.map"], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("flags a sourceMappingURL comment in emitted JS", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: "console.log(1);\n//# sourceMappingURL=index-C8jCNzUt.js.map" },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: ["assets/index-C8jCNzUt.js"],
      localPaths: [],
      secrets: [],
    });
  });

  it("flags a sourceMappingURL comment in emitted CSS", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        {
          path: "assets/index-DVeYZWBb.css",
          content: "body{color:red}\n/*# sourceMappingURL=index-DVeYZWBb.css.map */",
        },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: ["assets/index-DVeYZWBb.css"],
      localPaths: [],
      secrets: [],
    });
  });

  it("does not flag a real sourceMappingURL comment sitting outside emitted JS/CSS", () => {
    // The runbook property is scoped to "emitted JS or CSS". This fixture uses
    // the EXACT `//#`-prefixed comment syntax the JS check matches — inside an
    // inline <script> in index.html — so the test is decisive about the
    // extension restriction rather than passing because the content never
    // matched the pattern in the first place (which a bare HTML comment like
    // `<!-- sourceMappingURL=oops.map -->` would do, vacuously).
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "index.html", content: "<script>//# sourceMappingURL=oops.map</script>" },
      ]),
    ).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("flags an absolute local filesystem path embedded in emitted JS", () => {
    // The general shape of a macOS home directory, not one hardcoded
    // developer's username: a bundled stack trace or a dev-only debug string
    // that leaked a build machine's path would trip this on ANY username.
    expect(
      classifyDistLeaks([
        ...CLEAN,
        {
          path: "assets/index-C8jCNzUt.js",
          content: 'const p = "/Users/example-dev/code/looprig/wui/app/src/App.tsx";',
        },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: [],
      localPaths: ["assets/index-C8jCNzUt.js"],
      secrets: [],
    });
  });

  it("flags a DIFFERENT username's home directory too — the check is not pinned to one name", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: 'throw new Error("/Users/someone-else/repo/build.log");' },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: [],
      localPaths: ["assets/index-C8jCNzUt.js"],
      secrets: [],
    });
  });

  it("flags a Linux home directory path too", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: 'const p = "/home/ci-runner/workspace/src/index.ts";' },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: [],
      localPaths: ["assets/index-C8jCNzUt.js"],
      secrets: [],
    });
  });

  it("does not flag a legitimate site-root-absolute hashed asset reference", () => {
    // "/assets/index-C8jCNzUt.js" is an absolute URL PATH the SPA serves from
    // its own root, not a local filesystem path — the leading slash alone
    // must not trip the rule.
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "index.html", content: '<script type="module" src="/assets/index-C8jCNzUt.js"></script>' },
      ]),
    ).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("does not flag a legitimate base64 data URI", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        {
          path: "assets/index-DVeYZWBb.css",
          content:
            "body{background:url(data:image/svg+xml;base64,PHN2ZyB4bWxucz0naHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmcnIHZpZXdCb3g9JzAgMCAyNCAyNCc+PC9zdmc+)}",
        },
      ]),
    ).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("reports all four problems at once rather than masking one behind another", () => {
    expect(
      classifyDistLeaks([
        { path: "assets/index-C8jCNzUt.js.map", content: "{}" },
        {
          path: "assets/index-C8jCNzUt.js",
          content:
            'console.log("/Users/example-dev/code","AKIAIOSFODNN7EXAMPLE");\n//# sourceMappingURL=index-C8jCNzUt.js.map',
        },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: ["assets/index-C8jCNzUt.js.map"],
      sourceMapRefs: ["assets/index-C8jCNzUt.js"],
      localPaths: ["assets/index-C8jCNzUt.js"],
      secrets: ["assets/index-C8jCNzUt.js: AWS access key id"],
    });
  });

  it("flags a leaked AWS access key id", () => {
    // AKIAIOSFODNN7EXAMPLE is AWS's own documented example key, so this test
    // names no real credential while exercising the real pattern.
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: 'const k="AKIAIOSFODNN7EXAMPLE";' },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: [],
      localPaths: [],
      secrets: ["assets/index-C8jCNzUt.js: AWS access key id"],
    });
  });

  it("flags a PEM private key block, whatever key type it names", () => {
    for (const header of ["-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----"]) {
      expect(
        classifyDistLeaks([...CLEAN, { path: "assets/key.txt", content: `${header}\nMIIE\n` }]),
      ).toEqual({
        ok: false,
        mapFiles: [],
        sourceMapRefs: [],
        localPaths: [],
        secrets: ["assets/key.txt: PEM private key block"],
      });
    }
  });

  it("flags a GitHub personal access token", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: 'fetch("",{headers:{a:"ghp_0123456789abcdefghijklmnopqrstuvwxyz"}})' },
      ]),
    ).toEqual({
      ok: false,
      mapFiles: [],
      sourceMapRefs: [],
      localPaths: [],
      secrets: ["assets/index-C8jCNzUt.js: GitHub token"],
    });
  });

  it("does not flag a token-shaped prefix that is too short to be one", () => {
    // Precision is the whole point: a guard that fires on `ghp_` alone, or on
    // a bare `AKIA`, is one a reader learns to override, which is worse than
    // no guard. Each pattern requires the credential's full documented length.
    expect(
      classifyDistLeaks([
        ...CLEAN,
        { path: "assets/index-C8jCNzUt.js", content: 'const a="ghp_short",b="AKIASHORT",c="sk-";' },
      ]),
    ).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });

  it("does not flag a base64 data URI or minified identifiers", () => {
    expect(
      classifyDistLeaks([
        ...CLEAN,
        {
          path: "assets/index-DVeYZWBb.css",
          content:
            "body{background:url(data:image/svg+xml;base64,PHN2ZyB4bWxucz0naHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmcnIHZpZXdCb3g9JzAgMCAyNCAyNCc+PC9zdmc+)}",
        },
        { path: "assets/index-C8jCNzUt.js", content: "const AIzaSyBhelper=1,xoxb=2,skProj=3;" },
      ]),
    ).toEqual({ ok: true, mapFiles: [], sourceMapRefs: [], localPaths: [], secrets: [] });
  });
});

describe("checkDist", () => {
  it("the committed dist tree carries no source maps, local paths or known credential formats", () => {
    // U5.3's runbook step 3 was verified by hand on the committed bundle; this
    // pins that property so a future build regressing it fails the build
    // instead of waiting for another manual check. `secrets` is the
    // well-known-format scan classifyDistLeaks documents, not a general
    // secrets scan, so the name says which claim is being made.
    const result = checkDist();
    expect(result.ok).toBe(true);
    expect(result.mapFiles).toEqual([]);
    expect(result.sourceMapRefs).toEqual([]);
    expect(result.localPaths).toEqual([]);
    expect(result.secrets).toEqual([]);
  });

  it("reports a LEAKING tree as not embeddable — the leak check reaches ok", () => {
    // The case above runs against the committed tree, which is clean, so it
    // passes whether or not classifyDistLeaks's answer is wired into the
    // decision. `ok` is the only thing anything downstream reads:
    // release-dist.mjs branches on it for the build candidate and again after
    // installing over dist/, and the CLI's exit status is it. So the leak
    // classifier being correct is worth nothing unless a leaking tree makes
    // `ok` false, which is what this constructs.
    const dir = mkdtempSync(join(tmpdir(), "wui-check-dist-leak-"));
    try {
      mkdirSync(join(dir, "assets"));
      writeFileSync(join(dir, "index.html"), '<script src="/assets/index-abc123.js"></script>');
      writeFileSync(
        join(dir, "assets", "index-abc123.js"),
        'console.log("/Users/example-dev/wui/app/src/main.tsx","AKIAIOSFODNN7EXAMPLE");\n//# sourceMappingURL=index-abc123.js.map\n',
      );
      writeFileSync(join(dir, "assets", "index-abc123.js.map"), "{}");

      const result = checkDist(dir);

      // Not merely `ok === false`: every leak field must have carried its own
      // offending path through, so a future change that wires only one of them
      // into `ok` is still visible here.
      expect(result.ok).toBe(false);
      expect(result.missingIndex).toBe(false);
      expect(result.skipped).toEqual([]);
      expect(result.mapFiles).toEqual(["assets/index-abc123.js.map"]);
      expect(result.sourceMapRefs).toEqual(["assets/index-abc123.js"]);
      expect(result.localPaths).toEqual(["assets/index-abc123.js"]);
      expect(result.secrets).toEqual(["assets/index-abc123.js: AWS access key id"]);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
