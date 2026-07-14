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
    expect(privateConfig.name).toBe("Multica (Private)");
    expect(privateConfig.scheme).toBe("multica-private");
    expect(privateConfig.android?.package).toBe("ai.multica.mobile.privateapp");
    expect(privateConfig.android?.permissions).toContain("android.permission.CAMERA");

    for (const env of ["development", "staging", "production"]) {
      const config = configFor(env);
      expect(config.name).not.toBe("Multica (Private)");
      expect(config.scheme).toBe("multica");
      expect(config.android?.package).not.toBe("ai.multica.mobile.privateapp");
    }
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
