import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, expect, test } from "vitest";
import { BUNDLE_MANIFEST_NAME, buildBundleManifest, readBundleInputs, writeBundleManifest } from "./write-bundle-manifest.mjs";

const repository = new URL("../..", import.meta.url).pathname;
const temporary: string[] = [];
function temp(): string { const dir = mkdtempSync(join(tmpdir(), "carbon-bundle-")); temporary.push(dir); return dir; }
afterEach(() => { for (const dir of temporary.splice(0)) rmSync(dir, { recursive: true, force: true }); });

test("reads exact installed client and React versions with the client contract snapshot", () => {
  expect(readBundleInputs(repository)).toEqual({ clientVersion: "0.3.0", reactVersion: "0.3.0", coreVersion: "v0.12.0", sessionwireVersion: 1 });
});

test("refuses a contract snapshot for another installed client", () => {
  const root = temp();
  mkdirSync(join(root, "web/node_modules/@looprig/client"), { recursive: true });
  mkdirSync(join(root, "web/node_modules/@looprig/react"), { recursive: true });
  writeFileSync(join(root, "web/client-contract.json"), JSON.stringify({ client_version: "0.1.0", core_version: "v0.12.0", sessionwire_version: 1 }));
  writeFileSync(join(root, "web/node_modules/@looprig/client/package.json"), JSON.stringify({ version: "0.2.0" }));
  writeFileSync(join(root, "web/node_modules/@looprig/react/package.json"), JSON.stringify({ version: "0.2.0" }));
  expect(() => readBundleInputs(root)).toThrow(/contract snapshot/);
});

test("writes a release marker with all version claims", () => {
  const out = temp();
  writeBundleManifest(out, { repository, release: true });
  expect(JSON.parse(readFileSync(join(out, BUNDLE_MANIFEST_NAME), "utf8"))).toEqual({
    client_version: "0.3.0", core_version: "v0.12.0", react_version: "0.3.0", release: true, sessionwire_version: 1,
  });
});

test.each([
  { release: "true" }, { clientVersion: "" }, { reactVersion: "" }, { coreVersion: "" }, { sessionwireVersion: 0 }, { sessionwireVersion: 1.5 },
])("refuses invalid manifest input %j", (override) => {
  expect(() => buildBundleManifest({ release: true, clientVersion: "0.2.0", reactVersion: "0.2.0", coreVersion: "v0.12.0", sessionwireVersion: 1, ...override } as Parameters<typeof buildBundleManifest>[0])).toThrow();
});

test("the committed marker describes the installed package and contract", () => {
  const committed = JSON.parse(readFileSync(join(repository, "internal/browserui/dist", BUNDLE_MANIFEST_NAME), "utf8"));
  const source = readBundleInputs(repository);
  expect(committed.client_version).toBe(source.clientVersion);
  expect(committed.react_version).toBe(source.reactVersion);
  expect(committed.core_version).toBe(source.coreVersion);
  expect(committed.sessionwire_version).toBe(source.sessionwireVersion);
});
