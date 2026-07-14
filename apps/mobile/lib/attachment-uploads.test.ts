import { describe, expect, it } from "vitest";
import { completedAttachmentIds } from "./attachment-uploads";

describe("attachment create payload", () => {
  it("includes completed ids only", () => {
    expect(
      completedAttachmentIds([
        { status: "completed", id: "attachment-1" },
        { status: "uploading" },
        { status: "failed", id: "attachment-failed" },
        { status: "completed" },
        { status: "completed", id: "attachment-2" },
      ]),
    ).toEqual(["attachment-1", "attachment-2"]);
  });
});
