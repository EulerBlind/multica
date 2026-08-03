import { afterEach, describe, expect, it } from "vitest";
import createConfig from "../app.config";

const originalEnv = { ...process.env };

afterEach(() => {
  process.env = { ...originalEnv };
});

function configFor(env: string) {
  process.env.APP_ENV = env;
  return createConfig({ config: {} } as Parameters<typeof createConfig>[0]);
}

describe("mobile app variants", () => {
  it("isolates the private package, name, and scheme", () => {
    const privateConfig = configFor("private");
    expect(privateConfig.name).toBe("multica");
    expect(privateConfig.scheme).toBe("multica-private");
    expect(privateConfig.android?.package).toBe("ai.multica.mobile.privateapp");
    expect(privateConfig.ios?.bundleIdentifier).toBe("ai.multica.mobile.privateapp");
    expect(privateConfig.android?.permissions).toContain("android.permission.CAMERA");
    expect(privateConfig.android?.blockedPermissions).toContain(
      "android.permission.SYSTEM_ALERT_WINDOW",
    );

    for (const env of ["development", "staging", "production"]) {
      const config = configFor(env);
      expect(config.name).not.toBe("Multica (Private)");
      expect(config.scheme).toBe("multica");
      expect(config.android?.package).not.toBe("ai.multica.mobile.privateapp");
      expect(config.ios?.bundleIdentifier).not.toBe("ai.multica.mobile.privateapp");
    }
  });

  it("applies one controlled version identity to Android and iOS", () => {
    process.env.MULTICA_MOBILE_VERSION = "0.4.1";
    process.env.MULTICA_MOBILE_BUILD_NUMBER = "4157";

    const config = configFor("private");
    expect(config.version).toBe("0.4.1");
    expect(config.android?.versionCode).toBe(4157);
    expect(config.ios?.buildNumber).toBe("4157");
  });

  it("rejects malformed controlled version inputs", () => {
    process.env.MULTICA_MOBILE_VERSION = "v0.4.1-57-gdeadbee";
    expect(() => configFor("private")).toThrow(/must be X\.Y\.Z/);

    process.env.MULTICA_MOBILE_VERSION = "0.4.1";
    process.env.MULTICA_MOBILE_BUILD_NUMBER = "0";
    expect(() => configFor("private")).toThrow(/positive integer/);
  });

  it("enables still-photo camera access without microphone access", () => {
    const config = configFor("private");
    const picker = config.plugins?.find(
      (plugin) => Array.isArray(plugin) && plugin[0] === "expo-image-picker",
    );
    expect(picker).toEqual([
      "expo-image-picker",
      expect.objectContaining({
        cameraPermission: expect.any(String),
        microphonePermission: false,
      }),
    ]);
  });
});
