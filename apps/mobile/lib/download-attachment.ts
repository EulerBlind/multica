/**
 * Authenticated attachment download for mobile.
 *
 * Opening `/api/attachments/:id/download` via `Linking.openURL` (system
 * browser) fails on private deployments — the browser has no App Bearer
 * token, so the reverse-proxied download returns 401. This helper fetches
 * through `expo-file-system/legacy` with the same Authorization +
 * X-Workspace-Slug headers the ApiClient uses, writes to the app cache,
 * then hands the local file to the OS viewer via a content:// URI
 * (Android) or file:// (iOS).
 *
 * Download base is always `EXPO_PUBLIC_API_URL` (server_url): the request
 * carries the session, so we talk to the API host directly. Do NOT fall
 * back to bare `Linking.openURL` of the web app_url.
 */
import { Alert, Linking, Platform } from "react-native";
import * as FileSystem from "expo-file-system/legacy";
import { api } from "@/data/api";
import { getCurrentSlug } from "@/data/workspace-store";

const CLIENT_OS = Platform.OS === "ios" ? "ios" : "android";
const CLIENT_VERSION =
  process.env.EXPO_PUBLIC_MULTICA_MOBILE_VERSION ?? "0.1.0";

function sanitizeFilename(name: string): string {
  const trimmed = name.trim() || "download";
  // Strip path separators and control chars; keep unicode letters.
  return trimmed.replace(/[\\/:*?"<>|\u0000-\u001f]/g, "_").slice(0, 180);
}

function buildDownloadUrl(attachmentId: string): string {
  const base = (process.env.EXPO_PUBLIC_API_URL ?? "").replace(/\/+$/, "");
  if (!base) {
    throw new Error("EXPO_PUBLIC_API_URL is not set");
  }
  return `${base}/api/attachments/${attachmentId}/download`;
}

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = {
    "X-Client-Platform": "mobile",
    "X-Client-OS": CLIENT_OS,
    "X-Client-Version": CLIENT_VERSION,
  };
  const token = api.getToken();
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  const slug = getCurrentSlug();
  if (slug) {
    headers["X-Workspace-Slug"] = slug;
  }
  return headers;
}

export interface DownloadAttachmentResult {
  localUri: string;
  status: number;
}

/**
 * Download an attachment with the current session and return the local
 * cache URI. Throws on missing auth, non-2xx, or filesystem failures.
 */
export async function downloadAttachmentAuthenticated(
  attachmentId: string,
  filename: string,
): Promise<DownloadAttachmentResult> {
  if (!api.getToken()) {
    throw new Error("Sign in to download attachments");
  }
  const cacheRoot = FileSystem.cacheDirectory;
  if (!cacheRoot) {
    throw new Error("File cache is unavailable on this device");
  }
  const dir = `${cacheRoot}attachments/`;
  const dirInfo = await FileSystem.getInfoAsync(dir);
  if (!dirInfo.exists) {
    await FileSystem.makeDirectoryAsync(dir, { intermediates: true });
  }
  const safeName = sanitizeFilename(filename);
  const dest = `${dir}${attachmentId}-${safeName}`;
  const result = await FileSystem.downloadAsync(
    buildDownloadUrl(attachmentId),
    dest,
    { headers: authHeaders() },
  );
  if (result.status < 200 || result.status >= 300) {
    throw new Error(
      result.status === 401 || result.status === 403
        ? "No permission to download this attachment"
        : `Download failed (HTTP ${result.status})`,
    );
  }
  return { localUri: result.uri, status: result.status };
}

/** Open a local file URI with the system viewer / share sheet. */
export async function openLocalAttachment(localUri: string): Promise<void> {
  if (Platform.OS === "android") {
    const contentUri = await FileSystem.getContentUriAsync(localUri);
    await Linking.openURL(contentUri);
    return;
  }
  await Linking.openURL(localUri);
}

/**
 * Download with auth, then open. Surfaces failures via Alert so call
 * sites can fire-and-forget from press handlers.
 */
export async function downloadAndOpenAttachment(
  attachmentId: string,
  filename: string,
): Promise<void> {
  try {
    const { localUri } = await downloadAttachmentAuthenticated(
      attachmentId,
      filename,
    );
    await openLocalAttachment(localUri);
  } catch (error) {
    const message =
      error instanceof Error ? error.message : "Download failed";
    Alert.alert("Download failed", message);
  }
}
