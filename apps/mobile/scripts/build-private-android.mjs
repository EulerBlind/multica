import crypto, { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const defaultMobileDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const apiUrl = "https://direct.multica-be.elvisiky.com:3000";
const webUrl = "https://direct.multica.elvisiky.com:3000";
const displayName = "multica";

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

function signerSha256(verifierOutput) {
  const digest = verifierOutput.match(/Signer #\d+ certificate SHA-256 digest:\s*([0-9a-f:]+)/i)?.[1];
  if (!digest) throw new Error("Verifier output is missing the signer certificate SHA-256 digest");
  return digest.replaceAll(":", "").toLowerCase();
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

function removePrivateAndroidDistArtifacts(distDir) {
  if (!fs.existsSync(distDir)) return;
  const artifacts = fs
    .readdirSync(distDir)
    .filter(
      (name) =>
        /^multica-private-android-.+\.(?:apk|provenance\.json)$/.test(name) ||
        /^\.multica-private-android-.+\.tmp$/.test(name),
    )
    .map((name) => path.join(distDir, name));
  removeFiles(artifacts);
}

export function parseMobileGitVersion(raw) {
  const gitVersion = raw.trim().replace(/^v/, "");
  const match = gitVersion.match(/^(\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?$/i);
  if (!match) {
    throw new Error(`git describe did not produce a tagged Multica version: ${raw.trim()}`);
  }
  return { gitVersion, releaseVersion: match[1] };
}

export function validatePrivateAndroidEnvironment({ env = process.env, spawn = spawnSync } = {}) {
  const packageId = env.EXPO_ANDROID_PACKAGE_PRIVATE;
  const signingMode = env.MULTICA_ANDROID_SIGNING_MODE;
  const sdkRoot = env.ANDROID_SDK_ROOT ?? env.ANDROID_HOME;

  function capture(command, args) {
    const result = spawn(command, args, { encoding: "utf8", env });
    if (result.status !== 0) throw new Error(`${command} is unavailable`);
    return `${result.stdout ?? ""}${result.stderr ?? ""}`;
  }

  const javaVersion = capture("java", ["-version"]);
  const javaMajor = Number(javaVersion.match(/version "(?:1\.)?(\d+)/)?.[1]);
  if (javaMajor !== 17) throw new Error(`JDK 17 is required; detected: ${javaVersion.trim()}`);
  if (!sdkRoot) throw new Error("ANDROID_SDK_ROOT or ANDROID_HOME is required");
  for (const requiredPath of [
    path.join(sdkRoot, "platforms", "android-36"),
    path.join(sdkRoot, "build-tools", "36.0.0"),
    path.join(sdkRoot, "ndk", "27.1.12297006"),
  ]) {
    if (!fs.existsSync(requiredPath)) throw new Error(`Android SDK component is missing: ${requiredPath}`);
  }
  if (!packageId) {
    throw new Error("EXPO_ANDROID_PACKAGE_PRIVATE must be owner-confirmed and set explicitly");
  }
  if (signingMode !== "sideload-debug") {
    throw new Error(
      "MULTICA_ANDROID_SIGNING_MODE=sideload-debug is required after the owner approves the test signing path; formal signing must use the owner-managed CI flow",
    );
  }

  return { displayName, javaVersion, packageId, sdkRoot, signingMode };
}

export function runPrivateAndroidBuild({
  mobileDir = defaultMobileDir,
  env = process.env,
  spawn = spawnSync,
  platform = process.platform,
  uniqueId = randomUUID,
  now = () => new Date(),
} = {}) {
  const sourceApk = path.join(mobileDir, "android", "app", "build", "outputs", "apk", "release", "app-release.apk");
  const distDir = path.join(mobileDir, "dist");

  // No preflight failure may leave a source, final, provenance, or stale staged artifact.
  removeFiles([sourceApk]);
  removePrivateAndroidDistArtifacts(distDir);

  let stagedApk;
  let stagedProvenance;
  let finalApk;
  let finalProvenance;
  let published = false;

  try {
    const environment = validatePrivateAndroidEnvironment({ env, spawn });
    const { javaVersion, packageId, sdkRoot } = environment;

    function capture(command, args, options = {}) {
      const result = spawn(command, args, { encoding: "utf8", env, ...options });
      if (result.status !== 0) throw new Error(`${command} is unavailable`);
      return `${result.stdout ?? ""}${result.stderr ?? ""}`;
    }

    const gitCommit = capture("git", ["rev-parse", "HEAD"], { cwd: mobileDir }).trim();
    const gitDirty = capture("git", ["status", "--porcelain"], { cwd: mobileDir }).trim().length > 0;
    if (gitDirty) {
      throw new Error(
        "Private Android build requires a clean git worktree; commit the reviewed source before rebuilding",
      );
    }
    const { gitVersion, releaseVersion } = parseMobileGitVersion(
      capture(
        "git",
        ["describe", "--tags", "--match", "v[0-9]*", "--always"],
        { cwd: mobileDir },
      ),
    );
    const nativeBuildNumber = Number(
      capture("git", ["rev-list", "--count", "HEAD"], { cwd: mobileDir }).trim(),
    );
    if (!Number.isSafeInteger(nativeBuildNumber) || nativeBuildNumber < 1) {
      throw new Error(`git rev-list returned an invalid mobile build number: ${nativeBuildNumber}`);
    }
    const finalApkName = `multica-private-android-${gitVersion}.apk`;
    const provenanceName = `multica-private-android-${gitVersion}.provenance.json`;
    finalApk = path.join(distDir, finalApkName);
    finalProvenance = path.join(distDir, provenanceName);
    const buildEnv = {
      ...env,
      APP_ENV: "private",
      EXPO_PUBLIC_API_URL: apiUrl,
      EXPO_PUBLIC_WEB_URL: webUrl,
      EXPO_ANDROID_PACKAGE_PRIVATE: packageId,
      MULTICA_MOBILE_VERSION: releaseVersion,
      MULTICA_MOBILE_BUILD_NUMBER: String(nativeBuildNumber),
      EXPO_PUBLIC_MULTICA_MOBILE_VERSION: gitVersion,
      ANDROID_SDK_ROOT: sdkRoot,
      ANDROID_HOME: sdkRoot,
      EXPO_NO_TELEMETRY: "1",
    };

    console.log(`JDK: ${javaVersion.trim().split("\n")[0]}`);
    console.log(`Android SDK: ${sdkRoot} (platform/build-tools 36, NDK 27.1.12297006)`);
    console.log(`applicationId: ${packageId}`);
    console.log(`displayName: ${displayName}`);
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

    run("pnpm", ["exec", "expo", "prebuild", "--platform", "android", "--clean", "--no-install"]);
    run(
      path.join(mobileDir, "android", platform === "win32" ? "gradlew.bat" : "gradlew"),
      ["app:assembleRelease", "--no-daemon"],
      { cwd: path.join(mobileDir, "android") },
    );
    if (!fs.existsSync(sourceApk)) throw new Error(`Gradle succeeded without producing ${sourceApk}`);

    fs.mkdirSync(distDir, { recursive: true });
    const buildId = uniqueId();
    stagedApk = path.join(distDir, `.${finalApkName}.${process.pid}.${buildId}.tmp`);
    stagedProvenance = path.join(distDir, `.${provenanceName}.${process.pid}.${buildId}.tmp`);
    fs.copyFileSync(sourceApk, stagedApk);
    const verifierOutput = run(
      "node",
      [path.join(mobileDir, "scripts", "verify-private-android.mjs"), stagedApk, packageId, displayName],
      { captureOutput: true },
    );

    const provenance = {
      schemaVersion: 1,
      buildId,
      builtAt: now().toISOString(),
      gitCommit,
      gitDirty: false,
      gitVersion,
      releaseVersion,
      nativeBuildNumber,
      applicationId: packageId,
      displayName,
      certificateSha256: signerSha256(verifierOutput),
      apkSha256: sha256(stagedApk),
      apkFile: finalApkName,
      privateUrls: [apiUrl, webUrl],
      verified: true,
    };
    fs.writeFileSync(stagedProvenance, `${JSON.stringify(provenance, null, 2)}\n`, "utf8");

    // APK and provenance are staged beside their targets. QA accepts the pair only.
    fs.renameSync(stagedApk, finalApk);
    stagedApk = undefined;
    fs.renameSync(stagedProvenance, finalProvenance);
    stagedProvenance = undefined;
    published = true;

    const result = {
      buildId,
      apkPath: finalApk,
      provenancePath: finalProvenance,
      applicationId: packageId,
      displayName,
      certificateSha256: provenance.certificateSha256,
      apkSha256: provenance.apkSha256,
      gitCommit,
      gitVersion,
      releaseVersion,
      nativeBuildNumber,
    };
    console.log(`Private Android APK: ${finalApk}`);
    console.log(`Provenance: ${finalProvenance}`);
    return result;
  } finally {
    removeFiles([
      sourceApk,
      ...(stagedApk ? [stagedApk] : []),
      ...(stagedProvenance ? [stagedProvenance] : []),
      ...(!published && finalApk ? [finalApk] : []),
      ...(!published && finalProvenance ? [finalProvenance] : []),
    ]);
  }
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  try {
    const result = runPrivateAndroidBuild();
    console.log(`MULTICA_PRIVATE_ANDROID_BUILD=${JSON.stringify(result)}`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exitCode = 1;
  }
}
