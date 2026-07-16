#!/usr/bin/env node
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  PRIVATE_MACOS_SIGNING_MODE,
  REQUIRED_PRIVATE_MACOS_ENV,
  validatePrivateMacosEnvironment,
} from "../apps/desktop/scripts/package-private.mjs";

export {
  PRIVATE_MACOS_SIGNING_MODE,
  REQUIRED_PRIVATE_MACOS_ENV,
  validatePrivateMacosEnvironment,
};

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const defaultAndroidApplicationId = "ai.multica.mobile.dev";
const defaultAndroidSigningMode = "sideload-debug";

export function resolvePrivateBuildPaths({ env = process.env, home = os.homedir() } = {}) {
  const buildRoot = path.resolve(env.MULTICA_BUILD_ROOT ?? path.join(home, ".multica-build"));
  return {
    buildRoot,
    javaHome: path.join(buildRoot, "jdk17"),
    androidSdkRoot: path.join(buildRoot, "android-sdk"),
    gradleUserHome: path.join(buildRoot, "gradle-home"),
  };
}

export function privateAndroidBuildEnv({ env = process.env, home } = {}) {
  const paths = resolvePrivateBuildPaths({ env, home });
  // The no-argument local build reuses the tracked mainline development id.
  // A private release id remains an explicit owner-controlled override.
  const hasApplicationId = Object.hasOwn(env, "EXPO_ANDROID_PACKAGE_PRIVATE");
  const hasSigningMode = Object.hasOwn(env, "MULTICA_ANDROID_SIGNING_MODE");
  const useLocalDefaults = !hasApplicationId && !hasSigningMode;
  const applicationId = useLocalDefaults
    ? defaultAndroidApplicationId
    : env.EXPO_ANDROID_PACKAGE_PRIVATE?.trim();
  const signingMode = useLocalDefaults
    ? defaultAndroidSigningMode
    : env.MULTICA_ANDROID_SIGNING_MODE?.trim();
  if (!applicationId || !signingMode) {
    throw new Error(
      "EXPO_ANDROID_PACKAGE_PRIVATE and MULTICA_ANDROID_SIGNING_MODE must both be non-blank when either is provided",
    );
  }
  const pathValue = [
    path.join(paths.javaHome, "bin"),
    path.join(paths.androidSdkRoot, "platform-tools"),
    env.PATH,
  ]
    .filter(Boolean)
    .join(path.delimiter);

  return {
    paths,
    env: {
      ...env,
      JAVA_HOME: paths.javaHome,
      ANDROID_SDK_ROOT: paths.androidSdkRoot,
      ANDROID_HOME: paths.androidSdkRoot,
      GRADLE_USER_HOME: paths.gradleUserHome,
      EXPO_ANDROID_PACKAGE_PRIVATE: applicationId,
      MULTICA_ANDROID_SIGNING_MODE: signingMode,
      PATH: pathValue,
    },
  };
}

function run(command, args, { cwd = repoRoot, env = process.env, spawn = spawnSync } = {}) {
  const result = spawn(command, args, { cwd, env, stdio: "inherit" });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`${command} failed with status ${result.status ?? "unknown"}`);
  }
}

export function packagePrivateAndroid({
  env = process.env,
  home,
  exists = fs.existsSync,
  spawn = spawnSync,
  root = repoRoot,
} = {}) {
  const resolved = privateAndroidBuildEnv({ env, home });
  for (const [label, directory] of [
    ["JDK 17", resolved.paths.javaHome],
    ["Android SDK", resolved.paths.androidSdkRoot],
    ["Gradle home", resolved.paths.gradleUserHome],
  ]) {
    if (!exists(directory)) {
      throw new Error(`${label} is missing from fixed build root: ${directory}`);
    }
  }

  run(
    process.execPath,
    [path.join(root, "apps", "mobile", "scripts", "build-private-android.mjs")],
    { cwd: root, env: resolved.env, spawn },
  );
  return resolved.paths;
}

export function packagePrivateMacos({
  env = process.env,
  platform = process.platform,
  spawn = spawnSync,
  root = repoRoot,
} = {}) {
  if (platform !== "darwin") {
    throw new Error(`Private macOS packaging requires a macOS host; current platform is ${platform}`);
  }
  validatePrivateMacosEnvironment(env);
  run(
    "pnpm",
    ["--filter", "@multica/desktop", "package:private", "--", "--mac", "--publish", "never"],
    { cwd: root, env, spawn },
  );
}

export function packagePrivate(target, options = {}) {
  if (target === "android") return packagePrivateAndroid(options);
  if (target === "macos") return packagePrivateMacos(options);
  throw new Error("Usage: node scripts/package-private.mjs <android|macos>");
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  try {
    if (process.argv[2] === "android") {
      console.log(`Using fixed Android build root: ${resolvePrivateBuildPaths().buildRoot}`);
    }
    packagePrivate(process.argv[2]);
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
