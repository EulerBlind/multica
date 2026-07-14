import { describe, expect, it } from "vitest";

import {
  assertPrivateAndroidPermissions,
  parseAndroidPermissions,
} from "./private-android-permissions.mjs";

const safeBadging = `package: name='ai.multica.mobile.privateapp'
uses-permission: name='android.permission.INTERNET'
uses-permission: name='android.permission.CAMERA'
uses-permission-sdk-23: name='android.permission.USE_BIOMETRIC'
uses-permission: name='android.permission.CAMERA'
`;

describe("private Android final permission assertions", () => {
  it("enumerates and de-duplicates every permission reported by the final APK", () => {
    expect(parseAndroidPermissions(safeBadging)).toEqual([
      "android.permission.CAMERA",
      "android.permission.INTERNET",
      "android.permission.USE_BIOMETRIC",
    ]);
  });

  it("accepts the required camera permission without forbidden permissions", () => {
    expect(assertPrivateAndroidPermissions(safeBadging)).toEqual([
      "android.permission.CAMERA",
      "android.permission.INTERNET",
      "android.permission.USE_BIOMETRIC",
    ]);
  });

  it.each(["RECORD_AUDIO", "SYSTEM_ALERT_WINDOW"])(
    "rejects android.permission.%s in the final APK",
    (permission) => {
      expect(() =>
        assertPrivateAndroidPermissions(
          `${safeBadging}uses-permission: name='android.permission.${permission}'\n`,
        ),
      ).toThrow(`android.permission.${permission} must not be present`);
    },
  );

  it("rejects a final APK without the required camera permission", () => {
    expect(() =>
      assertPrivateAndroidPermissions("uses-permission: name='android.permission.INTERNET'\n"),
    ).toThrow("android.permission.CAMERA permission is missing");
  });
});
