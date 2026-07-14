import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import { runPrivateAndroidBuild } from "./build-private-android.mjs";

const fixtureDirs = [];
const certificateSha256 = "a".repeat(64);
const gitCommit = "b".repeat(40);

function createFixture() {
  const mobileDir = fs.mkdtempSync(path.join(os.tmpdir(), "qia319-private-android-"));
  fixtureDirs.push(mobileDir);

  const sdkRoot = path.join(mobileDir, "sdk");
  for (const requiredPath of [
    path.join(sdkRoot, "platforms", "android-36"),
    path.join(sdkRoot, "build-tools", "36.0.0"),
    path.join(sdkRoot, "ndk", "27.1.12297006"),
  ]) {
    fs.mkdirSync(requiredPath, { recursive: true });
  }

  const sourceApk = path.join(mobileDir, "android", "app", "build", "outputs", "apk", "release", "app-release.apk");
  const finalApk = path.join(mobileDir, "dist", "multica-private-android-0.1.0.apk");
  const provenance = path.join(mobileDir, "dist", "multica-private-android-0.1.0.provenance.json");
  const staleTmp = path.join(mobileDir, "dist", ".multica-private-android-0.1.0.apk.previous.tmp");
  fs.mkdirSync(path.dirname(sourceApk), { recursive: true });
  fs.mkdirSync(path.dirname(finalApk), { recursive: true });
  fs.writeFileSync(sourceApk, "stale-source");
  fs.writeFileSync(finalApk, "stale-final");
  fs.writeFileSync(provenance, "stale-provenance");
  fs.writeFileSync(staleTmp, "stale-tmp");

  return {
    mobileDir,
    sourceApk,
    finalApk,
    provenance,
    env: {
      ANDROID_SDK_ROOT: sdkRoot,
      EXPO_ANDROID_PACKAGE_PRIVATE: "ai.multica.private.test",
      MULTICA_ANDROID_SIGNING_MODE: "sideload-debug",
    },
  };
}

function fakeSpawn(fixture, failedStage, observed = {}) {
  return (command, args, options) => {
    if (command === "java") {
      return failedStage === "java"
        ? { status: 127, stdout: "", stderr: "java not found" }
        : { status: 0, stdout: "", stderr: 'openjdk version "17.0.12"' };
    }
    if (command === "git") {
      return args[0] === "rev-parse"
        ? { status: 0, stdout: `${gitCommit}\n`, stderr: "" }
        : { status: 0, stdout: " M apps/mobile/app.config.ts\n", stderr: "" };
    }
    if (command === "pnpm") {
      return { status: failedStage === "prebuild" ? 2 : 0 };
    }
    if (path.basename(command).startsWith("gradlew")) {
      observed.gradleCwd = options?.cwd;
      fs.mkdirSync(path.dirname(fixture.sourceApk), { recursive: true });
      fs.writeFileSync(fixture.sourceApk, failedStage === "gradle" ? "partial-apk" : "fresh-apk");
      return { status: failedStage === "gradle" ? 3 : 0 };
    }
    if (command === "node") {
      observed.verifierApk = args[1];
      return {
        status: failedStage === "verifier" ? 4 : 0,
        stdout: `Signer #1 certificate SHA-256 digest: ${certificateSha256}\n`,
        stderr: "",
      };
    }
    throw new Error(`Unexpected command: ${command}`);
  };
}

afterEach(() => {
  for (const fixtureDir of fixtureDirs.splice(0)) {
    fs.rmSync(fixtureDir, { recursive: true, force: true });
  }
});

describe("private Android APK publication", () => {
  for (const failedStage of ["java", "prebuild", "gradle", "verifier"]) {
    it(`removes source, final, provenance, and temporary APKs when ${failedStage} fails`, () => {
      const fixture = createFixture();

      expect(() =>
        runPrivateAndroidBuild({
          mobileDir: fixture.mobileDir,
          env: fixture.env,
          spawn: fakeSpawn(fixture, failedStage),
          uniqueId: () => "test-build",
        }),
      ).toThrow();

      expect(fs.existsSync(fixture.sourceApk)).toBe(false);
      expect(fs.existsSync(fixture.finalApk)).toBe(false);
      expect(fs.existsSync(fixture.provenance)).toBe(false);
      expect(fs.readdirSync(path.dirname(fixture.finalApk))).toEqual([]);
    });
  }

  it("publishes only the verified final APK and matching provenance", () => {
    const fixture = createFixture();
    const observed = {};

    const result = runPrivateAndroidBuild({
      mobileDir: fixture.mobileDir,
      env: fixture.env,
      spawn: fakeSpawn(fixture, undefined, observed),
      uniqueId: () => "test-build",
      now: () => new Date("2026-07-13T14:00:00.000Z"),
    });

    expect(result.apkPath).toBe(fixture.finalApk);
    expect(result.provenancePath).toBe(fixture.provenance);
    expect(fs.existsSync(fixture.sourceApk)).toBe(false);
    expect(fs.readFileSync(fixture.finalApk, "utf8")).toBe("fresh-apk");
    expect(observed.gradleCwd).toBe(path.join(fixture.mobileDir, "android"));
    expect(observed.verifierApk).not.toBe(fixture.finalApk);
    expect(observed.verifierApk).toMatch(/\.tmp$/);

    const provenance = JSON.parse(fs.readFileSync(fixture.provenance, "utf8"));
    expect(provenance).toMatchObject({
      schemaVersion: 1,
      buildId: "test-build",
      builtAt: "2026-07-13T14:00:00.000Z",
      gitCommit,
      gitDirty: true,
      applicationId: fixture.env.EXPO_ANDROID_PACKAGE_PRIVATE,
      certificateSha256,
      apkFile: path.basename(fixture.finalApk),
      verified: true,
    });
    expect(provenance.apkSha256).toBe(result.apkSha256);
    expect(fs.readdirSync(path.dirname(fixture.finalApk)).sort()).toEqual(
      [path.basename(fixture.finalApk), path.basename(fixture.provenance)].sort(),
    );
  });
});
