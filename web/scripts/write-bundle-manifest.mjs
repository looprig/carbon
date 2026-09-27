import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const BUNDLE_MANIFEST_NAME = "looprig-bundle.json";
export const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
export const DIST_DIR = join(REPOSITORY_ROOT, "internal/browserui/dist");

const readJson = (path) => JSON.parse(readFileSync(path, "utf8"));

// The installed registry packages own their versions. The client contract
// snapshot comes from client v0.2.0's contract/VERSION and negotiation schema;
// its client_version ties it to that exact installed package.
export function readBundleInputs(repository = REPOSITORY_ROOT) {
  const web = join(repository, "web");
  const provenance = readJson(join(web, "client-contract.json"));
  const clientVersion = readJson(join(web, "node_modules/@looprig/client/package.json")).version;
  const reactVersion = readJson(join(web, "node_modules/@looprig/react/package.json")).version;
  if (provenance.client_version !== clientVersion) {
    throw new Error(`client contract snapshot is for ${provenance.client_version}, installed client is ${clientVersion}`);
  }
  return { clientVersion, reactVersion, coreVersion: provenance.core_version, sessionwireVersion: provenance.sessionwire_version };
}

export function buildBundleManifest({ release, clientVersion, reactVersion, coreVersion, sessionwireVersion }) {
  if (typeof release !== "boolean") throw new TypeError("release must be boolean");
  for (const [name, value] of Object.entries({ clientVersion, reactVersion, coreVersion })) {
    if (typeof value !== "string" || value === "") throw new TypeError(`${name} must be nonempty`);
  }
  if (!Number.isInteger(sessionwireVersion) || sessionwireVersion < 1) throw new TypeError("sessionwireVersion must be a positive integer");
  return { client_version: clientVersion, core_version: coreVersion, react_version: reactVersion, release, sessionwire_version: sessionwireVersion };
}

export function writeBundleManifest(directory = DIST_DIR, options = {}) {
  const { release = false, repository = REPOSITORY_ROOT } = options;
  const manifest = buildBundleManifest({ ...readBundleInputs(repository), release });
  writeFileSync(join(directory, BUNDLE_MANIFEST_NAME), `${JSON.stringify(manifest, null, 2)}\n`);
  return manifest;
}

if (import.meta.url === new URL(process.argv[1], "file:").href) {
  const args = process.argv.slice(2);
  const release = args.includes("--release");
  const directory = args.find((argument) => !argument.startsWith("--")) ?? DIST_DIR;
  console.log(writeBundleManifest(directory, { release }));
}
