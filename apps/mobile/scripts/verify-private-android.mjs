import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";

import { assertPrivateAndroidPermissions } from "./private-android-permissions.mjs";

const [apkArg, expectedPackage = "ai.multica.mobile.privateapp", expectedDisplayName = "multica"] = process.argv.slice(2);
if (!apkArg) {
  throw new Error("Usage: node verify-private-android.mjs <apk> [applicationId] [displayName]");
}

const apk = path.resolve(apkArg);
if (!fs.existsSync(apk)) throw new Error(`APK does not exist: ${apk}`);

const sdkRoot = process.env.ANDROID_SDK_ROOT ?? process.env.ANDROID_HOME;
if (!sdkRoot) throw new Error("ANDROID_SDK_ROOT or ANDROID_HOME is required");
const buildTools = path.join(sdkRoot, "build-tools", "36.0.0");
const aapt2 = path.join(buildTools, process.platform === "win32" ? "aapt2.exe" : "aapt2");
const apksigner = path.join(buildTools, process.platform === "win32" ? "apksigner.bat" : "apksigner");

function output(command, args, options = {}) {
  const result = spawnSync(command, args, {
    encoding: "utf8",
    maxBuffer: 128 * 1024 * 1024,
    ...options,
  });
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(" ")} failed:\n${result.stderr || result.stdout}`);
  }
  return result.stdout;
}

function requireMatch(value, pattern, message) {
  if (!pattern.test(value)) throw new Error(message);
}

const badging = output(aapt2, ["dump", "badging", apk]);
requireMatch(
  badging,
  new RegExp(`package: name='${expectedPackage.replaceAll(".", "\\.")}'`),
  `applicationId is not ${expectedPackage}`,
);
requireMatch(
  badging,
  new RegExp(`^application-label:'${expectedDisplayName.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}'$`, "m"),
  `application label is not ${expectedDisplayName}`,
);
const permissions = assertPrivateAndroidPermissions(badging);

const resources = output(aapt2, ["dump", "resources", apk]);
const launcherResource =
  resources.match(/^\s{4}resource 0x[0-9a-f]+ mipmap\/ic_launcher\n(?:^\s{6,}.*\n)*/m)?.[0] ?? "";
requireMatch(
  launcherResource,
  /\(anydpi-v26\) \(file\).*type=XML/,
  "Adaptive icon XML is missing",
);
requireMatch(resources, /resource 0x[0-9a-f]+ mipmap\/ic_launcher_foreground\b/, "Adaptive icon foreground is missing");
requireMatch(resources, /resource 0x[0-9a-f]+ mipmap\/ic_launcher_monochrome\b/, "Android 13 monochrome icon resource is missing");

const bundle = output("unzip", ["-p", apk, "assets/index.android.bundle"]);
// QIA-393 private contract: the API and web app share one unified domain
// (server_url == app_url). The bundle must contain that address and must not
// leak the legacy `-be` backend host or the public production endpoints.
const requiredUrls = [
  "https://direct.multica.elvisiky.com:3000",
];
for (const url of requiredUrls) {
  if (!bundle.includes(url)) throw new Error(`Private URL is missing from bundle: ${url}`);
}
for (const url of ["https://api.multica.ai", "https://multica.ai", "https://direct.multica-be.elvisiky.com:3000"]) {
  if (bundle.includes(url)) throw new Error(`Public production URL leaked into private bundle: ${url}`);
}

const signature = output(apksigner, ["verify", "--print-certs", apk]);
const sha256 = crypto.createHash("sha256").update(fs.readFileSync(apk)).digest("hex");

console.log(`APK: ${apk}`);
console.log(`applicationId: ${expectedPackage}`);
console.log(`displayName: ${expectedDisplayName}`);
console.log(`permissions (${permissions.length}): ${permissions.join(", ")}`);
console.log("permission assertions: CAMERA=yes, RECORD_AUDIO=no, SYSTEM_ALERT_WINDOW=no");
console.log("adaptiveIcon: yes; monochromeIcon: yes");
console.log(`private URLs: ${requiredUrls.join(", ")}`);
console.log(signature.trim());
console.log(`SHA-256: ${sha256}`);
