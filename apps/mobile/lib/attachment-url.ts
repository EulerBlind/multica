/**
 * Resolve a server-relative attachment URL against the configured API base.
 *
 * Background: when the backend has no CloudFront signer configured (e.g.
 * the self-hosted RustFS / private-S3 case in MUL-2976), `attachment.url`
 * and `attachment.download_url` come back as server-relative paths like
 * `/api/attachments/{id}/download`. Web is happy with that — same-origin
 * `<img src="/api/...">` resolves against the document base — but RN
 * needs an absolute http(s) URL for both `Linking.openURL` (`Cannot open
 * URL` otherwise) and `<Image source={{ uri }}>` (no document origin to
 * resolve against; the request is silently dropped).
 *
 * Mirrors `packages/core/workspace/avatar-url.ts:resolvePublicFileUrl`
 * exactly. We don't import the core helper because its `getBaseUrl()`
 * pulls from a singleton ApiClient that lives in `@multica/core/api` —
 * not on the mobile sharing whitelist (apps/mobile/CLAUDE.md "mirror,
 * don't import"). Mobile reads its own `EXPO_PUBLIC_API_URL` from the
 * Expo env, the same value the rest of `data/api.ts` uses.
 *
 * Contract:
 *   - null / undefined / "" → null (caller should treat as "no URL").
 *   - already-absolute URL  → returned unchanged.
 *   - server-relative path  → API base + path, with a single boundary
 *                             slash (we trim trailing slashes from the
 *                             base before joining).
 */

const API_URL = process.env.EXPO_PUBLIC_API_URL ?? "";
// User-facing page/download URLs (anything handed to `Linking.openURL` and
// opened in the system browser) resolve against the frontend `app_url`
// (`EXPO_PUBLIC_WEB_URL`), NOT the API `server_url`. The web app reverse-
// proxies `/api/*` to the backend (apps/web/next.config.ts `rewrites`), so
// a frontend-anchored attachment URL reaches the same download endpoint
// without exposing the private backend host to end users. API calls
// (data/api.ts) and native `<Image>` loads keep using `API_URL` directly.
// Falls back to `API_URL` when no web URL is configured (e.g. a dev build
// with only `EXPO_PUBLIC_API_URL` set) so downloads still work.
const DOWNLOAD_BASE_URL = process.env.EXPO_PUBLIC_WEB_URL || API_URL;

export function resolveAttachmentUrlWithBase(
  rawUrl: string | null | undefined,
  baseUrl: string,
): string | null {
  if (!rawUrl) return null;
  if (!rawUrl.startsWith("/")) return rawUrl;
  const trimmedBaseUrl = baseUrl.replace(/\/+$/, "");
  return `${trimmedBaseUrl}${rawUrl}`;
}

export function resolveAttachmentUrl(
  rawUrl: string | null | undefined,
): string | null {
  return resolveAttachmentUrlWithBase(rawUrl, API_URL);
}

/**
 * Resolve an attachment URL that will be opened as a page/download in the
 * system browser (`Linking.openURL`) against the frontend `app_url`.
 *
 * Distinct from `resolveAttachmentUrl` (which targets the API base for
 * native `<Image>` / avatar loads): browser-opened URLs must use the
 * user-facing frontend host so the backend `server_url` is never exposed
 * to the end user. See the private mobile build contract in
 * `scripts/build-private-android.mjs` (`EXPO_PUBLIC_WEB_URL` vs
 * `EXPO_PUBLIC_API_URL`).
 */
export function resolveAttachmentDownloadUrl(
  rawUrl: string | null | undefined,
): string | null {
  return resolveAttachmentUrlWithBase(rawUrl, DOWNLOAD_BASE_URL);
}
