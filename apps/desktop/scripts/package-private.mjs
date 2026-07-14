#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  parsePackageArgs,
  resolveBuildMatrix,
  stripLeadingSeparator,
} from "./package.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const desktopRoot = resolve(here, "..");

export const PRIVATE_PACKAGE_ENV = Object.freeze({
  VITE_API_URL: "https://direct.multica-be.elvisiky.com:3000",
  VITE_WS_URL: "wss://direct.multica-be.elvisiky.com:3000/ws",
  VITE_APP_URL: "https://direct.multica.elvisiky.com:3000",
});

export const PRIVATE_MACOS_SIGNING_MODE = "developer-id-notarized";
export const REQUIRED_PRIVATE_MACOS_ENV = Object.freeze([
  "CSC_LINK",
  "CSC_KEY_PASSWORD",
  "APPLE_ID",
  "APPLE_APP_SPECIFIC_PASSWORD",
  "APPLE_TEAM_ID",
]);

export function privatePackageEnv(env = process.env) {
  return {
    ...env,
    ...PRIVATE_PACKAGE_ENV,
  };
}

export function validatePrivateMacosEnvironment(env = process.env) {
  if (env.MULTICA_MACOS_SIGNING_MODE !== PRIVATE_MACOS_SIGNING_MODE) {
    throw new Error(
      `Private macOS packaging requires MULTICA_MACOS_SIGNING_MODE=${PRIVATE_MACOS_SIGNING_MODE}`,
    );
  }

  const missing = REQUIRED_PRIVATE_MACOS_ENV.filter(
    (name) => typeof env[name] !== "string" || env[name].trim() === "",
  );
  if (missing.length > 0) {
    throw new Error(
      `Private macOS packaging is missing controlled signing/notarization inputs: ${missing.join(", ")}`,
    );
  }

  if (env.CSC_IDENTITY_AUTO_DISCOVERY?.trim().toLowerCase() === "false") {
    throw new Error(
      "Private macOS packaging forbids CSC_IDENTITY_AUTO_DISCOVERY=false because release signing is required",
    );
  }
}

export function packagePrivateDesktop({
  args = process.argv.slice(2),
  env = process.env,
  platform = process.platform,
  arch = process.arch,
  spawn = spawnSync,
  root = desktopRoot,
} = {}) {
  const parsed = parsePackageArgs(stripLeadingSeparator(args));
  const buildMatrix = resolveBuildMatrix(parsed, platform, arch);
  const childEnv = privatePackageEnv(env);
  if (buildMatrix.some((target) => target.platform === "mac")) {
    validatePrivateMacosEnvironment(childEnv);
  }

  return spawn(process.execPath, [resolve(root, "scripts", "package.mjs"), ...args], {
    cwd: root,
    env: childEnv,
    stdio: "inherit",
  });
}

function main() {
  let result;
  try {
    result = packagePrivateDesktop();
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  }

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
