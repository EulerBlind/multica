#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const desktopRoot = resolve(here, "..");
const packageScript = resolve(here, "package.mjs");

export const PRIVATE_PACKAGE_ENV = Object.freeze({
  VITE_API_URL: "https://direct.multica-be.elvisiky.com:3000",
  VITE_WS_URL: "wss://direct.multica-be.elvisiky.com:3000/ws",
  VITE_APP_URL: "https://direct.multica.elvisiky.com:3000",
});

export function privatePackageEnv(env = process.env) {
  return {
    ...env,
    ...PRIVATE_PACKAGE_ENV,
  };
}

function main() {
  const result = spawnSync(process.execPath, [packageScript, ...process.argv.slice(2)], {
    cwd: desktopRoot,
    env: privatePackageEnv(),
    stdio: "inherit",
  });

  if (result.error) {
    console.error("[package:private] failed to spawn package script:", result.error.message);
    process.exit(1);
  }
  process.exit(result.status ?? 1);
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  main();
}
