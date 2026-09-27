import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { cpSync, lstatSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { checkDist, walkBundleEntries } from "./check-dist.mjs";
import { BUNDLE_MANIFEST_NAME } from "./write-bundle-manifest.mjs";

const repository = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const dist = join(repository, "internal/browserui/dist");
const signalExitCode = { SIGHUP: 129, SIGINT: 130, SIGTERM: 143 };
const handledSignals = Object.keys(signalExitCode);
const posixReleasePlatforms = new Set(["aix", "android", "darwin", "freebsd", "linux", "openbsd", "sunos"]);
const terminationGraceMilliseconds = 500;
const killWaitMilliseconds = 5_000;

class ReleaseSignalError extends Error {
  constructor(signal) {
    super(`release publication interrupted by ${signal}`);
    this.name = "ReleaseSignalError";
    this.signal = signal;
    this.exitCode = signalExitCode[signal];
  }
}

/**
 * The bundle walk's entry classifier, and the sole reporter of an entry kind
 * the release cannot publish.
 *
 * It classifies by `lstat`, so it judges the ENTRY, never the entry's target:
 * a directory is a directory, a regular file is hashed, and everything else —
 * symlink of any kind, FIFO, socket, device — is refused by name.
 *
 * This is only the sole reporter because nothing upstream of it stats the same
 * tree by a path-following call. `checkDist`, which `build` runs FIRST, used to
 * use `statSync`; a dangling symlink or a symlink loop made it throw ENOENT or
 * ELOOP and the release died there, so the one guard that exists to name the
 * offending entry never ran. `checkDist` therefore also lstats, and
 * `bundle entry classification` in `bundle-workflow.test.ts` holds that
 * ordering by asserting, per entry kind, that `checkDist` reaches a verdict
 * and that this function is the thing that reports the refusal.
 *
 * @param {string} directory Bundle directory to walk.
 */
export function bundleManifest(directory) {
  const root = lstatSync(directory);
  if (!root.isDirectory() || root.isSymbolicLink()) {
    throw new Error("unsupported bundle entry type at output root");
  }
  return walkBundleEntries(directory)
    .map(({ path, metadata }) => {
      if (metadata.isDirectory()) return { path, type: "directory" };
      if (!metadata.isFile()) throw new Error(`unsupported bundle entry type: ${path}`);
      return {
        path,
        type: "file",
        sha256: createHash("sha256").update(readFileSync(join(directory, path))).digest("hex"),
      };
    })
    .sort((left, right) => left.path.localeCompare(right.path));
}

/**
 * Reads the marker out of a candidate bundle and insists it claims a release.
 *
 * The flag is an input to the build (`CARBON_BUNDLE_RELEASE`, read by
 * `vite.config.ts`'s bundle manifest plugin), so a release staged by any route
 * that did not set it would install a tree `wui.BundleProtocolVersion` reports
 * as a development build and Factory's default command refuses. That is a
 * defect discovered after the tag, by a consumer. Reading the claim back out of
 * the installed tree turns it into one discovered before the commit exists.
 *
 * An ABSENT manifest is reported separately, because it is a different fault
 * with a different remedy: the build emitted no marker at all, which is the
 * v0.1.0 failure one level up — a published bundle no consumer can identify.
 *
 * What this is NOT, so nobody later reads more into it than it says: it is a
 * WIRING check, not a proof of provenance. It reads back a value the build
 * wrote from `CARBON_BUNDLE_RELEASE` alone, so it establishes that the flag
 * reached the build that produced this tree — nothing more. It does not
 * corroborate that the two isolated builds ran, that they compared equal, or
 * that anyone but a developer with an editor set the flag. Those are properties
 * of the surrounding transaction in `stageReproducibleDist`, and the only thing
 * that would make the claim independently checkable by a consumer is a
 * signature, which this repository deliberately does not attempt.
 *
 * @param {string} directory Bundle directory to inspect.
 */
export function assertReleaseMarker(directory) {
  const path = join(directory, BUNDLE_MANIFEST_NAME);
  let raw;
  try {
    raw = readFileSync(path, "utf8");
  } catch {
    throw new Error(`installed release bundle carries no manifest at ${BUNDLE_MANIFEST_NAME}`);
  }
  let marker;
  try {
    marker = JSON.parse(raw);
  } catch (error) {
    throw new Error(`installed release bundle has an unreadable manifest: ${error.message}`);
  }
  if (marker?.release !== true) {
    throw new Error(
      `installed release bundle does not declare itself a release: ${BUNDLE_MANIFEST_NAME} says release=${JSON.stringify(marker?.release)}`,
    );
  }
}

function runChecked(command, args, label, options = {}) {
  const result = spawnSync(command, args, { cwd: repository, stdio: "inherit", ...options });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${label} exited with status ${result.status}`);
  return result;
}

function delay(milliseconds) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
}

function processGroupExists(processGroupId) {
  try {
    process.kill(-processGroupId, 0);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") return false;
    if (error?.code === "EPERM") return true;
    throw error;
  }
}

function signalProcessGroup(processGroupId, signal) {
  try {
    process.kill(-processGroupId, signal);
  } catch (error) {
    if (error?.code !== "ESRCH") throw error;
  }
}

async function waitForProcessGroupExit(processGroupId, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (processGroupExists(processGroupId)) {
    if (Date.now() >= deadline) return false;
    await delay(20);
  }
  return true;
}

async function terminateProcessGroup(processGroupId, signal) {
  signalProcessGroup(processGroupId, signal);
  if (await waitForProcessGroupExit(processGroupId, terminationGraceMilliseconds)) return;
  signalProcessGroup(processGroupId, "SIGKILL");
  if (!await waitForProcessGroupExit(processGroupId, killWaitMilliseconds)) {
    throw new Error(`owned process group ${processGroupId} survived SIGKILL`);
  }
}

class ManagedCommands {
  constructor() {
    this.active = undefined;
    this.receivedSignal = undefined;
    this.termination = undefined;
    this.handlers = new Map(handledSignals.map((signal) => [signal, () => this.interrupt(signal)]));
    for (const [signal, handler] of this.handlers) process.on(signal, handler);
  }

  interrupt(signal) {
    if (this.receivedSignal !== undefined) return;
    this.receivedSignal = signal;
    if (this.active !== undefined && this.termination === undefined) {
      this.termination = terminateProcessGroup(this.active.processGroupId, signal);
    }
  }

  throwIfInterrupted() {
    if (this.receivedSignal !== undefined) throw new ReleaseSignalError(this.receivedSignal);
  }

  async run(command, args, label, options = {}) {
    this.throwIfInterrupted();
    const child = spawn(command, args, {
      cwd: repository,
      stdio: "inherit",
      detached: true,
      ...options,
    });
    const completion = new Promise((resolvePromise) => {
      child.once("error", (error) => resolvePromise({ error }));
      child.once("close", (code, signal) => resolvePromise({ code, signal }));
    });
    const processGroupId = child.pid;
    if (processGroupId === undefined) {
      const result = await completion;
      throw result.error ?? new Error(`could not start ${label}`);
    }
    this.active = { child, processGroupId };
    if (this.receivedSignal !== undefined && this.termination === undefined) {
      this.termination = terminateProcessGroup(processGroupId, this.receivedSignal);
    }
    const result = await completion;
    if (this.termination === undefined && processGroupExists(processGroupId)) {
      this.termination = terminateProcessGroup(processGroupId, "SIGTERM");
    }
    if (this.termination !== undefined) await this.termination;
    this.active = undefined;
    this.termination = undefined;
    this.throwIfInterrupted();
    if (result.error !== undefined) throw result.error;
    if (result.code !== 0) {
      throw new Error(`${label} exited with ${result.signal ?? `status ${result.code}`}`);
    }
  }

  dispose() {
    for (const [signal, handler] of this.handlers) process.off(signal, handler);
  }

  hasLiveProcessGroup() {
    return this.active !== undefined && processGroupExists(this.active.processGroupId);
  }
}

function assertDistClean() {
  const result = spawnSync(
    "git",
    ["status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--", "internal/browserui/dist"],
    { cwd: repository, encoding: "utf8" },
  );
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`git status dist exited with status ${result.status}`);
  if (result.stdout !== "") throw new Error("dist must be clean before release publication");
}

function restoreCommittedDist() {
  runChecked("git", ["clean", "-ffdx", "--", "internal/browserui/dist"], "git clean dist");
  runChecked(
    "git",
    ["restore", "--source=HEAD", "--staged", "--worktree", "--", "internal/browserui/dist"],
    "git restore dist",
  );
}

async function build(commands, command, args, output) {
  const expanded = args.map((argument) => argument === "{out}" ? output : argument);
  await commands.run(command, expanded, "bundle build");
  const classification = checkDist(output);
  if (!classification.ok) {
    throw new Error(`bundle build is not embeddable: ${JSON.stringify(classification)}`);
  }
}

export async function stageReproducibleDist(command, args, platform = process.platform) {
  if (!posixReleasePlatforms.has(platform)) {
    throw new Error(
      `release-dist cannot run on ${platform}: it requires a POSIX release host because transactional rollback terminates release-owned process groups by negative PID; run it from a supported Unix host instead of Windows`,
    );
  }
  if (command === undefined || !args.includes("{out}")) {
    throw new TypeError("usage: release-dist.mjs COMMAND ... {out} ...");
  }
  const commands = new ManagedCommands();
  let temporary;
  try {
    temporary = mkdtempSync(join(tmpdir(), "looprig-carbon-release-dist-"));
    const first = join(temporary, "first");
    const second = join(temporary, "second");
    assertDistClean();
    await build(commands, command, args, first);
    await build(commands, command, args, second);
    const firstManifest = bundleManifest(first);
    const secondManifest = bundleManifest(second);
    if (JSON.stringify(firstManifest) !== JSON.stringify(secondManifest)) {
      throw new Error("release bundle is not reproducible: two isolated builds produced different manifests");
    }
    await new Promise((resolvePromise) => setImmediate(resolvePromise));
    commands.throwIfInterrupted();
    assertDistClean();

    let publicationStarted = false;
    try {
      commands.throwIfInterrupted();
      publicationStarted = true;
      rmSync(dist, { recursive: true, force: true });
      cpSync(first, dist, { recursive: true, force: true });
      if (JSON.stringify(bundleManifest(dist)) !== JSON.stringify(firstManifest)) {
        throw new Error("installed release bundle differs from the validated candidate");
      }
      const classification = checkDist(dist);
      if (!classification.ok) {
        throw new Error(`installed release bundle is not embeddable: ${JSON.stringify(classification)}`);
      }
      if (readFileSync(join(dist, "index.html"), "utf8").includes("placeholder")) {
        throw new Error("installed release bundle is still the placeholder");
      }
      assertReleaseMarker(dist);
      runChecked("git", ["add", "-f", "--all", "--", "internal/browserui/dist"], "git add dist");
      await commands.run(
        "go",
        ["test", "-race", "-count=1", "./..."],
        "Go race gate",
        { env: { ...process.env, GOWORK: "off" } },
      );
      await commands.run(
        "go",
        ["build", "./..."],
        "Go build gate",
        { env: { ...process.env, GOWORK: "off" } },
      );
      runChecked("git", ["diff", "--cached", "--stat", "--", "internal/browserui/dist"], "git staged dist summary");
      await new Promise((resolvePromise) => setImmediate(resolvePromise));
      commands.throwIfInterrupted();
    } catch (error) {
      if (publicationStarted) {
        if (commands.hasLiveProcessGroup()) {
          throw new AggregateError(
            [error],
            "release stopped without rollback because an owned process group could still mutate dist",
          );
        }
        try {
          restoreCommittedDist();
        } catch (rollbackError) {
          throw new AggregateError([error, rollbackError], "release publication and dist rollback both failed");
        }
      }
      commands.throwIfInterrupted();
      throw error;
    }
  } finally {
    if (temporary !== undefined && !commands.hasLiveProcessGroup()) {
      rmSync(temporary, { recursive: true, force: true });
    }
    commands.dispose();
  }
}

if (import.meta.url === new URL(process.argv[1], "file:").href) {
  try {
    await stageReproducibleDist(process.argv[2], process.argv.slice(3));
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exitCode = error instanceof ReleaseSignalError ? error.exitCode : 1;
  }
}
