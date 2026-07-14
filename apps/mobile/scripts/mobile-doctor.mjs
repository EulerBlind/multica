import path from "node:path";
import { fileURLToPath } from "node:url";

import { validatePrivateAndroidEnvironment } from "./build-private-android.mjs";

export function runMobileDoctor({ platform, env = process.env } = {}) {
  if (platform !== "android") {
    throw new Error(
      "PLATFORM=android is required in this delivery; iOS archive and device validation are scheduled separately",
    );
  }
  const result = validatePrivateAndroidEnvironment({ env });
  return {
    platform,
    java: result.javaVersion.trim().split("\n")[0],
    sdkRoot: result.sdkRoot,
    compileSdk: 36,
    buildTools: "36.0.0",
    ndk: "27.1.12297006",
    applicationId: result.packageId,
    displayName: result.displayName,
    signingMode: result.signingMode,
  };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  try {
    console.log(JSON.stringify(runMobileDoctor({ platform: process.argv[2] ?? process.env.PLATFORM })));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
