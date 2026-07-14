import {
  attachmentIdFromDownloadURL,
  type Attachment,
} from "@multica/core/types";

/**
 * Mobile mirror of `packages/views/editor/utils/preview.ts`.
 *
 * Keep the classification table aligned with the web preview dispatcher and
 * `server/internal/handler/file.go:isTextPreviewable`. Mobile intentionally
 * owns the file because importing a view-layer module would pull DOM code into
 * the React Native bundle.
 */

export type AttachmentPreviewKind =
  | "image"
  | "pdf"
  | "video"
  | "audio"
  | "markdown"
  | "html"
  | "text";

export type AttachmentOpenMode = "in-app" | "system" | "download";

const TEXT_EXTENSIONS = new Set([
  "md", "markdown", "txt", "log", "csv", "tsv",
  "html", "htm", "json", "xml",
  "yml", "yaml", "toml", "ini", "conf",
  "dockerfile", "makefile", "gitignore",
  "sh", "bash", "zsh",
  "py", "rb", "go", "rs",
  "ts", "tsx", "js", "jsx", "mjs", "cjs",
  "css", "scss", "sass", "less", "sql",
  "java", "kt", "swift", "c", "cc", "cpp", "h", "hpp",
  "cs", "php", "lua", "vim",
]);

const TEXT_CONTENT_TYPES = new Set([
  "application/json",
  "application/javascript",
  "application/xml",
  "application/x-yaml",
  "application/yaml",
  "application/toml",
  "application/x-sh",
  "application/x-httpd-php",
]);

const TEXT_BASENAMES = new Set([
  "dockerfile",
  "makefile",
  ".env",
  ".gitignore",
]);

const VIDEO_EXTENSIONS = new Set([
  "mp4", "m4v", "mov", "webm", "mkv", "avi", "ogv",
]);
const AUDIO_EXTENSIONS = new Set([
  "mp3", "wav", "m4a", "ogg", "oga", "flac", "aac", "opus",
]);
const IMAGE_EXTENSIONS = new Set([
  "png", "jpg", "jpeg", "gif", "webp", "avif", "bmp", "ico", "svg",
]);

function extensionOf(filename: string): string {
  const base = filename.toLowerCase().split(/[\\/]/).pop() ?? "";
  const dot = base.lastIndexOf(".");
  return dot <= 0 ? "" : base.slice(dot + 1);
}

function basenameOf(filename: string): string {
  return (filename.toLowerCase().split(/[\\/]/).pop() ?? "").trim();
}

function normalizeContentType(contentType: string): string {
  const value = contentType.toLowerCase().trim();
  const semicolon = value.indexOf(";");
  return (semicolon >= 0 ? value.slice(0, semicolon) : value).trim();
}

function isTextLike(contentType: string, filename: string): boolean {
  const normalized = normalizeContentType(contentType);
  if (normalized.startsWith("text/")) return true;
  if (TEXT_CONTENT_TYPES.has(normalized)) return true;
  const extension = extensionOf(filename);
  if (extension && TEXT_EXTENSIONS.has(extension)) return true;
  return TEXT_BASENAMES.has(basenameOf(filename));
}

export function getAttachmentPreviewKind(
  contentType: string,
  filename: string,
): AttachmentPreviewKind | null {
  const normalized = normalizeContentType(contentType);
  const extension = extensionOf(filename);

  if (normalized === "application/pdf" || extension === "pdf") return "pdf";
  if (
    normalized.startsWith("video/") ||
    (extension && VIDEO_EXTENSIONS.has(extension))
  ) {
    return "video";
  }
  if (
    normalized.startsWith("audio/") ||
    (extension && AUDIO_EXTENSIONS.has(extension))
  ) {
    return "audio";
  }
  // SVG must stay an image; its XML/text shape must not send it to the text
  // renderer and lose the existing lightbox/system preview semantics.
  if (
    normalized.startsWith("image/") ||
    (extension && IMAGE_EXTENSIONS.has(extension))
  ) {
    return "image";
  }
  if (
    normalized === "text/markdown" ||
    extension === "md" ||
    extension === "markdown"
  ) {
    return "markdown";
  }
  if (
    normalized === "text/html" ||
    extension === "html" ||
    extension === "htm"
  ) {
    return "html";
  }
  return isTextLike(contentType, filename) ? "text" : null;
}

/** Text-backed kinds use the authenticated in-app screen. Binary media are
 * handed to the OS, while unknown types retain the existing download path. */
export function getAttachmentOpenMode(
  contentType: string,
  filename: string,
): AttachmentOpenMode {
  const kind = getAttachmentPreviewKind(contentType, filename);
  if (kind === "markdown" || kind === "html" || kind === "text") {
    return "in-app";
  }
  return kind ? "system" : "download";
}

function stripQueryAndFragment(url: string): string {
  return url.split(/[?#]/, 1)[0] ?? "";
}

/**
 * Resolve a rendered markdown link to an attachment on the same record.
 * Stable download URLs are matched by UUID; legacy raw, signed download and
 * markdown URLs are matched both exactly and without transient query/fragment
 * suffixes. External links that do not belong to an attachment return null.
 */
export function findAttachmentForUrl(
  url: string,
  attachments: Attachment[] | undefined,
): Attachment | null {
  if (!url || !attachments?.length) return null;

  const stableId = attachmentIdFromDownloadURL(url);
  if (stableId) {
    const byId = attachments.find(
      (attachment) => attachment.id.toLowerCase() === stableId.toLowerCase(),
    );
    if (byId) return byId;
  }

  const strippedUrl = stripQueryAndFragment(url);
  return (
    attachments.find((attachment) =>
      [
        attachment.url,
        attachment.download_url,
        attachment.markdown_url,
      ].some((candidate) => {
        if (!candidate) return false;
        return (
          candidate === url ||
          stripQueryAndFragment(candidate) === strippedUrl
        );
      }),
    ) ?? null
  );
}

const NON_CONTENT_HTML_ELEMENT_RE =
  /<(script|style|template|iframe|object|embed)\b[^>]*>[\s\S]*?<\/\1\s*>/gi;

const HTML_ENTITIES: Record<string, string> = {
  amp: "&",
  apos: "'",
  gt: ">",
  lt: "<",
  nbsp: " ",
  quot: '"',
};

function decodeHtmlEntities(value: string): string {
  return value.replace(
    /&(#x[\da-f]+|#\d+|[a-z][\w]+);/gi,
    (entity, body: string) => {
      if (body.startsWith("#")) {
        const hex = body[1]?.toLowerCase() === "x";
        const codePoint = Number.parseInt(
          body.slice(hex ? 2 : 1),
          hex ? 16 : 10,
        );
        if (
          Number.isInteger(codePoint) &&
          codePoint >= 0 &&
          codePoint <= 0x10ffff
        ) {
          return String.fromCodePoint(codePoint);
        }
        return "\uFFFD";
      }
      return HTML_ENTITIES[body.toLowerCase()] ?? entity;
    },
  );
}

/**
 * Convert uploaded HTML to inert native text. The result is rendered only by
 * React Native's Text component, never by WebView or another HTML interpreter,
 * so scripts, navigation, subframes and remote resource loads have no runtime
 * channel. The lightweight tag removal is for readability, not the security
 * boundary; even malformed markup that survives is displayed as plain text.
 */
export function htmlToStaticText(source: string): string {
  const text = source
    .replaceAll("\u0000", "\uFFFD")
    .replace(/<!--[\s\S]*?-->/g, "")
    .replace(NON_CONTENT_HTML_ELEMENT_RE, "")
    .replace(/<!doctype[^>]*>/gi, "")
    .replace(/<(?:br|hr)\b[^>]*\/?\s*>/gi, "\n")
    .replace(/<li\b[^>]*>/gi, "• ")
    .replace(
      /<\/(?:p|div|section|article|header|footer|h[1-6]|li|tr|table|ul|ol)\s*>/gi,
      "\n",
    )
    .replace(/<[^>]*>/g, "");

  return decodeHtmlEntities(text)
    .replace(/\r\n?/g, "\n")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}
