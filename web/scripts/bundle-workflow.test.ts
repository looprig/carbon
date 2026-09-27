import type { ChildProcess } from "node:child_process";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  closeSync,
  cpSync,
  copyFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  readdirSync,
  rmSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { checkDist } from "./check-dist.mjs";
import { assertReleaseMarker, bundleManifest } from "./release-dist.mjs";
import { BUNDLE_MANIFEST_NAME } from "./write-bundle-manifest.mjs";

const repository = fileURLToPath(new URL("../..", import.meta.url));
const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

function run(command: string, args: string[], cwd: string, env: NodeJS.ProcessEnv = process.env): string {
  return execFileSync(command, args, { cwd, env, encoding: "utf8" });
}

function cloneWithCurrentWorkflow(): string {
  const parent = mkdtempSync(join(tmpdir(), "carbon-bundle-workflow-"));
  temporaryDirectories.push(parent);
  const clone = join(parent, "clone");
  run("git", ["clone", "--quiet", "--no-hardlinks", repository, clone], repository);
  copyFileSync(join(repository, "Makefile"), join(clone, "Makefile"));
  copyFileSync(join(repository, ".gitignore"), join(clone, ".gitignore"));
  mkdirSync(join(clone, "web"), { recursive: true });
  copyFileSync(join(repository, "web/package.json"), join(clone, "web/package.json"));
  // Every script the release entry point LOADS, not just the entry point. The
  // clone is of HEAD, so overlaying `release-dist.mjs` alone left it importing
  // the committed `check-dist.mjs`; a working-tree change that spans the two
  // failed every clone-backed case at import time rather than being tested.
  mkdirSync(join(clone, "web/scripts"), { recursive: true });
  for (const script of ["release-dist.mjs", "check-dist.mjs", "write-bundle-manifest.mjs"]) {
    const source = join(repository, "web/scripts", script);
    if (existsSync(source)) copyFileSync(source, join(clone, "web/scripts", script));
  }
  cpSync(join(repository, "internal/browserui/dist"), join(clone, "internal/browserui/dist"), { recursive: true });
  run("git", ["add", "-A", "Makefile", ".gitignore", "web"], clone);
  run("git", ["add", "-f", "--all", "--", "internal/browserui/dist"], clone);
  run("git", ["-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "test fixture"], clone);
  return clone;
}

function manifest(root: string): Record<string, string> {
  const dist = join(root, "internal/browserui/dist");
  return Object.fromEntries(
    readdirSync(dist, { recursive: true, withFileTypes: true })
      .filter((entry) => entry.isFile())
      .map((entry) => {
        const absolute = join(entry.parentPath, entry.name);
        const relative = absolute.slice(dist.length + 1);
        return [relative, createHash("sha256").update(readFileSync(absolute)).digest("hex")];
      })
      .sort(([left], [right]) => left.localeCompare(right)),
  );
}

function trackedJavaScript(root: string): string {
  const asset = Object.keys(manifest(root)).find((path) => path.startsWith("assets/") && path.endsWith(".js"));
  if (asset === undefined) throw new Error("tracked dist snapshot has no JavaScript entry asset");
  return join(root, "internal/browserui/dist", asset);
}

type BuildMode =
  | "deterministic"
  | "nondeterministic"
  | "nondeterministic-filename"
  | "placeholder"
  | "symlink-absolute-same"
  | "symlink-absolute-different"
  | "symlink-relative-same"
  | "symlink-relative-different"
  | "symlink-dangling-absolute"
  | "symlink-directory"
  | "symlink-loop"
  | "fifo";

type HandledReleaseSignal = "SIGHUP" | "SIGINT" | "SIGTERM";

const signalExitCode: Record<HandledReleaseSignal, number> = {
  SIGHUP: 129,
  SIGINT: 130,
  SIGTERM: 143,
};

function installFakeReleaseTools(
  clone: string,
  mode: BuildMode,
  failures: {
    gate?: boolean;
    gateSleep?: boolean;
    gateGrandchild?: boolean;
    gateExitRace?: boolean;
    buildSleep?: boolean;
    gitAdd?: boolean;
    duringBuildEdit?: "tracked" | "ignored" | "staged";
    marker?: "absent" | "non-release";
  } = {},
) {
  const bin = join(clone, ".test-bin");
  mkdirSync(bin);
  const npm = join(bin, "npm");
  writeFileSync(npm, `#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { existsSync, lstatSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
const args = process.argv.slice(2);
if (args[0] === "ci") process.exit(0);
if (args[0] !== "run" || args[1] !== "build") process.exit(64);
const countFile = process.env.BUNDLE_TEST_COUNT;
let count = 0;
try { count = Number(readFileSync(countFile, "utf8")); } catch {}
count += 1;
writeFileSync(countFile, String(count));
const outIndex = args.indexOf("--outDir");
const out = resolve(outIndex === -1 ? "internal/browserui/dist" : args[outIndex + 1]);
rmSync(out, { recursive: true, force: true });
mkdirSync(resolve(out, "assets"), { recursive: true });
if (process.env.BUNDLE_TEST_BUILD_SLEEP === "1" && count === 1) {
  writeFileSync(process.env.BUNDLE_TEST_BUILD_READY, "ready");
  setInterval(() => {}, 1000);
}
const marker = process.env.BUNDLE_TEST_MODE === "nondeterministic" ? String(count) : "stable";
const index = process.env.BUNDLE_TEST_MODE === "placeholder"
  ? "placeholder"
  : '<script type="module" src="/assets/app.js"></script>';
// The asset's NAME varies per build in nondeterministic-filename, while every
// byte in the bundle stays identical -- index.html deliberately keeps pointing
// at the fixed /assets/app.js so the ONLY difference between the two builds is
// the entry's path. This models Vite, which names assets by content hash, so a
// genuinely nondeterministic SPA build shows up as a RENAME far more often than
// as the same filename with different bytes.
const assetName = process.env.BUNDLE_TEST_MODE === "nondeterministic-filename"
  ? "assets/app-" + count + ".js"
  : "assets/app.js";
writeFileSync(resolve(out, "index.html"), index);
writeFileSync(resolve(out, assetName), 'export const marker = "' + marker + '";');
// Stands in for vite.config.ts's bundleManifestPlugin: every build writes the
// marker into its own --outDir, and the release flag is an input to the build.
if (process.env.BUNDLE_TEST_MARKER !== "absent") {
  const release = process.env.BUNDLE_TEST_MARKER === "non-release"
    ? false
    : process.env.CARBON_BUNDLE_RELEASE === "1";
  writeFileSync(
    resolve(out, "looprig-bundle.json"),
    JSON.stringify({ core_version: "v0.7.0", client_version: "0.2.0", react_version: "0.2.0", release, sessionwire_version: 1 }),
  );
}
if (count === 2) {
  const edit = process.env.BUNDLE_TEST_DURING_BUILD_EDIT;
  if (edit === "tracked" || edit === "staged") {
    writeFileSync(resolve("internal/browserui/dist/index.html"), "caller edit during build");
    if (edit === "staged") {
      const staged = spawnSync("git", ["add", "internal/browserui/dist/index.html"]);
      if (staged.status !== 0) process.exit(staged.status ?? 1);
    }
  }
  if (edit === "ignored") writeFileSync(resolve("internal/browserui/dist/assets/caller-during-build.js"), "caller edit during build");
}
const mode = process.env.BUNDLE_TEST_MODE;
// One entry kind per mode, named rather than derived from substrings of the
// mode name: the previous version read "relative"/"different" out of the string
// and silently gave any unlisted symlink- mode an absolute /etc/hosts target.
const symlinkTargets = {
  "symlink-absolute-same": "/etc/hosts",
  "symlink-absolute-different": count === 2 ? "/etc/passwd" : "/etc/hosts",
  "symlink-relative-same": "target",
  "symlink-relative-different": "target-" + count,
  "symlink-dangling-absolute": "/looprig-no-such-target",
  "symlink-directory": ".",
};
// Which modes must produce a link that does NOT resolve. A fixture that
// silently made a VALID link would let its test pass for the wrong reason, so
// the build asserts the kind it just created and fails loudly if it differs.
const danglingModes = new Set([
  "symlink-relative-same",
  "symlink-relative-different",
  "symlink-dangling-absolute",
  "symlink-loop",
]);
if (mode === "symlink-loop") {
  symlinkSync("loop-b", resolve(out, "assets/link"));
  symlinkSync("link", resolve(out, "assets/loop-b"));
} else if (symlinkTargets[mode] !== undefined) {
  symlinkSync(symlinkTargets[mode], resolve(out, "assets/link"));
}
if (mode?.startsWith("symlink-")) {
  const link = resolve(out, "assets/link");
  if (!lstatSync(link).isSymbolicLink()) {
    console.error("fixture for " + mode + " did not create a symlink");
    process.exit(70);
  }
  if (existsSync(link) === danglingModes.has(mode)) {
    console.error("fixture for " + mode + " is " + (existsSync(link) ? "resolvable" : "dangling") + ", which the mode does not claim");
    process.exit(71);
  }
}
if (mode === "fifo") {
  const result = spawnSync("mkfifo", [resolve(out, "assets/pipe")]);
  if (result.status !== 0) process.exit(result.status ?? 1);
}
`);
  chmodSync(npm, 0o755);
  const go = join(bin, "go");
  writeFileSync(go, `#!/usr/bin/env node
import { appendFileSync, writeFileSync } from "node:fs";
import { spawn } from "node:child_process";
appendFileSync(process.env.BUNDLE_TEST_GO_CALLS, process.argv.slice(2).join(" ") + "\\n");
if (process.env.BUNDLE_TEST_GATE_SLEEP === "1" && process.argv[2] === "test") {
  if (process.env.BUNDLE_TEST_GATE_GRANDCHILD === "1") {
    const source = 'import { mkdirSync, writeFileSync } from "node:fs"; process.on("SIGINT", () => {}); process.on("SIGTERM", () => {}); process.on("SIGHUP", () => {}); setTimeout(() => { mkdirSync(process.env.BUNDLE_TEST_LATE_TEMP, { recursive: true }); writeFileSync(process.env.BUNDLE_TEST_LATE_TEMP + "/written", "late"); writeFileSync("internal/browserui/dist/late-grandchild", "late"); }, 800); setInterval(() => {}, 1000);';
    spawn(process.execPath, ["--input-type=module", "-e", source], { stdio: "ignore", env: process.env });
  }
  writeFileSync(process.env.BUNDLE_TEST_GATE_READY, "ready");
  if (process.env.BUNDLE_TEST_GATE_EXIT_RACE === "1") {
    writeFileSync(process.env.BUNDLE_TEST_GATE_LEADER_EXITED, "exiting");
    process.exit(0);
  }
  setInterval(() => {}, 1000);
}
if (process.env.BUNDLE_TEST_GATE_FAIL === "1" && process.argv[2] === "test") process.exit(42);
`);
  chmodSync(go, 0o755);
  const realGit = run("which", ["git"], clone).trim();
  const git = join(bin, "git");
  writeFileSync(git, `#!/usr/bin/env node
import { spawnSync } from "node:child_process";
const args = process.argv.slice(2);
if (process.env.BUNDLE_TEST_GIT_ADD_FAIL === "1" && args[0] === "add") process.exit(43);
const result = spawnSync(${JSON.stringify(realGit)}, args, { stdio: "inherit" });
process.exit(result.status ?? 1);
`);
  chmodSync(git, 0o755);
  return {
    ...process.env,
    PATH: `${bin}:${process.env.PATH ?? ""}`,
    BUNDLE_TEST_COUNT: join(clone, ".build-count"),
    BUNDLE_TEST_MODE: mode,
    BUNDLE_TEST_GO_CALLS: join(clone, ".go-calls"),
    BUNDLE_TEST_GATE_FAIL: failures.gate ? "1" : "0",
    BUNDLE_TEST_GATE_SLEEP: failures.gateSleep ? "1" : "0",
    BUNDLE_TEST_GATE_GRANDCHILD: failures.gateGrandchild ? "1" : "0",
    BUNDLE_TEST_GATE_EXIT_RACE: failures.gateExitRace ? "1" : "0",
    BUNDLE_TEST_GATE_READY: join(clone, ".gate-ready"),
    BUNDLE_TEST_GATE_LEADER_EXITED: join(clone, ".gate-leader-exited"),
    BUNDLE_TEST_BUILD_SLEEP: failures.buildSleep ? "1" : "0",
    BUNDLE_TEST_BUILD_READY: join(clone, ".build-ready"),
    BUNDLE_TEST_LATE_TEMP: join(clone, ".late-temporary-write"),
    BUNDLE_TEST_GIT_ADD_FAIL: failures.gitAdd ? "1" : "0",
    BUNDLE_TEST_DURING_BUILD_EDIT: failures.duringBuildEdit ?? "",
    BUNDLE_TEST_MARKER: failures.marker ?? "",
  };
}

function expectPristineDist(clone: string, expected: Record<string, string>): void {
  expect(manifest(clone)).toEqual(expected);
  expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
}

interface RunningRelease {
  child: ChildProcess;
  logPath: string;
}

/**
 * Starts a release the way the interrupt cases need it: as a direct `node`
 * invocation, so a signal can be delivered to the release process itself rather
 * than to `make`.
 *
 * `CARBON_BUNDLE_RELEASE=1` is set HERE, and it is the whole reason these cases
 * were failing. Going around `make` also goes around the only place the flag is
 * set (`Makefile`'s `release-dist` recipe), so the fake build wrote a marker
 * saying `release=false` and `assertReleaseMarker` — added to the release path
 * long after these fixtures were written — refused the staged bundle BEFORE the
 * Go gate ever spawned. `.gate-ready` was therefore never created, and the only
 * symptom was `waitForFile` timing out ten seconds later with nothing to say.
 * That is why it read as host slowness for days; it is not a timing fault and no
 * timeout is large enough to fix it.
 *
 * Output goes to a FILE, not a pipe: the release passes `stdio: "inherit"` to
 * its own children, so a pipe would be held open by every descendant and the
 * `close` these cases await could outlive the process group they are testing.
 */
function spawnRelease(clone: string, env: NodeJS.ProcessEnv): RunningRelease {
  const logPath = join(clone, ".release-output");
  const log = openSync(logPath, "a");
  try {
    const child = spawn(
      "node",
      [
        "web/scripts/release-dist.mjs",
        "npm", "run", "build", "--workspace", "web", "--", "--outDir", "{out}", "--emptyOutDir",
      ],
      { cwd: clone, env: { ...env, CARBON_BUNDLE_RELEASE: "1" }, stdio: ["ignore", log, log] },
    );
    return { child, logPath };
  } finally {
    closeSync(log);
  }
}

function releaseOutput(release: RunningRelease): string {
  return existsSync(release.logPath) ? readFileSync(release.logPath, "utf8") : "(no output)";
}

/**
 * Waits for a handshake file, and refuses to report only that it did not appear.
 *
 * "The expected file is never created" is a symptom shared by every way a
 * release can fail before reaching the step that writes it. Racing the child's
 * exit turns the common case — the release already died, and said why — into an
 * immediate failure carrying its output, instead of a ten-second wait ending in
 * a message naming a path and no cause.
 */
async function waitForFile(path: string, release?: RunningRelease): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (!existsSync(path)) {
    if (release !== undefined && release.child.exitCode !== null) {
      throw new Error(
        `release exited with code ${release.child.exitCode} before creating ${path}:\n${releaseOutput(release)}`,
      );
    }
    if (Date.now() >= deadline) {
      throw new Error(
        `timed out waiting for ${path}${release === undefined ? "" : `; release output so far:\n${releaseOutput(release)}`}`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

async function interruptRelease(clone: string, signal: HandledReleaseSignal) {
  const expected = manifest(clone);
  const env = installFakeReleaseTools(clone, "deterministic", { gateSleep: true, gateGrandchild: true });
  const temporaryRoot = join(clone, ".release-temporary");
  mkdirSync(temporaryRoot);
  env.TMPDIR = temporaryRoot;
  const release = spawnRelease(clone, env);
  const child = release.child;
  await waitForFile(env.BUNDLE_TEST_GATE_READY!, release);
  child.kill(signal);
  const result = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, closedBy) => resolve({ code, signal: closedBy }));
  });
  return { env, expected, result, temporaryRoot };
}

async function interruptFirstBuild(clone: string, signal: HandledReleaseSignal) {
  const expected = manifest(clone);
  const env = installFakeReleaseTools(clone, "deterministic", { buildSleep: true });
  const temporaryRoot = join(clone, ".release-temporary");
  mkdirSync(temporaryRoot);
  env.TMPDIR = temporaryRoot;
  const release = spawnRelease(clone, env);
  const child = release.child;
  await waitForFile(env.BUNDLE_TEST_BUILD_READY!, release);
  child.kill(signal);
  const result = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, closedBy) => resolve({ code, signal: closedBy }));
  });
  return { env, expected, result, temporaryRoot };
}

async function interruptAfterGateLeaderExit(clone: string) {
  const expected = manifest(clone);
  const env = installFakeReleaseTools(clone, "deterministic", {
    gateSleep: true,
    gateGrandchild: true,
    gateExitRace: true,
  });
  const temporaryRoot = join(clone, ".release-temporary");
  mkdirSync(temporaryRoot);
  env.TMPDIR = temporaryRoot;
  const release = spawnRelease(clone, env);
  const child = release.child;
  await waitForFile(env.BUNDLE_TEST_GATE_LEADER_EXITED!, release);
  child.kill("SIGTERM");
  const result = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, closedBy) => resolve({ code, signal: closedBy }));
  });
  return { env, expected, result, temporaryRoot };
}

/**
 * The staged bundle's marker, checked as a pure function.
 *
 * The workflow cases below drive the whole release through `make`, which is
 * where the Makefile's `CARBON_BUNDLE_RELEASE` and the build plugin are actually
 * exercised. These are the same guard's rejections at unit cost, so each
 * refusal is pinned to its own message rather than to "the release failed".
 */
describe("release marker check", () => {
  function stage(body?: string): string {
    const directory = mkdtempSync(join(tmpdir(), "carbon-release-marker-"));
    temporaryDirectories.push(directory);
    if (body !== undefined) writeFileSync(join(directory, BUNDLE_MANIFEST_NAME), body);
    return directory;
  }

  it("accepts a marker that claims a release", () => {
    expect(() => assertReleaseMarker(stage('{"release":true}'))).not.toThrow();
  });

  it.each([
    ["no manifest at all", undefined, /carries no manifest/],
    ["a manifest that is not JSON", "looprig", /unreadable manifest/],
    ["a development marker", '{"release":false}', /does not declare itself a release/],
    ["an absent release key", '{"core_version":"v0.7.0"}', /does not declare itself a release/],
    // Truthiness is not the test: only a JSON `true` is a release claim.
    ["a stringly-typed claim", '{"release":"true"}', /does not declare itself a release/],
    ["a JSON document that is not an object", '"release"', /does not declare itself a release/],
  ])("refuses %s", (_label, body, message) => {
    expect(() => assertReleaseMarker(stage(body))).toThrow(message);
  });
});

/**
 * Every entry kind the bundle walk can meet, and which component reports it.
 *
 * This exists because of a defect whose shape matters more than its instance:
 * `bundleManifest` is the guard that names an unsupported entry, and it was
 * UNREACHABLE for two of these kinds. `build` runs `checkDist` first, and
 * `checkDist` classified with `statSync` — a path-FOLLOWING call. On a dangling
 * symlink that throws ENOENT and on a symlink loop it throws ELOOP, so the
 * release died with a raw fs error naming a path, and the guard that exists to
 * say WHICH entry is unsupported never executed. Raising a timeout, rerunning
 * on a quieter host, or fixing only the relative-dangling case the bug report
 * happened to name would all have left the neighbouring kinds broken.
 *
 * So each case asserts BOTH halves, and the first half is the reachability
 * claim: `checkDist` returns a verdict rather than throwing, and then
 * `bundleManifest` is the thing that reports. A future change that makes
 * `checkDist` follow a link again fails here, on the kind it broke, by name.
 *
 * `relative vs absolute` is deliberately crossed with `dangling` because it is
 * NOT the axis the defect lay on: a dangling ABSOLUTE symlink fails `statSync`
 * exactly as a dangling relative one does. The original fixture only ever
 * pointed its absolute links at paths that exist, which is why the absolute
 * cases passed and made the fault look like it was about relative links.
 */
describe("bundle entry classification", () => {
  function stageTree(): string {
    const directory = mkdtempSync(join(tmpdir(), "carbon-bundle-entry-"));
    temporaryDirectories.push(directory);
    mkdirSync(join(directory, "assets"));
    writeFileSync(join(directory, "index.html"), '<script type="module" src="/assets/app.js"></script>');
    writeFileSync(join(directory, "assets/app.js"), 'export const marker = "stable";');
    return directory;
  }

  /**
   * `checkDist`'s verdict, or the error it died with, as a VALUE.
   *
   * The defect was an unhandled throw, so "it did not throw" has to be
   * something a test reads and asserts rather than something it merely fails to
   * notice. Catching turns a regression into a named assertion failure carrying
   * the fs error, instead of an opaque ENOENT escaping the test body.
   */
  function verdictOf(directory: string): ReturnType<typeof checkDist> | Error {
    try {
      return checkDist(directory);
    } catch (error) {
      return error instanceof Error ? error : new Error(String(error));
    }
  }

  function expectReachedClassifier(directory: string): void {
    const verdict = verdictOf(directory);
    expect(
      verdict instanceof Error ? `checkDist threw ${verdict.message}` : "checkDist reached a verdict",
    ).toBe("checkDist reached a verdict");
    expect((verdict as ReturnType<typeof checkDist>).ok).toBe(true);
  }

  /** A symlink fixture is only a fixture if it is the link it claims to be. */
  function link(directory: string, name: string, target: string, expectDangling: boolean): string {
    const path = join(directory, name);
    symlinkSync(target, path);
    expect(lstatSync(path).isSymbolicLink(), `${name} was not created as a symlink`).toBe(true);
    expect(existsSync(path), `${name} dangling-ness is not what the case claims`).toBe(!expectDangling);
    return path;
  }

  const supported = [
    ["a regular file", (d: string) => writeFileSync(join(d, "assets/extra.js"), "export const extra = 1;"), "assets/extra.js", "file"],
    ["a directory", (d: string) => mkdirSync(join(d, "assets/nested")), "assets/nested", "directory"],
  ] as const;

  it.each(supported)("admits %s", (_label, create, path, type) => {
    const directory = stageTree();
    create(directory);

    expectReachedClassifier(directory);
    const entry = bundleManifest(directory).find((candidate) => candidate.path === path);
    expect(entry?.type).toBe(type);
    if (type === "file") expect(entry?.sha256).toMatch(/^[0-9a-f]{64}$/);
  });

  const unsupported = [
    ["a relative symlink to a file that exists", (d: string) => link(d, "assets/link", "app.js", false)],
    ["an absolute symlink to a file that exists", (d: string) => link(d, "assets/link", "/etc/hosts", false)],
    ["a relative symlink to a directory that exists", (d: string) => link(d, "assets/link", ".", false)],
    ["an absolute symlink to a directory that exists", (d: string) => link(d, "assets/link", "/etc", false)],
    // The reported instance.
    ["a dangling relative symlink", (d: string) => link(d, "assets/link", "target", true)],
    // Its neighbour, which the original fixture never constructed.
    ["a dangling absolute symlink", (d: string) => link(d, "assets/link", "/looprig-no-such-target", true)],
    // A symlink loop: statSync answers ELOOP rather than ENOENT, same blindness.
    ["a symlink loop", (d: string) => {
      symlinkSync("loop-b", join(d, "assets/link"));
      symlinkSync("link", join(d, "assets/loop-b"));
      expect(lstatSync(join(d, "assets/link")).isSymbolicLink()).toBe(true);
      expect(() => readFileSync(join(d, "assets/link"))).toThrow(/ELOOP/);
    }],
    ["a FIFO", (d: string) => {
      const result = spawnSync("mkfifo", [join(d, "assets/pipe")]);
      expect(result.status).toBe(0);
      expect(lstatSync(join(d, "assets/pipe")).isFIFO()).toBe(true);
    }],
  ] as const;

  it.each(unsupported)("refuses %s, and reaches the classifier to say so", (_label, create) => {
    const directory = stageTree();
    create(directory);

    // Reachability: checkDist has no opinion about a non-regular entry and
    // must hand the tree on rather than dying on it.
    expectReachedClassifier(directory);
    // And the classifier is what reports, naming the entry.
    expect(() => bundleManifest(directory)).toThrow(/unsupported bundle entry type: assets\//);
  });

  it("refuses a symlink standing in for the output root itself", () => {
    const directory = stageTree();
    const parent = mkdtempSync(join(tmpdir(), "carbon-bundle-root-"));
    temporaryDirectories.push(parent);
    const root = join(parent, "internal/browserui/dist");
    mkdirSync(join(parent, "internal/browserui"), { recursive: true });
    symlinkSync(directory, root);
    expect(lstatSync(root).isSymbolicLink()).toBe(true);

    expect(() => bundleManifest(root)).toThrow(/unsupported bundle entry type at output root/);
  });

  /**
   * The leak scan reads entry CONTENT, so following a link would have it scan
   * bytes that are not in the bundle at all — a symlink to a private file would
   * be scanned for credentials and, worse, a clean result would be reported for
   * a tree whose actual published entry is unreadable. It classifies the entry.
   */
  it("does not read through a symlink when scanning for leaks", () => {
    const directory = stageTree();
    writeFileSync(join(directory, "assets/secret.txt"), "AKIAIOSFODNN7EXAMPLE");
    link(directory, "assets/link", "secret.txt", false);

    const result = checkDist(directory);

    expect(result.secrets).toStrictEqual(["assets/secret.txt: AWS access key id"]);
  });
});

describe("bundle release workflow", () => {
  it("rejects a non-POSIX release host before creating temporary state or running a build", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "deterministic");
    const temporaryRoot = join(clone, ".release-temporary");
    mkdirSync(temporaryRoot);
    env.TMPDIR = temporaryRoot;
    const releaseModule = pathToFileURL(join(clone, "web/scripts/release-dist.mjs")).href;
    const source = `
      import { stageReproducibleDist } from ${JSON.stringify(releaseModule)};
      await stageReproducibleDist(
        "npm",
        ["run", "build", "--workspace", "web", "--", "--outDir", "{out}", "--emptyOutDir"],
        "win32",
      );
    `;

    const result = spawnSync("node", ["--input-type=module", "--eval", source], {
      cwd: clone,
      env,
      encoding: "utf8",
    });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("requires a POSIX release host");
    expect(manifest(clone)).toEqual(expected);
    expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
    expect(readdirSync(temporaryRoot)).toStrictEqual([]);
    expect(existsSync(env.BUNDLE_TEST_COUNT!)).toBe(false);
    expect(existsSync(env.BUNDLE_TEST_GO_CALLS!)).toBe(false);
  });

  it("documents the POSIX release-host boundary and its process-group reason", () => {
    const readme = readFileSync(join(repository, "README.md"), "utf8").replaceAll(/\s+/g, " ");

    expect(readme).toContain("POSIX release host");
    expect(readme).toContain("negative process-group IDs");
    expect(readme).toContain("native Windows");
  });

  it("dist-reset restores the exact tracked snapshot and removes generated extras", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    writeFileSync(join(clone, "internal/browserui/dist/index.html"), "rewritten");
    run("git", ["add", "internal/browserui/dist/index.html"], clone);
    unlinkSync(trackedJavaScript(clone));
    run("git", ["add", "-u", "internal/browserui/dist"], clone);
    writeFileSync(join(clone, "internal/browserui/dist/assets/generated-extra.js"), "extra");
    writeFileSync(join(clone, "internal/browserui/dist/generated-extra.txt"), "extra");

    run("make", ["dist-reset"], clone);

    expect(manifest(clone)).toEqual(expected);
    expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
    const index = readFileSync(join(clone, "internal/browserui/dist/index.html"), "utf8");
    const references = [...index.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((match) => match[1]!);
    expect(references.length).toBeGreaterThan(0);
    expect(references.every((reference) => existsSync(join(clone, "internal/browserui/dist", reference)))).toBe(true);
  });

  it("the npm dist reset entry point restores the same complete snapshot", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    writeFileSync(join(clone, "internal/browserui/dist/index.html"), "rewritten");
    unlinkSync(trackedJavaScript(clone));
    run("git", ["add", "-u", "internal/browserui/dist"], clone);
    writeFileSync(join(clone, "internal/browserui/dist/assets/generated-extra.js"), "extra");

    run("npm", ["run", "dist:reset", "--prefix", "web"], clone);

    expect(manifest(clone)).toEqual(expected);
    expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
  });

  it("dist-reset removes a nested Git repository before restoring the snapshot", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const nested = join(clone, "internal/browserui/dist/generated-nested-repository");
    mkdirSync(nested);
    run("git", ["init", "--quiet"], nested);
    writeFileSync(join(nested, "owned-by-generated-repo"), "extra");

    run("make", ["dist-reset"], clone);

    expect(existsSync(nested)).toBe(false);
    expectPristineDist(clone, expected);
  });

  it("dist-reset recovers a simulated SIGKILL or power-loss publication residue", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    rmSync(join(clone, "internal/browserui/dist"), { recursive: true, force: true });
    mkdirSync(join(clone, "internal/browserui/dist/assets"), { recursive: true });
    writeFileSync(join(clone, "internal/browserui/dist/index.html"), "interrupted candidate");
    writeFileSync(join(clone, "internal/browserui/dist/assets/interrupted.js"), "interrupted candidate");
    run("git", ["add", "-f", "--all", "--", "internal/browserui/dist"], clone);

    run("make", ["dist-reset"], clone);

    expectPristineDist(clone, expected);
  });

  it("release-dist stages one of two byte-identical isolated builds", () => {
    const clone = cloneWithCurrentWorkflow();
    const env = installFakeReleaseTools(clone, "deterministic");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status, `${result.stdout}\n${result.stderr}`).toBe(0);
    expect(readFileSync(env.BUNDLE_TEST_COUNT!, "utf8")).toBe("2");
    expect(readFileSync(env.BUNDLE_TEST_GO_CALLS!, "utf8").trim().split("\n").filter((call) => !call.startsWith("list -f"))).toStrictEqual([
      "test -race -count=1 ./...",
      "build ./...",
    ]);
    expect(readFileSync(join(clone, "internal/browserui/dist/assets/app.js"), "utf8")).toContain('"stable"');
    expect(run("git", ["diff", "--name-only", "--cached", "--", "internal/browserui/dist"], clone)).not.toBe("");
    expect(existsSync(join(clone, "internal/browserui/dist/assets/app.js"))).toBe(true);
  });

  /**
   * The marker is what Factory's default command gates on, and `make
   * release-dist` is the only thing that can honestly set it: it is the only
   * step that knows the tree it is installing was built twice, compared, and
   * verified. So the release flag is passed into the build here, and read back
   * out of the installed tree before the commit is allowed to exist.
   */
  it("stages a bundle whose marker declares itself a release", () => {
    const clone = cloneWithCurrentWorkflow();
    const env = installFakeReleaseTools(clone, "deterministic");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status, `${result.stdout}\n${result.stderr}`).toBe(0);
    const marker = JSON.parse(readFileSync(join(clone, "internal/browserui/dist/looprig-bundle.json"), "utf8"));
    expect(marker.release).toBe(true);
    // Staged, not merely written: an unstaged marker is not in the release commit.
    expect(run("git", ["diff", "--name-only", "--cached", "--", "internal/browserui/dist"], clone).split("\n"))
      .toContain("internal/browserui/dist/looprig-bundle.json");
  });

  it.each([
    // The v0.1.0 defect, one level up: a build that emits no marker publishes a
    // tree for which wui.BundleProtocolVersion answers ErrNoBundleManifest.
    ["a build that emits no marker", "absent" as const, "carries no manifest"],
    // A build that ran without the release flag, or with a stale marker left in
    // place. Factory would refuse the published bundle; this refuses the tag.
    ["a marker that does not claim a release", "non-release" as const, "does not declare itself a release"],
  ])("refuses to stage %s", (_label, marker, message) => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "deterministic", { marker });

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain(message);
    // Rolled back: the refusal must leave the previous release installed.
    expectPristineDist(clone, expected);
  });

  it("release-dist rejects nondeterministic builds without changing the snapshot or index", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "nondeterministic");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("not reproducible");
    expect(readFileSync(env.BUNDLE_TEST_COUNT!, "utf8")).toBe("2");
    expect(manifest(clone)).toEqual(expected);
    expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
  });

  /**
   * The PATH axis of the same comparison, which the content case above cannot
   * reach.
   *
   * `bundleManifest` compares three axes -- path, type and sha256 -- and
   * `stageReproducibleDist` compares the whole sorted entry list. Until this
   * case, only the content axis was rowed: narrowing the comparison to
   * `map((entry) => entry.sha256)` survived all 53 tests in this file, so
   * nothing here discriminated a build that renamed its output. That is the
   * wrong axis to leave open, because Vite names assets by content hash and a
   * rename is the likeliest signature of a real nondeterministic build.
   *
   * The fixture holds every byte fixed and moves only the name, so the two
   * builds produce an IDENTICAL multiset of sha256 values in an identical
   * sorted order. A comparison that looked only at content would see two equal
   * bundles and publish one of them.
   *
   * The third axis, `type`, deliberately gets no case, and the reason is that
   * NO case could kill a mutant that drops it. `bundleManifest` emits exactly
   * two kinds -- `{path, type: "directory"}` and `{path, type: "file", sha256}`
   * -- and throws on every other entry kind, so `type` and the PRESENCE of
   * `sha256` are the same bit written twice. For any two manifests it can
   * produce, comparing `{path, sha256}` yields the same verdict as comparing
   * `{path, type, sha256}`: two entries that differ in type differ in whether
   * `sha256` is there at all. Dropping `type` is therefore an EQUIVALENT
   * mutant, not a coverage gap, and equivalent mutants cannot be killed.
   *
   * Measured rather than reasoned, and the measurement corrected the reasoning:
   * that mutant was first predicted to die on the content case above, was run,
   * and SURVIVED 54/54 -- which is what equivalence looks like. If
   * `bundleManifest` ever grows a third entry kind, or emits `sha256` for a
   * non-file, this paragraph stops being true and the axis needs a real case.
   */
  it("release-dist rejects a build that renames its output, with every byte identical", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "nondeterministic-filename");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("not reproducible");
    // Both builds ran: a refusal that short-circuited after one would pass the
    // assertions above for the wrong reason.
    expect(readFileSync(env.BUNDLE_TEST_COUNT!, "utf8")).toBe("2");
    expect(manifest(clone)).toEqual(expected);
    expect(run("git", ["status", "--porcelain=v1", "--", "internal/browserui/dist"], clone)).toBe("");
  });

  it("refuses to overwrite caller dist changes before it starts building", () => {
    const clone = cloneWithCurrentWorkflow();
    writeFileSync(join(clone, "internal/browserui/dist/index.html"), "caller change");
    const before = manifest(clone);
    const env = installFakeReleaseTools(clone, "deterministic");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("dist must be clean");
    expect(manifest(clone)).toEqual(before);
    expect(existsSync(env.BUNDLE_TEST_COUNT!)).toBe(false);
  });

  it("also refuses ignored generated dist files owned by the caller", () => {
    const clone = cloneWithCurrentWorkflow();
    writeFileSync(join(clone, "internal/browserui/dist/assets/caller-generated.js"), "caller change");
    const before = manifest(clone);
    const env = installFakeReleaseTools(clone, "deterministic");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("dist must be clean");
    expect(manifest(clone)).toEqual(before);
    expect(existsSync(env.BUNDLE_TEST_COUNT!)).toBe(false);
  });

  it.each(["tracked", "ignored", "staged"] as const)(
    "preserves a caller %s dist edit introduced during the builds",
    (duringBuildEdit) => {
      const clone = cloneWithCurrentWorkflow();
      const env = installFakeReleaseTools(clone, "deterministic", { duringBuildEdit });

      const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

      expect(result.status).not.toBe(0);
      expect(`${result.stdout}\n${result.stderr}`).toContain("dist must be clean");
      expect(readFileSync(env.BUNDLE_TEST_COUNT!, "utf8")).toBe("2");
      if (duringBuildEdit === "ignored") {
        expect(readFileSync(join(clone, "internal/browserui/dist/assets/caller-during-build.js"), "utf8")).toBe("caller edit during build");
      } else {
        expect(readFileSync(join(clone, "internal/browserui/dist/index.html"), "utf8")).toBe("caller edit during build");
      }
      expect(existsSync(join(clone, "internal/browserui/dist/assets/app.js"))).toBe(false);
    },
  );

  it.each(["SIGHUP", "SIGINT", "SIGTERM"] as const)(
    "terminates its gate process group before rollback on direct %s",
    async (signal) => {
      const clone = cloneWithCurrentWorkflow();

      const interrupted = await interruptRelease(clone, signal);

      expectPristineDist(clone, interrupted.expected);
      expect(readdirSync(interrupted.temporaryRoot).filter((entry) => entry.startsWith("looprig-carbon-release-dist-")))
        .toStrictEqual([]);
      expect(interrupted.result).toStrictEqual({ code: signalExitCode[signal], signal: null });
      await new Promise((resolve) => setTimeout(resolve, 1_000));
      expectPristineDist(clone, interrupted.expected);
      expect(existsSync(interrupted.env.BUNDLE_TEST_LATE_TEMP!)).toBe(false);
    },
  );

  it("retains ownership when a gate leader exits as interruption arrives", async () => {
    const clone = cloneWithCurrentWorkflow();

    const interrupted = await interruptAfterGateLeaderExit(clone);

    expect(interrupted.result).toStrictEqual({ code: 143, signal: null });
    await new Promise((resolve) => setTimeout(resolve, 1_000));
    expectPristineDist(clone, interrupted.expected);
    expect(existsSync(interrupted.env.BUNDLE_TEST_LATE_TEMP!)).toBe(false);
    expect(readdirSync(interrupted.temporaryRoot).filter((entry) => entry.startsWith("looprig-carbon-release-dist-")))
      .toStrictEqual([]);
  });

  it.each(["SIGHUP", "SIGINT", "SIGTERM"] as const)(
    "cleans temporary output when directly interrupted during build one with %s",
    async (signal) => {
      const clone = cloneWithCurrentWorkflow();

      const interrupted = await interruptFirstBuild(clone, signal);

      expectPristineDist(clone, interrupted.expected);
      expect(readdirSync(interrupted.temporaryRoot).filter((entry) => entry.startsWith("looprig-carbon-release-dist-")))
        .toStrictEqual([]);
      expect(interrupted.result).toStrictEqual({ code: signalExitCode[signal], signal: null });
    },
  );

  it.each([
    ["post-stage Go gate", { gate: true }],
    ["git add", { gitAdd: true }],
  ] as const)("rolls back exact dist bytes and index when %s fails", (_name, failures) => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "deterministic", failures);

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expectPristineDist(clone, expected);
  });

  it("rolls back a post-install bundle validation failure", () => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, "placeholder");

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expectPristineDist(clone, expected);
  });

  /**
   * The same entry-kind space `bundle entry classification` covers, driven end
   * to end through `make`. The three link kinds below the original five are the
   * neighbours of the reported defect: a dangling ABSOLUTE link fails a
   * following `stat` exactly as a dangling relative one does, a loop answers
   * ELOOP instead of ENOENT, and a DIRECTORY link is worse than either, because
   * `readdirSync(..., { recursive: true })` follows it and walks out of the
   * bundle. Only `symlink-relative-*` was ever reported; fixing those alone
   * would have left all three.
   */
  it.each([
    "symlink-absolute-same",
    "symlink-absolute-different",
    "symlink-relative-same",
    "symlink-relative-different",
    "symlink-dangling-absolute",
    "symlink-directory",
    "symlink-loop",
    "fifo",
  ] as const)("rejects unsupported %s output before publication", (mode) => {
    const clone = cloneWithCurrentWorkflow();
    const expected = manifest(clone);
    const env = installFakeReleaseTools(clone, mode);

    const result = spawnSync("make", ["release-dist"], { cwd: clone, env, encoding: "utf8" });

    expect(result.status).not.toBe(0);
    expect(`${result.stdout}\n${result.stderr}`).toContain("unsupported bundle entry");
    expectPristineDist(clone, expected);
  });
});
