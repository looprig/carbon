import { lstatSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Judges whether a built `dist` tree is one the Go side can actually serve.
 *
 * Two independent faults, reported independently.
 *
 * **Skipped entries.** `//go:embed dist` — WITHOUT the `all:` prefix — silently
 * omits every path whose name begins with `_` or `.`, at every level of the
 * walk, not just the pattern's immediate children. The result is a binary that
 * compiles, starts, serves a blank page, and reports nothing anywhere.
 * SvelteKit's `_app/` is exactly this trap, sitting unnoticed in
 * `client/pkg/webui/dist` today. Vite's default `assetsDir: "assets"` produces
 * no such name; `internal/browserui/assets.go` uses `all:dist` as well. This check is what
 * makes both of those a pinned property rather than a coincidence.
 *
 * **A missing `index.html`.** It is the SPA fallback target and the tracked
 * placeholder `//go:embed all:dist` needs to exist at compile time on a machine
 * with no Node toolchain. A build that dropped it is a broken build.
 *
 * The two are separate fields rather than one list of strings: 05-app.md's
 * version returned `skipped: ["missing index.html"]`, which conflates them and
 * hides any genuinely skipped entry whenever the index is also absent.
 *
 * @param {readonly string[]} entries Paths relative to the tree root, in either separator style.
 * @returns {{ ok: boolean, missingIndex: boolean, skipped: string[] }}
 */
export function classifyDistEntries(entries) {
  const paths = entries.map((entry) => entry.split("\\").join("/"));
  const skipped = paths.filter((path) =>
    path.split("/").some((segment) => segment.startsWith("_") || segment.startsWith(".")),
  );
  const missingIndex = !paths.includes("index.html");
  return { ok: !missingIndex && skipped.length === 0, missingIndex, skipped };
}

/**
 * A `//# sourceMappingURL=` (JS) or `/*# sourceMappingURL=` (CSS) comment.
 * Either terminator (`//` or `/*`) may introduce it; Vite emits the `//` form
 * for JS and the `/*` form for CSS, and this checks emitted files of both
 * kinds, so both are matched rather than assuming one file extension implies
 * one comment style.
 */
const SOURCE_MAP_COMMENT_PATTERN = /(?:\/\/|\/\*)[#@]\s*sourceMappingURL=/;

/**
 * The general SHAPE of a local development machine's absolute filesystem
 * path, not any one developer's username: `/Users/<name>/...` (macOS) and
 * `/home/<name>/...` (Linux), each followed by at least one further path
 * segment so a bare `/Users/` or `/home/` — which would already be an odd
 * thing to find in a browser bundle, but names no one's machine — still does
 * not particularly matter to widen the match for.
 *
 * Deliberately does NOT match a bare leading slash: `/assets/index-HASH.js`
 * is a site-root-absolute URL path the SPA serves from its own origin, and a
 * pattern anchored only on "/" would flag every reference to it.
 */
const LOCAL_FILESYSTEM_PATH_PATTERN = /\/(?:Users|home)\/[^\s"'`)>]+\/[^\s"'`)>]*/;

/**
 * The credential formats this scan recognises, each by its own documented
 * shape and full documented length.
 *
 * This list is deliberately short and deliberately strict. A secrets scan is
 * only worth having if a hit means something: a pattern that fires on `ghp_`
 * or a bare `AKIA` would fire on minified identifiers and base64 payloads,
 * and a guard people learn to override is worse than no guard. So each entry
 * matches a credential whose issuer publishes a fixed prefix AND a fixed (or
 * minimum) body length, and nothing here matches on entropy, on a variable
 * name like `apiKey`, or on a prefix alone.
 *
 * The consequence, which `classifyDistLeaks` states again for its caller, is
 * that this is NOT a general secrets scan. A bespoke token, a password, or a
 * credential in a format not listed here passes it. What it does catch is the
 * realistic accident: a well-known provider credential pasted into app source
 * or an env file and inlined by the bundler.
 */
const SECRET_PATTERNS = [
  // Any PEM private key, whatever type names it ("RSA ", "EC ", "OPENSSH ",
  // "ENCRYPTED "), including the bare PKCS#8 header.
  { name: "PEM private key block", pattern: /-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----/ },
  // AWS long-term (AKIA) and temporary (ASIA) access key ids: the prefix plus
  // exactly 16 further uppercase-or-digit characters.
  { name: "AWS access key id", pattern: /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/ },
  // GitHub's documented token formats: ghp_/gho_/ghu_/ghs_/ghr_ plus 36
  // characters, and the fine-grained github_pat_ form.
  { name: "GitHub token", pattern: /\b(?:gh[pours]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{40,})\b/ },
  // Google API key: "AIza" plus exactly 35 of its charset.
  { name: "Google API key", pattern: /\bAIza[0-9A-Za-z_-]{35}\b/ },
  // Slack bot/user/app tokens.
  { name: "Slack token", pattern: /\bxox[abopsr]-[A-Za-z0-9]{10,}/ },
  // Anthropic and OpenAI keys, by their full prefixes rather than a bare
  // "sk-": "sk-" alone appears in base64url payloads and in minified names.
  { name: "Anthropic API key", pattern: /\bsk-ant-api[0-9]{2}-[A-Za-z0-9_-]{24,}/ },
  { name: "OpenAI project key", pattern: /\bsk-proj-[A-Za-z0-9_-]{24,}/ },
];

/**
 * Scans already-read file contents for the properties U5.3's runbook step 3
 * requires of a released bundle and which, until now, were only ever checked
 * by hand: no source map files, no `sourceMappingURL` references in emitted
 * JS/CSS, no local filesystem paths, and no credential in one of the
 * well-known formats {@link SECRET_PATTERNS} lists.
 *
 * `secrets` is worth reading twice before trusting it: it recognises a fixed
 * list of provider credential formats, NOT secrets in general. An empty
 * `secrets` means "none of those formats is present", which is a useful thing
 * to know automatically and is not the same statement as "this bundle leaks
 * no secret". The runbook's secrets criterion is therefore partly automated
 * and still partly a reviewer's job.
 *
 * The four are independent fields, for the same reason `classifyDistEntries`
 * keeps its two independent: a tree can fail any subset of them, and folding
 * them into one list would hide whichever fault a test happened to construct
 * second.
 *
 * `sourceMapRefs` is scoped to files whose name ends `.js` or `.css` — the
 * runbook property is "no sourceMappingURL references in emitted JS or CSS",
 * and a comment or string containing the same words in, say, `index.html`
 * is not a leaked source map.
 *
 * `localPaths` and `secrets` are scanned across every file regardless of
 * extension — including binary assets — because a leaked build-machine path
 * or credential is exactly as real inside a font's embedded metadata as
 * inside a JS string, and grep makes no such distinction either.
 *
 * @param {readonly { path: string, content: string }[]} files Every FILE (not directory)
 *   in the tree, with its raw content already read. `content` should be read
 *   as `latin1` (one byte per code unit) rather than `utf8`, so scanning a
 *   binary asset for an ASCII byte pattern cannot itself throw on invalid
 *   UTF-8 and cannot re-encode bytes into different characters first.
 * @returns {{ ok: boolean, mapFiles: string[], sourceMapRefs: string[], localPaths: string[], secrets: string[] }}
 *   `secrets` entries are `"<path>: <format name>"`, so a failure names which
 *   pattern fired rather than leaving a reader to re-derive it.
 */
export function classifyDistLeaks(files) {
  const mapFiles = [];
  const sourceMapRefs = [];
  const localPaths = [];
  const secrets = [];
  for (const { path, content } of files) {
    const normalized = path.split("\\").join("/");
    if (normalized.endsWith(".map")) {
      mapFiles.push(normalized);
    }
    if ((normalized.endsWith(".js") || normalized.endsWith(".css")) && SOURCE_MAP_COMMENT_PATTERN.test(content)) {
      sourceMapRefs.push(normalized);
    }
    if (LOCAL_FILESYSTEM_PATH_PATTERN.test(content)) {
      localPaths.push(normalized);
    }
    for (const { name, pattern } of SECRET_PATTERNS) {
      if (pattern.test(content)) {
        secrets.push(`${normalized}: ${name}`);
      }
    }
  }
  return {
    ok:
      mapFiles.length === 0 &&
      sourceMapRefs.length === 0 &&
      localPaths.length === 0 &&
      secrets.length === 0,
    mapFiles,
    sourceMapRefs,
    localPaths,
    secrets,
  };
}

/**
 * Walks a built tree, classifying every entry by `lstat` and NEVER descending
 * through a symlink.
 *
 * `readdirSync(dir, { recursive: true })` cannot be used here, with or without
 * `withFileTypes`: BOTH variants follow a symlinked directory and recurse into
 * its target. Measured on this tree, a `dist/assets/link -> .` yields 66 entries
 * (`link/link/link/...`) before the kernel answers ELOOP, and a
 * `dist/assets/link -> /etc` walks out of the bundle entirely and throws EACCES
 * on `/etc/cups/certs`. Neither is a path the release can survive, and neither
 * is a path a build output should contain.
 *
 * Descending only when `lstat` says DIRECTORY is what makes the walk total: an
 * lstat of a symlink is never a directory, so no link is ever followed, and a
 * dangling link, a loop and a link to an unreadable directory all reduce to the
 * same thing — one entry, classified, reported by name. That is the property
 * `bundleManifest` depends on to be reachable; see its doc comment.
 *
 * @param {string} root Absolute path of the tree to walk.
 * @returns {{ path: string, metadata: import("node:fs").Stats }[]} Entries with
 *   root-relative, forward-slash-separated paths, parents before children.
 */
export function walkBundleEntries(root) {
  const entries = [];
  const visit = (relative) => {
    const absolute = relative === "" ? root : join(root, relative);
    for (const name of readdirSync(absolute).sort()) {
      const path = relative === "" ? name : `${relative}/${name}`;
      const metadata = lstatSync(join(absolute, name));
      entries.push({ path, metadata });
      if (metadata.isDirectory()) visit(path);
    }
  };
  visit("");
  return entries;
}

/** The tree `vite.config.ts`'s `build.outDir` writes and `internal/browserui/assets.go` embeds. */
export const DIST_DIR = new URL("../../internal/browserui/dist", import.meta.url);

export function checkDist(dir = DIST_DIR) {
  const dirPath = typeof dir === "string" ? dir : fileURLToPath(dir);
  const walked = walkBundleEntries(dirPath);
  const entryResult = classifyDistEntries(walked.map((entry) => entry.path));
  const files = walked
    .filter((entry) => entry.metadata.isFile())
    .map((entry) => ({ path: entry.path, content: readFileSync(join(dirPath, entry.path), "latin1") }));
  const leakResult = classifyDistLeaks(files);
  return {
    ok: entryResult.ok && leakResult.ok,
    missingIndex: entryResult.missingIndex,
    skipped: entryResult.skipped,
    mapFiles: leakResult.mapFiles,
    sourceMapRefs: leakResult.sourceMapRefs,
    localPaths: leakResult.localPaths,
    secrets: leakResult.secrets,
  };
}

if (import.meta.url === new URL(process.argv[1], "file:").href) {
  const result = checkDist();
  if (!result.ok) {
    if (result.missingIndex) console.error("Carbon dist has no index.html: the build did not produce a shell");
    if (result.skipped.length > 0) {
      console.error(`Carbon dist is not embeddable by a bare //go:embed dist: ${result.skipped.join(", ")}`);
    }
    if (result.mapFiles.length > 0) {
      console.error(`Carbon dist ships source map files, which U5.3 forbids: ${result.mapFiles.join(", ")}`);
    }
    if (result.sourceMapRefs.length > 0) {
      console.error(
        `Carbon dist ships a sourceMappingURL reference in emitted JS/CSS: ${result.sourceMapRefs.join(", ")}`,
      );
    }
    if (result.localPaths.length > 0) {
      console.error(`Carbon dist embeds a local filesystem path: ${result.localPaths.join(", ")}`);
    }
    if (result.secrets.length > 0) {
      console.error(`Carbon dist embeds a well-known credential format: ${result.secrets.join(", ")}`);
    }
    process.exit(1);
  }
  console.log("Carbon dist is embeddable");
}
