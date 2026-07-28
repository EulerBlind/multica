import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";

vi.mock("expo-file-system/legacy", () => ({
  cacheDirectory: "file:///cache/",
  getInfoAsync: vi.fn(async () => ({ exists: true, isDirectory: true })),
  makeDirectoryAsync: vi.fn(async () => undefined),
  downloadAsync: vi.fn(async () => ({
    uri: "file:///cache/attachments/att-1-report.pdf",
    status: 200,
    headers: {},
    mimeType: "application/pdf",
  })),
  getContentUriAsync: vi.fn(async (uri: string) => `content://multica/${uri}`),
}));

vi.mock("@/data/api", () => ({
  api: {
    getToken: vi.fn(() => "test-token"),
  },
}));

vi.mock("@/data/workspace-store", () => ({
  getCurrentSlug: vi.fn(() => "acme"),
}));

vi.mock("react-native", () => ({
  Alert: { alert: vi.fn() },
  Linking: { openURL: vi.fn(async () => undefined) },
  Platform: { OS: "android" },
}));

describe("downloadAttachmentAuthenticated", () => {
  const ORIGINAL_API = process.env.EXPO_PUBLIC_API_URL;

  beforeEach(() => {
    process.env.EXPO_PUBLIC_API_URL = "https://api.example.test";
    vi.clearAllMocks();
  });

  afterEach(() => {
    process.env.EXPO_PUBLIC_API_URL = ORIGINAL_API;
  });

  it("downloads against the API host with Bearer + workspace headers", async () => {
    const FileSystem = await import("expo-file-system/legacy");
    const { downloadAttachmentAuthenticated } = await import(
      "./download-attachment"
    );

    const result = await downloadAttachmentAuthenticated(
      "att-1",
      "report.pdf",
    );

    expect(result.localUri).toBe("file:///cache/attachments/att-1-report.pdf");
    expect(FileSystem.downloadAsync).toHaveBeenCalledWith(
      "https://api.example.test/api/attachments/att-1/download",
      "file:///cache/attachments/att-1-report.pdf",
      {
        headers: expect.objectContaining({
          Authorization: "Bearer test-token",
          "X-Workspace-Slug": "acme",
          "X-Client-Platform": "mobile",
        }),
      },
    );
  });

  it("rejects unauthenticated downloads instead of opening a bare URL", async () => {
    const { api } = await import("@/data/api");
    vi.mocked(api.getToken).mockReturnValueOnce(null);
    const { downloadAttachmentAuthenticated } = await import(
      "./download-attachment"
    );

    await expect(
      downloadAttachmentAuthenticated("att-1", "report.pdf"),
    ).rejects.toThrow(/Sign in/i);
  });

  it("maps 401/403 into a permission error", async () => {
    const FileSystem = await import("expo-file-system/legacy");
    vi.mocked(FileSystem.downloadAsync).mockResolvedValueOnce({
      uri: "file:///cache/attachments/att-1-report.pdf",
      status: 401,
      headers: {},
      mimeType: "application/pdf",
    } as never);
    const { downloadAttachmentAuthenticated } = await import(
      "./download-attachment"
    );

    await expect(
      downloadAttachmentAuthenticated("att-1", "report.pdf"),
    ).rejects.toThrow(/permission/i);
  });
});
