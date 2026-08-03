import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const defaultMobileDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
// QIA-393: the private deployment serves both the API (server_url) and the
// web app (app_url) from the same unified domain
// (https://direct.multica.elvisiky.com:3000).
const apiUrl = "https://direct.multica.elvisiky.com:3000";
const webUrl = "https://direct.multica.elvisiky.com:3000";
const displayName = "multica";

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function removeFiles(files) {
  let firstError;
  for (const file of files) {
    try {
      fs.rmSync(file, { force: true });
    } catch (error) {
      firstError ??= error;
    }
  }
  if (firstError) throw firstError;
}

export function validatePrivateIOSEnvironment({ env = process.env, spawn = spawnSync } = {}) {
  const bundleId = env.EXPO_BUNDLE_IDENTIFIER_PRIVATE ?? "ai.multica.mobile.privateapp";
  const signingMode = env.MULTICA_IOS_SIGNING_MODE ?? "simulator";
  if (!["simulator", "sideload"].includes(signingMode)) {
    throw new Error("MULTICA_IOS_SIGNING_MODE must be simulator or sideload");
  }

  function capture(command, args) {
    const result = spawn(command, args, { encoding: "utf8", env });
    if (result.status !== 0) throw new Error(`${command} is unavailable`);
    return `${result.stdout ?? ""}${result.stderr ?? ""}`;
  }

  const platform = capture("uname", ["-s"]).trim();
  if (platform !== "Darwin") {
    throw new Error(`iOS builds require macOS; detected ${platform}`);
  }
  const xcode = capture("xcodebuild", ["-version"]);
  const xcodeLine = xcode.split("\n")[0] ?? "";
  if (!xcodeLine.includes("Xcode")) {
    throw new Error(`Xcode is not available:\n${xcode}`);
  }
  const pod = capture("pod", ["--version"]).trim();
  if (!pod) throw new Error("CocoaPods is not installed (pod --version returned empty)");

  return { bundleId, signingMode, platform, xcodeLine, pod };
}

export function runPrivateIOSBuild({
  mobileDir = defaultMobileDir,
  env = process.env,
  spawn = spawnSync,
  uniqueId = randomUUID,
  now = () => new Date(),
} = {}) {
  const iosDir = path.join(mobileDir, "ios");
  const distDir = path.join(mobileDir, "dist");

  let stagedApp;
  let stagedProvenance;
  let finalApp;
  let finalProvenance;
  let published = false;

  try {
    const environment = validatePrivateIOSEnvironment({ env, spawn });
    const { bundleId, signingMode, xcodeLine, pod } = environment;

    function capture(command, args, options = {}) {
      const result = spawn(command, args, { encoding: "utf8", env, ...options });
      if (result.status !== 0) throw new Error(`${command} ${args.join(" ")} failed:\n${result.stderr || result.stdout}`);
      return `${result.stdout ?? ""}${result.stderr ?? ""}`;
    }

    const gitCommit = capture("git", ["rev-parse", "HEAD"], { cwd: mobileDir }).trim();
    const gitDirty = capture("git", ["status", "--porcelain"], { cwd: mobileDir }).trim().length > 0;
    if (gitDirty) {
      throw new Error(
        "Private iOS build requires a clean git worktree; commit the reviewed source before rebuilding",
      );
    }
    const { gitVersion, releaseVersion } = parseMobileGitVersion(
      capture("git", ["describe", "--tags", "--match", "v[0-9]*", "--always"], { cwd: mobileDir }),
    );
    const nativeBuildNumber = Number(
      capture("git", ["rev-list", "--count", "HEAD"], { cwd: mobileDir }).trim(),
    );
    if (!Number.isSafeInteger(nativeBuildNumber) || nativeBuildNumber < 1) {
      throw new Error(`git rev-list returned an invalid mobile build number: ${nativeBuildNumber}`);
    }
    const finalAppName = `multica-private-ios-${gitVersion}.app`;
    const provenanceName = `multica-private-ios-${gitVersion}.provenance.json`;
    finalApp = path.join(distDir, finalAppName);
    finalProvenance = path.join(distDir, provenanceName);

    const buildEnv = {
      ...env,
      APP_ENV: "private",
      EXPO_PUBLIC_API_URL: apiUrl,
      EXPO_PUBLIC_WEB_URL: webUrl,
      EXPO_BUNDLE_IDENTIFIER_PRIVATE: bundleId,
      MULTICA_MOBILE_VERSION: releaseVersion,
      MULTICA_MOBILE_BUILD_NUMBER: String(nativeBuildNumber),
      EXPO_PUBLIC_MULTICA_MOBILE_VERSION: gitVersion,
      EXPO_NO_TELEMETRY: "1",
    };

    console.log(`macOS: ${process.platform} / ${process.arch}`);
    console.log(`${xcodeLine}`);
    console.log(`CocoaPods: ${pod}`);
    console.log(`bundleIdentifier: ${bundleId}`);
    console.log(`displayName: ${displayName}`);
    console.log(`signingMode: ${signingMode}`);
    console.log(`mobile version: ${releaseVersion} (${nativeBuildNumber}; ${gitVersion})`);
    console.log(`private API: ${apiUrl}`);
    console.log(`private Web: ${webUrl}`);

    function run(command, args, { captureOutput = false, cwd = mobileDir } = {}) {
      console.log(`$ ${command} ${args.join(" ")}`);
      const result = spawn(command, args, {
        cwd,
        env: buildEnv,
        ...(captureOutput
          ? { encoding: "utf8", maxBuffer: 128 * 1024 * 1024 }
          : { stdio: "inherit" }),
      });
      const commandOutput = `${result.stdout ?? ""}${result.stderr ?? ""}`;
      if (result.status !== 0) {
        throw new Error(`${command} failed with status ${result.status ?? "unknown"}${commandOutput ? `:\n${commandOutput}` : ""}`);
      }
      if (captureOutput && commandOutput.trim()) console.log(commandOutput.trim());
      return commandOutput;
    }

    // Prebuild generates the ios/ native project from app.config.ts.
    run("pnpm", ["exec", "expo", "prebuild", "--platform", "ios", "--clean", "--no-install"]);

    if (signingMode === "sideload") {
      // Device archive path — requires a configured signing identity +
      // provisioning profile. Without one, xcodebuild archive fails.
      run(
        "xcodebuild",
        [
          "-workspace", path.join(iosDir, "multica-mobile.xcworkspace"),
          "-scheme", "multica-mobile",
          "-configuration", "Release",
          "-destination", "generic/platform=iOS",
          "-archivePath", path.join(distDir, "multica-private.xcarchive"),
          "CODE_SIGNING_ALLOWED=NO",
          "archive",
        ],
        { cwd: iosDir },
      );
      throw new Error("sideload archive produced without signing; expected a signing identity. Use simulator mode unless a device build is explicitly required.");
    }

    // Simulator path — no signing required. Build the .app into the
    // derived data dir, then stage a copy into dist/.
    const derivedData = path.join(mobileDir, "dist", ".ios-derived-data");
    fs.mkdirSync(derivedData, { recursive: true });
    run(
      "xcodebuild",
      [
        "-workspace", path.join(iosDir, "multica-mobile.xcworkspace"),
        "-scheme", "multica-mobile",
        "-configuration", "Debug",
        "-destination", "generic/platform=iOS Simulator",
        "-derivedDataPath", derivedData,
        "CODE_SIGNING_ALLOWED=NO",
        "build",
      ],
      { cwd: iosDir },
    );

    const builtApp = path.join(derivedData, "Build", "Products", "Debug-iphonesimulator", "multica-mobile.app");
    if (!fs.existsSync(builtApp)) {
      throw new Error(`Simulator build succeeded without producing ${builtApp}`);
    }

    fs.mkdirSync(distDir, { recursive: true });
    const buildId = uniqueId();
    stagedApp = path.join(distDir, `.${finalAppName}.${process.pid}.${buildId}.tmp`);
    stagedProvenance = path.join(distDir, `.${provenanceName}.${process.pid}.${buildId}.tmp`);
    fs.cpSync(builtApp, stagedApp, { recursive: true });

    // Verify the built app's Info.plist carries the private URLs and bundle id.
    const plist = run("plutil", ["-p", path.join(stagedApp, "Info.plist")], { captureOutput: true });
    const bundleIdCheck = plist.includes(bundleId);
    if (!bundleIdCheck) {
      throw new Error(`Built app bundle id is not ${bundleId}`);
    }

    const provenance = {
      schemaVersion: 1,
      buildId,
      builtAt: now().toISOString(),
      gitCommit,
      gitDirty: false,
      gitVersion,
      releaseVersion,
      nativeBuildNumber,
      bundleIdentifier: bundleId,
      displayName,
      signingMode,
      platform: "ios-simulator",
      apkSha256: sha256Dir(stagedApp),
      appFile: finalAppName,
      privateUrls: [apiUrl, webUrl],
      verified: true,
    };
    fs.writeFileSync(stagedProvenance, `${JSON.stringify(provenance, null, 2)}\n`, "utf8");

    fs.renameSync(stagedApp, finalApp);
    stagedApp = undefined;
    fs.renameSync(stagedProvenance, finalProvenance);
    stagedProvenance = undefined;
    published = true;

    const result = {
      buildId,
      appPath: finalApp,
      provenancePath: finalProvenance,
      bundleIdentifier: bundleId,
      displayName,
      gitCommit,
      gitVersion,
      releaseVersion,
      nativeBuildNumber,
      signingMode,
      platform: "ios-simulator",
    };
    console.log(`Private iOS app: ${finalApp}`);
    console.log(`Provenance: ${finalProvenance}`);
    return result;
  } finally {
    removeFiles([
      ...(stagedApp ? [stagedApp] : []),
      ...(stagedProvenance ? [stagedProvenance] : []),
      ...(!published && finalApp ? [finalApp] : []),
      ...(!published && finalProvenance ? [finalProvenance] : []),
    ]);
  }
}

/** Recursively hash a .app bundle — deterministic over file contents. */
function sha256Dir(dir) {
  const entries = [];
  (function walk(p) {
    for (const name of fs.readdirSync(p).sort()) {
      const full = path.join(p, name);
      const stat = fs.lstatSync(full);
      if (stat.isDirectory()) {
        walk(full);
      } else {
        entries.push(full);
      }
    }
  })(dir);
  const hash = crypto.createHash("sha256");
  for (const file of entries) {
    hash.update(path.relative(dir, file));
    hash.update(fs.readFileSync(file));
  }
  return hash.digest("hex");
}

export function parseMobileGitVersion(raw) {
  const gitVersion = raw.trim().replace(/^v/, "");
  const match = gitVersion.match(/^(\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?$/i);
  if (!match) {
    throw new Error(`git describe did not produce a tagged Multica version: ${raw.trim()}`);
  }
  return { gitVersion, releaseVersion: match[1] };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  try {
    const result = runPrivateIOSBuild();
    console.log(`MULTICA_PRIVATE_IOS_BUILD=${JSON.stringify(result)}`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
