export type ClientOS = "android" | "ios" | "unknown";

export function normalizeClientOS(platform: string): ClientOS {
  if (platform === "android" || platform === "ios") return platform;
  return "unknown";
}
