import path from "node:path";
import assert from "node:assert/strict";
import test from "node:test";

import {
  PRIVATE_MACOS_SIGNING_MODE,
  REQUIRED_PRIVATE_MACOS_ENV,
  packagePrivate,
  packagePrivateAndroid,
  packagePrivateMacos,
  privateAndroidBuildEnv,
  resolvePrivateBuildPaths,
  validatePrivateMacosEnvironment,
} from "./package-private.mjs";

function recordingSpawn(result = { status: 0 }) {
  const calls = [];
  return {
    calls,
    spawn(...args) {
      calls.push(args);
      return result;
    },
  };
}

function privateMacosReleaseEnv(overrides = {}) {
  return {
    PATH: "/bin",
    MULTICA_MACOS_SIGNING_MODE: PRIVATE_MACOS_SIGNING_MODE,
    CSC_LINK: "/secure/developer-id.p12",
    CSC_KEY_PASSWORD: "certificate-password",
    APPLE_ID: "release@example.test",
    APPLE_APP_SPECIFIC_PASSWORD: "app-password",
    APPLE_TEAM_ID: "TEAM123456",
    ...overrides,
  };
}

test("resolves Android dependencies outside task workspaces by default", () => {
  assert.deepEqual(resolvePrivateBuildPaths({ env: {}, home: "/home/tester" }), {
    buildRoot: "/home/tester/.multica-build",
    javaHome: "/home/tester/.multica-build/jdk17",
    androidSdkRoot: "/home/tester/.multica-build/android-sdk",
    gradleUserHome: "/home/tester/.multica-build/gradle-home",
  });
});

test("prepends fixed Java and adb paths while preserving explicit Android identity inputs", () => {
  const input = {
    PATH: "/usr/bin",
    EXPO_ANDROID_PACKAGE_PRIVATE: "owner.confirmed.package",
    MULTICA_ANDROID_SIGNING_MODE: "owner-confirmed-mode",
  };
  const result = privateAndroidBuildEnv({ env: input, home: "/home/tester" });

  assert.deepEqual(
    {
      JAVA_HOME: result.env.JAVA_HOME,
      ANDROID_SDK_ROOT: result.env.ANDROID_SDK_ROOT,
      ANDROID_HOME: result.env.ANDROID_HOME,
      GRADLE_USER_HOME: result.env.GRADLE_USER_HOME,
      EXPO_ANDROID_PACKAGE_PRIVATE: result.env.EXPO_ANDROID_PACKAGE_PRIVATE,
      MULTICA_ANDROID_SIGNING_MODE: result.env.MULTICA_ANDROID_SIGNING_MODE,
    },
    {
      JAVA_HOME: "/home/tester/.multica-build/jdk17",
      ANDROID_SDK_ROOT: "/home/tester/.multica-build/android-sdk",
      ANDROID_HOME: "/home/tester/.multica-build/android-sdk",
      GRADLE_USER_HOME: "/home/tester/.multica-build/gradle-home",
      EXPO_ANDROID_PACKAGE_PRIVATE: "owner.confirmed.package",
      MULTICA_ANDROID_SIGNING_MODE: "owner-confirmed-mode",
    },
  );
  assert.equal(
    result.env.PATH,
    [
      "/home/tester/.multica-build/jdk17/bin",
      "/home/tester/.multica-build/android-sdk/platform-tools",
      "/usr/bin",
    ].join(path.delimiter),
  );
});

test("uses the mainline development identity for a no-argument Android build", () => {
  const result = privateAndroidBuildEnv({
    env: { PATH: "/usr/bin" },
    home: "/home/tester",
  });

  assert.equal(result.env.EXPO_ANDROID_PACKAGE_PRIVATE, "ai.multica.mobile.dev");
  assert.equal(result.env.MULTICA_ANDROID_SIGNING_MODE, "sideload-debug");
});

test("runs the existing verified Android builder with fixed dependency paths", () => {
  const recorder = recordingSpawn();
  const env = {
    PATH: "/usr/bin",
    EXPO_ANDROID_PACKAGE_PRIVATE: "owner.confirmed.package",
    MULTICA_ANDROID_SIGNING_MODE: "owner-confirmed-mode",
  };

  packagePrivateAndroid({
    env,
    home: "/home/tester",
    exists: () => true,
    spawn: recorder.spawn,
    root: "/repo",
  });

  assert.equal(recorder.calls.length, 1);
  const [command, args, options] = recorder.calls[0];
  assert.equal(command, process.execPath);
  assert.deepEqual(args, ["/repo/apps/mobile/scripts/build-private-android.mjs"]);
  assert.equal(options.cwd, "/repo");
  assert.equal(options.env.JAVA_HOME, "/home/tester/.multica-build/jdk17");
  assert.equal(options.env.EXPO_ANDROID_PACKAGE_PRIVATE, "owner.confirmed.package");
});

test("the no-argument Android entry reaches the verified builder with safe defaults", () => {
  const recorder = recordingSpawn();

  packagePrivateAndroid({
    env: { PATH: "/usr/bin" },
    home: "/home/tester",
    exists: () => true,
    spawn: recorder.spawn,
    root: "/repo",
  });

  assert.equal(recorder.calls.length, 1);
  const [, , options] = recorder.calls[0];
  assert.equal(options.env.EXPO_ANDROID_PACKAGE_PRIVATE, "ai.multica.mobile.dev");
  assert.equal(options.env.MULTICA_ANDROID_SIGNING_MODE, "sideload-debug");
});

test("fails before spawning when a fixed Android dependency directory is missing", () => {
  const recorder = recordingSpawn();
  assert.throws(
    () =>
      packagePrivateAndroid({
        env: {},
        home: "/home/tester",
        exists: (directory) => !directory.endsWith("android-sdk"),
        spawn: recorder.spawn,
      }),
    /Android SDK is missing from fixed build root/,
  );
  assert.equal(recorder.calls.length, 0);
});

test("routes a controlled macOS release through the private desktop packager", () => {
  const recorder = recordingSpawn();
  const env = privateMacosReleaseEnv();
  packagePrivateMacos({
    platform: "darwin",
    spawn: recorder.spawn,
    root: "/repo",
    env,
  });

  assert.deepEqual(recorder.calls, [
    [
      "pnpm",
      [
        "--filter",
        "@multica/desktop",
        "package:private",
        "--",
        "--mac",
        "--publish",
        "never",
      ],
      { cwd: "/repo", env, stdio: "inherit" },
    ],
  ]);
  assert.equal(recorder.calls[0][1].includes("-c.mac.notarize=false"), false);
});

test("rejects every missing macOS signing or notarization input before spawning", async (t) => {
  for (const name of ["MULTICA_MACOS_SIGNING_MODE", ...REQUIRED_PRIVATE_MACOS_ENV]) {
    await t.test(name, () => {
      const recorder = recordingSpawn();
      const env = privateMacosReleaseEnv({ [name]: "" });

      assert.throws(
        () => packagePrivateMacos({ platform: "darwin", env, spawn: recorder.spawn }),
        new RegExp(name),
      );
      assert.equal(recorder.calls.length, 0);
    });
  }
});

test("rejects the unsigned smoke-build override before spawning", () => {
  const recorder = recordingSpawn();
  const env = privateMacosReleaseEnv({ CSC_IDENTITY_AUTO_DISCOVERY: "false" });

  assert.throws(
    () => packagePrivateMacos({ platform: "darwin", env, spawn: recorder.spawn }),
    /forbids CSC_IDENTITY_AUTO_DISCOVERY=false/,
  );
  assert.equal(recorder.calls.length, 0);
});

test("validates release inputs without returning or logging credential values", () => {
  assert.equal(validatePrivateMacosEnvironment(privateMacosReleaseEnv()), undefined);
});

test("rejects macOS packaging on non-macOS hosts before spawning", () => {
  const recorder = recordingSpawn();
  assert.throws(
    () => packagePrivateMacos({ platform: "linux", spawn: recorder.spawn }),
    /Private macOS packaging requires a macOS host/,
  );
  assert.equal(recorder.calls.length, 0);
});

test("rejects unknown package targets", () => {
  assert.throws(
    () => packagePrivate("windows"),
    /Usage: node scripts\/package-private\.mjs <android\|macos>/,
  );
});
