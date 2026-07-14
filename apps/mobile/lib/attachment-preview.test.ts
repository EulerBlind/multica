import { describe, expect, it } from "vitest";
import type { Attachment } from "@multica/core/types";
import {
  findAttachmentForUrl,
  getAttachmentOpenMode,
  getAttachmentPreviewKind,
  htmlToStaticText,
} from "./attachment-preview";

const ATTACHMENT_ID = "72f4ac6b-9b1e-43fa-8304-0c20d8e65d91";

function attachment(overrides: Partial<Attachment> = {}): Attachment {
  return {
    id: ATTACHMENT_ID,
    workspace_id: "ws-1",
    issue_id: "issue-1",
    comment_id: null,
    chat_session_id: null,
    chat_message_id: null,
    uploader_type: "member",
    uploader_id: "member-1",
    filename: "report.html",
    url: "https://files.example/uploads/report.html?sig=raw",
    download_url:
      "https://files.example/api/attachments/72f4ac6b-9b1e-43fa-8304-0c20d8e65d91/download?sig=current",
    markdown_url:
      "https://private.example/api/attachments/72f4ac6b-9b1e-43fa-8304-0c20d8e65d91/download",
    content_type: "text/html",
    size_bytes: 128,
    created_at: "2026-07-13T00:00:00Z",
    ...overrides,
  };
}

describe("getAttachmentPreviewKind", () => {
  it.each([
    ["text/markdown", "notes.bin", "markdown"],
    ["text/plain; charset=utf-8", "README.md", "markdown"],
    ["text/html", "report.bin", "html"],
    ["application/octet-stream", "report.html", "html"],
    ["application/json", "payload.bin", "text"],
    ["text/plain", "Dockerfile", "text"],
    ["application/pdf", "report.bin", "pdf"],
    ["video/mp4", "clip.bin", "video"],
    ["audio/mpeg", "recording.bin", "audio"],
  ] as const)("classifies %s / %s as %s", (contentType, filename, expected) => {
    expect(getAttachmentPreviewKind(contentType, filename)).toBe(expected);
  });

  it("keeps SVG in the image path instead of treating it as XML text", () => {
    expect(getAttachmentPreviewKind("image/svg+xml", "diagram.svg")).toBe(
      "image",
    );
  });

  it("returns null for an unsupported binary", () => {
    expect(
      getAttachmentPreviewKind("application/zip", "archive.zip"),
    ).toBeNull();
  });
});

describe("getAttachmentOpenMode", () => {
  it("opens Markdown, HTML and text through the authenticated in-app route", () => {
    expect(getAttachmentOpenMode("text/markdown", "notes.md")).toBe(
      "in-app",
    );
    expect(getAttachmentOpenMode("text/html", "report.html")).toBe("in-app");
    expect(getAttachmentOpenMode("application/json", "data.json")).toBe(
      "in-app",
    );
  });

  it("delegates binary media preview to the system", () => {
    expect(getAttachmentOpenMode("application/pdf", "report.pdf")).toBe(
      "system",
    );
  });

  it("preserves download fallback for unsupported types", () => {
    expect(getAttachmentOpenMode("application/zip", "archive.zip")).toBe(
      "download",
    );
  });
});

describe("findAttachmentForUrl", () => {
  it.each([
    { shape: "stable path", url: `/api/attachments/${ATTACHMENT_ID}/download` },
    { shape: "markdown_url", url: attachment().markdown_url },
    { shape: "raw url", url: attachment().url },
    { shape: "current download_url", url: attachment().download_url },
    {
      shape: "expired signature on the same raw url",
      url: "https://files.example/uploads/report.html?sig=expired",
    },
  ])("matches $shape", ({ url }) => {
    expect(findAttachmentForUrl(url, [attachment()])?.id).toBe(ATTACHMENT_ID);
  });

  it("does not claim an unrelated external link", () => {
    expect(
      findAttachmentForUrl("https://docs.example/report.html", [attachment()]),
    ).toBeNull();
  });
});

describe("htmlToStaticText", () => {
  it("keeps readable content while removing every executable/network attack vector", () => {
    const source = `<!doctype html>
      <h1>Quarterly report</h1>
      <script>
        top.location = "https://evil.test/top";
        location.href = "https://evil.test/self";
        fetch("https://evil.test/fetch");
        window.open("https://evil.test/popup");
      </script>
      <iframe src="https://evil.test/frame"><p>frame fallback</p></iframe>
      <img src="https://evil.test/pixel" onerror="alert(1)">
      <a href="https://evil.test/link">Read report</a>`;
    const text = htmlToStaticText(source);

    expect(text).toContain("Quarterly report");
    expect(text).toContain("Read report");
    expect(text).not.toContain("https://evil.test");
    expect(text).not.toContain("fetch(");
    expect(text).not.toContain("location");
    expect(text).not.toContain("frame fallback");
  });
});
